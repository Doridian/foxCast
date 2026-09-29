package sender

import (
	"testing"
	"time"
)

// exchange builds a sample against a receiver clock that reads local time
// (since base) plus offset, with the given one-way delays and processing
// time, and its timestamps truncated to resolution.
func exchange(base time.Time, at, offset, outbound, processing, inbound, resolution time.Duration) clockSample {
	truncate := func(d time.Duration) time.Duration {
		if resolution == 0 {
			return d
		}
		return d - d%resolution
	}
	sent := base.Add(at)
	remoteReceived := at + outbound + offset
	return clockSample{
		localSent:      sent,
		localReceived:  sent.Add(outbound + processing + inbound),
		remoteReceived: truncate(remoteReceived),
		remoteSent:     truncate(remoteReceived + processing),
		resolution:     resolution,
	}
}

// estimateError is how far an estimate is from the receiver clock of
// exchange at the same local instant.
func estimateError(base time.Time, offset time.Duration, local time.Time, remote time.Duration) time.Duration {
	return remote - (local.Sub(base) + offset)
}

func TestClockEstimatorSymmetricDelayIsExact(t *testing.T) {
	base := time.Unix(100, 0)
	const offset = 5*time.Second + 123456*time.Nanosecond
	var e clockEstimator
	local, remote := e.add(exchange(base, time.Second, offset, 3*time.Millisecond, 7*time.Millisecond, 3*time.Millisecond, 0))
	if err := estimateError(base, offset, local, remote); err != 0 {
		t.Fatalf("estimate off by %v, want exact", err)
	}
	if local != base.Add(time.Second+13*time.Millisecond) {
		t.Fatalf("estimate is for %v, want the exchange's completion", local)
	}
}

func TestClockEstimatorPrefersLowDelayExchanges(t *testing.T) {
	base := time.Unix(100, 0)
	const offset = 42 * time.Millisecond
	var e clockEstimator
	// A clean exchange, then one queued 30 ms on the way back: on its own it
	// would put the receiver 15 ms early.
	e.add(exchange(base, 0, offset, time.Millisecond, 0, time.Millisecond, 0))
	local, remote := e.add(exchange(base, 2*time.Second, offset, time.Millisecond, 0, 31*time.Millisecond, 0))
	if err := estimateError(base, offset, local, remote); err != 0 {
		t.Fatalf("estimate off by %v, want the clean exchange's exact estimate", err)
	}
}

func TestClockEstimatorAveragesMillisecondQuantisation(t *testing.T) {
	base := time.Unix(100, 0)
	const offset = 10*time.Second + 300*time.Microsecond
	var e clockEstimator
	var local time.Time
	var remote time.Duration
	worst := time.Duration(0)
	// Exchanges land at different phases of the receiver's millisecond.
	for i := 0; i < clockEstimatorWindow; i++ {
		at := time.Duration(i)*2*time.Second + time.Duration(i)*125*time.Microsecond
		sample := exchange(base, at, offset, time.Millisecond, 0, time.Millisecond, time.Millisecond)
		var single clockEstimator
		sLocal, sRemote := single.add(sample)
		if err := estimateError(base, offset, sLocal, sRemote); err > worst || -err > worst {
			worst = max(err, -err)
		}
		local, remote = e.add(sample)
	}
	err := estimateError(base, offset, local, remote)
	if err < -100*time.Microsecond || err > 100*time.Microsecond {
		t.Fatalf("averaged estimate off by %v, want within 100µs (single exchanges up to %v)", err, worst)
	}
	if worst < 400*time.Microsecond {
		t.Fatalf("single exchanges were only %v off; the test does not exercise quantisation", worst)
	}
}

func TestClockEstimatorForgetsExchangesOutsideTheWindow(t *testing.T) {
	base := time.Unix(100, 0)
	var e clockEstimator
	// An old exchange from before the receiver's clock moved by 50 ms.
	e.add(exchange(base, 0, 0, 0, 0, 0, 0))
	const offset = 50 * time.Millisecond
	var local time.Time
	var remote time.Duration
	for i := 1; i <= clockEstimatorWindow; i++ {
		local, remote = e.add(exchange(base, time.Duration(i)*2*time.Second, offset, time.Millisecond, 0, time.Millisecond, 0))
	}
	if len(e.samples) != clockEstimatorWindow {
		t.Fatalf("estimator keeps %d exchanges, want %d", len(e.samples), clockEstimatorWindow)
	}
	if err := estimateError(base, offset, local, remote); err != 0 {
		t.Fatalf("estimate off by %v once the old exchange left the window", err)
	}
}
