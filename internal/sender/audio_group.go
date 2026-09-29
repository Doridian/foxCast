package sender

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// groupChunkSamples is the fan-out unit. Outputs reassemble chunks into
	// their own codec frame size, so it need not match any codec.
	groupChunkSamples = 352
	// groupOutputQueue bounds each receiver's backlog (about 0.5 s). A
	// receiver that falls further behind loses chunks instead of holding up
	// the others; StreamAudio sees the gap as a source discontinuity.
	groupOutputQueue = 64

	// The -test source walks a beep around the layout, one channel at a
	// time, so each speaker can be matched to its position by ear.
	groupToneFrequency = 660
	groupToneSlot      = 600 * time.Millisecond
	groupToneOn        = 400 * time.Millisecond
	groupToneAmplitude = 0.3
)

// GroupOutput routes channels of a group capture to one receiver's stereo
// stream.
type GroupOutput struct {
	// Left and Right are channel indexes in the capture layout, or -1 for
	// silence. Equal indexes send one channel to both sides (a mono speaker).
	Left, Right int
	// Delay makes this receiver play later than the rest, to trim for
	// distance or for devices with more output latency.
	Delay time.Duration
	// Codec is the codec the receiver's session negotiated.
	Codec AudioCodec
}

// GroupCapture records one multichannel source and splits it into a stereo
// stream per receiver. Every output keeps the source's capture timestamps,
// so receivers timed from this sender's clock with the same playout lead
// play each sample at the same moment.
type GroupCapture struct {
	outputs []*AudioCapture
	queues  []chan groupChunk
	cancel  context.CancelFunc
	gstCmd  *exec.Cmd
	pipe    io.Closer
	done    chan struct{}
	running bool // fanOut was started and will close done
	stop    sync.Once
}

type groupChunk struct {
	pcm      []byte // interleaved stereo S16LE
	position audioPCMFramePosition
}

// groupSource yields interleaved frames of every layout channel.
type groupSource interface {
	ReadPCMFramePosition(dst []byte) (audioPCMFramePosition, error)
}

// StartGroupAudioCapture records source with the given layout and starts
// feeding one AudioCapture per output. With source.TestTone, a beep walks
// through the layout's channels in order instead.
func StartGroupAudioCapture(ctx context.Context, source AudioSource, layout ChannelLayout, outputs []GroupOutput) (*GroupCapture, error) {
	if len(outputs) == 0 {
		return nil, errors.New("group capture needs at least one output")
	}
	for i, output := range outputs {
		for _, channel := range []int{output.Left, output.Right} {
			if channel < -1 || channel >= len(layout) {
				return nil, fmt.Errorf("output %d uses channel %d outside the %d-channel layout", i, channel, len(layout))
			}
		}
		if output.Delay < 0 {
			return nil, fmt.Errorf("output %d has a negative delay", i)
		}
		if output.Codec != AudioCodecALAC && output.Codec != AudioCodecAACELD {
			return nil, fmt.Errorf("output %d has unsupported audio codec %d", i, output.Codec)
		}
	}

	captureCtx, cancel := context.WithCancel(ctx)
	g := &GroupCapture{cancel: cancel, done: make(chan struct{})}
	var src groupSource
	if source.TestTone {
		src = newGroupToneSource(captureCtx, len(layout))
		dbg("[AUDIO] group test source: beep walking %s", layout)
	} else {
		var err error
		if src, err = g.startRecording(captureCtx, source.Device, layout); err != nil {
			cancel()
			return nil, err
		}
	}

	for _, output := range outputs {
		queue := make(chan groupChunk, groupOutputQueue)
		ac := &AudioCapture{
			pcmFrames: &groupOutputReader{queue: queue},
			waitCh:    make(chan struct{}),
			codec:     output.Codec,
		}
		if output.Codec == AudioCodecAACELD {
			var err error
			// The !fdk_aac stub always fails, which staticcheck flags as SA4023.
			ac.eld, err = newELDEncoder() //nolint:staticcheck
			if err != nil {               //nolint:staticcheck
				g.outputs = append(g.outputs, ac)
				g.Stop()
				return nil, err
			}
		}
		g.outputs = append(g.outputs, ac)
		g.queues = append(g.queues, queue)
	}
	g.running = true
	go g.fanOut(captureCtx, src, len(layout), outputs)
	return g, nil
}

