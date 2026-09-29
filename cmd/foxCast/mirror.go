package main

// Screen mirroring. Adapted from doubletake's cmd/doubletake single-target
// path (LGPL-3.0-or-later).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

// mirrorUDPPorts is the number of consecutive UDP ports a mirror session
// needs (timing, audio control, audio data).
const mirrorUDPPorts = 3

// mirrorOptions are the mirroring flags.
type mirrorOptions struct {
	fps             int
	bitrate         int
	targetLatencyMs int
	hwaccel         string
	videoCodec      string
	testMode        bool
	noEncrypt       bool
	directKey       bool
	noAudio         bool
	audioOnly       bool
	audioSource     string
	keepDefaultSink bool
	x11WindowID     string
	x11WindowName   string
	noCursor        bool
	rememberSource  bool

	xid uint64
}

func (m *mirrorOptions) register(flags *flag.FlagSet) {
	flags.IntVar(&m.fps, "fps", 30, "frames per second")
	flags.IntVar(&m.bitrate, "bitrate", 0, "video bitrate in kbps (0 = auto)")
	flags.IntVar(&m.targetLatencyMs, "target-latency-ms", 0, "joint audio/video playout latency override in ms (0 = automatic)")
	flags.StringVar(&m.hwaccel, "hwaccel", "auto", "encoder: auto, nvenc, vaapi, openh264, none (x264/x265)")
	flags.StringVar(&m.videoCodec, "video-codec", "auto", "screen codec: auto, h264, or hevc")
	flags.BoolVar(&m.testMode, "test", false, "use synthetic video/audio instead of screen capture")
	flags.BoolVar(&m.noEncrypt, "no-encrypt", false, "disable RTSP header encryption (debugging only)")
	flags.BoolVar(&m.directKey, "direct-key", false, "use shk/shiv directly without SHA-512 derivation")
	flags.BoolVar(&m.noAudio, "no-audio", false, "disable audio streaming")
	flags.BoolVar(&m.audioOnly, "audio-only", false, "stream only audio, using the receiver as a speaker (automatic for receivers without screen mirroring)")
	flags.StringVar(&m.audioSource, "audio-source", audioSourceSink, "audio to forward: \"sink\" (a virtual output device for this receiver), \"monitor\" (whatever the default output plays), or a PulseAudio source name")
	flags.BoolVar(&m.keepDefaultSink, "keep-default-sink", false, "with -audio-source sink, do not make the virtual output device the default")
	flags.StringVar(&m.x11WindowID, "x11-window-id", "", "X11 window id to capture, decimal or 0xhex")
	flags.StringVar(&m.x11WindowName, "x11-window-name", "", "X11 window name to capture; prefer -x11-window-id")
	flags.BoolVar(&m.noCursor, "no-cursor", false, "hide the mouse cursor in the captured video")
	flags.BoolVar(&m.rememberSource, "remember-source", false, "on Wayland, reuse this receiver's last screen/window choice instead of asking every time")
}

// finish validates the parsed flags and applies process-wide settings.
func (m *mirrorOptions) finish() error {
	if m.audioOnly && m.noAudio {
		return errors.New("-audio-only and -no-audio exclude each other")
	}
	if err := sender.ValidateHWAccel(m.hwaccel); err != nil {
		return fmt.Errorf("invalid -hwaccel: %w", err)
	}
	if err := sender.ValidateVideoCodec(m.videoCodec); err != nil {
		return fmt.Errorf("invalid -video-codec: %w", err)
	}
	var err error
	if m.xid, err = parseXID(m.x11WindowID); err != nil {
		return fmt.Errorf("invalid -x11-window-id: %w", err)
	}
	sender.SetTargetLatency(time.Duration(m.targetLatencyMs) * time.Millisecond)
	return nil
}

