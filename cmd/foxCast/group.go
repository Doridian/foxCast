package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

// groupSinkName is the virtual output device of a group session.
const groupSinkName = "foxcast_group"

// groupMember is one receiver of a group and the channels it plays.
type groupMember struct {
	receiver string
	// channels is one position (played on both sides) or a left,right
	// pair; empty means the layout's front pair.
	channels []sender.ChannelPosition
	delay    time.Duration
}

// parseGroupMember parses RECEIVER[=CH[,CH]][@DELAY].
func parseGroupMember(arg string) (groupMember, error) {
	var m groupMember
	if at := strings.LastIndex(arg, "@"); at >= 0 {
		delay, err := time.ParseDuration(arg[at+1:])
		if err != nil {
			return m, fmt.Errorf("%q: invalid delay: %w", arg, err)
		}
		if delay < 0 {
			return m, fmt.Errorf("%q: delay cannot be negative (delay the other receivers instead)", arg)
		}
		m.delay, arg = delay, arg[:at]
	}
	receiver, channels, hasChannels := strings.Cut(arg, "=")
	m.receiver = strings.TrimSpace(receiver)
	if m.receiver == "" {
		return m, fmt.Errorf("%q: missing receiver", arg)
	}
	if hasChannels {
		for _, field := range strings.Split(channels, ",") {
			position, err := sender.ParseChannelPosition(field)
			if err != nil {
				return m, fmt.Errorf("%q: %w", arg, err)
			}
			m.channels = append(m.channels, position)
		}
		if len(m.channels) > 2 {
			return m, fmt.Errorf("%q: a receiver plays one channel or a left,right pair", arg)
		}
	}
	return m, nil
}

// output maps m onto layout.
func (m groupMember) output(layout sender.ChannelLayout, codec sender.AudioCodec) (sender.GroupOutput, error) {
	channels := m.channels
	if len(channels) == 0 {
		channels = []sender.ChannelPosition{"FL", "FR"}
	}
	var indexes []int
	for _, position := range channels {
		i, ok := layout.Index(position)
		if !ok {
			return sender.GroupOutput{}, fmt.Errorf("%s: layout %s has no %s channel", m.receiver, layout, position)
		}
		indexes = append(indexes, i)
	}
	right := indexes[len(indexes)-1]
	return sender.GroupOutput{Left: indexes[0], Right: right, Delay: m.delay, Codec: codec}, nil
}

func (m groupMember) describe() string {
	channels := "FL,FR"
	if len(m.channels) > 0 {
		names := make([]string, len(m.channels))
		for i, position := range m.channels {
			names[i] = string(position)
		}
		channels = strings.Join(names, ",")
	}
	if m.delay > 0 {
		return fmt.Sprintf("%s (+%v)", channels, m.delay)
	}
	return channels
}

