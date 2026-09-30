package sender

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"howett.net/plist"
)

// ReceiverPlaybackStats summarises URL playback commands seen by the test
// receiver.
type ReceiverPlaybackStats struct {
	PlayRequests         uint64
	RateRequests         uint64
	ScrubRequests        uint64
	StopRequests         uint64
	SetPropertyRequests  uint64
	PlaybackInfoRequests uint64
	// URL is the most recent /play Content-Location.
	URL string
	// StartSeconds is the most recent /play Start-Position-Seconds.
	StartSeconds float64
	// Rate is the most recent /rate value.
	Rate float64
	// Properties are the /setProperty names in arrival order.
	Properties []string
}

type receiverPlaybackCounters struct {
	play         atomic.Uint64
	rate         atomic.Uint64
	scrub        atomic.Uint64
	stop         atomic.Uint64
	setProperty  atomic.Uint64
	playbackInfo atomic.Uint64

	mu           sync.Mutex
	url          string
	startSeconds float64
	currentRate  float64
	properties   []string
	playing      bool
}

const receiverPlaybackDuration = 60.0

func receiverIsPlaybackRequest(request receiverRequest, path string) bool {
	path, _, _ = strings.Cut(path, "?")
	switch path {
	case "/play", "/rate", "/scrub", "/stop", "/setProperty", "/playback-info":
		return true
	}
	return false
}

func (c *receiverConnection) playbackSessionActive() bool {
	return c.playback &&
		(c.sessionState == receiverSessionControlPrepared || c.sessionState == receiverSessionRecorded)
}

func (c *receiverConnection) handlePlayback(request receiverRequest, path string) receiverResponse {
	counters := &c.server.stats.playback
	path, query, _ := strings.Cut(path, "?")
	if !c.playback || c.sessionState != receiverSessionRecorded {
		return c.invalidSessionState(path)
	}

	switch {
	case request.method == "POST" && path == "/play":
		counters.play.Add(1)
		var body struct {
			URL   string  `plist:"Content-Location"`
			Start float64 `plist:"Start-Position-Seconds"`
		}
		if _, err := plist.Unmarshal(request.body, &body); err != nil {
			return receiverError(400, fmt.Errorf("decode /play: %w", err))
		}
		if body.URL == "" {
			return receiverError(400, fmt.Errorf("/play omitted Content-Location"))
		}
		counters.mu.Lock()
		counters.url, counters.startSeconds, counters.playing = body.URL, body.Start, true
		counters.mu.Unlock()
		return receiverOK(nil, "")

	case request.method == "POST" && path == "/rate":
		counters.rate.Add(1)
		value, err := receiverQueryFloat(query, "value")
		if err != nil {
			return receiverError(400, err)
		}
		counters.mu.Lock()
		counters.currentRate = value
		counters.mu.Unlock()
		return receiverOK(nil, "")

	case request.method == "POST" && path == "/scrub":
		counters.scrub.Add(1)
		if _, err := receiverQueryFloat(query, "position"); err != nil {
			return receiverError(400, err)
		}
		return receiverOK(nil, "")

	case request.method == "POST" && path == "/stop":
		counters.stop.Add(1)
		counters.mu.Lock()
		counters.playing = false
		counters.mu.Unlock()
		return receiverOK(nil, "")

	case request.method == "PUT" && path == "/setProperty":
		counters.setProperty.Add(1)
		var body map[string]any
		if _, err := plist.Unmarshal(request.body, &body); err != nil {
			return receiverError(400, fmt.Errorf("decode setProperty: %w", err))
		}
		if _, ok := body["value"]; !ok {
			return receiverError(400, fmt.Errorf("setProperty omitted value"))
		}
		counters.mu.Lock()
		counters.properties = append(counters.properties, query)
		counters.mu.Unlock()
		return receiverOK(nil, "")

	case request.method == "GET" && path == "/playback-info":
		counters.playbackInfo.Add(1)
		counters.mu.Lock()
		info := map[string]any{"readyToPlay": counters.playing}
		if counters.playing {
			info["duration"] = receiverPlaybackDuration
			info["position"] = counters.startSeconds
			info["rate"] = counters.currentRate
		}
		counters.mu.Unlock()
		body, err := plist.Marshal(info, plist.BinaryFormat)
		if err != nil {
			return receiverError(500, err)
		}
		return receiverOK(body, "application/x-apple-binary-plist")
	}
	return receiverResponse{status: 404}
}

func receiverQueryFloat(query, key string) (float64, error) {
	for _, pair := range strings.Split(query, "&") {
		name, value, _ := strings.Cut(pair, "=")
		if name == key {
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid %s %q", key, value)
			}
			return parsed, nil
		}
	}
	return 0, fmt.Errorf("missing %s", key)
}

func (s *ReceiverServer) playbackStats(stats *ReceiverStats) {
	counters := &s.stats.playback
	stats.Playback = ReceiverPlaybackStats{
		PlayRequests:         counters.play.Load(),
		RateRequests:         counters.rate.Load(),
		ScrubRequests:        counters.scrub.Load(),
		StopRequests:         counters.stop.Load(),
		SetPropertyRequests:  counters.setProperty.Load(),
		PlaybackInfoRequests: counters.playbackInfo.Load(),
	}
	counters.mu.Lock()
	stats.Playback.URL = counters.url
	stats.Playback.StartSeconds = counters.startSeconds
	stats.Playback.Rate = counters.currentRate
	stats.Playback.Properties = append([]string(nil), counters.properties...)
	counters.mu.Unlock()
}
