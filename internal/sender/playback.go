package sender

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"

	"howett.net/plist"
)

// URL playback ("AirPlay video") on AirPlay 2 receivers. The receiver fetches
// and decodes the media itself; the sender only drives the session. Current
// tvOS (26/27) needs the play-queue flow (pyatv PR #2899):
//
//	pair-verify (encrypted control) → SETUP (PTP video session)
//	→ event channel on eventPort → RECORD → GET /info → SETUP (remote
//	control session) → POST /command insertPlayQueueItem, setProperty,
//	setRate → /feedback every 2s
//
// Older receivers get pyatv's AirPlayV2.play_url order instead:
//
//	pair-verify → SETUP (NTP timing port) → event channel on eventPort
//	→ /feedback every 2s → RECORD → POST /play → setProperty → /rate?value=1
//
// The control connection must remain open for the whole playback; closing it
// stops the video on the receiver.
const (
	playbackFeedbackInterval = 2 * time.Second
	playbackPlayRetries      = 3
	playbackPlayRetryDelay   = time.Second
	playbackPollInterval     = time.Second
	// playbackStartGrace is how many polls without a duration are tolerated
	// before assuming playback never started. pyatv uses 5, but an HLS item
	// (e.g. a transmuxed 4K file) can take longer to load.
	playbackStartGrace = 15

	playbackContentType     = "application/x-apple-binary-plist"
	playbackSourceVersion   = "690.7.1"
	playbackSenderModel     = "iPhone14,3"
	playbackOSName          = "iPhone OS"
	playbackOSVersion       = "16.5"
	playbackOSBuildVersion  = "20F66"
	playbackClientBundleID  = "network.foxden.foxCast"
	playbackStreamType      = 1
	playbackReferenceLimits = 3

	// Remote control session registration (type 130, controlType 1); the
	// MRP data channel uses controlType 2 and a different client type UUID.
	remoteControlStreamType  = 130
	remoteControlSessionType = 1
	remoteControlClientType  = "A6B27562-B43A-4F2D-B75F-82391E250194"

	playbackStateStopped = "stopped"
	playbackStateBuffer  = 16
	playbackStartTimeout = 30 * time.Second
)

// PlaybackConfig configures a URL playback session.
type PlaybackConfig struct {
	// StartSeconds is the initial playback position in seconds.
	StartSeconds float64
	// PortMin/PortMax optionally confine the local UDP timing port (legacy
	// NTP sessions only; PTP sessions open no local port).
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
	// dacpHeaders identify this sender on every session request, as pyatv
	// (and Apple senders) do.
	dacpHeaders map[string]string
	// queue is set for play-queue (POST /command) sessions.
	queue       bool
	rcsStreamID int
	itemUUID    string
	// state receives playbackState names pushed by the receiver.
	state chan string
	// lost is closed when the receiver drops the control connection.
	lost       chan struct{}
	lostErr    error
	eventConn  net.Conn
	timingConn net.PacketConn
	cancel     context.CancelFunc
	workers    sync.WaitGroup
	closeOnce  sync.Once
}

// newDACPHeaders returns random DACP-ID/Active-Remote/Client-Instance
// headers identifying one playback session.
func newDACPHeaders() map[string]string {
	dacpID := fmt.Sprintf("%X", mathrand.Uint64())
	return map[string]string{
		"DACP-ID":         dacpID,
		"Active-Remote":   fmt.Sprintf("%d", mathrand.Uint32()),
		"Client-Instance": dacpID,
	}
}

