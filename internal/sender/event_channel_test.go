package sender

import (
	"bufio"
	"context"
	"crypto/cipher"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"howett.net/plist"
)

func TestEncryptedEventChannelAcknowledgesSplitCommand(t *testing.T) {
	clientConn, receiverConn := net.Pipe()
	defer clientConn.Close()
	defer receiverConn.Close()

	secret := []byte("0123456789abcdef0123456789abcdef")
	clientChannel, err := newEventChannel(clientConn, true, secret)
	if err != nil {
		t.Fatalf("create client event channel: %v", err)
	}
	receiverChannel := newTestReceiverEventChannel(t, receiverConn, secret)

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveEventChannel(ctx, clientChannel, nil, nil)
	}()

	commandBody := []byte(strings.Repeat("x", 2200))
	request := eventTestRequest(41, commandBody)
	separator := strings.Index(string(request), "\r\n\r\n")
	if separator < 0 {
		t.Fatal("test request has no header separator")
	}
	// End the first encrypted frame one byte before the final LF. This verifies
	// that RTSP parsing is independent of HAP frame boundaries. The large body
	// also makes the second write span multiple 1024-byte frames.
	split := separator + 3
	if _, err := receiverChannel.Write(request[:split]); err != nil {
		t.Fatalf("write first request fragment: %v", err)
	}
	if _, err := receiverChannel.Write(request[split:]); err != nil {
		t.Fatalf("write second request fragment: %v", err)
	}

	readEventTestResponse(t, bufio.NewReader(receiverChannel), 41)

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve event channel after cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("event server did not stop after cancellation")
	}
}

func TestPlaintextEventChannelPreservesPipelinedRequests(t *testing.T) {
	clientConn, receiverConn := net.Pipe()
	defer clientConn.Close()
	defer receiverConn.Close()

	clientChannel, err := newEventChannel(clientConn, false, nil)
	if err != nil {
		t.Fatalf("create plaintext event channel: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveEventChannel(ctx, clientChannel, nil, nil)
	}()

	body1 := []byte("first")
	body2 := []byte("second")
	requests := append(eventTestRequest(7, body1), eventTestRequest(8, body2)...)
	writeErr := make(chan error, 1)
	go func() {
		_, err := receiverConn.Write(requests)
		writeErr <- err
	}()

	reader := bufio.NewReader(receiverConn)
	readEventTestResponse(t, reader, 7)
	readEventTestResponse(t, reader, 8)
	if err := <-writeErr; err != nil {
		t.Fatalf("write pipelined requests: %v", err)
	}
	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve plaintext event channel after cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("plaintext event server did not stop after cancellation")
	}
}

func TestEventChannelUpdatesTimingPeerAndStillAcknowledges(t *testing.T) {
	clientConn, receiverConn := net.Pipe()
	defer clientConn.Close()
	defer receiverConn.Close()

	clientChannel, err := newEventChannel(clientConn, false, nil)
	if err != nil {
		t.Fatalf("create plaintext event channel: %v", err)
	}
	anchorLocal := time.Now().Add(-time.Second)
	clock := &mediaClock{
		anchorLocal:  anchorLocal,
		anchorRemote: 10 * time.Second,
		timelineID:   1,
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveEventChannel(ctx, clientChannel, clock, nil)
	}()

	const timeline = uint64(0x48e15caa8da00008)
	body, err := plist.Marshal(map[string]interface{}{
		"type": "updateTimingPeerInfo",
		"value": map[string]interface{}{
			"ClockID":   timeline,
			"Addresses": []interface{}{"192.0.2.1"},
		},
	}, plist.BinaryFormat)
	if err != nil {
		t.Fatalf("encode timing peer update: %v", err)
	}
	writeErr := make(chan error, 1)
	go func() {
		_, err := receiverConn.Write(eventTestRequest(19, body))
		writeErr <- err
	}()
	readEventTestResponse(t, bufio.NewReader(receiverConn), 19)
	if err := <-writeErr; err != nil {
		t.Fatalf("write timing peer update: %v", err)
	}

	probe := anchorLocal.Add(2 * time.Second)
	got, gotTimeline, _ := clock.at(probe, 0)
	if gotTimeline != timeline {
		t.Fatalf("timeline = 0x%016x, want 0x%016x", gotTimeline, timeline)
	}
	if want := compactTimestamp(12 * time.Second); got != want {
		t.Fatalf("timestamp after timeline switch = 0x%016x, want 0x%016x (continuous)", got, want)
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve event channel after cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("event server did not stop after cancellation")
	}
}

func TestEncryptedEventChannelRejectsInvalidFrames(t *testing.T) {
	for _, test := range []struct {
		name  string
		frame []byte
		want  string
	}{
		{name: "zero length", frame: []byte{0, 0}, want: "invalid event frame size 0"},
		{name: "oversize", frame: []byte{1, 4}, want: "invalid event frame size 1025"},
		{name: "bad tag", frame: append([]byte{1, 0}, make([]byte, 17)...), want: "decrypt event frame 0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clientConn, receiverConn := net.Pipe()
			defer clientConn.Close()
			defer receiverConn.Close()
			channel, err := newEventChannel(clientConn, true, []byte("0123456789abcdef0123456789abcdef"))
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				_, _ = receiverConn.Write(test.frame)
			}()
			var dst [1]byte
			if _, err := channel.Read(dst[:]); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Read error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func newTestReceiverEventChannel(t *testing.T, conn net.Conn, secret []byte) *eventChannel {
	t.Helper()
	// Apple reverses the event transport relative to control: the receiver reads
	// the sender's Events-Read key and sends with Events-Write.
	readKey := hkdfSHA512(secret, []byte("Events-Salt"), []byte("Events-Read-Encryption-Key"), chacha20poly1305.KeySize)
	writeKey := hkdfSHA512(secret, []byte("Events-Salt"), []byte("Events-Write-Encryption-Key"), chacha20poly1305.KeySize)
	newCipher := func(key []byte) cipher.AEAD {
		aead, err := chacha20poly1305.New(key)
		if err != nil {
			t.Fatalf("create test cipher: %v", err)
		}
		return aead
	}
	return &eventChannel{
		conn:        conn,
		readCipher:  newCipher(readKey),
		writeCipher: newCipher(writeKey),
	}
}

func eventTestRequest(cseq uint64, body []byte) []byte {
	header := fmt.Sprintf("POST /command RTSP/1.0\r\nCSeq: %d\r\nContent-Type: application/x-apple-binary-plist\r\nContent-Length: %d\r\n\r\n", cseq, len(body))
	return append([]byte(header), body...)
}

func readEventTestResponse(t *testing.T, reader *bufio.Reader, wantCSeq uint64) {
	t.Helper()
	status, _, err := readEventLine(reader, eventHeaderSizeLimit)
	if err != nil {
		t.Fatalf("read event response status: %v", err)
	}
	if status != "RTSP/1.0 200 OK" {
		t.Fatalf("event response status = %q", status)
	}
	headers := make(map[string]string)
	for {
		line, _, err := readEventLine(reader, eventHeaderSizeLimit)
		if err != nil {
			t.Fatalf("read event response header: %v", err)
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("invalid response header %q", line)
		}
		headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
	if got := headers["cseq"]; got != fmt.Sprint(wantCSeq) {
		t.Fatalf("event response CSeq = %q, want %d", got, wantCSeq)
	}
	if got := headers["content-length"]; got != "0" {
		t.Fatalf("event response content length = %q", got)
	}
}
