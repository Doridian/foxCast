package rtmp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"testing"
	"time"
)

// testClient is a scripted publishing client.
type testClient struct {
	t         *testing.T
	conn      net.Conn
	r         *bufio.Reader
	chunkSize int
	peer      *Publisher // the server side, only for reading its chunk size
	inChunk   uint32
	inStreams map[uint32]*chunkStream
}

func newTestPair(t *testing.T) (*testClient, <-chan *Publisher, <-chan error) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	pubs := make(chan *Publisher, 1)
	errs := make(chan error, 1)
	go func() {
		p, err := Accept(server, 5*time.Second)
		if err != nil {
			errs <- err
			return
		}
		pubs <- p
	}()
	c := &testClient{t: t, conn: client, r: bufio.NewReader(client), chunkSize: defaultChunkSize, inChunk: defaultChunkSize, inStreams: map[uint32]*chunkStream{}}
	return c, pubs, errs
}

func (c *testClient) handshake() {
	c.t.Helper()
	c1 := make([]byte, 1+handshakeSize)
	c1[0] = handshakeVersion
	for i := range c1[1:] {
		c1[1+i] = byte(i)
	}
	go func() { _, _ = c.conn.Write(c1) }()
	s := make([]byte, 1+2*handshakeSize)
	if _, err := io.ReadFull(c.r, s); err != nil {
		c.t.Fatal(err)
	}
	if s[0] != handshakeVersion {
		c.t.Fatalf("S0 = %d", s[0])
	}
	if binary.BigEndian.Uint32(s[5:9]) != 0 {
		c.t.Fatal("S1 must carry a zero version so librtmp uses the simple handshake")
	}
	if !bytes.Equal(s[1+handshakeSize:], c1[1:]) {
		c.t.Fatal("S2 does not echo C1")
	}
	if _, err := c.conn.Write(s[1 : 1+handshakeSize]); err != nil {
		c.t.Fatal(err)
	}
}

// write sends a message, starting with a type 0 header and continuing with
// type 3 chunks. A timestamp of 0xffffff or more uses the extended field.
func (c *testClient) write(csid uint32, format byte, typeID uint8, streamID, stamp uint32, payload []byte) {
	c.t.Helper()
	field := min(stamp, extendedStamp)
	var b []byte
	b = append(b, format<<6|byte(csid))
	switch format {
	case 0:
		b = append(b, byte(field>>16), byte(field>>8), byte(field), byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload)), typeID)
		b = binary.LittleEndian.AppendUint32(b, streamID)
	case 1:
		b = append(b, byte(field>>16), byte(field>>8), byte(field), byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload)), typeID)
	case 2:
		b = append(b, byte(field>>16), byte(field>>8), byte(field))
	}
	if field == extendedStamp {
		b = binary.BigEndian.AppendUint32(b, stamp)
	}
	for i := 0; ; {
		n := min(c.chunkSize, len(payload)-i)
		b = append(b, payload[i:i+n]...)
		i += n
		if i >= len(payload) {
			break
		}
		b = append(b, 0xc0|byte(csid))
		if field == extendedStamp {
			b = binary.BigEndian.AppendUint32(b, stamp)
		}
	}
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *testClient) command(csid, streamID uint32, values ...any) {
	c.t.Helper()
	c.write(csid, 0, msgCommandAMF0, streamID, 0, encodeAMF0(nil, values...))
}

// expect reads server messages until a command named name arrives.
func (c *testClient) expect(name string) []any {
	c.t.Helper()
	// Reuse the server's chunk parser for the other direction.
	p := &Publisher{conn: c.conn, r: c.r, chunkSize: c.inChunk, streams: c.inStreams, timeout: 5 * time.Second}
	defer func() { c.inChunk = p.chunkSize }()
	for {
		msg, err := p.readMessage()
		if err != nil {
			c.t.Fatalf("waiting for %s: %v", name, err)
		}
		if msg.typeID != msgCommandAMF0 {
			continue
		}
		values, err := decodeAMF0(msg.payload)
		if err != nil {
			c.t.Fatal(err)
		}
		if values[0] == name {
			return values
		}
	}
}