// localIP is this side's address on the control connection.
func (c *AirPlayClient) localIP() string {
	if c.conn != nil {
		if addr, ok := c.conn.LocalAddr().(*net.TCPAddr); ok {
			return addr.IP.String()
		}
	}
	return "127.0.0.1"
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
//
// Current tvOS (26/27) no longer loads items sent with POST /play: the item
// is created (a spinner shows) but never fetched. It expects a PTP-timed
// video session, a remote control session registered on the same connection,
// and the item inserted into a play queue with POST /command (pyatv PR #2899;
// see docs/05-video-url-playback.md). Receivers that reject either setup fall
// back to the legacy NTP + POST /play flow.
func (c *AirPlayClient) PlayURL(ctx context.Context, url string, cfg PlaybackConfig) (*PlaybackSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &PlaybackSession{
		client:      c,
		sessionURI:  fmt.Sprintf("rtsp://%s/%d", c.localIP(), mathrand.Uint32()),
		dacpHeaders: newDACPHeaders(),
		cancel:      cancel,
		itemUUID:    strings.ToUpper(generateUUID()),
		state:       make(chan string, playbackStateBuffer),
		lost:        make(chan struct{}),
	}
	ok := false
	defer func() {
		if !ok {
			s.shutdown(false)
		}
	}()

	setupResp, err := s.setup(c.videoSessionPlist())
	if err != nil {
		var statusErr *HTTPStatusError
		if !errors.As(err, &statusErr) {
			return nil, err
		}
		dbg("[PLAY] PTP video session rejected (%v), using legacy setup", err)
		if err := s.startLegacy(sessionCtx, url, cfg); err != nil {
			return nil, err
		}
		ok = true
		return s, nil
	}
	if eventPort := plistInt(setupResp["eventPort"]); eventPort > 0 {
		s.eventConn, err = c.connectEventChannelWithHandler(sessionCtx, eventPort, nil, s.handleEvent)
		if err != nil {
			return nil, err
		}
	}
	if err := s.record(); err != nil {
		return nil, err
	}

	if s.setupRemoteControl() {
		s.queue = true
		if err := s.playQueue(url, cfg.StartSeconds); err != nil {
			return nil, err
		}
	} else {
		dbg("[PLAY] no remote control session, falling back to POST /play")
		if err := s.startPlay(url, cfg.StartSeconds); err != nil {
			return nil, err
		}
	}
	// Feedback shares the control connection; it must not start before the
	// play commands, or the receiver drops the connection mid-queue.
	s.startWorker(func() { feedbackLoop(sessionCtx, c, s.dacpHeaders, s.connectionLost) })

	ok = true
	return s, nil
}

// videoSessionPlist is the PTP-timed SETUP body for video sessions.
func (c *AirPlayClient) videoSessionPlist() map[string]interface{} {
	deviceID := uuidToMAC(c.sessionID)
	peer := map[string]interface{}{
		"ID":                                strings.ToUpper(c.sessionID),
		"Addresses":                         []string{c.localIP()},
		"DeviceType":                        int64(0),
		"SupportsClockPortMatchingOverride": true,
	}
	return map[string]interface{}{
		"timingProtocol":         timingProtocolPTP,
		"timingPeerInfo":         peer,
		"timingPeerList":         []interface{}{peer},
		"sessionUUID":            strings.ToUpper(generateUUID()),
		"sessionCorrelationUUID": strings.ToUpper(c.sessionID),
		"updateSessionRequest":   false,
		"statsCollectionEnabled": false,
		"isMultiSelectAirPlay":   false,
		"deviceID":               deviceID,
		"macAddress":             deviceID,
		"model":                  playbackSenderModel,
		"name":                   pairingClientName(),
		"osName":                 playbackOSName,
		"osVersion":              playbackOSVersion,
		"osBuildVersion":         playbackOSBuildVersion,
		"sourceVersion":          playbackSourceVersion,
	}
}

// setup sends a session SETUP and decodes the response.
func (s *PlaybackSession) setup(body map[string]interface{}) (map[string]interface{}, error) {
	raw, err := plist.Marshal(body, plist.BinaryFormat)
	if err != nil {
		return nil, fmt.Errorf("marshal SETUP: %w", err)
	}
	respBody, _, err := s.client.rtspRequest("SETUP", s.sessionURI, playbackContentType, raw, s.dacpHeaders)
	if err != nil {
		return nil, fmt.Errorf("playback SETUP: %w", err)
	}
	var resp map[string]interface{}
	if _, err := plist.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal playback SETUP response: %w", err)
	}
	dbg("[SETUP] playback response: %+v", resp)
	return resp, nil
}

func (s *PlaybackSession) record() error {
	recordHeaders := s.headers(map[string]string{
		"Range":    "npt=0-",
		"RTP-Info": "seq=0;rtptime=0",
	})
	if _, _, err := s.client.rtspRequest("RECORD", s.sessionURI, "", nil, recordHeaders); err != nil {
		return fmt.Errorf("RECORD: %w", err)
	}
	return nil
}

