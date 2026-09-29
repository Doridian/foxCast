package sender

import (
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"
)

const (
	// mediaClockMaxSlew is the fastest a correction is worked in: 500 ppm, so
	// 1 ms takes 2 s. Slewing keeps the timeline continuous and never running
	// backwards, which audio and video timestamps rely on.
	mediaClockMaxSlew = 500e-6
	// mediaClockStepThreshold is how far behind the estimate the clock may be
	// before it jumps forward instead of slewing.
	mediaClockStepThreshold = 20 * time.Millisecond
	// receiverClockHeaderResolution is the granularity of
	// X-Apple-RequestReceivedTimestamp and X-Apple-ProcessingTime.
	receiverClockHeaderResolution = time.Millisecond
	// clockWarmupExchanges extra /feedback requests, clockWarmupInterval
	// apart, start a PTP session's clock estimate.
	clockWarmupExchanges = 5
	clockWarmupInterval  = 200 * time.Millisecond
)

// mediaClock maps local monotonic time onto the receiver's PTP timeline. The
// receiver's X-Apple-RequestReceivedTimestamp is in the same boot-relative
// domain as its PTP Follow_Up timestamps, so no local PTP stack is required:
// every RTSP exchange carrying it is a clock sample for clockEstimator, and
// the mapping slews toward the estimate.
type mediaClock struct {
	mu           sync.RWMutex
	anchorLocal  time.Time
	anchorRemote time.Duration
	// rate is how much faster than the local clock the receiver's runs, as
	// a fraction; only PTP measures it.
	rate float64
	// slewRate is added to the clock's rate from anchorLocal until slewUntil.
	slewRate   float64
	slewUntil  time.Time
	timelineID uint64
	estimator  clockEstimator
	// ptpSampledAt is when the last PTP sample arrived. While PTP samples
	// keep coming, RTSP clock headers only keep their estimator warm.
	ptpSampledAt time.Time
}

// requestTimes are the local times an RTSP request was sent and its response
// arrived.
type requestTimes struct {
	sent, received time.Time
}

func (c *mediaClock) configureFromSetup(response map[string]interface{}, headers map[string]string, times requestTimes) error {
	peer, _ := response["timingPeerInfo"].(map[string]interface{})
	timelineID := plistUint64(peer["ClockID"])
	if timelineID == 0 {
		return fmt.Errorf("SETUP response omitted timingPeerInfo.ClockID")
	}

	c.mu.Lock()
	// Keep the receiver's identity even when the private clock-anchor headers
	// are absent. The fallback can then anchor local boot time to the actual
	// advertised PTP timeline.
	c.timelineID = timelineID
	c.mu.Unlock()
	if err := c.observe(headers, times); err != nil {
		return err
	}
	dbg("[PTP] receiver clock: timeline=0x%016x", timelineID)
	return nil
}

// configureFromLocalClock anchors a PTP timeline to the sender's boot clock.
// Some third-party PTP receivers return ClockID but omit Apple's private clock
// headers. A ClockID remains mandatory: inventing one would describe a timeline
// the receiver has never advertised and would make both audio and video invalid.
func (c *mediaClock) configureFromLocalClock() error {
	anchorLocal := time.Now()
	anchorRemote := bootRelativeNow()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timelineID == 0 {
		return fmt.Errorf("cannot configure local PTP clock without receiver ClockID")
	}
	c.anchorLocal = anchorLocal
	c.anchorRemote = anchorRemote
	c.slewRate, c.slewUntil = 0, time.Time{}
	return nil
}

func (c *mediaClock) identity() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.timelineID
}

// receiverClockSample reads the receiver's clock headers of one RTSP exchange.
func receiverClockSample(headers map[string]string, times requestTimes) (clockSample, error) {
	receivedMillis, err := strconv.ParseUint(headers["x-apple-requestreceivedtimestamp"], 10, 64)
	if err != nil {
		return clockSample{}, fmt.Errorf("invalid X-Apple-RequestReceivedTimestamp %q", headers["x-apple-requestreceivedtimestamp"])
	}
	processingMillis, err := strconv.ParseUint(headers["x-apple-processingtime"], 10, 64)
	if err != nil {
		return clockSample{}, fmt.Errorf("invalid X-Apple-ProcessingTime %q", headers["x-apple-processingtime"])
	}
	maxMillis := uint64(math.MaxInt64 / int64(time.Millisecond))
	if receivedMillis > maxMillis || processingMillis > maxMillis-receivedMillis {
		return clockSample{}, fmt.Errorf("receiver timestamp is out of range")
	}
	if times.sent.IsZero() || times.received.Before(times.sent) {
		return clockSample{}, fmt.Errorf("invalid request times")
	}
	return clockSample{
		localSent:      times.sent,
		localReceived:  times.received,
		remoteReceived: time.Duration(receivedMillis) * time.Millisecond,
		remoteSent:     time.Duration(receivedMillis+processingMillis) * time.Millisecond,
		resolution:     receiverClockHeaderResolution,
	}, nil
}