func (g *GroupCapture) startRecording(ctx context.Context, device string, layout ChannelLayout) (groupSource, error) {
	srcArgs, err := recordingSourceArgs(device)
	if err != nil {
		return nil, err
	}
	timestamped := supportsTimestampedAudioOutput()
	if !timestamped {
		audioTimestampFallbackWarning.Do(func() {
			log.Printf("[AUDIO] warning: GStreamer RTP/ONVIF timestamp elements are unavailable; using read-time audio clock fallback")
		})
	}
	args := audioCapturePipelineArgsLayout(srcArgs, groupChunkSamples, layout, timestamped)
	dbg("[AUDIO] group capture pipeline: gst-launch-1.0 %s", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "gst-launch-1.0", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("gst stdout pipe: %w", err)
	}
	stderr, _ := cmd.StderrPipe()
	waitResult, err := startGStreamerCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("start group audio capture pipeline: %w", err)
	}
	go logStderr("AUDIO-GST", stderr)
	g.gstCmd, g.pipe = cmd, stdout
	go func() {
		if err := <-waitResult; err != nil && ctx.Err() == nil {
			dbg("[AUDIO] group capture pipeline exited: %v", err)
		}
	}()
	frameBytes := len(layout) * audioBytesPerSample
	if timestamped {
		return &rtpL16PCMFrameReader{reader: stdout, now: time.Now, frameBytes: frameBytes}, nil
	}
	return &rawGroupSource{reader: stdout, frameBytes: frameBytes}, nil
}

// Output returns the capture feeding output i, for StreamAudio. It is
// stopped by Stop, not on its own.
func (g *GroupCapture) Output(i int) *AudioCapture { return g.outputs[i] }

// Stop ends the capture and every output.
func (g *GroupCapture) Stop() {
	g.stop.Do(func() {
		g.cancel()
		if g.pipe != nil {
			g.pipe.Close()
		}
		if g.gstCmd != nil && g.gstCmd.Process != nil {
			_ = g.gstCmd.Process.Kill()
		}
		if g.running {
			<-g.done
		}
		for _, ac := range g.outputs {
			ac.eldMu.Lock()
			if ac.eld != nil {
				ac.eld.Close()
				ac.eld = nil
			}
			ac.eldMu.Unlock()
		}
	})
}

// fanOut splits every source chunk into the outputs' queues until the
// source fails or ctx ends, then ends the outputs.
func (g *GroupCapture) fanOut(ctx context.Context, src groupSource, channels int, outputs []GroupOutput) {
	defer close(g.done)
	frame := make([]byte, groupChunkSamples*channels*audioBytesPerSample)
	dropped := make([]uint64, len(outputs))
	var err error
	for ctx.Err() == nil {
		var position audioPCMFramePosition
		if position, err = src.ReadPCMFramePosition(frame); err != nil {
			break
		}
		for i, output := range outputs {
			chunk := groupChunk{pcm: splitGroupChannels(frame, channels, output.Left, output.Right), position: position}
			chunk.position.PTS = chunk.position.PTS.Add(output.Delay)
			select {
			case g.queues[i] <- chunk:
			default:
				dropped[i]++
				if dropped[i] == 1 || dropped[i]%100 == 0 {
					dbg("[AUDIO] group output %d is behind; dropped %d chunks", i, dropped[i])
				}
			}
		}
	}
	if ctx.Err() != nil {
		err = io.EOF
	}
	for i, ac := range g.outputs {
		ac.waitErr = err
		if errors.Is(err, io.EOF) {
			ac.waitErr = nil
		}
		close(ac.waitCh)
		close(g.queues[i])
	}
}

// splitGroupChannels copies the left and right channels of an interleaved
// multichannel frame into a stereo frame. -1 is silence.
func splitGroupChannels(frame []byte, channels, left, right int) []byte {
	samples := len(frame) / (channels * audioBytesPerSample)
	out := make([]byte, samples*audioBytesPerSampleFrame)
	for s := range samples {
		in := frame[s*channels*audioBytesPerSample:]
		o := out[s*audioBytesPerSampleFrame:]
		if left >= 0 {
			copy(o[0:2], in[left*audioBytesPerSample:])
		}
		if right >= 0 {
			copy(o[2:4], in[right*audioBytesPerSample:])
		}
	}
	return out
}

// groupOutputReader reassembles one output's chunks into frames of
// whatever size its codec reads, carrying the chunk positions along.
type groupOutputReader struct {
	queue   <-chan groupChunk
	pending []byte
	next    audioPCMFramePosition // position of pending[0]
}