func (c *testClient) publish(key string) {
	c.t.Helper()
	c.handshake()
	c.command(csidCommand, 0, "connect", 1.0, object{{"app", "live"}, {"tcUrl", "rtmp://127.0.0.1/live"}})
	result := c.expect("_result")
	if info := result[3].(map[string]any); info["code"] != "NetConnection.Connect.Success" {
		c.t.Fatalf("connect: %v", result)
	}
	// Like OBS: a larger chunk size, then the publish sequence.
	c.write(csidControl, 0, msgSetChunkSize, 0, 0, binary.BigEndian.AppendUint32(nil, 4096))
	c.chunkSize = 4096
	c.command(csidCommand, 0, "releaseStream", 2.0, nil, key)
	c.command(csidCommand, 0, "FCPublish", 3.0, nil, key)
	c.command(csidCommand, 0, "createStream", 4.0, nil)
	if result := c.expect("_result"); result[3] != float64(publishStreamID) {
		c.t.Fatalf("createStream: %v", result)
	}
	c.command(csidCommand+1, publishStreamID, "publish", 5.0, nil, key, "live")
	if status := c.expect("onStatus"); status[3].(map[string]any)["code"] != "NetStream.Publish.Start" {
		c.t.Fatalf("publish: %v", status)
	}
}

type flvTag struct {
	typeID    uint8
	timestamp uint32
	payload   []byte
}

// readFLV parses an FLV stream until EOF.
func readFLV(r io.Reader) ([]flvTag, error) {
	head := make([]byte, len(flvHeader))
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	if !bytes.Equal(head, flvHeader) {
		return nil, fmt.Errorf("header % x", head)
	}
	var tags []flvTag
	for {
		h := make([]byte, flvTagHeaderSize)
		if _, err := io.ReadFull(r, h); err != nil {
			if errors.Is(err, io.EOF) {
				return tags, nil
			}
			return tags, err
		}
		size := uint32(h[1])<<16 | uint32(h[2])<<8 | uint32(h[3])
		stamp := uint32(h[7])<<24 | uint32(h[4])<<16 | uint32(h[5])<<8 | uint32(h[6])
		body := make([]byte, size+4)
		if _, err := io.ReadFull(r, body); err != nil {
			return tags, err
		}
		if prev := binary.BigEndian.Uint32(body[size:]); prev != size+flvTagHeaderSize {
			return tags, fmt.Errorf("previous tag size %d, want %d", prev, size+flvTagHeaderSize)
		}
		tags = append(tags, flvTag{h[0], stamp, body[:size]})
	}
}