// observe takes the receiver's clock headers from one RTSP exchange as a
// clock sample.
func (c *mediaClock) observe(headers map[string]string, times requestTimes) error {
	sample, err := receiverClockSample(headers, times)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	local, remote := c.estimator.add(sample)
	if !c.ptpSampledAt.IsZero() && local.Sub(c.ptpSampledAt) < ptpFreshness {
		dbg("[PTP] clock headers: delay=%v, %v from the PTP estimate", sample.delay(), remote-c.remoteAtLocked(local))
		return nil
	}
	correction := c.steerLocked(local, remote)
	dbg("[PTP] clock headers: delay=%v correction=%v", sample.delay(), correction)
	return nil
}

// observePTP steers the mapping toward a PTP estimate of the receiver's
// clock: remote at local, running rate faster than the local clock (when
// rated). It returns how far the mapping was off.
func (c *mediaClock) observePTP(local time.Time, remote time.Duration, rate float64, rated bool) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ptpSampledAt = local
	correction := c.steerLocked(local, remote)
	if rated {
		// steerLocked anchored the mapping at local, so the new rate takes
		// effect from here without a jump.
		c.rate = rate
	}
	return correction
}

// steerLocked moves the mapping toward remote at local: at once the first
// time or when far behind, otherwise by slewing. It returns how far the
// mapping was off.
func (c *mediaClock) steerLocked(local time.Time, remote time.Duration) time.Duration {
	if c.anchorLocal.IsZero() {
		c.anchorLocal, c.anchorRemote = local, remote
		return 0
	}
	current := c.remoteAtLocked(local)
	c.anchorLocal, c.anchorRemote = local, current
	c.slewRate, c.slewUntil = 0, time.Time{}
	correction := remote - current
	switch {
	case correction > mediaClockStepThreshold:
		c.anchorRemote = remote
	case correction > 0:
		c.slewRate = mediaClockMaxSlew
		c.slewUntil = local.Add(time.Duration(float64(correction) / mediaClockMaxSlew))
	case correction < 0:
		c.slewRate = -mediaClockMaxSlew
		c.slewUntil = local.Add(time.Duration(float64(-correction) / mediaClockMaxSlew))
	}
	return correction
}

// remoteAtLocked is the receiver time the mapping gives for local.
func (c *mediaClock) remoteAtLocked(local time.Time) time.Duration {
	elapsed := local.Sub(c.anchorLocal)
	remote := c.anchorRemote + elapsed + time.Duration(float64(elapsed)*c.rate)
	if c.slewRate != 0 {
		slewing := elapsed
		if limit := c.slewUntil.Sub(c.anchorLocal); slewing > limit {
			slewing = limit
		}
		remote += time.Duration(float64(slewing) * c.slewRate)
	}
	return remote
}

// updateTimingPeerInfo switches to a newly advertised PTP timeline. Event
// channel timing updates carry only peer metadata, not a receiver timestamp,
// so the mapping carries on unchanged under the new identity.
func (c *mediaClock) updateTimingPeerInfo(peer map[string]interface{}) error {
	timelineID := plistUint64(peer["ClockID"])
	if timelineID == 0 {
		return fmt.Errorf("updateTimingPeerInfo omitted ClockID")
	}

	c.mu.Lock()
	previousTimeline := c.timelineID
	c.timelineID = timelineID
	c.mu.Unlock()

	dbg("[PTP] event timing peer update: timeline=0x%016x (was 0x%016x)", timelineID, previousTimeline)
	return nil
}

func (c *mediaClock) now(bias time.Duration) (timestamp, timelineID uint64, ok bool) {
	return c.at(time.Now(), bias)
}

// at maps a local monotonic capture time onto the receiver's PTP timeline.
// Unlike now, it preserves time spent in capture, scaling, and encoding.
func (c *mediaClock) at(local time.Time, bias time.Duration) (timestamp, timelineID uint64, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if local.IsZero() || c.anchorLocal.IsZero() || c.timelineID == 0 {
		return 0, 0, false
	}
	remote := c.remoteAtLocked(local) + bias
	if remote < 0 {
		return 0, 0, false
	}
	return compactTimestamp(remote), c.timelineID, true
}
