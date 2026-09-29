package sender

import "time"

const (
	// clockEstimatorWindow is how many recent exchanges an estimate draws on.
	// At one /feedback every 2 s that is about 16 s: short enough that crystal
	// drift across the window (tens of ppm) stays well below the 1 ms
	// resolution of the receiver's clock headers.
	clockEstimatorWindow = 8
	// clockEstimatorDelayMargin is the least slack, above the lowest delay in
	// the window, within which exchanges are averaged. Coarse remote
	// timestamps widen it to their resolution.
	clockEstimatorDelayMargin = 200 * time.Microsecond
)

// clockSample is one request/response exchange with a receiver, described by
// the same four timestamps NTP and PTP delay measurement use: the request
// leaves (localSent), the receiver takes it in and answers (remoteReceived,
// remoteSent, on the receiver's clock), and the answer arrives
// (localReceived). Remote timestamps are taken to be truncated to resolution.
type clockSample struct {
	localSent, localReceived   time.Time
	remoteReceived, remoteSent time.Duration
	resolution                 time.Duration
}

// delay is the time the exchange spent on the network: the round trip less
// the receiver's own processing time.
func (s clockSample) delay() time.Duration {
	return s.localReceived.Sub(s.localSent) - (s.remoteSent - s.remoteReceived)
}

// midpoint pairs the local and remote middles of the exchange. With the same
// delay in each direction they are the same instant; however the delay is
// split, they are at most half of it apart.
func (s clockSample) midpoint() (local time.Time, remote time.Duration) {
	local = s.localSent.Add(s.localReceived.Sub(s.localSent) / 2)
	remote = s.remoteReceived + (s.remoteSent-s.remoteReceived)/2 + s.resolution/2
	return local, remote
}

// clockEstimator estimates a receiver's clock from exchanges with it. It keeps
// the recent exchanges with the lowest delay, whose midpoints are the most
// trustworthy, and averages them, which also averages out the quantisation
// of millisecond remote timestamps. Samples can come from RTSP clock headers
// or from PTP delay measurement alike.
type clockEstimator struct {
	samples []clockSample
}

// add records an exchange and returns the receiver's estimated time at the
// local instant the exchange completed.
func (e *clockEstimator) add(sample clockSample) (local time.Time, remote time.Duration) {
	if len(e.samples) == clockEstimatorWindow {
		copy(e.samples, e.samples[1:])
		e.samples = e.samples[:len(e.samples)-1]
	}
	e.samples = append(e.samples, sample)

	minDelay := sample.delay()
	for _, s := range e.samples {
		if d := s.delay(); d < minDelay {
			minDelay = d
		}
	}
	local = sample.localReceived
	var base, sum time.Duration
	count := 0
	for _, s := range e.samples {
		margin := s.resolution
		if margin < clockEstimatorDelayMargin {
			margin = clockEstimatorDelayMargin
		}
		if s.delay() > minDelay+margin {
			continue
		}
		midLocal, midRemote := s.midpoint()
		projected := midRemote + local.Sub(midLocal)
		if count == 0 {
			base = projected
		}
		sum += projected - base
		count++
	}
	return local, base + sum/time.Duration(count)
}
