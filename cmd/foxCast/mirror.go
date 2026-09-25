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
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

// mirrorUDPPorts is the number of consecutive UDP ports a mirror session
// needs (timing, audio control, audio data).
const mirrorUDPPorts = 3

func cmdMirror(ctx context.Context, args []string) error {
	var opts connectOptions
	flags := flag.NewFlagSet("mirror", flag.ContinueOnError)
	opts.register(flags)
	fps := flags.Int("fps", 30, "frames per second")
	bitrate := flags.Int("bitrate", 0, "video bitrate in kbps (0 = auto)")
	targetLatencyMs := flags.Int("target-latency-ms", 0, "joint audio/video playout latency override in ms (0 = automatic)")
	hwaccel := flags.String("hwaccel", "auto", "encoder: auto, nvenc, vaapi, openh264, none (x264/x265)")
	videoCodec := flags.String("video-codec", "auto", "screen codec: auto, h264, or hevc")
	testMode := flags.Bool("test", false, "use synthetic video/audio instead of screen capture")
	noEncrypt := flags.Bool("no-encrypt", false, "disable RTSP header encryption (debugging only)")
	directKey := flags.Bool("direct-key", false, "use shk/shiv directly without SHA-512 derivation")
	noAudio := flags.Bool("no-audio", false, "disable audio streaming")
	x11WindowID := flags.String("x11-window-id", "", "X11 window id to capture, decimal or 0xhex")
	x11WindowName := flags.String("x11-window-name", "", "X11 window name to capture; prefer -x11-window-id")
	noCursor := flags.Bool("no-cursor", false, "hide the mouse cursor in the captured video")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := sender.ValidateHWAccel(*hwaccel); err != nil {
		return fmt.Errorf("invalid -hwaccel: %w", err)
	}
	if err := sender.ValidateVideoCodec(*videoCodec); err != nil {
		return fmt.Errorf("invalid -video-codec: %w", err)
	}
	xid, err := parseXID(*x11WindowID)
	if err != nil {
		return fmt.Errorf("invalid -x11-window-id: %w", err)
	}
	if err := opts.finish(mirrorUDPPorts); err != nil {
		return err
	}
	sender.SetTargetLatency(time.Duration(*targetLatencyMs) * time.Millisecond)

	conn, err := connect(ctx, &opts, true)
	if err != nil {
		return err
	}
	defer conn.Close()

	streamCfg := sender.StreamConfig{
		FPS:        *fps,
		Bitrate:    *bitrate,
		VideoCodec: sender.VideoCodec(*videoCodec),
		NoEncrypt:  *noEncrypt,
		DirectKey:  *directKey,
		NoAudio:    *noAudio,
		PortMin:    opts.portMin,
		PortMax:    opts.portMax,
	}
	// Complete the potentially interactive Wayland portal request before SETUP,
	// but delay encoder startup until control SETUP returns session-time
	// display information (some receivers omit displays before that).
	captureCfg := sender.CaptureConfig{
		FPS:           *fps,
		Bitrate:       *bitrate,
		HWAccel:       *hwaccel,
		VideoCodec:    sender.VideoCodec(*videoCodec),
		X11WindowID:   xid,
		X11WindowName: *x11WindowName,
		ShowCursor:    !*noCursor,
	}
	var preparation *sender.CapturePreparation
	if *testMode {
		log.Println("using synthetic test source")
		preparation, err = sender.PrepareTestCapture(ctx, captureCfg)
	} else {
		deviceID := conn.info.DeviceID
		if creds := conn.store.Lookup(deviceID); creds != nil {
			captureCfg.RestoreToken = creds.RestoreToken
		}
		captureCfg.SaveRestoreToken = func(token string) error {
			return conn.store.SaveRestoreToken(deviceID, token)
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
			liveVideoLead, err = sender.MeasureVideoCaptureLatency(ctx, started, *fps)
			if err != nil {
				started.Stop()
				return sender.VideoPreparationResult{}, fmt.Errorf("measure production HEVC timing: %w", err)
			}
			log.Printf("production HEVC timing requires at least %v video lead", liveVideoLead)
		}
		active := sender.NewBroadcastCaptureWithFrameRate(started, *fps)
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
			capture.Stop()
			<-broadcastDone
		}
	}()

	session, err := conn.client.SetupMirrorWithCalibratedVideoPreparation(ctx, streamCfg, prepareVideo)
	if errors.Is(err, sender.ErrCredentialsRequired) {
		// Some receivers only reveal a configured password by challenging the
		// first media SETUP. Keep pairing/FairPlay state and retry once.
		credential := readCredential("Enter the receiver's code or configured password: ")
		if credential == "" {
			return errors.New("receiver code/password cannot be empty")
		}
		conn.client.SetPassword(credential)
		if startedCodec != "" {
			// The running encoder is single-use; pin its codec for the retry.
			streamCfg.VideoCodec = startedCodec
		}
		session, err = conn.client.SetupMirrorWithCalibratedVideoPreparation(ctx, streamCfg, prepareVideo)
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
		capture.Stop()
		session.Close()
	}()

	if !*noAudio && session.HasAudio() {
		audioCapture, err := sender.StartAudioCapture(ctx, *testMode, session.AudioCodec())
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
	} else if !*noAudio {
		log.Println("audio disabled (receiver did not provide audio ports)")
	}

	videoSink, err := broadcast.AddBackpressuredSink()
	if err != nil {
		return fmt.Errorf("attach video capture: %w", err)
	}
	defer videoSink.Close()
	if err := session.StreamFrames(ctx, videoSink.AsCapture(), 0); err != nil && ctx.Err() == nil {
		return fmt.Errorf("streaming: %w", err)
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
