package sender

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestParseChannelLayout(t *testing.T) {
	for _, test := range []struct {
		in, want, pulse string
		mask            uint64
	}{
		{in: "stereo", want: "FL,FR", pulse: "front-left,front-right", mask: 0x3},
		{in: "Quad", want: "FL,FR,RL,RR", pulse: "front-left,front-right,rear-left,rear-right", mask: 0x33},
		{in: "5.1", want: "FL,FR,FC,LFE,RL,RR", mask: 0x3f},
		{in: "7.1", want: "FL,FR,FC,LFE,RL,RR,SL,SR", mask: 0xc3f},
		// Listed order does not matter: channels interleave in GStreamer order.
		{in: "rr, fl ,FR,rl", want: "FL,FR,RL,RR", mask: 0x33},
		{in: "FL,FR,RC", want: "FL,FR,RC", mask: 0x103},
	} {
		layout, err := ParseChannelLayout(test.in)
		if err != nil {
			t.Fatalf("ParseChannelLayout(%q): %v", test.in, err)
		}
		if layout.String() != test.want || layout.gstChannelMask() != test.mask {
			t.Errorf("ParseChannelLayout(%q) = %s mask 0x%x, want %s mask 0x%x", test.in, layout, layout.gstChannelMask(), test.want, test.mask)
		}
		if test.pulse != "" && layout.pulseChannelMap() != test.pulse {
			t.Errorf("pulse channel map of %q = %s, want %s", test.in, layout.pulseChannelMap(), test.pulse)
		}
	}
	for _, bad := range []string{"", "FL", "FL,FL", "FL,XX", "surround"} {
		if _, err := ParseChannelLayout(bad); err == nil {
			t.Errorf("ParseChannelLayout(%q) succeeded", bad)
		}
	}
}

func TestMultichannelCaptureCaps(t *testing.T) {
	quad, _ := ParseChannelLayout("quad")
	if got := audioCaptureCaps(quad, "S16BE"); got != "audio/x-raw,rate=44100,channels=4,format=S16BE,layout=interleaved,channel-mask=(bitmask)0x33" {
		t.Errorf("quad caps = %s", got)
	}
	if got := audioCaptureCaps(StereoLayout, "S16LE"); got != "audio/x-raw,rate=44100,channels=2,format=S16LE,layout=interleaved" {
		t.Errorf("stereo caps = %s", got)
	}
	args := virtualSinkLoadArgsLayout("foxcast_group", "Group", quad)
	if args[4] != "channels=4" || args[5] != "channel_map=front-left,front-right,rear-left,rear-right" {
		t.Errorf("quad sink args = %q", args)
	}
}

func TestSplitGroupChannels(t *testing.T) {
	// Two sample frames of four channels; each sample's value is 10*frame+channel.
	frame := make([]byte, 2*4*2)
	for s := range 2 {
		for c := range 4 {
			binary.LittleEndian.PutUint16(frame[(s*4+c)*2:], uint16(10*s+c))
		}
	}
	for _, test := range []struct {
		left, right int
		want        []uint16
	}{
		{left: 2, right: 3, want: []uint16{2, 3, 12, 13}},
		{left: 1, right: 1, want: []uint16{1, 1, 11, 11}},
		{left: 0, right: -1, want: []uint16{0, 0, 10, 0}},
	} {
		out := splitGroupChannels(frame, 4, test.left, test.right)
		for i, want := range test.want {
			if got := binary.LittleEndian.Uint16(out[i*2:]); got != want {
				t.Errorf("split %d/%d sample %d = %d, want %d", test.left, test.right, i, got, want)
			}
		}
	}
}

