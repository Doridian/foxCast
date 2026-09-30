package sender

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"reflect"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// Vectors computed with Python's cryptography package following pyatv's
// Chacha20Cipher(nonce_length=12) and hkdf_expand("", info, shared).
func TestCompanionFrameVector(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		t.Fatal(err)
	}
	payload := mustHex(t, "e141614161")
	var nonce uint64
	for _, want := range []string{
		"08000015f9f92370ccf21492bd2390212071c4d1568ef7e9e0",
		"08000015757e1aafa50503a78b820148b8fb69eafe0e7e0d1a",
	} {
		got, err := sealCompanionFrame(aead, &nonce, companionFrameEOPACK, payload)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, mustHex(t, want)) {
			t.Fatalf("frame %d = %x, want %s", nonce-1, got, want)
		}
	}

	var readNonce uint64
	for i, frame := range []string{
		"08000015f9f92370ccf21492bd2390212071c4d1568ef7e9e0",
		"08000015757e1aafa50503a78b820148b8fb69eafe0e7e0d1a",
	} {
		frameType, plain, err := readCompanionFrame(bytes.NewReader(mustHex(t, frame)), aead, &readNonce)
		if err != nil {
			t.Fatal(err)
		}
		if frameType != companionFrameEOPACK || !bytes.Equal(plain, payload) {
			t.Fatalf("read frame %d = %d %x", i, frameType, plain)
		}
	}
}

func TestCompanionSessionKeyVector(t *testing.T) {
	shared := make([]byte, 32)
	for i := range shared {
		shared[i] = byte(32 + i)
	}
	for info, want := range map[string]string{
		companionClientEncryptInfo: "a69f5969f6849d4148d6ada51472b32bc29bd4fa7f156db150ae728e501965c7",
		companionServerEncryptInfo: "2558f51ebb48ee693d7b2d607ac629d0b88284eff7728055a2f39ca30174582d",
	} {
		if got := hkdfSHA512(shared, nil, []byte(info), 32); !bytes.Equal(got, mustHex(t, want)) {
			t.Errorf("%s key = %x, want %s", info, got, want)
		}
	}
}

func TestCompanionUnencryptedFrame(t *testing.T) {
	frame, err := sealCompanionFrame(nil, new(uint64), companionFramePSStart, []byte{0xe0})
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{3, 0, 0, 1, 0xe0}; !bytes.Equal(frame, want) {
		t.Fatalf("frame = % x, want % x", frame, want)
	}
}

func startCompanionReceiver(t *testing.T, cfg CompanionReceiverConfig) *CompanionReceiver {
	t.Helper()
	receiver, err := NewCompanionReceiver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- receiver.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return receiver
}

func pairCompanion(t *testing.T, ctx context.Context, addr, pin string) *CompanionCredentials {
	t.Helper()
	client, err := DialCompanion(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	pairing, err := client.BeginPairing(ctx)
	if err != nil {
		t.Fatalf("BeginPairing: %v", err)
	}
	creds, err := pairing.Finish(ctx, pin, "foxCast test")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return creds
}

func TestCompanionPairAndLaunch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receiver := startCompanionReceiver(t, CompanionReceiverConfig{
		PIN:  "1234",
		Apps: map[string]string{"com.google.ios.youtube": "YouTube"},
	})
	addr := receiver.Addr().String()

	creds := pairCompanion(t, ctx, addr, "1234")
	if !creds.Valid() || creds.ReceiverID != receiver.identifier || !bytes.Equal(creds.ReceiverPublic, receiver.privateKey.Public().(ed25519.PublicKey)) {
		t.Fatalf("credentials = %+v", creds)
	}

	client, err := DialCompanion(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Verify(ctx, creds); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := client.StartSession(ctx, CompanionSystemInfo{Name: "foxCast", PairingID: creds.PairingID}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	const link = "youtube://www.youtube.com/watch?v=dQw4w9WgXcQ"
	if err := client.LaunchApp(ctx, link); err != nil {
		t.Fatalf("LaunchApp(url): %v", err)
	}
	if err := client.LaunchApp(ctx, "com.google.ios.youtube"); err != nil {
		t.Fatalf("LaunchApp(bundle): %v", err)
	}
	apps, err := client.Apps(ctx)
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if !reflect.DeepEqual(apps, map[string]string{"com.google.ios.youtube": "YouTube"}) {
		t.Fatalf("Apps = %v", apps)
	}
	var cerr *CompanionError
	if _, err := client.Request(ctx, "noSuchCommand", map[string]any{}); !errors.As(err, &cerr) {
		t.Fatalf("unknown command error = %v, want CompanionError", err)
	}
	if got := receiver.Launched(); !reflect.DeepEqual(got, []string{link, "com.google.ios.youtube"}) {
		t.Fatalf("Launched = %v", got)
	}
}

func TestCompanionWrongPIN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receiver := startCompanionReceiver(t, CompanionReceiverConfig{PIN: "1234"})
	client, err := DialCompanion(ctx, receiver.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	pairing, err := client.BeginPairing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pairing.Finish(ctx, "4321", "foxCast"); !errors.Is(err, ErrPairingAuthentication) {
		t.Fatalf("Finish with wrong PIN = %v, want ErrPairingAuthentication", err)
	}
}

func TestCompanionVerifyRejectsUnknownController(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := startCompanionReceiver(t, CompanionReceiverConfig{PIN: "1234"})
	second := startCompanionReceiver(t, CompanionReceiverConfig{PIN: "1234"})
	creds := pairCompanion(t, ctx, first.Addr().String(), "1234")

	client, err := DialCompanion(ctx, second.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// The second receiver's V2 signature does not match the first's key.
	if err := client.Verify(ctx, creds); err == nil {
		t.Fatal("Verify against another receiver succeeded")
	}

	// Without the receiver key, the receiver itself rejects the controller.
	creds.ReceiverPublic = nil
	client2, err := DialCompanion(ctx, second.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()
	if err := client2.Verify(ctx, creds); err == nil {
		t.Fatal("Verify with an unknown controller succeeded")
	}
}

func TestCompanionRequestNeedsVerify(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receiver := startCompanionReceiver(t, CompanionReceiverConfig{PIN: "1234"})
	client, err := DialCompanion(ctx, receiver.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.LaunchApp(ctx, "youtube://"); err == nil {
		t.Fatal("LaunchApp before Verify succeeded")
	}
}