func cmdMirror(ctx context.Context, args []string) error {
	var opts connectOptions
	var mo mirrorOptions
	flags := flag.NewFlagSet("mirror", flag.ContinueOnError)
	opts.register(flags)
	mo.register(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := mo.finish(); err != nil {
		return err
	}
	if err := opts.finish(mirrorUDPPorts); err != nil {
		return err
	}
	return runMirror(ctx, &opts, &mo, mirrorHooks{})
}

// mirrorHooks lets the GUI follow and steer a mirror session.
type mirrorHooks struct {
	// status, when set, reports progress before started.
	status func(string)
	// started, when set, runs once the screen (or audio) reaches the receiver.
	started func()
	// switchable, when set, receives a function that asks for a new capture
	// source and switches the running session to it. It is only called when
	// the capture can offer a choice (the Wayland portal). switchSource blocks
	// while the picker is open, and fails, leaving the current source in
	// place, if the picker is dismissed.
	switchable func(switchSource func(context.Context) error)
}

// runMirror connects to a receiver and mirrors the screen to it until ctx is
// cancelled or the stream ends.
func runMirror(ctx context.Context, opts *connectOptions, mo *mirrorOptions, hooks mirrorHooks) error {
	conn, err := connect(ctx, opts, true)
	if err != nil {
		return err
	}
	defer conn.Close()

	audioOnly := mo.audioOnly
	if !audioOnly && !conn.info.SupportsScreen() {
		if mo.noAudio {
			return fmt.Errorf("%s does not support screen mirroring, and -no-audio leaves nothing to stream", conn.info.Name)
		}
		log.Printf("%s does not support screen mirroring; streaming audio only", conn.info.Name)
		audioOnly = true
	}
	if audioOnly {
		return runSpeaker(ctx, conn, opts, mo, hooks.started)
	}

	streamCfg := sender.StreamConfig{
		FPS:        mo.fps,
		Bitrate:    mo.bitrate,
		VideoCodec: sender.VideoCodec(mo.videoCodec),
		NoEncrypt:  mo.noEncrypt,
		DirectKey:  mo.directKey,
		NoAudio:    mo.noAudio,
		PortMin:    opts.portMin,
		PortMax:    opts.portMax,
	}
	// The session can end because the user dismissed the source picker.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	// Like macOS, pair and connect first. On Wayland the receiver shows a
	// "choosing what to share" placeholder while the portal picker is open,
	// which also keeps the picker outside the receiver's first-frame deadline.
	// Encoder startup waits until control SETUP returns session-time display
	// information (some receivers omit displays before that).
	captureCfg := sender.CaptureConfig{
		FPS:           mo.fps,
		Bitrate:       mo.bitrate,
		HWAccel:       mo.hwaccel,
		VideoCodec:    sender.VideoCodec(mo.videoCodec),
		X11WindowID:   mo.xid,
		X11WindowName: mo.x11WindowName,
		ShowCursor:    !mo.noCursor,
		DeferSource:   !mo.testMode,
	}
	var preparation *sender.CapturePreparation
	if mo.testMode {
		log.Println("using synthetic test source")
		preparation, err = sender.PrepareTestCapture(ctx, captureCfg)
	} else {
		if mo.rememberSource {
			deviceID := conn.info.DeviceID
			if creds := conn.store.Lookup(deviceID); creds != nil {
				captureCfg.RestoreToken = creds.RestoreToken
			}
			captureCfg.SaveRestoreToken = func(token string) error {
				return conn.store.SaveRestoreToken(deviceID, token)
			}
		}
		preparation, err = sender.PrepareCapture(ctx, captureCfg)
	}
	if err != nil {
		return fmt.Errorf("prepare screen capture: %w", err)
	}
	streamCfg.AutomaticHEVCAvailable = preparation.AutomaticHEVCAvailable()
	streamCfg.MeasuredVideoLatency = preparation.MeasuredVideoLatency()

	var capture *sender.ScreenCapture
	var broadcast *sender.BroadcastCapture
	var broadcastDone chan error
	startedWidth, startedHeight := -1, -1
	startedCodec := sender.VideoCodec("")
	var liveVideoLead time.Duration
	prepareVideo := func(width, height int, codec sender.VideoCodec) (sender.VideoPreparationResult, error) {
		if capture != nil {
			if codec == startedCodec {
				if width != startedWidth || height != startedHeight {
					log.Printf("receiver updated the %s canvas to %dx%d after capture started; keeping %dx%d", codec, width, height, startedWidth, startedHeight)
				}
				return sender.VideoPreparationResult{MinimumVideoLead: liveVideoLead}, nil
			}
			return sender.VideoPreparationResult{}, fmt.Errorf("receiver changed video from %s %dx%d to %s %dx%d during setup", startedCodec, startedWidth, startedHeight, codec, width, height)
		}
		started, err := preparation.StartWithCodec(width, height, codec)
		if err != nil {
			return sender.VideoPreparationResult{}, err
		}
		if codec == sender.VideoCodecHEVC && !sender.HasExplicitTargetLatency() {
			liveVideoLead, err = sender.MeasureVideoCaptureLatency(ctx, started, mo.fps)
			if err != nil {
				started.Stop()
				return sender.VideoPreparationResult{}, fmt.Errorf("measure production HEVC timing: %w", err)
			}
			log.Printf("production HEVC timing requires at least %v video lead", liveVideoLead)
		}
		active := sender.NewBroadcastCaptureWithFrameRate(started, mo.fps)
		done := make(chan error, 1)
		capture, broadcast, broadcastDone = started, active, done
		startedWidth, startedHeight, startedCodec = width, height, codec
		go func() { done <- active.Run() }()
		log.Printf("screen capture started at %dx%d using %s", width, height, codec)
		return sender.VideoPreparationResult{MinimumVideoLead: liveVideoLead}, nil
	}
	defer func() {
		preparation.Close()
		if capture != nil {
			broadcast.StopSource()
			<-broadcastDone
		}
	}()

	session, err := conn.client.SetupMirrorWithCalibratedVideoPreparation(ctx, streamCfg, prepareVideo)
	if errors.Is(err, sender.ErrCredentialsRequired) {
		// Some receivers only reveal a configured password by challenging the
		// first media SETUP. Keep pairing/FairPlay state and retry once.
		credential, askErr := opts.askCredential(ctx, conn.info.Name, credentialPINOrPassword, "receiver code/password")
		if askErr != nil {
			return askErr
		}
		conn.client.SetPassword(credential)
		if startedCodec != "" {
			// The running encoder is single-use; pin its codec for the retry.
			streamCfg.VideoCodec = startedCodec
		}
		session, err = conn.client.SetupMirrorWithCalibratedVideoPreparation(ctx, streamCfg, prepareVideo)
		if err == nil {
			conn.savePassword(credential)
		}
	}
	if err != nil {
		return fmt.Errorf("mirror setup: %w", err)
	}
	if capture == nil || broadcast == nil {
		return errors.New("mirror setup completed without preparing video capture")
	}
	defer session.Close()
	log.Printf("mirror session ready (data port: %d)", session.DataPort)

	go func() {
		<-ctx.Done()
		broadcast.StopSource()
		session.Close()
	}()

	if !mo.noAudio && session.HasAudio() {
		source, closeSource, err := openAudioSource(mo.audioSource, mo.testMode, !mo.keepDefaultSink, conn.info)
		var audioCapture *sender.AudioCapture
		if err == nil {
			defer closeSource()
			audioCapture, err = sender.StartAudioCapture(ctx, source, session.AudioCodec())
		}
		if err != nil {
			log.Printf("warning: audio capture failed: %v (continuing without audio)", err)
		} else {
			defer audioCapture.Stop()
			go func() {
				if err := session.StreamAudio(ctx, audioCapture, session.AudioStream()); err != nil && ctx.Err() == nil {
					log.Printf("audio streaming error: %v", err)
				}
			}()
			log.Println("audio capture started")
		}
	} else if !mo.noAudio {
		log.Println("audio disabled (receiver did not provide audio ports)")
	}

	videoSink, err := broadcast.AddBackpressuredSink()
	if err != nil {
		return fmt.Errorf("attach video capture: %w", err)
	}
	defer videoSink.Close()

	var switching sync.Mutex
	switchSource := func(pickCtx context.Context, restore bool) error {
		switching.Lock()
		defer switching.Unlock()
		return switchCaptureSource(ctx, pickCtx, preparation, broadcast, startedWidth, startedHeight, startedCodec, restore)
	}
	ready := func() {
		if hooks.started != nil {
			hooks.started()
		}
		if hooks.switchable != nil && preparation.CanSwitchSource() {
			hooks.switchable(func(pickCtx context.Context) error { return switchSource(pickCtx, false) })
		}
	}
	if preparation.CanSwitchSource() {
		// The placeholder is streaming; now ask what to share.
		go func() {
			if hooks.status != nil {
				hooks.status("Choosing what to share…")
			}
			if err := switchSource(ctx, true); err != nil {
				cancel(err)
				return
			}
			ready()
		}()
	} else {
		ready()
	}
	err = session.StreamFrames(ctx, videoSink.AsCapture(), 0)
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("streaming: %w", err)
	}
	log.Println("stream ended")
	return nil
}

