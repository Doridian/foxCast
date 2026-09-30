package sender

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

// An RTMP ingest session shows a placeholder on the session compositor (see
// capture_mixer.go) and puts each published stream on top of it while it
// plays. A stream is decoded by its own pipeline, which hands frames to the
// session through intervideosink/intervideosrc and plays its audio into the
// receiver's virtual output device, which the session already forwards. A
// broken or malformed stream so only ends its own pipeline, and the next
// client starts afresh; the receiver's session and encoder never restart.
const (
	ingestSinkName     = "ingestvideo"
	ingestChannelBase  = "foxcast-ingest"
	ingestFirstFrame   = 15 * time.Second
	ingestFramePoll    = 20 * time.Millisecond
	ingestRawFormat    = "I420"
	ingestFlvReadBlock = 64 << 10
)

// ingestElements are the GStreamer elements an ingest needs besides the
// session's encoder.
var ingestElements = []string{"compositor", "fdsrc", "flvdemux", "decodebin", "intervideosink", "intervideosrc"}

// IngestDisplay shows published FLV streams on a session compositor, one at
// a time. The newest stream wins: Play replaces whatever is playing.
type IngestDisplay struct {
	mixer     *videoMixer
	audioSink string // PulseAudio sink for stream audio; empty drops it

	mu      sync.Mutex
	next    int
	stop    context.CancelFunc
	stopped chan struct{}
}

// NewIngestDisplay takes over capture, the session started from
// PrepareIngestCapture. Stream audio is played into audioSink, the
// PulseAudio sink whose monitor the session forwards; empty drops it.
func NewIngestDisplay(capture *ScreenCapture, audioSink string) (*IngestDisplay, error) {
	if capture == nil || capture.mixer == nil {
		return nil, errors.New("ingest needs a session compositor capture")
	}
	return &IngestDisplay{mixer: capture.mixer, audioSink: audioSink}, nil
}

// Play decodes the FLV stream and shows it until the stream ends, ctx is
// cancelled, or a newer Play replaces it; then the placeholder shows again.
// Play closes stream before it returns. A stream that ends normally returns
// nil.
func (d *IngestDisplay) Play(ctx context.Context, stream io.ReadCloser) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	d.mu.Lock()
	if d.stop != nil {
		d.stop()
	}
	previous := d.stopped
	d.stop, d.stopped = cancel, done
	d.next++
	channel := fmt.Sprintf("%s-%p-%d", ingestChannelBase, d, d.next)
	d.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = stream.Close()
	}()
	if previous != nil {
		select {
		case <-previous:
		case <-ctx.Done():
			return nil
		}
	}
	return d.play(ctx, stream, channel)
}

func (d *IngestDisplay) play(ctx context.Context, stream io.Reader, channel string) error {
	m := d.mixer
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("ingest pipe: %w", err)
	}
	defer pw.Close()
	decodeCtx, stopDecode := context.WithCancel(ctx)
	defer stopDecode()
	cmd := newGstCommand(decodeCtx, ingestDecodeArgs(channel, m.width, m.height, d.audioSink)...)
	cmd.ExtraFiles = []*os.File{pr}
	err = cmd.Start()
	_ = pr.Close() // the pipeline has its own duplicate
	if err != nil {
		return fmt.Errorf("start stream decoder: %w", err)
	}
	defer func() {
		cmd.Kill()
		<-cmd.done
	}()

	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(pw, stream)
		_ = pw.Close()
		copied <- err
	}()

	// Put the stream on screen once it has decoded a picture.
	shown := make(chan *mixerSource, 1)
	go func() {
		defer close(shown)
		if err := waitForIngestFrame(decodeCtx, cmd); err != nil {
			if decodeCtx.Err() == nil {
				log.Printf("[INGEST] %v", err)
			}
			return
		}
		source, err := m.add(ingestSourceBin(channel, m.width, m.height, m.fps))
		if err != nil {
			log.Printf("[INGEST] warning: show stream: %v", err)
			return
		}
		m.show(source)
		log.Printf("[INGEST] showing the stream")
		shown <- source
	}()
	defer func() {
		stopDecode()
		if source := <-shown; source != nil {
			m.remove(source, nil)
			log.Printf("[INGEST] stream gone; showing the placeholder")
		}
	}()

	select {
	case err := <-copied:
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("read stream: %w", err)
		}
		return nil
	case <-cmd.done:
		if cmd.err != nil {
			return fmt.Errorf("decode stream: %w", cmd.err)
		}
		return nil
	case <-ctx.Done():
		return nil
	}
}

// waitForIngestFrame waits until the decoder has handed a frame to its
// intervideosink.
func waitForIngestFrame(ctx context.Context, cmd *gstCommand) error {
	sink := cmd.Element(ingestSinkName)
	if sink == nil {
		return errors.New("stream decoder has no video sink")
	}
	defer sink.Release()
	pad, err := sink.StaticPad("sink")
	if err != nil {
		return err
	}
	defer pad.Release()
	pad.CountBuffers()
	deadline := time.After(ingestFirstFrame)
	for pad.Buffers() == 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cmd.done:
			return errGstStopped
		case <-deadline:
			return fmt.Errorf("the stream sent no picture in %v", ingestFirstFrame)
		case <-time.After(ingestFramePoll):
		}
	}
	return nil
}

// ingestDecodeArgs decodes FLV from fd=3: video, fitted to the canvas, to an
// intervideosink on channel, and audio to the PulseAudio sink audioSink.
// Neither sink syncs: the publisher paces the stream in real time, and the
// session's compositor takes whatever frame is newest.
func ingestDecodeArgs(channel string, width, height int, audioSink string) []string {
	args := []string{
		"fdsrc", "fd=3", fmt.Sprintf("blocksize=%d", ingestFlvReadBlock),
		"!", "flvdemux", "name=demux",
		"demux.video", "!", "queue",
		"!", "decodebin",
		"!", "videoconvert",
		"!", "videoscale", "add-borders=true",
		"!", fmt.Sprintf("video/x-raw,format=%s,width=%d,height=%d,pixel-aspect-ratio=1/1", ingestRawFormat, width, height),
		"!", "intervideosink", "name=" + ingestSinkName, "channel=" + channel, "sync=false", "async=false",
		"demux.audio", "!", "queue",
	}
	if audioSink == "" || !hasGstElement("pulsesink") {
		return append(args, "!", "fakesink", "sync=false", "async=false")
	}
	return append(args,
		"!", "decodebin",
		"!", "audioconvert",
		"!", "audioresample",
		"!", "pulsesink", "device="+audioSink, "sync=false", "async=false",
		"client-name=foxCast RTMP ingest",
	)
}

// ingestSourceBin reads a decoded stream's frames into the compositor.
func ingestSourceBin(channel string, width, height, fps int) string {
	return gstDescription([]gstStage{
		{"intervideosrc", "channel=" + channel},
		{"capsfilter", fmt.Sprintf("caps=video/x-raw,format=%s,width=%d,height=%d,framerate=%d/1", ingestRawFormat, width, height, fps)},
	})
}