func TestPublishToFLV(t *testing.T) {
	c, pubs, errs := newTestPair(t)
	done := make(chan []flvTag, 1)
	readErr := make(chan error, 1)
	go func() {
		var p *Publisher
		select {
		case p = <-pubs:
		case err := <-errs:
			readErr <- err
			return
		}
		if p.App != "live" || p.Key != "obs-key" {
			readErr <- fmt.Errorf("app %q key %q", p.App, p.Key)
			return
		}
		tags, err := readFLV(p)
		if err != nil {
			readErr <- err
			return
		}
		done <- tags
	}()

	c.publish("obs-key")
	go func() { _, _ = io.Copy(io.Discard, c.r) }() // the ping reply
	c.write(4, 0, msgDataAMF0, publishStreamID, 0, encodeAMF0(nil, "@setDataFrame", "onMetaData", object{{"width", 1280}}))
	big := bytes.Repeat([]byte{0xab}, 10000) // spans three 4096-byte chunks
	c.write(6, 0, msgVideo, publishStreamID, 0, []byte{0x17, 0, 0, 0, 0, 1, 2, 3})
	c.write(4, 0, msgAudio, publishStreamID, 0, []byte{0xaf, 0, 0x12, 0x10})
	c.write(6, 1, msgVideo, publishStreamID, 33, big)
	c.write(6, 3, msgVideo, publishStreamID, 33, big) // a new message reusing the delta
	c.write(6, 1, msgVideo, publishStreamID, 33, []byte{0x27, 1})
	c.write(6, 2, msgVideo, publishStreamID, 10, []byte{0x27, 2}) // same length, new delta
	c.write(4, 1, msgAudio, publishStreamID, 23, []byte{0xaf, 1, 9})
	// An extended timestamp, repeated on its continuation chunks.
	c.write(6, 0, msgVideo, publishStreamID, 0x01000000, big)
	c.write(6, 0, msgUserControl, 0, 0, []byte{0, eventPingRequest, 0, 0, 0, 7})
	c.command(csidCommand, 0, "FCUnpublish", 6.0, nil, "obs-key")

	var tags []flvTag
	select {
	case tags = <-done:
	case err := <-readErr:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	want := []flvTag{
		{msgVideo, 0, []byte{0x17, 0, 0, 0, 0, 1, 2, 3}},
		{msgAudio, 0, []byte{0xaf, 0, 0x12, 0x10}},
		{msgVideo, 33, big},
		{msgVideo, 66, big},
		{msgVideo, 99, []byte{0x27, 1}},
		{msgVideo, 109, []byte{0x27, 2}},
		{msgAudio, 23, []byte{0xaf, 1, 9}},
		{msgVideo, 0x01000000, big},
	}
	if len(tags) != len(want) {
		t.Fatalf("got %d tags, want %d", len(tags), len(want))
	}
	for i := range want {
		if tags[i].typeID != want[i].typeID || tags[i].timestamp != want[i].timestamp || !bytes.Equal(tags[i].payload, want[i].payload) {
			t.Errorf("tag %d: type %d ts %d len %d, want type %d ts %d len %d", i,
				tags[i].typeID, tags[i].timestamp, len(tags[i].payload), want[i].typeID, want[i].timestamp, len(want[i].payload))
		}
	}
}

func TestPlayIsRejected(t *testing.T) {
	c, _, errs := newTestPair(t)
	c.handshake()
	c.command(csidCommand, 0, "connect", 1.0, object{{"app", "live"}})
	c.expect("_result")
	c.command(csidCommand, 0, "createStream", 2.0, nil)
	c.expect("_result")
	go c.command(csidCommand+1, publishStreamID, "play", 3.0, nil, "key")
	select {
	case err := <-errs:
		if !errors.Is(err, ErrNotPublishing) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestDisconnectEndsStream(t *testing.T) {
	c, pubs, errs := newTestPair(t)
	result := make(chan error, 1)
	go func() {
		select {
		case p := <-pubs:
			_, err := readFLV(p)
			result <- err
		case err := <-errs:
			result <- err
		}
	}()
	c.publish("k")
	c.write(6, 0, msgVideo, publishStreamID, 0, []byte{0x17, 0, 0, 0, 0})
	_ = c.conn.Close()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("stream ended with %v, want a clean EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

// TestFFmpegPublish publishes a short H.264/AAC stream with ffmpeg, whose
// RTMP client (like OBS's librtmp) exercises a real implementation.
func TestFFmpegPublish(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=30:duration=1",
		"-f", "lavfi", "-i", "sine=duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac",
		"-f", "flv", fmt.Sprintf("rtmp://%s/live/ffmpeg-key", ln.Addr()))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Accept(conn, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.App != "live" || p.Key != "ffmpeg-key" {
		t.Errorf("app %q key %q", p.App, p.Key)
	}
	tags, err := readFLV(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, stderr.String())
	}
	var video, audio int
	for _, tag := range tags {
		switch tag.typeID {
		case msgVideo:
			video++
		case msgAudio:
			audio++
		}
	}
	// One second at 30 fps, and AAC's ~43 frames a second.
	if video < 25 || audio < 30 {
		t.Fatalf("got %d video and %d audio tags", video, audio)
	}
	if tags[0].payload[0] != 0x17 && tags[1].payload[0] != 0x17 {
		t.Error("stream does not start with the AVC sequence header")
	}
}
