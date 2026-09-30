package sender

import (
	"bufio"
	"errors"
	"net"
	"testing"
	"time"

	"howett.net/plist"
)

func TestPlayQueueSetPropertyUsesCommand(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	deadline := time.Now().Add(5 * time.Second)
	_ = clientConn.SetDeadline(deadline)
	_ = serverConn.SetDeadline(deadline)

	session := &PlaybackSession{
		client:      &AirPlayClient{conn: clientConn},
		queue:       true,
		rcsStreamID: 7,
		itemUUID:    "item-uuid",
	}
	type received struct {
		request rtspTestRequest
		err     error
	}
	done := make(chan received, 1)
	go func() {
		request, err := readRTSPTestRequest(bufio.NewReader(serverConn))
		if err == nil {
			err = writeRTSPTestResponse(serverConn, 200, nil, nil)
		}
		done <- received{request, err}
	}()

	if err := session.SetProperty("actionAtItemEnd", int64(0)); err != nil {
		t.Fatalf("SetProperty: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("receiver: %v", got.err)
	}
	if got.request.method != "POST" || got.request.uri != "/command" {
		t.Fatalf("request = %s %s, want POST /command", got.request.method, got.request.uri)
	}
	if id := got.request.headers["x-apple-streamid"]; id != "7" {
		t.Fatalf("X-Apple-StreamID = %q, want 7", id)
	}
	var outer struct {
		Params struct {
			Data []byte `plist:"data"`
		} `plist:"params"`
	}
	if _, err := plist.Unmarshal(got.request.body, &outer); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	var command struct {
		Type     string `plist:"type"`
		Property string `plist:"property"`
		Value    int64  `plist:"value"`
		Item     struct {
			UUID string `plist:"uuid"`
		} `plist:"item"`
	}
	if _, err := plist.Unmarshal(outer.Params.Data, &command); err != nil {
		t.Fatalf("decode command: %v", err)
	}
	if command.Type != "setProperty" || command.Property != "actionAtItemEnd" || command.Value != 0 || command.Item.UUID != "item-uuid" {
		t.Fatalf("command = %+v", command)
	}
}

func TestPlayQueueScrubIsUnsupported(t *testing.T) {
	session := &PlaybackSession{queue: true}
	if err := session.Scrub(30); !errors.Is(err, ErrScrubUnsupported) {
		t.Fatalf("Scrub = %v, want ErrScrubUnsupported", err)
	}
}
