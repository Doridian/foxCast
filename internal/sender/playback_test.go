package sender

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestPlayURLEndToEndModernReceiver(t *testing.T) {
	server, client, ctx := newReceiverServerTestPair(t, ReceiverConfig{
		Profile: ReceiverProfileModern,
		Auth:    ReceiverAuthNone,
	})
	if err := client.Pair(ctx, ""); err != nil {
		t.Fatalf("pair: %v", err)
	}
	if err := client.FairPlaySetup(ctx); err != nil {
		t.Fatalf("FairPlay setup: %v", err)
	}

	const url = "http://192.0.2.1:8080/video.mp4"
	session, err := client.PlayURL(ctx, url, PlaybackConfig{StartSeconds: 12.5})
	if err != nil {
		t.Fatalf("PlayURL: %v", err)
	}

	stats := server.Stats()
	if stats.SetupRequests != 1 || stats.RecordRequests != 1 {
		t.Fatalf("SETUP/RECORD = %d/%d, want 1/1", stats.SetupRequests, stats.RecordRequests)
	}
	if stats.EventConnections != 1 {
		t.Fatalf("event connections = %d, want 1", stats.EventConnections)
	}
	if stats.Playback.PlayRequests != 1 || stats.Playback.URL != url || stats.Playback.StartSeconds != 12.5 {
		t.Fatalf("play = %+v, want one /play of %s at 12.5", stats.Playback, url)
	}
	if stats.Playback.Rate != 1 {
		t.Fatalf("rate = %v, want 1", stats.Playback.Rate)
	}
	wantProperties := []string{"isInterestedInDateRange", "actionAtItemEnd"}
	if !slices.Equal(stats.Playback.Properties, wantProperties) {
		t.Fatalf("properties = %v, want %v", stats.Playback.Properties, wantProperties)
	}

	info, err := session.Info()
	if err != nil {
		t.Fatalf("playback info: %v", err)
	}
	if !info.HasDuration || info.Duration != receiverPlaybackDuration || info.Rate != 1 {
		t.Fatalf("playback info = %+v", info)
	}

	if err := session.Rate(0); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := session.Scrub(30); err != nil {
		t.Fatalf("scrub: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if err := session.Wait(waitCtx); err != context.DeadlineExceeded {
		t.Fatalf("Wait while playing = %v, want deadline", err)
	}
	if got := server.Stats().FeedbackRequests; got == 0 {
		t.Fatal("no /feedback sent during playback")
	}

	if err := session.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	stats = server.Stats()
	if stats.Playback.StopRequests != 1 || stats.TeardownRequests != 1 {
		t.Fatalf("stop/teardown = %d/%d, want 1/1", stats.Playback.StopRequests, stats.TeardownRequests)
	}
}

func TestReceiverRejectsPlaybackOnMirroringSession(t *testing.T) {
	_, client, ctx := newReceiverServerTestPair(t, ReceiverConfig{
		Profile: ReceiverProfileModern,
		Auth:    ReceiverAuthNone,
	})
	if err := client.Pair(ctx, ""); err != nil {
		t.Fatalf("pair: %v", err)
	}
	if err := client.FairPlaySetup(ctx); err != nil {
		t.Fatalf("FairPlay setup: %v", err)
	}
	requireReceiverStatus(t, receiverTestRTSPRequest(client, "POST", "/play", nil), 455)
	requireReceiverOK(t, receiverTestRTSPRequest(client, "SETUP", "/session", receiverTestSetupBody(t)))
	requireReceiverOK(t, receiverTestRTSPRequest(client, "RECORD", "/session", nil))
	requireReceiverStatus(t, receiverTestRTSPRequest(client, "POST", "/rate?value=1.0", nil), 455)
}
