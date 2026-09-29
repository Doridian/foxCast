package sender

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// silentPCM is an endless, unframed PCM source.
type silentPCM struct{}

func (silentPCM) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func (silentPCM) Close() error { return nil }

func TestSetupAudioOnlyEndToEndProfiles(t *testing.T) {
	SetTargetLatency(0)
	t.Cleanup(func() { SetTargetLatency(0) })

	for _, test := range []struct {
		name       string
		profile    ReceiverProfile
		wantSetups uint64
		wantEvents uint64
	}{
		{name: "modern HAP control-first", profile: ReceiverProfileModern, wantSetups: 2, wantEvents: 1},
		{name: "Roku media-first", profile: ReceiverProfileRoku, wantSetups: 2, wantEvents: 1},
		{name: "LG HAP media-first PTP receiver", profile: ReceiverProfileLG, wantSetups: 2, wantEvents: 1},
		{name: "AppleTV3 media-first", profile: ReceiverProfileAppleTV3, wantSetups: 2, wantEvents: 1},
		{name: "UxPlay without eventPort", profile: ReceiverProfileUxPlay, wantSetups: 2},
		{name: "AirServer AAC-ELD with descriptor retry", profile: ReceiverProfileAirServer, wantSetups: 3, wantEvents: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, client, ctx := newReceiverServerTestPair(t, ReceiverConfig{Profile: test.profile, AudioOnly: true})
			if client.info.SupportsScreen() {
				t.Fatal("audio-only receiver advertises screen mirroring")
			}
			if err := client.Pair(ctx, ""); err != nil {
				t.Fatalf("pair: %v", err)
			}
			if client.info.SupportsFairPlaySAP() {
				if err := client.FairPlaySetup(ctx); err != nil {
					t.Fatalf("FairPlay setup: %v", err)
				}
			}

			session, err := client.SetupAudioOnly(ctx, StreamConfig{})
			if err != nil {
				t.Fatalf("setup audio-only session: %v", err)
			}
			if !session.HasAudio() {
				t.Fatal("audio-only session has no audio stream")
			}
			if got := session.AudioStream().latencySamples; got != samplesFor44k1(defaultSpeakerAudioLatency) {
				t.Fatalf("playout lead = %d samples, want %d", got, samplesFor44k1(defaultSpeakerAudioLatency))
			}

			// AAC-ELD needs the optional fdk_aac encoder; its SETUP is still
			// covered above.
			if session.AudioCodec() == AudioCodecALAC {
				capture := &AudioCapture{pcmPipe: silentPCM{}, waitCh: make(chan struct{}), codec: AudioCodecALAC}
				streamCtx, cancel := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { done <- session.StreamAudio(streamCtx, capture, session.AudioStream()) }()
				deadline := time.Now().Add(2 * time.Second)
				for server.Stats().AudioPackets < 10 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("StreamAudio = %v, want cancellation", err)
				}
				if got := server.Stats().AudioPackets; got < 10 {
					t.Fatalf("receiver got %d audio packets, want at least 10", got)
				}
			}

			deadline := time.Now().Add(time.Second)
			for server.Stats().EventConnections != test.wantEvents && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if err := session.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Fatalf("close session: %v", err)
			}
			stats := server.Stats()
			if stats.SetupRequests != test.wantSetups {
				t.Fatalf("SETUP requests = %d, want %d", stats.SetupRequests, test.wantSetups)
			}
			if stats.RecordRequests != 1 || stats.FeedbackRequests < 1 || stats.TeardownRequests != 1 {
				t.Fatalf("control stats = %+v", stats)
			}
			if stats.VideoConnections != 0 || stats.EventConnections != test.wantEvents {
				t.Fatalf("media connections = %+v", stats)
			}
		})
	}
}

func TestAudioOnlyReceiverRejectsScreenMirroring(t *testing.T) {
	_, client, ctx := newReceiverServerTestPair(t, ReceiverConfig{Profile: ReceiverProfileRoku, AudioOnly: true})
	if err := client.Pair(ctx, ""); err != nil {
		t.Fatalf("pair: %v", err)
	}
	session, err := client.SetupMirror(ctx, StreamConfig{NoAudio: true})
	if err == nil {
		session.Close()
		t.Fatal("audio-only receiver accepted a screen mirroring session")
	}
}

func TestAudioOnlySessionPlist(t *testing.T) {
	request := mirrorSetupRequest{timingProtocol: timingProtocolNTP, timingPort: 7010, audioOnly: true}.sessionPlist()
	if request["isScreenMirroringSession"] != false || request["isMultiSelectAirPlay"] != false || request["senderSupportsRelay"] != false {
		t.Fatalf("audio-only session plist = %+v", request)
	}
	mirror := mirrorSetupRequest{timingProtocol: timingProtocolNTP, timingPort: 7010}.sessionPlist()
	if mirror["isScreenMirroringSession"] != true {
		t.Fatalf("mirror session plist = %+v", mirror)
	}
	if _, ok := mirror["isMultiSelectAirPlay"]; ok {
		t.Fatalf("mirror session plist gained speaker fields: %+v", mirror)
	}
}

func TestSpeakerLatencySamples(t *testing.T) {
	t.Cleanup(func() { SetTargetLatency(0) })
	for _, test := range []struct {
		target time.Duration
		want   uint32
	}{
		{target: 0, want: 22050},
		{target: 50 * time.Millisecond, want: speakerLatencyMinSamples},
		{target: time.Second, want: 44100},
		{target: 2 * time.Second, want: speakerLatencyMaxSamples},
	} {
		SetTargetLatency(test.target)
		if got := speakerLatencySamples(); got != test.want {
			t.Errorf("speakerLatencySamples() with target %v = %d, want %d", test.target, got, test.want)
		}
	}
}

func TestSpeakerAudioCodec(t *testing.T) {
	for _, test := range []struct {
		name    string
		mask    uint64
		want    AudioCodec
		wantErr bool
	}{
		{name: "no mask", want: AudioCodecALAC},
		{name: "ALAC and AAC-ELD", mask: screenAudioFormatALAC | screenAudioFormatAACELD44100Stereo, want: AudioCodecALAC},
		{name: "AAC-ELD only", mask: screenAudioFormatAACELD44100Stereo, want: AudioCodecAACELD},
		{name: "unsupported", mask: 0x2, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			info := &ReceiverInfo{SupportedFormats: StreamFormats{AudioStream: FormatMask(test.mask)}}
			got, err := speakerAudioCodec(info)
			if (err != nil) != test.wantErr || err == nil && got != test.want {
				t.Fatalf("speakerAudioCodec(0x%x) = %v, %v; want %v (error %t)", test.mask, got, err, test.want, test.wantErr)
			}
		})
	}
}