func cmdGroup(ctx context.Context, args []string) error {
	var opts connectOptions
	var layoutName, audioSource string
	var targetLatencyMs int
	var testMode, keepDefaultSink bool
	flags := flag.NewFlagSet("group", flag.ContinueOnError)
	opts.registerSession(flags)
	flags.StringVar(&layoutName, "layout", "stereo", "channels of the group's output device: stereo, quad, 5.1, 7.1, or a list such as FL,FR,RC")
	flags.StringVar(&audioSource, "audio-source", audioSourceSink, "audio to forward: \"sink\" (a virtual output device with the group's layout) or a PulseAudio source name")
	flags.BoolVar(&keepDefaultSink, "keep-default-sink", false, "with -audio-source sink, do not make the virtual output device the default")
	flags.IntVar(&targetLatencyMs, "target-latency-ms", 0, "playout lead in ms, the same for every receiver (0 = 500)")
	flags.BoolVar(&testMode, "test", false, "play a beep that walks through the layout's channels instead of recording")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), `Usage: foxCast group [flags] RECEIVER[=CH[,CH]][@DELAY] ...

Plays this computer's sound on several receivers in step, each playing some
channels of one multichannel output device. RECEIVER is a discovered name,
a device ID, or an address. CH is a layout channel: one plays on both sides
of that receiver, two are its left and right. Without =CH a receiver plays
FL,FR. @DELAY (e.g. @15ms) makes one receiver play later than the rest.

Examples:
  foxCast group Kitchen Den                         # the same stereo in two rooms
  foxCast group Left=FL Right=FR                    # two speakers as a stereo pair
  foxCast group -layout quad A=FL B=FR C=RL D=RR    # quadrophonic
  foxCast group -layout quad "Front Pair=FL,FR" "Back Pair=RL,RR"

Flags:
`)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		flags.Usage()
		return errors.New("no receivers given")
	}
	layout, err := sender.ParseChannelLayout(layoutName)
	if err != nil {
		return fmt.Errorf("invalid -layout: %w", err)
	}
	var members []groupMember
	for _, arg := range flags.Args() {
		m, err := parseGroupMember(arg)
		if err != nil {
			return err
		}
		if _, err := m.output(layout, sender.AudioCodecALAC); err != nil {
			return err
		}
		members = append(members, m)
	}
	if err := opts.finish(mirrorUDPPorts * len(members)); err != nil {
		return err
	}
	sender.SetTargetLatency(time.Duration(targetLatencyMs) * time.Millisecond)
	if opts.store, err = newCredentialStore(opts.credBackend, opts.credFile); err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	return runGroup(ctx, &opts, layout, members, audioSource, testMode, !keepDefaultSink)
}

// groupReceiver is a connected member with its audio session.
type groupReceiver struct {
	member  groupMember
	conn    *connection
	session *sender.MirrorSession
}

func runGroup(ctx context.Context, opts *connectOptions, layout sender.ChannelLayout, members []groupMember, audioSource string, testMode, makeDefault bool) error {
	devices, err := resolveGroupMembers(ctx, members)
	if err != nil {
		return err
	}
	var receivers []*groupReceiver
	defer func() {
		for _, r := range receivers {
			if r.session != nil {
				r.session.Close()
			}
			r.conn.Close()
		}
	}()
	// Connect one at a time so pairing prompts do not interleave.
	for i, m := range members {
		o := *opts
		if devices[i] != nil {
			o.device = devices[i]
		} else {
			o.target, o.port = splitGroupAddress(m.receiver)
		}
		log.Printf("%s: connecting", m.receiver)
		conn, err := connect(ctx, &o, true)
		if err != nil {
			return fmt.Errorf("%s: %w", m.receiver, err)
		}
		r := &groupReceiver{member: m, conn: conn}
		receivers = append(receivers, r)
		if r.session, err = setupGroupSession(ctx, &o, conn); err != nil {
			return fmt.Errorf("%s: audio setup: %w", m.receiver, err)
		}
	}

	outputs := make([]sender.GroupOutput, len(receivers))
	for i, r := range receivers {
		if outputs[i], err = r.member.output(layout, r.session.AudioCodec()); err != nil {
			return err
		}
	}
	source := sender.AudioSource{TestTone: testMode}
	if !testMode {
		closeSource, err := openGroupAudioSource(&source, audioSource, layout, makeDefault, receivers)
		if err != nil {
			return err
		}
		defer closeSource()
	}
	capture, err := sender.StartGroupAudioCapture(ctx, source, layout, outputs)
	if err != nil {
		return fmt.Errorf("audio capture: %w", err)
	}
	defer capture.Stop()

	var wg sync.WaitGroup
	for i, r := range receivers {
		log.Printf("%s: playing %s", r.conn.info.Name, r.member.describe())
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A receiver that drops out leaves the others playing.
			if err := r.session.StreamAudio(ctx, capture.Output(i), r.session.AudioStream()); err != nil && ctx.Err() == nil {
				log.Printf("%s: audio streaming stopped: %v", r.conn.info.Name, err)
			}
		}()
	}
	wg.Wait()
	log.Println("group stream ended")
	return nil
}

