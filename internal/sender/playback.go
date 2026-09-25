package sender

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"howett.net/plist"
)

// URL playback ("AirPlay video") on AirPlay 2 receivers. The receiver fetches
// and decodes the media itself; the sender only drives the session. The order
// follows pyatv's AirPlayV2.play_url, which is known to work on current tvOS:
//
//	pair-verify (encrypted control) → control SETUP (NTP timing port)
//	→ event channel on eventPort → /feedback every 2s → RECORD
//	→ POST /play → setProperty → /rate?value=1
//
// The control connection must remain open for the whole playback; closing it
// stops the video on the receiver.
const (
	playbackFeedbackInterval = 2 * time.Second
	playbackPlayRetries      = 3
	playbackPlayRetryDelay   = time.Second
	playbackPollInterval     = time.Second
	// playbackStartGrace is how many polls without a duration are tolerated
	// before assuming playback never started (pyatv uses 5).
	playbackStartGrace = 5

	playbackContentType     = "application/x-apple-binary-plist"
	playbackSourceVersion   = "690.7.1"
	playbackSenderModel     = "iPhone14,3"
	playbackOSName          = "iPhone OS"
	playbackOSVersion       = "16.5"
	playbackOSBuildVersion  = "20F66"
	playbackClientBundleID  = "network.foxden.foxCast"
	playbackStreamType      = 1
	playbackReferenceLimits = 3
)

// PlaybackConfig configures a URL playback session.
type PlaybackConfig struct {
	// StartSeconds is the initial playback position in seconds.
	StartSeconds float64
	// PortMin/PortMax optionally confine the local UDP timing port.
	PortMin, PortMax int
}

// PlaybackInfo is the subset of GET /playback-info used by the sender.
type PlaybackInfo struct {
	Duration    float64 `plist:"duration"`
	Position    float64 `plist:"position"`
	Rate        float64 `plist:"rate"`
	ReadyToPlay bool    `plist:"readyToPlay"`
	// HasDuration reports whether the receiver returned a duration, i.e.
	// whether an item is currently loaded.
	HasDuration bool `plist:"-"`
	// Error is set when the receiver reports a playback error.
	Error *PlaybackError `plist:"error"`
}

// PlaybackError is a receiver-reported playback failure.
type PlaybackError struct {
	Code   int64  `plist:"code"`
	Domain string `plist:"domain"`
}

func (e *PlaybackError) Error() string {
	return fmt.Sprintf("receiver playback error %d (%s)", e.Code, e.Domain)
}

// PlaybackSession is an active URL playback session.
type PlaybackSession struct {
	client     *AirPlayClient
	sessionURI string
	eventConn  net.Conn
	timingConn net.PacketConn
	cancel     context.CancelFunc
	workers    sync.WaitGroup
	closeOnce  sync.Once
}

// playbackSessionPlist is the control SETUP body for URL playback.
func (c *AirPlayClient) playbackSessionPlist(timingPort int) map[string]interface{} {
	deviceID := uuidToMAC(c.sessionID)
	return map[string]interface{}{
		"deviceID":                 deviceID,
		"macAddress":               deviceID,
		"sessionUUID":              strings.ToUpper(generateUUID()),
		"timingPort":               int64(timingPort),
		"timingProtocol":           timingProtocolNTP,
		"isMultiSelectAirPlay":     true,
		"groupContainsGroupLeader": false,
		"model":                    playbackSenderModel,
		"name":                     pairingClientName(),
		"osBuildVersion":           playbackOSBuildVersion,
		"osName":                   playbackOSName,
		"osVersion":                playbackOSVersion,
		"senderSupportsRelay":      false,
		"sourceVersion":            playbackSourceVersion,
		"statsCollectionEnabled":   false,
	}
}