func TestGroupOutputReaderReframesAndRestartsAfterGap(t *testing.T) {
	queue := make(chan groupChunk, 8)
	start := time.Unix(1000, 0)
	chunk := func(sample uint32) groupChunk {
		pcm := make([]byte, groupChunkSamples*audioBytesPerSampleFrame)
		for i := range groupChunkSamples {
			binary.LittleEndian.PutUint16(pcm[i*4:], uint16(sample)+uint16(i))
		}
		return groupChunk{pcm: pcm, position: audioPCMFramePosition{
			PTS: start.Add(audioSamplesDuration(uint64(sample))), SourceRTP: sample, HasSourceRTP: true,
		}}
	}
	queue <- chunk(0)
	queue <- chunk(352)
	queue <- chunk(1056) // 704 was dropped
	queue <- chunk(1408)
	close(queue)
	r := &groupOutputReader{queue: queue}
	dst := make([]byte, 480*audioBytesPerSampleFrame) // AAC-ELD frame size

	position, err := r.ReadPCMFramePosition(dst)
	if err != nil || position.SourceRTP != 0 || !position.PTS.Equal(start) {
		t.Fatalf("first frame = %+v, %v", position, err)
	}
	// The second frame would span the gap, so it restarts at the chunk after it.
	position, err = r.ReadPCMFramePosition(dst)
	if err != nil || position.SourceRTP != 1056 || !position.PTS.Equal(start.Add(audioSamplesDuration(1056))) {
		t.Fatalf("frame after gap = %+v, %v", position, err)
	}
	if got := binary.LittleEndian.Uint16(dst); got != 1056%65536 {
		t.Fatalf("frame after gap starts with sample %d, want 1056", got)
	}
	if _, err := r.ReadPCMFramePosition(dst); err == nil {
		t.Fatal("read past the closed queue succeeded")
	}
}

func TestGroupToneWalksChannels(t *testing.T) {
	slot := uint64(durationToAudioSamples(groupToneSlot))
	for channel := range 4 {
		dst := make([]byte, 64*4*audioBytesPerSample)
		fillGroupTone(dst, 4, uint64(channel)*slot+100)
		for c := range 4 {
			var energy bool
			for i := 0; i < len(dst); i += 4 * audioBytesPerSample {
				if dst[i+c*2] != 0 || dst[i+c*2+1] != 0 {
					energy = true
				}
			}
			if energy != (c == channel) {
				t.Errorf("slot %d: channel %d has tone %t", channel, c, energy)
			}
		}
	}
	quiet := make([]byte, 64*4*audioBytesPerSample)
	fillGroupTone(quiet, 4, uint64(durationToAudioSamples(groupToneOn))+10)
	if !bytes.Equal(quiet, make([]byte, len(quiet))) {
		t.Error("tone sounds during the pause between beeps")
	}
}

// TestGroupOutputsAnnounceTheSameClock checks the property group playback
// relies on: streams fed by one group capture map the same audio to the
// same network time, offset only by each output's delay.
func TestGroupOutputsAnnounceTheSameClock(t *testing.T) {
	const delay = 20 * time.Millisecond
	quad, _ := ParseChannelLayout("quad")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	group, err := StartGroupAudioCapture(ctx, AudioSource{TestTone: true}, quad, []GroupOutput{
		{Left: 0, Right: 1, Codec: AudioCodecALAC},
		{Left: 3, Right: 3, Codec: AudioCodecALAC, Delay: delay},
	})
	if err != nil {
		t.Fatalf("start group capture: %v", err)
	}
	defer group.Stop()

	started := make(chan struct{})
	close(started)
	announced := make([]chan uint64, 2)
	done := make(chan error, 2)
	for i := range 2 {
		ctrlConn, ctrlPeer := udpPair(t)
		dataConn, dataPeer := udpPair(t)
		session := &MirrorSession{firstFrameSent: started, timingProtocol: timingProtocolNTP}
		stream := &AudioStream{
			conn:            dataConn,
			ctrlConn:        ctrlConn,
			remoteAddr:      dataPeer.LocalAddr().(*net.UDPAddr),
			ctrlAddr:        ctrlPeer.LocalAddr().(*net.UDPAddr),
			spf:             352,
			ct:              byte(AudioCodecALAC),
			latencySamples:  speakerLatencySamples(),
			securityMode:    audioSecurityLegacyAES,
			chachaNonceMode: defaultAudioChaChaNonceMode(),
			chachaAADMode:   defaultAudioChaChaAADMode(),
		}
		announced[i] = make(chan uint64, 1)
		go func(peer net.PacketConn, out chan<- uint64) {
			buf := make([]byte, 64)
			for {
				n, _, err := peer.ReadFrom(buf)
				if err != nil {
					return
				}
				if n == 20 && buf[0] == 0x90 && buf[1] == audioSyncPayloadTypeNTP {
					out <- binary.BigEndian.Uint64(buf[8:16])
					return
				}
			}
		}(ctrlPeer, announced[i])
		go func() { done <- session.StreamAudio(ctx, group.Output(i), stream) }()
	}

	var times [2]time.Duration
	for i := range 2 {
		select {
		case ntp := <-announced[i]:
			times[i] = time.Duration(ntp>>32)*time.Second + time.Duration((ntp&0xffffffff)*uint64(time.Second)>>32)
		case err := <-done:
			t.Fatalf("StreamAudio ended before announcing: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatalf("output %d sent no initial TimeAnnounce", i)
		}
	}
	// Both outputs start with the first chunk, so their first announces name
	// the same source sample.
	if skew := times[1] - times[0] - delay; skew < -time.Millisecond || skew > time.Millisecond {
		t.Fatalf("output clocks differ by %v, want the %v delay (skew %v)", times[1]-times[0], delay, skew)
	}
	cancel()
	for range 2 {
		// Cancelling also ends the shared capture, which a stream may see first.
		if err := <-done; !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
			t.Fatalf("StreamAudio = %v, want cancellation", err)
		}
	}
}