// setupGroupSession is runSpeaker's session setup, with its one retry for a
// receiver that asks for a password only at SETUP.
func setupGroupSession(ctx context.Context, opts *connectOptions, conn *connection) (*sender.MirrorSession, error) {
	cfg := sender.StreamConfig{PortMin: opts.portMin, PortMax: opts.portMax}
	session, err := conn.client.SetupAudioOnly(ctx, cfg)
	if !errors.Is(err, sender.ErrCredentialsRequired) {
		return session, err
	}
	credential, err := opts.askCredential(ctx, conn.info.Name, credentialPINOrPassword, "receiver code/password")
	if err != nil {
		return nil, err
	}
	conn.client.SetPassword(credential)
	if session, err = conn.client.SetupAudioOnly(ctx, cfg); err == nil {
		conn.savePassword(credential)
	}
	return session, err
}

// resolveGroupMembers finds each member's advertisement by name or device
// ID. Members given as addresses or unmatched hostnames get nil and are
// connected to directly.
func resolveGroupMembers(ctx context.Context, members []groupMember) ([]*sender.AirPlayDevice, error) {
	resolved := make([]*sender.AirPlayDevice, len(members))
	var devices []sender.AirPlayDevice
	discovered := false
	for i, m := range members {
		if isGroupAddress(m.receiver) {
			continue
		}
		if !discovered {
			fmt.Println("searching for AirPlay receivers...")
			var err error
			if devices, err = discover(ctx); err != nil {
				return nil, fmt.Errorf("discovery: %w", err)
			}
			discovered = true
		}
		for j := range devices {
			d := &devices[j]
			if strings.EqualFold(d.Name, m.receiver) || strings.EqualFold(d.DeviceID, m.receiver) {
				resolved[i] = d
				break
			}
		}
		if resolved[i] == nil && strings.Contains(m.receiver, ".") {
			continue // a hostname such as homepod.local
		}
		if resolved[i] == nil {
			var names []string
			for _, d := range devices {
				names = append(names, strconv.Quote(d.Name))
			}
			return nil, fmt.Errorf("receiver %q not found (found: %s)", m.receiver, strings.Join(names, ", "))
		}
	}
	return resolved, nil
}

// isGroupAddress reports whether a member names an IP address or host:port
// rather than a receiver.
func isGroupAddress(s string) bool {
	host, _ := splitGroupAddress(s)
	return net.ParseIP(host) != nil || host != s
}

// splitGroupAddress splits an optional :port off a member address.
func splitGroupAddress(s string) (string, int) {
	if host, port, err := net.SplitHostPort(s); err == nil {
		if n, err := strconv.Atoi(port); err == nil {
			return host, n
		}
	}
	return s, 7000
}

// openGroupAudioSource points source at the group's recording: a virtual
// output device with the layout's channels, or a named PulseAudio source.
func openGroupAudioSource(source *sender.AudioSource, mode string, layout sender.ChannelLayout, makeDefault bool, receivers []*groupReceiver) (func(), error) {
	if mode != audioSourceSink {
		source.Device = mode
		return func() {}, nil
	}
	names := make([]string, len(receivers))
	for i, r := range receivers {
		names[i] = r.conn.info.Name
	}
	description := strings.Join(names, " + ") + " (foxCast)"
	sink, err := sender.CreateVirtualSinkLayout(groupSinkName, description, layout, makeDefault)
	if err != nil {
		return nil, fmt.Errorf("create audio output device: %w", err)
	}
	if makeDefault {
		log.Printf("audio output switched to %q (%s)", description, layout)
	} else {
		log.Printf("audio output device %q (%s) created; route applications to it", description, layout)
	}
	source.Device = sink.MonitorSource()
	return func() {
		if err := sink.Close(); err != nil {
			log.Printf("warning: %v", err)
		}
	}, nil
}