// setupRemoteControl registers a remote control session, which enables
// POST /command. The receiver answers without a dataPort; nothing connects.
func (s *PlaybackSession) setupRemoteControl() bool {
	body, err := s.commandProto("GET", "/info", protocolHTTP, "", nil)
	if err != nil {
		dbg("[PLAY] GET /info: %v", err)
		return false
	}
	var info map[string]interface{}
	if _, err := plist.Unmarshal(body, &info); err != nil {
		return false
	}
	psi, _ := info["psi"].(string)
	if psi == "" {
		dbg("[PLAY] receiver reports no psi; no remote control session")
		return false
	}
	resp, err := s.setup(map[string]interface{}{
		"streams": []interface{}{map[string]interface{}{
			"type":           int64(remoteControlStreamType),
			"controlType":    int64(remoteControlSessionType),
			"channelID":      psi + "-RCS-1",
			"clientUUID":     strings.ToUpper(generateUUID()),
			"clientTypeUUID": remoteControlClientType,
		}},
	})
	if err != nil {
		dbg("[PLAY] remote control session rejected: %v", err)
		return false
	}
	streams, _ := resp["streams"].([]interface{})
	if len(streams) == 0 {
		return false
	}
	s.rcsStreamID = 1
	if st, ok := streams[0].(map[string]interface{}); ok {
		if id := plistInt(st["streamID"]); id > 0 {
			s.rcsStreamID = id
		}
	}
	return true
}

// sendCommand posts a play-queue command. The body's "data" member is itself
// a serialized binary plist.
func (s *PlaybackSession) sendCommand(command map[string]interface{}) error {
	inner, err := plist.Marshal(command, plist.BinaryFormat)
	if err != nil {
		return err
	}
	body, err := plist.Marshal(map[string]interface{}{"params": map[string]interface{}{"data": inner}}, plist.BinaryFormat)
	if err != nil {
		return err
	}
	headers := map[string]string{"X-Apple-StreamID": fmt.Sprint(s.rcsStreamID)}
	if _, _, err := s.client.controlRequest("POST", "/command", protocolHTTP, playbackContentType, body, headers); err != nil {
		return fmt.Errorf("command %v: %w", command["type"], err)
	}
	return nil
}

func (s *PlaybackSession) playQueue(url string, startSeconds float64) error {
	item := map[string]interface{}{
		"uuid":                       s.itemUUID,
		"Content-Location":           url,
		"mediaType":                  "file",
		"IsTLSEnabled":               strings.HasPrefix(url, "https://"),
		"playbackRestrictions":       int64(0),
		"referenceRestrictions":      int64(2),
		"supportsIntegratedTimeline": false,
		"snapTimeToPausePlayback":    false,
		"clientBundleID":             playbackClientBundleID,
		"clientProcName":             "foxCast",
		// At most 6 characters, or the receiver drops the connection.
		"playerLoggingID":     "FOXCST",
		"playerItemLoggingID": "I/FOXCAST.01",
		"Start-Position": map[string]interface{}{
			"flags": int64(1), "value": int64(startSeconds), "epoch": int64(0), "timescale": int64(1),
		},
	}
	if err := s.sendCommand(map[string]interface{}{"type": "insertPlayQueueItem", "item": item}); err != nil {
		return err
	}
	if err := s.SetProperty("isInterestedInDateRange", true); err != nil {
		dbg("[PLAY] %v", err)
	}
	// Playback starts paused without this.
	return s.sendCommand(map[string]interface{}{"type": "setRate", "rate": 1.0})
}

// connectionLost records that the control connection is gone.
func (s *PlaybackSession) connectionLost(err error) {
	s.lostErr = err
	close(s.lost)
}

// handleEvent records play-queue state pushed on the event channel.
func (s *PlaybackSession) handleEvent(event map[string]interface{}) {
	kind, _ := event["type"].(string)
	name, _ := event["name"].(string)
	switch kind {
	case "playbackState":
		select {
		case s.state <- name:
		default:
		}
	case "notification":
		dbg("[PLAY] notification: %s", name)
		if name == "itemPlayedToEnd" {
			select {
			case s.state <- playbackStateStopped:
			default:
			}
		}
	}
}

