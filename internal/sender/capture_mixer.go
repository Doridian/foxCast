package sender

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/gst"
)

// A Wayland mirror session runs one pipeline for its whole life: a compositor
// feeding the encoder. What is shared is a source bin on one of the
// compositor's pads: first a "choosing what to share" placeholder, then each
// screen or window the user picks. Changing sources fades the current pad's
// alpha to the black background, swaps the bin, and fades the new pad in, so
// the encoder, its keyframes and the receiver's decoder never restart. At
// alpha 1 the compositor copies the frame, so steady state costs nothing
// extra; blending happens only during a fade.
const (
	mixerName      = "mix"
	mixerSinkPads  = "sink_%u"
	fadeDuration   = 500 * time.Millisecond
	firstFrameWait = 5 * time.Second

	placeholderText       = "Choosing what to share…"
	placeholderBackground = "0xff1c1c1e"
)

// videoMixer is the running session pipeline.
type videoMixer struct {
	cmd           *gstCommand
	mix           *gst.Element
	width, height int
	fps           int

	mu      sync.Mutex
	closed  bool
	sources []*mixerSource

	// The placeholder's fade-in runs on its own; a switch stops it first.
	introCancel context.CancelFunc
	introDone   chan struct{}
}

// mixerSource is a source bin linked to a compositor pad.
type mixerSource struct {
	bin     *gst.Element
	pad     *gst.Pad
	files   []*os.File // descriptors the bin reads, closed after it is removed
	removed bool       // guarded by videoMixer.mu
}

// startMixerCapture starts a Wayland session pipeline showing the placeholder,
// fading in from black.
func startMixerCapture(ctx context.Context, cfg CaptureConfig, encoder encoderResult, timestampedOutput bool) (*ScreenCapture, error) {
	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	width, height := cfg.MaxWidth&^1, cfg.MaxHeight&^1
	if width <= 0 || height <= 0 {
		width, height = testCaptureWidth, testCaptureHeight
	}
	source := gstStage{"compositor", "name=" + mixerName, "force-live=true", "ignore-inactive-pads=true", "background=black"}
	// A fixed format keeps a new source from renegotiating the encoder. Adding
	// a pad still makes the compositor emit one frame stamped at the start of
	// its timeline; videorate drops it, since the encoder needs monotonic PTS.
	stages := []gstStage{
		{fmt.Sprintf("video/x-raw,format=%s,width=%d,height=%d,framerate=%d/1", encoder.rawFormat, width, height, fps)},
		{"videorate", "drop-only=true"},
		lowLatencyVideoQueueStage(),
	}
	gstArgs := buildGstVideoPipeline(source, stages, nil, encoder, 0, 0, timestampedOutput)

	captureCtx, cancel := context.WithCancel(ctx)
	cmd := newGstCommand(captureCtx, gstArgs...)
	capture, err := startCaptureCommand(cmd, cancel, encoder.codec, timestampedOutput)
	if err != nil {
		return nil, err
	}
	if err := cmd.withPipeline(func(p *gst.Pipeline) error { return p.WaitPlaying(firstFrameWait) }); err != nil {
		capture.Stop()
		return nil, err
	}
	mix := cmd.Element(mixerName)
	if mix == nil {
		capture.Stop()
		return nil, errors.New("capture pipeline has no compositor")
	}
	m := &videoMixer{cmd: cmd, mix: mix, width: width, height: height, fps: fps}
	capture.mixer = m
	go func() {
		<-cmd.done
		m.close()
	}()

	text := cfg.PlaceholderText
	if text == "" {
		text = placeholderText
	}
	placeholder, err := m.add(placeholderBin(width, height, fps, text, hasGstElement("textoverlay")))
	if err != nil {
		capture.Stop()
		return nil, fmt.Errorf("show placeholder: %w", err)
	}
	introCtx, introCancel := context.WithCancel(captureCtx)
	m.introCancel, m.introDone = introCancel, make(chan struct{})
	go func() {
		defer close(m.introDone)
		if err := m.fadeIn(introCtx, placeholder); err != nil && introCtx.Err() == nil {
			log.Printf("[CAPTURE] warning: placeholder: %v", err)
		}
	}()
	log.Printf("[CAPTURE] showing the %q placeholder at %dx%d", text, width, height)
	return capture, nil
}

// placeholderBin draws the placeholder card, with text when textoverlay is
// available.
func placeholderBin(width, height, fps int, text string, overlay bool) string {
	stages := []gstStage{
		{"videotestsrc", "is-live=true", "pattern=solid-color", "foreground-color=" + placeholderBackground},
		{"capsfilter", fmt.Sprintf("caps=video/x-raw,width=%d,height=%d,framerate=%d/1", width, height, fps)},
	}
	if overlay {
		stages = append(stages, gstStage{
			"textoverlay", "text=" + text,
			fmt.Sprintf("font-desc=Sans %d", max(8, height/30)),
			"valignment=center", "halignment=center", "shaded-background=false",
		})
	}
	return gstDescription(stages)
}

// pipeWireBin reads PipeWire node nodeID through fd and fits it to the canvas.
// The portal's stream size is in logical pixels while PipeWire delivers
// physical ones, so the frames are scaled rather than cropped to the canvas.
func pipeWireBin(fd int, nodeID uint32, width, height, fps int) string {
	stages := []gstStage{pipeWireVideoSourceStage(fd, nodeID, fps)}
	if hasGstElement("vapostproc") {
		stages = append(stages, gstStage{"vapostproc"})
	}
	stages = append(stages,
		gstStage{"videoscale", "add-borders=true"},
		gstStage{"capsfilter", fmt.Sprintf("caps=video/x-raw,width=%d,height=%d,pixel-aspect-ratio=1/1", width, height)},
	)
	return gstDescription(stages)
}

