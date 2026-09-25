package sender

import (
	"math"
	"sync/atomic"
	"time"
)

type connectionLatencyHint int8

const (
	connectionLatencyLow    connectionLatencyHint = -1
	connectionLatencyNormal connectionLatencyHint = 0
	connectionLatencyHigh   connectionLatencyHint = 1

	// Apple's ordinary screen path selects separate video and audio leads from
	// the same semantic connection hint. These are APSScreenLatencyMs and
	// APSAudioLatencyForScreenMs defaults in the AirPlaySupport artifacts.
	defaultVideoLatencyLow    = 40 * time.Millisecond
	defaultVideoLatencyNormal = 75 * time.Millisecond
	defaultVideoLatencyHigh   = 100 * time.Millisecond
	defaultAudioLatencyLow    = 50 * time.Millisecond
	defaultAudioLatencyNormal = 85 * time.Millisecond
	defaultAudioLatencyHigh   = 170 * time.Millisecond
)

type screenLatencyTargets struct {
	video time.Duration
	audio time.Duration
}

// withMinimumVideoLead gives a locally measured capture pipeline enough room
// while preserving the relative audio/video policy selected by Apple for the
// connection hint. This is sender scheduling compensation, not a receiver or
// codec-specific clock offset.
func (targets screenLatencyTargets) withMinimumVideoLead(minimum time.Duration) screenLatencyTargets {
	if minimum <= targets.video {
		return targets
	}
	delta := minimum - targets.video
	targets.video += delta
	targets.audio += delta
	return targets
}

var targetLatencyNS atomic.Int64

// SetTargetLatency sets the application's explicit joint playout lead. Apple's
// own screen and audio overrides are independent, but doubletake historically
// exposed one flag for both; applying the same explicit value preserves that
// contract without inventing a relationship between Apple's two settings. A
// non-positive value restores the artifact-derived automatic policy.
func SetTargetLatency(d time.Duration) {
	if d <= 0 {
		targetLatencyNS.Store(0)
		return
	}
	if d < 5*time.Millisecond {
		d = 5 * time.Millisecond
	}
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	targetLatencyNS.Store(int64(d))
}

// TargetLatency returns the video lead for an ordinary connection. It remains
// the compatibility accessor for callers that only need the video timestamp
// bias; new session setup should use screenLatenciesForHint.
func TargetLatency() time.Duration {
	return screenLatenciesForHint(connectionLatencyNormal).video
}

// HasExplicitTargetLatency reports whether SetTargetLatency currently replaces
// automatic capture calibration with one joint audio/video lead.
func HasExplicitTargetLatency() bool {
	return targetLatencyIsExplicit()
}

func targetLatencyIsExplicit() bool {
	return targetLatencyNS.Load() > 0
}

func screenLatenciesForHint(hint connectionLatencyHint) screenLatencyTargets {
	var targets screenLatencyTargets
	switch hint {
	case connectionLatencyLow:
		targets = screenLatencyTargets{video: defaultVideoLatencyLow, audio: defaultAudioLatencyLow}
	case connectionLatencyHigh:
		targets = screenLatencyTargets{video: defaultVideoLatencyHigh, audio: defaultAudioLatencyHigh}
	default:
		targets = screenLatencyTargets{video: defaultVideoLatencyNormal, audio: defaultAudioLatencyNormal}
	}

	if override := time.Duration(targetLatencyNS.Load()); override > 0 {
		targets.video = override
		targets.audio = override
	}
	return targets
}

func targetLatencySamples44k1() uint32 {
	return samplesFor44k1(screenLatenciesForHint(connectionLatencyNormal).audio)
}

func samplesFor44k1(d time.Duration) uint32 {
	// Apple's sender converts its millisecond latency to the integral SETUP and
	// RTP-timebase value by truncation. Keep that byte-exact behavior: 85 ms is
	// 3748.5 samples and is advertised as 3748, not rounded to 3749.
	samples := int64(d/time.Second)*44100 + int64(d%time.Second)*44100/int64(time.Second)
	if samples < 1 {
		samples = 1
	}
	if samples > math.MaxUint32 {
		samples = math.MaxUint32
	}
	return uint32(samples)
}