// startLegacy is the pre-tvOS 26 flow: NTP timing, POST /play and
// setProperty/rate commands, with state polled from /playback-info.
func (s *PlaybackSession) startLegacy(ctx context.Context, url string, cfg PlaybackConfig) error {
	c := s.client
	ports, err := allocateConsecutiveUDPPortsInRange(1, cfg.PortMin, cfg.PortMax)
	if err != nil {
		return fmt.Errorf("allocate timing port: %w", err)
	}
	s.timingConn = ports[0]
	timingConn := s.timingConn
	timingPort := timingConn.LocalAddr().(*net.UDPAddr).Port
	s.startWorker(func() { ntpTimingResponder(ctx, timingConn) })

	setupResp, err := s.setup(c.playbackSessionPlist(timingPort))
	if err != nil {
		return err
	}
	if receiverTimingPort := plistInt(setupResp["timingPort"]); receiverTimingPort > 0 {
		s.startWorker(func() { sendNTPTimingProbes(ctx, timingConn, c.host, receiverTimingPort) })
	}
	// The event channel carries the receiver's keep-alive commands. Without
	// it RECORD fails and the session is torn down after ~30s.
	if eventPort := plistInt(setupResp["eventPort"]); eventPort > 0 {
		s.eventConn, err = c.connectEventChannel(ctx, eventPort, nil)
		if err != nil {
			return err
		}
	}
	s.startWorker(func() { feedbackLoop(ctx, c, s.dacpHeaders, nil) })
	if err := s.record(); err != nil {
		return err
	}
	return s.startPlay(url, cfg.StartSeconds)
}

