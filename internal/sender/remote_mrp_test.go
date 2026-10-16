package sender

import (
	"context"
	"crypto/rand"
	"math"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

func pbDouble(f float64) pbValue { return pbValue{wire: 1, n: math.Float64bits(f)} }

func pbFloat(f float32) pbValue { return pbValue{wire: 5, n: uint64(math.Float32bits(f))} }

func TestPBRoundTrip(t *testing.T) {
	msg := pbMessage{{1, pbVarint(300)}, {2, pbString("hi")}, {3, pbBytes([]byte{1, 2})}}
	f := pbParse(msg.encode())
	if f.varint(1) != 300 || f.str(2) != "hi" || len(f.bytes(3)) != 2 {
		t.Fatalf("round trip = %+v", f)
	}
}

// setState builds a SET_STATE ProtocolMessage like the ones an Apple TV sends
// (shape from a capture of an Apple TV 4K playing YouTube).
func setState(state uint64, withItem bool) []byte {
	body := pbMessage{
		{5, pbString("YouTube")},
		{6, pbVarint(state)},
		{9, pbBytes(pbMessage{
			{2, pbBytes(pbMessage{{2, pbString("com.google.ios.youtube")}}.encode())},
		}.encode())},
	}
	if withItem {
		item := pbMessage{
			{1, pbString("Title")},
			{7, pbString("Channel")},
			{14, pbDouble(1485)},
			{35, pbDouble(12.5)},
			{39, pbFloat(1)},
			{74, pbDouble(1000)},
		}
		queue := pbMessage{{1, pbVarint(0)}, {2, pbBytes(pbMessage{{2, pbBytes(item.encode())}}.encode())}}
		body = append(body, struct {
			field int
			value pbValue
		}{3, pbBytes(queue.encode())})
	}
	return pbMessage{{mrpFieldType, pbVarint(mrpTypeSetState)}, {mrpFieldSetState, pbBytes(body.encode())}}.encode()
}

func TestParseNowPlayingAndMerge(t *testing.T) {
	if _, ok := ParseNowPlaying(pbMessage{{mrpFieldType, pbVarint(mrpTypeDeviceInfo)}}.encode()); ok {
		t.Fatal("parsed a non-state message")
	}
	full, ok := ParseNowPlaying(setState(1, true))
	if !ok || full.BundleID != "com.google.ios.youtube" || full.App != "YouTube" || full.State != "playing" ||
		full.Title != "Title" || full.Artist != "Channel" || full.Duration != 1485 || full.Elapsed != 12.5 || full.Rate != 1 {
		t.Fatalf("ParseNowPlaying = %+v", full)
	}
	partial, _ := ParseNowPlaying(setState(2, false))
	merged := full.merge(partial)
	if merged.State != "paused" || merged.Title != "Title" || merged.Duration != 1485 {
		t.Fatalf("merge = %+v", merged)
	}
}

func TestPosition(t *testing.T) {
	at := time.Unix(2_000_000_000, 0)
	np := NowPlaying{State: "playing", Rate: 1, Elapsed: 10, Duration: 100, ElapsedAt: float64(at.Unix() - appleEpochOffset)}
	if got := np.Position(at.Add(30 * time.Second)); math.Abs(got-40) > 0.01 {
		t.Errorf("playing position = %v, want 40", got)
	}
	np.State = "paused"
	if got := np.Position(at.Add(30 * time.Second)); got != 10 {
		t.Errorf("paused position = %v, want 10", got)
	}
	np.State, np.Elapsed = "playing", 90
	if got := np.Position(at.Add(30 * time.Second)); got != 100 {
		t.Errorf("position past the end = %v, want 100", got)
	}
}

// fakeApple is the receiver side of a data stream over a pipe: it decrypts
// frames from the sender, replies to requests and pushes state.

func newFakeStream(t *testing.T) (sender, peer *RemoteControl) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	k1, k2 := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(k1)
	_, _ = rand.Read(k2)
	mk := func(conn net.Conn, w, r []byte) *RemoteControl {
		rc := &RemoteControl{
			conn: conn, pending: map[string]chan []byte{}, messages: make(chan []byte, 64),
			done: make(chan struct{}), players: map[string]NowPlaying{}, changed: make(chan struct{}),
			seq: 0x100000000,
		}
		rc.writeAEAD, _ = chacha20poly1305.New(w)
		rc.readAEAD, _ = chacha20poly1305.New(r)
		return rc
	}
	return mk(a, k1, k2), mk(b, k2, k1)
}

func TestRemoteControlFraming(t *testing.T) {
	s, p := newFakeStream(t)
	go s.readLoop()
	go p.readLoop()
	// The peer answers every request with an echo carrying its identifier, and
	// pushes a state after the client-updates message.
	go func() {
		for m := range p.messages {
			f := pbParse(m)
			switch f.varint(mrpFieldType) {
			case mrpTypeDeviceInfo, mrpTypeSendCommand:
				reply := pbMessage{{mrpFieldType, pbVarint(f.varint(mrpFieldType))}, {mrpFieldIdentifier, pbString(f.str(mrpFieldIdentifier))}}
				_ = p.writeProtobuf(reply.encode())
			case mrpTypeClientUpdates:
				_ = p.writeProtobuf(setState(1, true))
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.handshake(ctx, "ABC"); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	np, err := s.WaitState(ctx, func(n NowPlaying) bool { return n.State == "playing" })
	if err != nil || np.Title != "Title" {
		t.Fatalf("WaitState = %+v, %v", np, err)
	}
	if err := s.Command(ctx, MRPCommandPause); err != nil {
		t.Fatalf("Command: %v", err)
	}
}
