package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/applink"
	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

func TestAppLink(t *testing.T) {
	file := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const yt = "https://youtu.be/dQw4w9WgXcQ"
	ytLink := applink.Link{App: "YouTube", URL: "youtube://www.youtube.com/watch?v=dQw4w9WgXcQ"}
	tests := []struct {
		location string
		mode     appMode
		want     applink.Link
		ok       bool
	}{
		{yt, appAuto, ytLink, true},
		{yt, appAlways, ytLink, true},
		{yt, appNever, applink.Link{}, false},
		{"https://example.com/video.mp4", appAuto, applink.Link{}, false},
		{"https://www.twitch.tv/somechannel", appAlways, applink.Link{App: "the app that handles it", URL: "https://www.twitch.tv/somechannel"}, true},
		{file, appAlways, applink.Link{}, false},
	}
	for _, tt := range tests {
		got, ok := appLink(tt.location, tt.mode)
		if ok != tt.ok || got != tt.want {
			t.Errorf("appLink(%q, %s) = %+v, %v; want %+v, %v", tt.location, tt.mode, got, ok, tt.want, tt.ok)
		}
	}
}

// countingPrompter answers every prompt with pin.
type countingPrompter struct {
	pin   string
	kinds []credentialKind
}

func (p *countingPrompter) promptCredential(_ context.Context, _ string, kind credentialKind) (string, error) {
	p.kinds = append(p.kinds, kind)
	return p.pin, nil
}

func TestOpenInAppPairsOnceThenReusesCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	airplay, err := sender.NewReceiverServer(sender.ReceiverConfig{
		ListenAddress: "127.0.0.1:0",
		Profile:       sender.ReceiverProfileModern,
		Name:          "Test Apple TV",
	})
	if err != nil {
		t.Fatal(err)
	}
	companion, err := sender.NewCompanionReceiver(sender.CompanionReceiverConfig{PIN: "4711"})
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServing := context.WithCancel(ctx)
	done := make(chan struct{}, 2)
	go func() { _ = airplay.Serve(serveCtx); done <- struct{}{} }()
	go func() { _ = companion.Serve(serveCtx); done <- struct{}{} }()
	defer func() {
		stopServing()
		<-done
		<-done
	}()

	store, err := sender.NewCredentialStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	prompter := &countingPrompter{pin: "4711"}
	opts := &connectOptions{
		target:        "127.0.0.1",
		port:          airplay.Addr().(*net.TCPAddr).Port,
		companionPort: companion.Addr().(*net.TCPAddr).Port,
		store:         store,
		prompter:      prompter,
	}

	first := applink.Link{App: "YouTube", URL: "youtube://www.youtube.com/watch?v=dQw4w9WgXcQ"}
	if err := openInApp(ctx, opts, first, nil); err != nil {
		t.Fatalf("first openInApp: %v", err)
	}
	if !reflect.DeepEqual(prompter.kinds, []credentialKind{credentialPINOrPassword}) {
		t.Fatalf("prompts = %v, want one PIN-or-password prompt", prompter.kinds)
	}

	second := applink.Link{App: "Apple TV", URL: "https://tv.apple.com/us/movie/spirited/umc.cmc.3lp7wqowerzdbej98tveildi3"}
	if err := openInApp(ctx, opts, second, nil); err != nil {
		t.Fatalf("second openInApp: %v", err)
	}
	if len(prompter.kinds) != 1 {
		t.Fatalf("second open prompted again: %v", prompter.kinds)
	}
	if got, want := companion.Launched(), []string{first.URL, second.URL}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Launched = %v, want %v", got, want)
	}

	// A wrong PIN on a forced re-pairing fails and keeps the old pairing.
	prompter.pin = "0000"
	opts.forcePair = true
	if err := openInApp(ctx, opts, first, nil); err == nil {
		t.Fatal("openInApp with a wrong PIN succeeded")
	}
	opts.forcePair = false
	if err := openInApp(ctx, opts, first, nil); err != nil {
		t.Fatalf("openInApp after failed re-pairing: %v", err)
	}
}

// An Apple TV with "Require Password" pairs with its AirPlay password and
// shows no PIN: a saved password pairs without asking, and a wrong one falls
// back to asking.
func TestOpenInAppPairsWithSavedPassword(t *testing.T) {
	const testDeviceID = "AA:BB:CC:DD:EE:FF"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	airplay, err := sender.NewReceiverServer(sender.ReceiverConfig{
		ListenAddress: "127.0.0.1:0",
		Profile:       sender.ReceiverProfileModern,
		Name:          "Test Apple TV",
		DeviceID:      testDeviceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	companion, err := sender.NewCompanionReceiver(sender.CompanionReceiverConfig{PIN: "s3cret pw"})
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServing := context.WithCancel(ctx)
	done := make(chan struct{}, 2)
	go func() { _ = airplay.Serve(serveCtx); done <- struct{}{} }()
	go func() { _ = companion.Serve(serveCtx); done <- struct{}{} }()
	defer func() {
		stopServing()
		<-done
		<-done
	}()

	store, err := sender.NewCredentialStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePassword(testDeviceID, "s3cret pw"); err != nil {
		t.Fatal(err)
	}
	prompter := &countingPrompter{pin: "s3cret pw"}
	opts := &connectOptions{
		target:        "127.0.0.1",
		port:          airplay.Addr().(*net.TCPAddr).Port,
		companionPort: companion.Addr().(*net.TCPAddr).Port,
		store:         store,
		prompter:      prompter,
	}

	link := applink.Link{App: "YouTube", URL: "youtube://www.youtube.com/watch?v=dQw4w9WgXcQ"}
	if err := openInApp(ctx, opts, link, nil); err != nil {
		t.Fatalf("openInApp with saved password: %v", err)
	}
	if len(prompter.kinds) != 0 {
		t.Fatalf("prompted despite a saved password: %v", prompter.kinds)
	}

	if err := store.SavePassword(testDeviceID, "stale"); err != nil {
		t.Fatal(err)
	}
	opts.forcePair = true
	if err := openInApp(ctx, opts, link, nil); err != nil {
		t.Fatalf("openInApp with a stale saved password: %v", err)
	}
	if !reflect.DeepEqual(prompter.kinds, []credentialKind{credentialPINOrPassword}) {
		t.Fatalf("prompts = %v, want one PIN-or-password prompt", prompter.kinds)
	}
}