// startPlay sends POST /play and the commands Apple senders follow it with.
func (s *PlaybackSession) startPlay(url string, startSeconds float64) error {
	if err := s.play(url, startSeconds); err != nil {
		return err
	}
	if err := s.SetProperty("isInterestedInDateRange", true); err != nil {
		dbg("[PLAY] setProperty isInterestedInDateRange: %v", err)
	}
	if err := s.SetProperty("actionAtItemEnd", int64(0)); err != nil {
		dbg("[PLAY] setProperty actionAtItemEnd: %v", err)
	}
	// /rate is required, as the item otherwise loads paused.
	if err := s.Rate(1.0); err != nil {
		return err
	}
	noEndTime := map[string]interface{}{"flags": int64(0), "value": int64(0), "epoch": int64(0), "timescale": int64(0)}
	for _, prop := range []string{"forwardEndTime", "reverseEndTime"} {
		if err := s.SetProperty(prop, noEndTime); err != nil {
			dbg("[PLAY] setProperty %s: %v", prop, err)
		}
	}
	return nil
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

// feedbackLoop sends POST /feedback every 2 s until ctx ends. A failure that
// is not an HTTP status (the receiver closed the control connection) is
// reported once through lost, if non-nil.
func feedbackLoop(ctx context.Context, c *AirPlayClient, headers map[string]string, lost func(error)) {
	ticker := time.NewTicker(playbackFeedbackInterval)
	defer ticker.Stop()
	for {
		// Feedback is best effort; the event channel is the real keep-alive.
		if _, _, err := c.rtspRequest("POST", "/feedback", "", nil, headers); err != nil {
			dbg("[FEEDBACK] error: %v", err)
			var statusErr *HTTPStatusError
			if lost != nil && ctx.Err() == nil && !errors.As(err, &statusErr) {
				lost(err)
				return
			}
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

// headers merges the session's DACP headers with extra.
func (s *PlaybackSession) headers(extra map[string]string) map[string]string {
	out := make(map[string]string, len(s.dacpHeaders)+len(extra))
	for k, v := range s.dacpHeaders {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func (s *PlaybackSession) command(method, path, contentType string, body []byte) ([]byte, error) {
	return s.commandProto(method, path, protocolRTSP, contentType, body)
}

func (s *PlaybackSession) commandProto(method, path, protocol, contentType string, body []byte) ([]byte, error) {
	resp, _, err := s.client.controlRequest(method, path, protocol, contentType, body, s.headers(s.client.playbackHeaders()))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

// waitQueue follows the playbackState events of a play-queue session: it
// returns once playback stops, or fails if it never starts.
func (s *PlaybackSession) waitQueue(ctx context.Context) error {
	started := false
	timeout := time.NewTimer(playbackStartTimeout)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.lost:
			// tvOS closes the connection when playback is stopped on the
			// receiver (e.g. Menu on the remote), without a stopped event.
			if started {
				dbg("[PLAY] receiver closed the session: %v", s.lostErr)
				return nil
			}
			return fmt.Errorf("receiver closed the session: %w", s.lostErr)
		case <-timeout.C:
			if !started {
				return errors.New("playback did not start (receiver reported no playing state)")
			}
		case name := <-s.state:
			dbg("[PLAY] playback state: %s", name)
			switch name {
			case "playing", "paused":
				started = true
			case playbackStateStopped:
				if started {
					return nil
				}
			}
		}
	}
}

// Rate sets the playback rate: 0 pauses, 1 plays at normal speed.
func (s *PlaybackSession) Rate(rate float64) error {
	if s.queue {
		return s.sendCommand(map[string]interface{}{"type": "setRate", "rate": rate})
	}
	_, err := s.command("POST", fmt.Sprintf("/rate?value=%f", rate), "", nil)
	return err
}

// ErrScrubUnsupported is returned by Scrub on play-queue sessions: no
// documented source gives their /command seek form, and the legacy /scrub
// endpoint is not part of that protocol.
var ErrScrubUnsupported = errors.New("seeking is not supported on play-queue (tvOS 26+) sessions")

// Scrub seeks to position seconds from the start. Play-queue sessions return
// ErrScrubUnsupported.
func (s *PlaybackSession) Scrub(position float64) error {
	if s.queue {
		return ErrScrubUnsupported
	}
	_, err := s.command("POST", fmt.Sprintf("/scrub?position=%f", position), "", nil)
	return err
}

// SetProperty sets a receiver playback property: a /command setProperty on
// the current item for play-queue sessions, PUT /setProperty otherwise.
func (s *PlaybackSession) SetProperty(name string, value interface{}) error {
	if s.queue {
		return s.sendCommand(map[string]interface{}{
			"type": "setProperty", "property": name, "value": value,
			"item": map[string]interface{}{"uuid": s.itemUUID},
		})
	}
	body, err := plist.Marshal(map[string]interface{}{"value": value}, plist.BinaryFormat)
	if err != nil {
		return fmt.Errorf("marshal setProperty %s: %w", name, err)
	}
	_, err = s.command("PUT", "/setProperty?"+name, playbackContentType, body)
	return err
}

// Info queries the receiver's current playback state with GET
// /playback-info. Play-queue receivers answer it with 500; their state
// arrives on the event channel instead.
func (s *PlaybackSession) Info() (*PlaybackInfo, error) {
	// pyatv polls this as a plain HTTP/1.1 request on the control connection.
	body, err := s.commandProto("GET", "/playback-info", protocolHTTP, "", nil)
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

// Wait blocks until playback ends, ctx is cancelled, or the receiver reports
// an error. Play-queue sessions follow the event channel's playbackState
// events; legacy sessions poll /playback-info.
func (s *PlaybackSession) Wait(ctx context.Context) error {
	if s.queue {
		return s.waitQueue(ctx)
	}
	grace := playbackStartGrace
	started := false
	ticker := time.NewTicker(playbackPollInterval)
	defer ticker.Stop()
	for {
		info, err := s.Info()
		var statusErr *HTTPStatusError
		if err != nil && !started && errors.As(err, &statusErr) && statusErr.StatusCode == 500 {
			// tvOS answers 500 until the item has been created.
			dbg("[PLAY] /playback-info not ready yet: %v", err)
			info, err = &PlaybackInfo{}, nil
		}
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
			if !s.queue {
				if _, err := s.command("POST", "/stop", "", nil); err != nil {
					dbg("[PLAY] stop: %v", err)
				}
			}
			if _, _, err := s.client.rtspRequest("TEARDOWN", s.sessionURI, "", nil, s.dacpHeaders); err != nil {
				dbg("[TEARDOWN] error: %v", err)
			}
		}
		if s.eventConn != nil {
			_ = s.eventConn.Close()
		}
		if s.timingConn != nil {
			_ = s.timingConn.Close()
		}
	})
}
