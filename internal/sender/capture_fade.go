package sender

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// Fades are drawn by a black overlay whose alpha foxCast animates. gst-launch
// cannot animate element properties, so each capture pipeline gets a second
// compositor input: tiny raw BGRA frames written to a pipe, scaled over the
// canvas. rawvideoparse timestamps them by frame index, and the compositor
// only takes a frame when its time comes, so a pipe that holds about one frame
// paces the writer in real time and keeps the overlay within a few frames of
// what is encoded. Closing the pipe ends the overlay input and the compositor
// carries on without it, so a finished fade costs nothing.
const (
	fadeOverlaySize  = 32 // pixels square; 4 KiB per BGRA frame, one pipe page
	fadeOverlayPipe  = fadeOverlaySize * fadeOverlaySize * 4
	fadeOverlayMixer = "fade"
	fadeOverlayPad   = fadeOverlayMixer + ".sink_1"

	// Frame counts are in units of a second, scaled by the capture frame rate.
	fadeDurationDiv = 2 // fade in or out over 1/2 s
	fadeHoldDiv     = 2 // content holds black 1/2 s while the pipeline starts
	fadeSettleDiv   = 3 // FadeOut reports black 1/3 s after reaching it, past encoder latency

	placeholderText       = "Choosing what to share…"
	placeholderBackground = "0xff1c1c1e"
)

// videoFader writes the overlay frames for one capture pipeline.
type videoFader struct {
	w      *os.File
	fps    int
	frames int // fade length in frames

	mu     sync.Mutex
	opaque bool          // target: black (true) or clear (false)
	notify chan struct{} // closed once the opaque target is settled
	closed bool
	// closeWhenClear ends the overlay after it first fades to clear, for
	// captures that never fade out.
	closeWhenClear bool
	done           chan struct{} // closed when the writer exits
}

// newVideoFader returns a fader starting at black, and the pipe end for the
// pipeline. hold frames of black precede the first fade to clear.
func newVideoFader(fps, hold int, closeWhenClear bool) (*videoFader, *os.File, error) {
	if fps <= 0 {
		fps = 30
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("fade overlay pipe: %w", err)
	}
	if raw, err := w.SyscallConn(); err == nil {
		_ = raw.Control(func(fd uintptr) {
			// Best effort: a larger pipe only lets the overlay run further ahead.
			_, _ = unix.FcntlInt(fd, unix.F_SETPIPE_SZ, fadeOverlayPipe)
		})
	}
	f := &videoFader{
		w:              w,
		fps:            fps,
		frames:         max(1, fps/fadeDurationDiv),
		closeWhenClear: closeWhenClear,
		done:           make(chan struct{}),
	}
	go f.run(hold)
	return f, r, nil
}

func (f *videoFader) run(hold int) {
	defer close(f.done)
	defer f.Close()
	frame := make([]byte, fadeOverlayPipe)
	alpha := 255
	step := max(1, (255+f.frames-1)/f.frames)
	settle := max(1, f.fps/fadeSettleDiv)
	settled := 0
	for {
		f.mu.Lock()
		opaque, closed := f.opaque, f.closed
		f.mu.Unlock()
		if closed {
			return
		}
		if hold > 0 {
			hold--
		} else if opaque {
			alpha = min(255, alpha+step)
		} else {
			alpha = max(0, alpha-step)
		}
		for i := 3; i < len(frame); i += 4 {
			frame[i] = byte(alpha)
		}
		if _, err := f.w.Write(frame); err != nil {
			return
		}
		if hold > 0 || (opaque && alpha != 255) || (!opaque && alpha != 0) {
			settled = 0
			continue
		}
		settled++
		if !opaque && f.closeWhenClear {
			return
		}
		if opaque && settled == settle {
			f.mu.Lock()
			if f.notify != nil {
				close(f.notify)
				f.notify = nil
			}
			f.mu.Unlock()
		}
	}
}

// fadeOut fades to black and returns a channel closed once black has been
// encoded. It is closed at once when the overlay has already ended.
func (f *videoFader) fadeOut() <-chan struct{} {
	ch := make(chan struct{})
	if f == nil {
		close(ch)
		return ch
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		close(ch)
		return ch
	}
	f.opaque = true
	if f.notify != nil {
		close(f.notify)
	}
	f.notify = ch
	return ch
}

// Close ends the overlay input. It is safe to call more than once.
func (f *videoFader) Close() {
	if f == nil {
		return
	}
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		_ = f.w.Close()
		if f.notify != nil {
			close(f.notify)
			f.notify = nil
		}
	}
	f.mu.Unlock()
}