// switchCaptureSource shows the portal picker (cancellable with pickCtx) and
// moves broadcast to a new encoder for the chosen source, with the running
// canvas and codec so the receiver sees only a new keyframe. The current
// source fades to black first and the new one fades in. The encoder lives
// until the session ctx ends. restore lets -remember-source skip the picker.
func switchCaptureSource(ctx, pickCtx context.Context, preparation *sender.CapturePreparation, broadcast *sender.BroadcastCapture, width, height int, codec sender.VideoCodec, restore bool) error {
	next, err := preparation.PrepareSource(pickCtx, restore)
	if err != nil {
		return fmt.Errorf("choose what to share: %w", err)
	}
	defer next.Close()
	select {
	case <-broadcast.Source().FadeOut():
	case <-ctx.Done():
		return ctx.Err()
	}
	started, err := next.StartWithContextAndCodec(ctx, width, height, codec)
	if err != nil {
		return fmt.Errorf("start capture: %w", err)
	}
	if err := broadcast.SwitchSource(started); err != nil {
		return err
	}
	log.Printf("switched screen capture source")
	return nil
}

// runSpeaker streams local audio to a connected receiver without video, as
// if it were a speaker, until ctx is cancelled or the stream ends.
func runSpeaker(ctx context.Context, conn *connection, opts *connectOptions, mo *mirrorOptions, onStarted func()) error {
	streamCfg := sender.StreamConfig{PortMin: opts.portMin, PortMax: opts.portMax}
	session, err := conn.client.SetupAudioOnly(ctx, streamCfg)
	if errors.Is(err, sender.ErrCredentialsRequired) {
		credential, askErr := opts.askCredential(ctx, conn.info.Name, credentialPINOrPassword, "receiver code/password")
		if askErr != nil {
			return askErr
		}
		conn.client.SetPassword(credential)
		session, err = conn.client.SetupAudioOnly(ctx, streamCfg)
		if err == nil {
			conn.savePassword(credential)
		}
	}
	if err != nil {
		return fmt.Errorf("audio setup: %w", err)
	}
	defer session.Close()

	source, closeSource, err := openAudioSource(mo.audioSource, mo.testMode, !mo.keepDefaultSink, conn.info)
	if err != nil {
		return err
	}
	defer closeSource()
	capture, err := sender.StartAudioCapture(ctx, source, session.AudioCodec())
	if err != nil {
		return fmt.Errorf("audio capture: %w", err)
	}
	defer capture.Stop()
	log.Println("audio streaming started")
	if onStarted != nil {
		onStarted()
	}
	if err := session.StreamAudio(ctx, capture, session.AudioStream()); err != nil && ctx.Err() == nil {
		return fmt.Errorf("audio streaming: %w", err)
	}
	log.Println("stream ended")
	return nil
}