// gstDescription joins stages into a gst-launch description string, quoting
// values that contain spaces.
func gstDescription(stages []gstStage) string {
	parts := make([]string, 0, len(stages))
	for _, stage := range stages {
		words := make([]string, len(stage))
		for i, word := range stage {
			if key, value, ok := strings.Cut(word, "="); ok && strings.ContainsAny(value, " \t\"") {
				word = key + "=" + `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
			}
			words[i] = word
		}
		parts = append(parts, strings.Join(words, " "))
	}
	return strings.Join(parts, " ! ")
}

// add links a new source bin, invisible, to the compositor. files are
// descriptors the bin reads; the mixer closes them when the source goes.
func (m *videoMixer) add(desc string, files ...*os.File) (*mixerSource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		closeFiles(files)
		return nil, errGstStopped
	}
	s := &mixerSource{files: files}
	err := m.cmd.withPipeline(func(p *gst.Pipeline) error {
		bin, err := p.AddBin(desc)
		if err != nil {
			return err
		}
		s.bin = bin
		src, err := bin.StaticPad("src")
		if err == nil {
			defer src.Release()
			s.pad, err = m.mix.RequestPad(mixerSinkPads)
		}
		if err == nil {
			s.pad.Set("alpha", "0")
			s.pad.CountBuffers()
			err = src.Link(s.pad)
		}
		if err == nil {
			err = bin.SyncState()
		}
		if err != nil {
			p.RemoveBin(bin)
			s.pad.Release()
		}
		return err
	})
	if err != nil {
		closeFiles(files)
		return nil, err
	}
	m.sources = append(m.sources, s)
	dbg("[CAPTURE] mixer source added: %s", desc)
	return s, nil
}

// stopIntro ends the placeholder's fade-in, leaving it where it got to.
func (m *videoMixer) stopIntro() {
	m.introCancel()
	<-m.introDone
}

// remove takes a source off the compositor. Stopping its bin can take a
// while (a failed pipewiresrc needs about 30 s), so that happens in the
// background, and then done runs.
func (m *videoMixer) remove(s *mixerSource, done func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.removed {
		return
	}
	s.removed = true
	for i, current := range m.sources {
		if current == s {
			m.sources = append(m.sources[:i], m.sources[i+1:]...)
			break
		}
	}
	go func() {
		// The bin stops before its compositor pad goes: a source still pushing
		// into a released pad would fail the pipeline with not-linked.
		err := m.cmd.withPipeline(func(p *gst.Pipeline) error {
			p.RemoveBin(s.bin)
			s.pad.Release()
			return nil
		})
		if err != nil {
			s.bin.Release()
			s.pad.Unref()
		}
		closeFiles(s.files)
		if done != nil {
			done()
		}
	}()
}

// fadeIn waits for the source's first frame and fades it in.
func (m *videoMixer) fadeIn(ctx context.Context, s *mixerSource) error {
	deadline := time.Now().Add(firstFrameWait)
	for m.buffers(s) == 0 {
		if time.Now().After(deadline) {
			return errors.New("the new source sent no picture")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.cmd.done:
			return errGstStopped
		case <-time.After(10 * time.Millisecond):
		}
	}
	return m.fade(ctx, s, 0, 1)
}

// show makes a source fully visible at once, without a fade.
func (m *videoMixer) show(s *mixerSource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !s.removed {
		s.pad.Set("alpha", "1")
	}
}

// buffers returns how many frames the source has delivered.
func (m *videoMixer) buffers(s *mixerSource) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.removed {
		return 0
	}
	return s.pad.Buffers()
}

// fade animates a source's alpha, one step per frame.
func (m *videoMixer) fade(ctx context.Context, s *mixerSource, from, to float64) error {
	steps := max(1, int(fadeDuration*time.Duration(m.fps)/time.Second))
	ticker := time.NewTicker(time.Second / time.Duration(m.fps))
	defer ticker.Stop()
	for i := 1; i <= steps; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.cmd.done:
			return errGstStopped
		case <-ticker.C:
		}
		m.mu.Lock()
		if !s.removed {
			// Ease in and out: t² (3 - 2t).
			t := float64(i) / float64(steps)
			t = t * t * (3 - 2*t)
			s.pad.Set("alpha", fmt.Sprintf("%.4f", from+(to-from)*t))
		}
		m.mu.Unlock()
	}
	return nil
}

// close releases the mixer's references once its pipeline has stopped.
func (m *videoMixer) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	// The pipeline is shutting down: drop references, release nothing.
	for _, s := range m.sources {
		s.removed = true
		s.bin.Release()
		s.pad.Unref()
		closeFiles(s.files)
	}
	m.sources = nil
	m.mix.Release()
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// startCaptureCommand starts a pipeline whose stdout is the encoded stream.
func startCaptureCommand(cmd *gstCommand, cancel context.CancelFunc, codec VideoCodec, timestampedOutput bool) (*ScreenCapture, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	waitResult, err := startGStreamerCommand(cmd)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start capture pipeline: %w", err)
	}
	capture := &ScreenCapture{
		cmd:    cmd,
		stdout: stdout,
		cancel: cancel,
		waitCh: make(chan struct{}),
	}
	if timestampedOutput {
		capture.frames = newRTPVideoAccessUnitReader(stdout, codec)
	}
	go func() {
		capture.waitErr = <-waitResult
		close(capture.waitCh)
	}()
	return capture, nil
}
