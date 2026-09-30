package sender

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// testFLV encodes a short H.264/AAC FLV with ffmpeg, without the AVC end of
// sequence that would end it, so it plays on like a live stream.
func testFLV(t *testing.T) []byte {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	var out, stderr bytes.Buffer
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=30:duration=1",
		"-f", "lavfi", "-i", "sine=duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-f", "flv", "-")
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, stderr.String())
	}
	const headerSize, tagHeaderSize = 13, 11
	data := out.Bytes()
	live := append([]byte(nil), data[:headerSize]...)
	for i := headerSize; i+tagHeaderSize <= len(data); {
		size := int(data[i+1])<<16 | int(data[i+2])<<8 | int(data[i+3])
		end := i + tagHeaderSize + size + 4
		if end > len(data) {
			break
		}
		body := data[i+tagHeaderSize : i+tagHeaderSize+size]
		if data[i] != 9 || len(body) < 2 || body[1] != 2 {
			live = append(live, data[i:end]...)
		}
		i = end
	}
	return live
}

func startTestIngest(t *testing.T) (*ScreenCapture, *IngestDisplay) {
	t.Helper()
	for _, element := range append([]string{"openh264enc", "avdec_h264"}, ingestElements...) {
		if !hasGstElement(element) {
			t.Skipf("GStreamer %s is needed", element)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := CaptureConfig{FPS: 30, HWAccel: "openh264", VideoCodec: VideoCodecH264, Placeholder: PlaceholderWaitingForStream}
	base, err := PrepareIngestCapture(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := base.StartWithCodec(640, 360, VideoCodecH264)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(capture.Stop)
	var dump *os.File
	if path := os.Getenv("FOXCAST_INGEST_DUMP"); path != "" {
		if dump, err = os.Create(path); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dump.Close() })
	}
	go func() {
		for {
			unit, err := capture.ReadVideoAccessUnit()
			if err != nil {
				return
			}
			if dump != nil {
				_, _ = dump.Write(unit.AnnexB)
			}
		}
	}()
	display, err := NewIngestDisplay(capture, "")
	if err != nil {
		t.Fatal(err)
	}
	return capture, display
}

// mixerSources counts the compositor's sources.
func mixerSources(m *videoMixer) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sources)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// openStream is an FLV stream that stays open, like a connected publisher,
// until closed.
func openStream(data []byte) (io.ReadCloser, *io.PipeWriter) {
	r, w := io.Pipe()
	go func() { _, _ = w.Write(data) }()
	return r, w
}

func TestIngestShowsStreamsAndFallsBack(t *testing.T) {
	flv := testFLV(t)
	capture, display := startTestIngest(t)
	m := capture.mixer
	time.Sleep(500 * time.Millisecond)
	if n := mixerSources(m); n != 1 {
		t.Fatalf("%d sources before a stream, want the placeholder alone", n)
	}

	// A stream shows on top of the placeholder, and goes when it ends.
	stream, w := openStream(flv)
	result := make(chan error, 1)
	go func() { result <- display.Play(context.Background(), stream) }()
	waitFor(t, "the first stream", func() bool { return mixerSources(m) == 2 })
	time.Sleep(500 * time.Millisecond)
	_ = w.Close()
	if err := <-result; err != nil {
		t.Fatalf("first stream: %v", err)
	}
	waitFor(t, "the placeholder alone", func() bool { return mixerSources(m) == 1 })

	// A reconnecting client replaces a stream that is still open.
	stale, staleW := openStream(flv)
	defer staleW.Close()
	staleResult := make(chan error, 1)
	go func() { staleResult <- display.Play(context.Background(), stale) }()
	waitFor(t, "the stale stream", func() bool { return mixerSources(m) == 2 })
	fresh, freshW := openStream(flv)
	freshResult := make(chan error, 1)
	go func() { freshResult <- display.Play(context.Background(), fresh) }()
	select {
	case err := <-staleResult:
		if err != nil {
			t.Fatalf("replaced stream: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stale stream was not replaced")
	}
	waitFor(t, "the fresh stream", func() bool { return mixerSources(m) == 2 })
	_ = freshW.Close()
	if err := <-freshResult; err != nil {
		t.Fatalf("fresh stream: %v", err)
	}
	waitFor(t, "the placeholder alone", func() bool { return mixerSources(m) == 1 })

	// Garbage fails only its own decoder; the session keeps running.
	// Well-framed FLV (as the RTMP server always writes) whose H.264 is not.
	garbage := append([]byte(nil), flv[:13]...)
	for i := range 60 {
		payload := append([]byte{0x17, byte(min(i, 1)), 0, 0, 0}, bytes.Repeat([]byte{byte(i), 0xff, 0x00}, 300)...)
		size := len(payload)
		garbage = append(garbage, 9, byte(size>>16), byte(size>>8), byte(size), 0, 0, byte(i*33), 0, 0, 0, 0)
		garbage = append(garbage, payload...)
		garbage = append(garbage, 0, 0, byte((size+11)>>8), byte(size+11))
	}
	garbageStream, garbageW := openStream(garbage)
	garbageResult := make(chan error, 1)
	go func() { garbageResult <- display.Play(context.Background(), garbageStream) }()
	time.Sleep(time.Second)
	_ = garbageW.Close()
	select {
	case err := <-garbageResult:
		t.Logf("garbage stream: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("a garbage stream kept playing after it closed")
	}
	select {
	case <-capture.waitCh:
		t.Fatalf("a bad stream stopped the session capture: %v", capture.waitErr)
	default:
	}
	stream, w = openStream(flv)
	go func() { result <- display.Play(context.Background(), stream) }()
	waitFor(t, "a stream after the bad one", func() bool { return mixerSources(m) == 2 })
	_ = w.Close()
	if err := <-result; err != nil {
		t.Fatalf("stream after the bad one: %v", err)
	}
}
