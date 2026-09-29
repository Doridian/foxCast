package sender

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// startTestMixer starts a session capture with the placeholder, encoded by
// openh264, and a switcher whose sources are test patterns.
func startTestMixer(t *testing.T) (*ScreenCapture, *CaptureSwitcher) {
	t.Helper()
	if !hasGstElement("openh264enc") || !hasGstElement("compositor") {
		t.Skip("GStreamer openh264enc and compositor are needed")
	}
	cfg := CaptureConfig{FPS: 30, HWAccel: "openh264", VideoCodec: VideoCodecH264, DeferSource: true}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	base := &CapturePreparation{ctx: ctx, cfg: cfg, kind: capturePreparationWayland, timestampedOutput: supportsTimestampedVideoOutput(VideoCodecH264)}
	capture, err := base.StartWithCodec(640, 360, VideoCodecH264)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(capture.Stop)
	switcher, err := NewCaptureSwitcher(ctx, base, capture)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(switcher.Close)
	nodes := []uint32{1, 2}
	switcher.pick = func(ctx context.Context, _ string) (*portalSource, error) {
		if len(nodes) == 0 {
			return nil, ErrPortalCancelled
		}
		node := nodes[0]
		nodes = nodes[1:]
		return &portalSource{nodeID: node}, nil
	}
	switcher.sourceBin = func(src *portalSource, width, height, fps int) (string, []*os.File, error) {
		pattern := map[uint32]string{1: "white", 2: "smpte"}[src.nodeID]
		return fmt.Sprintf("videotestsrc is-live=true pattern=%s ! capsfilter caps=video/x-raw,width=%d,height=%d,framerate=%d/1", pattern, width, height, fps), nil, nil
	}
	return capture, switcher
}

func TestCaptureSwitcherKeepsOneEncoderAcrossSwitches(t *testing.T) {
	capture, switcher := startTestMixer(t)
	type frame struct {
		at  time.Time
		pts time.Time
		sps string
	}
	frames := make(chan frame, 1024)
	var dump *os.File
	if path := os.Getenv("FOXCAST_SWITCH_DUMP"); path != "" {
		dump, _ = os.Create(path)
		defer dump.Close()
	}
	go func() {
		defer close(frames)
		for {
			unit, err := capture.ReadVideoAccessUnit()
			if err != nil {
				return
			}
			if dump != nil {
				_, _ = dump.Write(unit.AnnexB)
			}
			f := frame{at: time.Now(), pts: unit.PTS}
			for _, nal := range splitAnnexBAccessUnit(unit.AnnexB) {
				if nalType(nal) == 7 {
					f.sps = string(stripStartCode(nal))
				}
			}
			frames <- f
		}
	}()

	time.Sleep(700 * time.Millisecond)
	for i := range 2 {
		started := time.Now()
		if err := switcher.Switch(context.Background(), false); err != nil {
			t.Fatalf("switch %d: %v", i+1, err)
		}
		// Fade out, then in: about twice the fade.
		if took := time.Since(started); took < 2*fadeDuration-100*time.Millisecond || took > 2*fadeDuration+time.Second {
			t.Errorf("switch %d took %v", i+1, took)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err := switcher.Switch(context.Background(), false); !errors.Is(err, ErrPortalCancelled) {
		t.Fatalf("cancelled pick = %v, want ErrPortalCancelled", err)
	}
	capture.Stop()

	var got []frame
	for f := range frames {
		got = append(got, f)
	}
	if len(got) < 60 {
		t.Fatalf("only %d frames", len(got))
	}
	// One encoder for the whole session: its parameter sets never change.
	// (Encoders may still add keyframes during a fade.)
	configs := map[string]bool{}
	for i, f := range got {
		if f.sps != "" {
			configs[f.sps] = true
		}
		if i == 0 {
			continue
		}
		if gap := f.at.Sub(got[i-1].at); gap > 250*time.Millisecond {
			t.Errorf("frame %d arrived %v after the previous one", i, gap)
		}
		if !f.pts.IsZero() && !f.pts.After(got[i-1].pts) {
			t.Errorf("frame %d PTS %v is not after %v", i, f.pts, got[i-1].pts)
		}
	}
	if len(configs) != 1 {
		t.Errorf("%d different SPS, want the one encoder's", len(configs))
	}
}