// withFadeOverlay names the (single) compositor stage and stacks the overlay
// input over its canvas.
func withFadeOverlay(stages []gstStage, width, height int) []gstStage {
	out := make([]gstStage, len(stages))
	for i, stage := range stages {
		if len(stage) > 0 && stage[0] == "compositor" {
			stage = append(append(gstStage(nil), stage...),
				"name="+fadeOverlayMixer,
				"sink_1::zorder=1",
				fmt.Sprintf("sink_1::width=%d", width),
				fmt.Sprintf("sink_1::height=%d", height),
			)
		}
		out[i] = stage
	}
	return out
}

// fadeOverlayChain is the separate gst-launch chain reading overlay frames
// from fd into the fade compositor.
func fadeOverlayChain(fd, fps int) []string {
	return []string{
		"fdsrc", fmt.Sprintf("fd=%d", fd), fmt.Sprintf("blocksize=%d", fadeOverlayPipe),
		"!", "rawvideoparse", "format=bgra",
		fmt.Sprintf("width=%d", fadeOverlaySize), fmt.Sprintf("height=%d", fadeOverlaySize),
		fmt.Sprintf("framerate=%d/1", fps),
		"!", fadeOverlayPad,
	}
}

// placeholderStages draws the "choosing what to share" card at the canvas
// size, fed into the fade compositor.
func placeholderStages(width, height, fps int, text bool) []gstStage {
	stages := []gstStage{
		{fmt.Sprintf("video/x-raw,width=%d,height=%d,framerate=%d/1", width, height, fps)},
	}
	if text {
		stages = append(stages, gstStage{
			"textoverlay", "text=" + placeholderText,
			fmt.Sprintf("font-desc=Sans %d", max(8, height/30)),
			"valignment=center", "halignment=center", "shaded-background=false",
		})
	}
	return append(stages, withFadeOverlay([]gstStage{
		{"compositor", "force-live=true", "ignore-inactive-pads=true", "background=black"},
		{fmt.Sprintf("video/x-raw,width=%d,height=%d,framerate=%d/1", width, height, fps)},
		lowLatencyVideoQueueStage(),
	}, width, height)...)
}

// startPlaceholderCapture streams the "choosing what to share" card, fading
// in from black. FadeOut fades it back to black before a switch.
func startPlaceholderCapture(ctx context.Context, cfg CaptureConfig, encoder encoderResult, timestampedOutput bool) (*ScreenCapture, error) {
	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	width, height := cfg.MaxWidth&^1, cfg.MaxHeight&^1
	if width <= 0 || height <= 0 {
		width, height = testCaptureWidth, testCaptureHeight
	}
	fader, overlay, err := newVideoFader(fps, 0, false)
	if err != nil {
		return nil, err
	}
	const overlayFd = 3
	source := gstStage{"videotestsrc", "is-live=true", "pattern=solid-color", "foreground-color=" + placeholderBackground}
	gstArgs := buildGstVideoPipeline(source, placeholderStages(width, height, fps, hasGstElement("textoverlay")), nil, encoder, 0, 0, timestampedOutput)
	gstArgs = append(gstArgs, fadeOverlayChain(overlayFd, fps)...)

	captureCtx, cancel := context.WithCancel(ctx)
	dbg("[CAPTURE] gst-launch-1.0 (placeholder) %s", strings.Join(gstArgs, " "))
	cmd := exec.CommandContext(captureCtx, "gst-launch-1.0", gstArgs...)
	cmd.ExtraFiles = []*os.File{overlay}
	capture, err := startCaptureCommand(cmd, cancel, encoder.codec, timestampedOutput)
	_ = overlay.Close() // the child inherited it
	if err != nil {
		fader.Close()
		return nil, err
	}
	capture.fader = fader
	log.Printf("[CAPTURE] showing the %q placeholder at %dx%d", placeholderText, width, height)
	return capture, nil
}

// startCaptureCommand starts a prepared gst-launch command whose stdout is
// the encoded stream.
func startCaptureCommand(cmd *exec.Cmd, cancel context.CancelFunc, codec VideoCodec, timestampedOutput bool) (*ScreenCapture, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gst stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gst stderr pipe: %w", err)
	}
	waitResult, err := startGStreamerCommand(cmd)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start gst-launch-1.0: %w", err)
	}
	go logStderr("GST", stderr)
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