func udpPair(t *testing.T) (net.PacketConn, net.PacketConn) {
	t.Helper()
	var conns [2]net.PacketConn
	for i := range conns {
		conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen UDP: %v", err)
		}
		conns[i] = conn
	}
	t.Cleanup(func() { conns[1].Close() })
	return conns[0], conns[1]
}

func TestGroupStreamsToSeveralReceivers(t *testing.T) {
	SetTargetLatency(0)
	quad, _ := ParseChannelLayout("quad")
	var servers []*ReceiverServer
	var outputs []GroupOutput
	var sessions []*MirrorSession
	var ctx context.Context
	for i := range 4 {
		server, client, serverCtx := newReceiverServerTestPair(t, ReceiverConfig{Profile: ReceiverProfileModern, AudioOnly: true})
		ctx = serverCtx
		if err := client.Pair(ctx, ""); err != nil {
			t.Fatalf("pair %d: %v", i, err)
		}
		if err := client.FairPlaySetup(ctx); err != nil {
			t.Fatalf("FairPlay setup %d: %v", i, err)
		}
		session, err := client.SetupAudioOnly(ctx, StreamConfig{})
		if err != nil {
			t.Fatalf("setup %d: %v", i, err)
		}
		t.Cleanup(func() { session.Close() })
		servers = append(servers, server)
		sessions = append(sessions, session)
		outputs = append(outputs, GroupOutput{Left: i, Right: i, Codec: session.AudioCodec()})
	}
	streamCtx, cancel := context.WithCancel(ctx)
	group, err := StartGroupAudioCapture(streamCtx, AudioSource{TestTone: true}, quad, outputs)
	if err != nil {
		t.Fatalf("start group capture: %v", err)
	}
	defer group.Stop()
	done := make(chan error, len(sessions))
	for i, session := range sessions {
		go func() { done <- session.StreamAudio(streamCtx, group.Output(i), session.AudioStream()) }()
	}
	deadline := time.Now().Add(3 * time.Second)
	for _, server := range servers {
		for server.Stats().AudioPackets < 10 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	for range sessions {
		// Cancelling also ends the shared capture, which a stream may see first.
		if err := <-done; !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
			t.Fatalf("StreamAudio = %v, want cancellation", err)
		}
	}
	for i, server := range servers {
		if got := server.Stats().AudioPackets; got < 10 {
			t.Errorf("receiver %d got %d audio packets, want at least 10", i, got)
		}
	}
}