// playPlist is the POST /play body.
func (c *AirPlayClient) playPlist(url string, startSeconds float64) map[string]interface{} {
	return map[string]interface{}{
		"Content-Location":                   url,
		"Start-Position-Seconds":             startSeconds,
		"uuid":                               strings.ToUpper(generateUUID()),
		"streamType":                         int64(playbackStreamType),
		"mediaType":                          "file",
		"rate":                               1.0,
		"volume":                             1.0,
		"playbackRestrictions":               int64(0),
		"referenceRestrictions":              int64(playbackReferenceLimits),
		"mightSupportStorePastisKeyRequests": true,
		"SenderMACAddress":                   uuidToMAC(c.sessionID),
		"model":                              playbackSenderModel,
		"clientBundleID":                     playbackClientBundleID,
		"clientProcName":                     playbackClientBundleID,
		"osBuildVersion":                     playbackOSBuildVersion,
	}
}

// playbackHeaders are the extra headers Apple senders put on media-control
// requests.
func (c *AirPlayClient) playbackHeaders() map[string]string {
	headers := map[string]string{
		"X-Apple-ProtocolVersion": "1",
		"X-Apple-Stream-ID":       "1",
	}
	if c.sessionID != "" {
		headers["X-Apple-Session-ID"] = c.sessionID
	}
	return headers
}

// PlayURL sets up a playback session and instructs the receiver to play url.
// The client must already be paired (pair-verify complete). The returned
// session must be closed to stop playback.
func (c *AirPlayClient) PlayURL(ctx context.Context, url string, cfg PlaybackConfig) (*PlaybackSession, error) {
	ports, err := allocateConsecutiveUDPPortsInRange(1, cfg.PortMin, cfg.PortMax)
	if err != nil {
		return nil, fmt.Errorf("allocate timing port: %w", err)
	}
	timingConn := ports[0]
	timingPort := timingConn.LocalAddr().(*net.UDPAddr).Port

	sessionCtx, cancel := context.WithCancel(ctx)
	s := &PlaybackSession{
		client:     c,
		sessionURI: fmt.Sprintf("rtsp://%s:%d/%d", c.host, c.port, time.Now().UnixNano()&0x7FFFFFFFFFFFFFFF),
		timingConn: timingConn,
		cancel:     cancel,
	}
	ok := false
	defer func() {
		if !ok {
			s.shutdown(false)
		}
	}()

	s.startWorker(func() { ntpTimingResponder(sessionCtx, timingConn) })

	setupResp, _, _, err := c.requestSetup(s.sessionURI, "playback", c.playbackSessionPlist(timingPort))
	if err != nil {
		return nil, err
	}
	if receiverTimingPort := plistInt(setupResp["timingPort"]); receiverTimingPort > 0 {
		s.startWorker(func() { sendNTPTimingProbes(sessionCtx, timingConn, c.host, receiverTimingPort) })
	}

	// The event channel carries the receiver's keep-alive commands. Without
	// it RECORD fails and the session is torn down after ~30s.
	if eventPort := plistInt(setupResp["eventPort"]); eventPort > 0 {
		s.eventConn, err = c.connectEventChannel(sessionCtx, eventPort, nil)
		if err != nil {
			return nil, err
		}
	}

	s.startWorker(func() { s.feedbackLoop(sessionCtx) })

	recordHeaders := map[string]string{
		"Range":    "npt=0-",
		"RTP-Info": "seq=0;rtptime=0",
	}
	if _, _, err := c.rtspRequest("RECORD", s.sessionURI, "", nil, recordHeaders); err != nil {
		return nil, fmt.Errorf("RECORD: %w", err)
	}

	if err := s.play(url, cfg.StartSeconds); err != nil {
		return nil, err
	}

	// Most of these mirror what Apple senders do; /rate is required, as the
	// item otherwise loads paused.
	if err := s.SetProperty("isInterestedInDateRange", true); err != nil {
		dbg("[PLAY] setProperty isInterestedInDateRange: %v", err)
	}
	if err := s.SetProperty("actionAtItemEnd", int64(0)); err != nil {
		dbg("[PLAY] setProperty actionAtItemEnd: %v", err)
	}
	if err := s.Rate(1.0); err != nil {
		return nil, err
	}

	ok = true
	return s, nil
}