func (r *groupOutputReader) ReadPCMFrame(dst []byte) (time.Time, error) {
	position, err := r.ReadPCMFramePosition(dst)
	return position.PTS, err
}

func (r *groupOutputReader) ReadPCMFramePosition(dst []byte) (audioPCMFramePosition, error) {
	if len(dst) == 0 || len(dst)%audioBytesPerSampleFrame != 0 {
		return audioPCMFramePosition{}, fmt.Errorf("group audio: PCM frame size %d is not whole stereo S16 samples", len(dst))
	}
	for len(r.pending) < len(dst) {
		chunk, ok := <-r.queue
		if !ok {
			return audioPCMFramePosition{}, io.EOF
		}
		expected := r.next.SourceRTP + uint32(len(r.pending)/audioBytesPerSampleFrame)
		if len(r.pending) == 0 || chunk.position.SourceRTP != expected {
			// A dropped chunk leaves a gap: restart from this chunk rather
			// than splice samples that were not adjacent.
			r.pending = r.pending[:0]
			r.next = chunk.position
		}
		r.pending = append(r.pending, chunk.pcm...)
	}
	position := r.next
	copy(dst, r.pending)
	r.pending = r.pending[len(dst):]
	samples := uint64(len(dst) / audioBytesPerSampleFrame)
	r.next.SourceRTP += uint32(samples)
	r.next.PTS = r.next.PTS.Add(audioSamplesDuration(samples))
	return position, nil
}

// rawGroupSource reads untimestamped interleaved S16LE and gives it a
// sample-counted clock anchored at the first read. Every output shares the
// resulting timestamps, which is what keeps them in step.
type rawGroupSource struct {
	reader     io.Reader
	frameBytes int
	anchor     time.Time
	samples    uint64
}

func (r *rawGroupSource) ReadPCMFramePosition(dst []byte) (audioPCMFramePosition, error) {
	if _, err := io.ReadFull(r.reader, dst); err != nil {
		return audioPCMFramePosition{}, err
	}
	if r.anchor.IsZero() {
		r.anchor = time.Now()
	}
	position := audioPCMFramePosition{
		PTS:          r.anchor.Add(audioSamplesDuration(r.samples)),
		SourceRTP:    uint32(r.samples),
		HasSourceRTP: true,
	}
	r.samples += uint64(len(dst) / r.frameBytes)
	return position, nil
}

// groupToneSource generates the -test beep in real time.
type groupToneSource struct {
	ctx      context.Context
	channels int
	start    time.Time
	samples  uint64
}

func newGroupToneSource(ctx context.Context, channels int) *groupToneSource {
	return &groupToneSource{ctx: ctx, channels: channels}
}

func (t *groupToneSource) ReadPCMFramePosition(dst []byte) (audioPCMFramePosition, error) {
	if t.start.IsZero() {
		t.start = time.Now()
	}
	frameBytes := t.channels * audioBytesPerSample
	count := len(dst) / frameBytes
	position := audioPCMFramePosition{
		PTS:          t.start.Add(audioSamplesDuration(t.samples)),
		SourceRTP:    uint32(t.samples),
		HasSourceRTP: true,
	}
	// Deliver each chunk once it has been "recorded", like a live source.
	wait := time.Until(position.PTS.Add(audioSamplesDuration(uint64(count))))
	if wait > 0 {
		select {
		case <-t.ctx.Done():
			return audioPCMFramePosition{}, io.EOF
		case <-time.After(wait):
		}
	}
	clear(dst)
	fillGroupTone(dst, t.channels, t.samples)
	t.samples += uint64(count)
	return position, nil
}

// fillGroupTone writes the beep for the frames starting at sample first:
// during slot k of each cycle, channel k%channels plays groupToneFrequency.
func fillGroupTone(dst []byte, channels int, first uint64) {
	slot := uint64(durationToAudioSamples(groupToneSlot))
	on := uint64(durationToAudioSamples(groupToneOn))
	frameBytes := channels * audioBytesPerSample
	for i := range len(dst) / frameBytes {
		n := first + uint64(i)
		if n%slot >= on {
			continue
		}
		channel := int(n/slot) % channels
		value := int16(groupToneAmplitude * math.MaxInt16 *
			math.Sin(2*math.Pi*groupToneFrequency*float64(n)/audioSampleRate))
		offset := i*frameBytes + channel*audioBytesPerSample
		dst[offset] = byte(value)
		dst[offset+1] = byte(uint16(value) >> 8)
	}
}