func parseXID(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 0, 64) // decimal or 0xhex
}

const (
	audioSourceSink    = "sink"
	audioSourceMonitor = "monitor"
)

// openAudioSource resolves -audio-source. For "sink" it creates the virtual
// output device, which lives until the returned close function runs.
func openAudioSource(mode string, testTone, makeDefault bool, info *sender.ReceiverInfo) (sender.AudioSource, func(), error) {
	switch {
	case testTone:
		return sender.AudioSource{TestTone: true}, func() {}, nil
	case mode == audioSourceMonitor:
		return sender.AudioSource{}, func() {}, nil
	case mode != audioSourceSink:
		return sender.AudioSource{Device: mode}, func() {}, nil
	}
	name, description := sender.VirtualSinkName(info.DeviceID), info.Name
	if description == "" {
		description = info.DeviceID
	}
	sink, err := sender.CreateVirtualSink(name, description+" (foxCast)", makeDefault)
	if err != nil {
		return sender.AudioSource{}, nil, fmt.Errorf("create audio output device (use -audio-source monitor to forward the default output instead): %w", err)
	}
	if makeDefault {
		log.Printf("audio output switched to %q while mirroring", description+" (foxCast)")
	} else {
		log.Printf("audio output device %q created; route applications to it to hear them on the receiver", description+" (foxCast)")
	}
	return sender.AudioSource{Device: sink.MonitorSource()}, func() {
		if err := sink.Close(); err != nil {
			log.Printf("warning: %v", err)
		}
	}, nil
}