func (s *PlaybackSession) play(url string, startSeconds float64) error {
	body, err := plist.Marshal(s.client.playPlist(url, startSeconds), plist.BinaryFormat)
	if err != nil {
		return fmt.Errorf("marshal /play: %w", err)
	}
	for attempt := 1; ; attempt++ {
		_, _, err = s.client.controlRequest("POST", "/play", protocolHTTP, playbackContentType, body, s.client.playbackHeaders())
		var statusErr *HTTPStatusError
		// Receivers intermittently answer /play with 500; retrying is what
		// Apple-compatible senders do.
		if err == nil || !errors.As(err, &statusErr) || statusErr.StatusCode != 500 || attempt >= playbackPlayRetries {
			break
		}
		dbg("[PLAY] /play returned 500, retry %d of %d", attempt, playbackPlayRetries)
		time.Sleep(playbackPlayRetryDelay)
	}
	if err != nil {
		return fmt.Errorf("POST /play: %w", err)
	}
	return nil
}

func (s *PlaybackSession) feedbackLoop(ctx context.Context) {
	ticker := time.NewTicker(playbackFeedbackInterval)
	defer ticker.Stop()
	for {
		// Feedback is best effort; the event channel is the real keep-alive.
		if _, _, err := s.client.rtspRequest("POST", "/feedback", "", nil, nil); err != nil {
			dbg("[FEEDBACK] error: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *PlaybackSession) startWorker(worker func()) {
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		worker()
	}()
}

func (s *PlaybackSession) command(method, path, contentType string, body []byte) ([]byte, error) {
	resp, _, err := s.client.controlRequest(method, path, protocolRTSP, contentType, body, s.client.playbackHeaders())
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

// Rate sets the playback rate: 0 pauses, 1 plays at normal speed.
func (s *PlaybackSession) Rate(rate float64) error {
	_, err := s.command("POST", fmt.Sprintf("/rate?value=%f", rate), "", nil)
	return err
}

// Scrub seeks to position seconds from the start.
func (s *PlaybackSession) Scrub(position float64) error {
	_, err := s.command("POST", fmt.Sprintf("/scrub?position=%f", position), "", nil)
	return err
}

// SetProperty sets a receiver playback property.
func (s *PlaybackSession) SetProperty(name string, value interface{}) error {
	body, err := plist.Marshal(map[string]interface{}{"value": value}, plist.BinaryFormat)
	if err != nil {
		return fmt.Errorf("marshal setProperty %s: %w", name, err)
	}
	_, err = s.command("PUT", "/setProperty?"+name, playbackContentType, body)
	return err
}

// Info queries the receiver's current playback state.
func (s *PlaybackSession) Info() (*PlaybackInfo, error) {
	body, err := s.command("GET", "/playback-info", "", nil)
	if err != nil {
		return nil, err
	}
	info := &PlaybackInfo{}
	if len(body) == 0 {
		return info, nil
	}
	if _, err := plist.Unmarshal(body, info); err != nil {
		return nil, fmt.Errorf("decode /playback-info: %w", err)
	}
	var raw map[string]interface{}
	if _, err := plist.Unmarshal(body, &raw); err == nil {
		_, info.HasDuration = raw["duration"]
	}
	return info, nil
}

// Wait polls the receiver until playback ends, ctx is cancelled, or the
// receiver reports an error.
func (s *PlaybackSession) Wait(ctx context.Context) error {
	grace := playbackStartGrace
	started := false
	ticker := time.NewTicker(playbackPollInterval)
	defer ticker.Stop()
	for {
		info, err := s.Info()
		if err != nil {
			return err
		}
		if info.Error != nil {
			return info.Error
		}
		switch {
		case info.HasDuration:
			started = true
		case started:
			return nil
		default:
			grace--
			if grace < 0 {
				return fmt.Errorf("playback did not start")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Close stops playback and tears the session down.
func (s *PlaybackSession) Close() error {
	s.shutdown(true)
	return nil
}

func (s *PlaybackSession) shutdown(stop bool) {
	s.closeOnce.Do(func() {
		s.cancel()
		s.workers.Wait()
		if stop {
			if _, err := s.command("POST", "/stop", "", nil); err != nil {
				dbg("[PLAY] stop: %v", err)
			}
			if _, _, err := s.client.rtspRequest("TEARDOWN", s.sessionURI, "", nil, nil); err != nil {
				dbg("[TEARDOWN] error: %v", err)
			}
		}
		if s.eventConn != nil {
			_ = s.eventConn.Close()
		}
		_ = s.timingConn.Close()
	})
}
