package sender

import (
	"testing"
	"time"
)

func TestScreenLatenciesForConnectionHint(t *testing.T) {
	SetTargetLatency(0)
	t.Cleanup(func() {
		SetTargetLatency(0)
	})

	tests := []struct {
		name  string
		hint  connectionLatencyHint
		video time.Duration
		audio time.Duration
	}{
		{name: "low", hint: connectionLatencyLow, video: 40 * time.Millisecond, audio: 50 * time.Millisecond},
		{name: "normal", hint: connectionLatencyNormal, video: 75 * time.Millisecond, audio: 85 * time.Millisecond},
		{name: "high", hint: connectionLatencyHigh, video: 100 * time.Millisecond, audio: 170 * time.Millisecond},
		{name: "unknown is normal", hint: connectionLatencyHint(99), video: 75 * time.Millisecond, audio: 85 * time.Millisecond},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := screenLatenciesForHint(test.hint)
			if got.video != test.video || got.audio != test.audio {
				t.Fatalf("latencies = video %v audio %v, want video %v audio %v", got.video, got.audio, test.video, test.audio)
			}
		})
	}
}

func TestTargetLatencyOverrideAppliesJointLead(t *testing.T) {
	SetTargetLatency(120 * time.Millisecond)
	t.Cleanup(func() {
		SetTargetLatency(0)
	})

	for _, hint := range []connectionLatencyHint{connectionLatencyLow, connectionLatencyNormal, connectionLatencyHigh} {
		got := screenLatenciesForHint(hint)
		if got.video != 120*time.Millisecond || got.audio != 120*time.Millisecond {
			t.Fatalf("hint %d override = video %v audio %v, want 120ms/120ms", hint, got.video, got.audio)
		}
	}
	if got := TargetLatency(); got != 120*time.Millisecond {
		t.Fatalf("TargetLatency = %v, want 120ms", got)
	}
	if !HasExplicitTargetLatency() {
		t.Fatal("explicit target latency was not reported")
	}
	if got, want := targetLatencySamples44k1(), samplesFor44k1(120*time.Millisecond); got != want {
		t.Fatalf("audio latency = %d samples, want %d", got, want)
	}
}

func TestTargetLatencyAutomaticAndClamp(t *testing.T) {
	SetTargetLatency(0)
	if HasExplicitTargetLatency() {
		t.Fatal("automatic latency policy was reported as explicit")
	}
	t.Cleanup(func() {
		SetTargetLatency(0)
	})

	SetTargetLatency(time.Millisecond)
	if got := screenLatenciesForHint(connectionLatencyNormal); got.video != 5*time.Millisecond || got.audio != 5*time.Millisecond {
		t.Fatalf("minimum override = video %v audio %v, want 5ms/5ms", got.video, got.audio)
	}

	SetTargetLatency(3 * time.Second)
	if got := screenLatenciesForHint(connectionLatencyNormal); got.video != 2*time.Second || got.audio != 2*time.Second {
		t.Fatalf("maximum override = video %v audio %v, want 2s/2s", got.video, got.audio)
	}

	SetTargetLatency(0)
	if got := screenLatenciesForHint(connectionLatencyNormal); got.video != 75*time.Millisecond || got.audio != 85*time.Millisecond {
		t.Fatalf("automatic policy = video %v audio %v, want 75ms/85ms", got.video, got.audio)
	}
}

func TestSamplesFor44k1MatchesSenderTruncation(t *testing.T) {
	if got := samplesFor44k1(85 * time.Millisecond); got != 3748 {
		t.Fatalf("85ms = %d samples, want artifact-compatible truncation to 3748", got)
	}
}

func TestMinimumVideoLeadPreservesAutomaticAudioVideoDelta(t *testing.T) {
	targets := screenLatencyTargets{
		video: defaultVideoLatencyNormal,
		audio: defaultAudioLatencyNormal,
	}
	if got := targets.withMinimumVideoLead(50 * time.Millisecond); got != targets {
		t.Fatalf("unneeded minimum changed targets to %#v", got)
	}
	got := targets.withMinimumVideoLead(150 * time.Millisecond)
	if got.video != 150*time.Millisecond || got.audio != 160*time.Millisecond {
		t.Fatalf("calibrated targets = video %v audio %v, want 150ms/160ms", got.video, got.audio)
	}
}
