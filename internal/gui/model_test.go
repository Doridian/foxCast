package gui

import (
	"os"
	"strings"
	"path/filepath"
	"testing"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

func TestReceiverListMergesAndExpires(t *testing.T) {
	l := newReceiverList()
	t0 := time.Unix(1000, 0)
	tv := sender.AirPlayDevice{Name: "Living Room", DeviceID: "AA:BB", IP: "10.0.0.5", Port: 7000, Features: sender.FeatureVideo | sender.FeatureScreen}
	speaker := sender.AirPlayDevice{Name: "kitchen", DeviceID: "CC:DD", IP: "10.0.0.6", Port: 7000, Features: sender.FeatureAudio}

	if !l.update([]sender.AirPlayDevice{tv, speaker}, t0) {
		t.Fatal("first round should change the listing")
	}
	got := l.receivers()
	if len(got) != 2 || got[0].Name != "kitchen" || got[1].Name != "Living Room" {
		t.Fatalf("receivers not sorted case-insensitively by name: %+v", got)
	}
	if got[0].CanMirror() || got[0].CanPlay() || !got[0].CanStreamAudio() {
		t.Fatal("audio-only receiver not offered audio alone")
	}
	if !got[1].CanMirror() || !got[1].CanPlay() {
		t.Fatal("Apple TV not offered video actions")
	}

	// A round that misses the speaker keeps it until it expires.
	if l.update([]sender.AirPlayDevice{tv}, t0.Add(receiverExpiry/2)) {
		t.Fatal("unchanged round reported a change")
	}
	if len(l.receivers()) != 2 {
		t.Fatal("receiver dropped before expiry")
	}
	if !l.update([]sender.AirPlayDevice{tv}, t0.Add(receiverExpiry+time.Second)) {
		t.Fatal("expiry did not report a change")
	}
	if got := l.receivers(); len(got) != 1 || got[0].DeviceID != "AA:BB" {
		t.Fatalf("expired receiver still listed: %+v", got)
	}

	// A new address for the same device updates it in place.
	moved := tv
	moved.IP = "10.0.0.9"
	if !l.update([]sender.AirPlayDevice{moved}, t0.Add(2*receiverExpiry)) {
		t.Fatal("address change not reported")
	}
	if got := l.receivers(); len(got) != 1 || got[0].IP != "10.0.0.9" {
		t.Fatalf("address change not applied: %+v", got)
	}
}

func TestReceiverListDeviceIDs(t *testing.T) {
	l := newReceiverList()
	l.update([]sender.AirPlayDevice{
		{Name: "b", DeviceID: "BB", IP: "10.0.0.2", Port: 7000},
		{Name: "a", DeviceID: "AA", IP: "10.0.0.1", Port: 7000},
	}, time.Unix(0, 0))
	manual := l.addManual("10.0.0.3", 7000)
	if got := strings.Join(l.deviceIDs(), ","); got != "AA,BB" {
		t.Fatalf("deviceIDs = %s; want AA,BB (manual receivers have none until identified)", got)
	}
	l.identify(manual.Key(), "c", "AppleTV14,1", "CC")
	if got := strings.Join(l.deviceIDs(), ","); got != "AA,BB,CC" {
		t.Fatalf("deviceIDs = %s; want AA,BB,CC", got)
	}
}

func TestReceiverListManualNeverExpires(t *testing.T) {
	l := newReceiverList()
	r := l.addManual("127.0.0.1", 7000)
	if !r.Manual || !r.CanMirror() || !r.CanPlay() || r.Key() != "127.0.0.1:7000" {
		t.Fatalf("unexpected manual receiver: %+v", r)
	}
	l.update(nil, time.Unix(0, 0).Add(10*receiverExpiry))
	if len(l.receivers()) != 1 {
		t.Fatal("manual receiver expired")
	}
	if !l.identify(r.Key(), "Test Receiver", "AppleTV14,1", "AA:BB") {
		t.Fatal("identify did not report a change")
	}
	if got := l.receivers()[0]; got.Name != "Test Receiver" || got.Model != "AppleTV14,1" || got.DeviceID != "AA:BB" || got.Key() != r.Key() {
		t.Fatalf("identify not applied: %+v", got)
	}
	if l.identify(r.Key(), "Test Receiver", "AppleTV14,1", "AA:BB") {
		t.Fatal("repeated identify reported a change")
	}
}

func TestReceiverListIdentifyKeepsAdvertisedName(t *testing.T) {
	l := newReceiverList()
	l.update([]sender.AirPlayDevice{{Name: "Den TV", DeviceID: "AA:BB", IP: "10.0.0.5", Port: 7000}}, time.Unix(0, 0))
	if l.identify("AA:BB", "Other", "AppleTV11,1", "AA:BB") || l.receivers()[0].Name != "Den TV" {
		t.Fatal("discovered receiver renamed")
	}
}

func TestParseAddress(t *testing.T) {
	for _, tc := range []struct {
		in   string
		host string
		port int
		err  bool
	}{
		{in: "10.0.0.5", host: "10.0.0.5", port: 7000},
		{in: " appletv.local:7001 ", host: "appletv.local", port: 7001},
		{in: "[fe80::1]:7000", host: "fe80::1", port: 7000},
		{in: "fe80::1", host: "fe80::1", port: 7000},
		{in: "host:0", err: true},
		{in: "host:http", err: true},
		{in: ":7000", err: true},
		{in: "", err: true},
	} {
		host, port, err := parseAddress(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("parseAddress(%q) = %q, %d; want error", tc.in, host, port)
			}
			continue
		}
		if err != nil || host != tc.host || port != tc.port {
			t.Errorf("parseAddress(%q) = %q, %d, %v; want %q, %d", tc.in, host, port, err, tc.host, tc.port)
		}
	}
}

func TestMediaLocation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "movie night.mkv")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ in, want string }{
		{"https://example.com/video.m3u8", "https://example.com/video.m3u8"},
		{"  http://nas.local/a.mkv\n", "http://nas.local/a.mkv"},
		{file, file},
		{"file://" + filepath.ToSlash(file), file},
		{"file:///does/not/exist.mkv", ""},
		{dir, ""},
		{"relative/movie.mkv", ""},
		{"just some copied text", ""},
		{"https://a.example/1\nhttps://a.example/2", ""},
		{"", ""},
	} {
		if got := mediaLocation(tc.in); got != tc.want {
			t.Errorf("mediaLocation(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestMediaTitle(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/mnt/nas/Movie%20(2020).mkv", "Movie%20(2020).mkv"},
		{"https://example.com/films/Movie%20Night.mkv?token=1", "Movie Night.mkv"},
		{"https://example.com/", "example.com"},
		{"https://example.com", "example.com"},
	} {
		if got := mediaTitle(tc.in); got != tc.want {
			t.Errorf("mediaTitle(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestGroupReceivers(t *testing.T) {
	video := sender.FeatureVideo | sender.FeatureScreen
	rs := []Receiver{
		{AirPlayDevice: sender.AirPlayDevice{Name: "Bedroom HomePod", DeviceID: "1", Features: sender.FeatureAudio}},
		{AirPlayDevice: sender.AirPlayDevice{Name: "Car", DeviceID: "5"}},
		{AirPlayDevice: sender.AirPlayDevice{Name: "Den TV", DeviceID: "2", Model: "AppleTV11,1", Features: video}},
		{AirPlayDevice: sender.AirPlayDevice{Name: "Kitchen", DeviceID: "3", Features: video}},
		{AirPlayDevice: sender.AirPlayDevice{Name: "Living Room TV", DeviceID: "4", Features: video}},
	}
	paired := func(r *Receiver) bool { return r.DeviceID == "4" }
	inUse := func(r *Receiver) bool { return r.DeviceID == "3" }

	known, other := groupReceivers(rs, paired, inUse, "")
	if names(known) != "Kitchen,Living Room TV" {
		t.Errorf("known = %s", names(known))
	}
	if names(other) != "Bedroom HomePod,Den TV,Car" {
		t.Errorf("other = %s (unusable receivers belong last)", names(other))
	}

	known, other = groupReceivers(rs, paired, inUse, " appletv ")
	if len(known) != 0 || names(other) != "Den TV" {
		t.Errorf("query by model: known = %s, other = %s", names(known), names(other))
	}
}

func names(rs []Receiver) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return strings.Join(out, ",")
}

func TestPopupPlacement(t *testing.T) {
	screen := rect{x: 1920, y: 0, w: 2560, h: 1440}
	const w, h = 400, 500
	for _, tc := range []struct {
		name   string
		cx, cy int
		want   placement
	}{
		{"bottom panel, tray at right", 1920 + 2400, 1420,
			placement{anchors: anchorBottom | anchorLeft, left: 2560 - 400, x: 1920 + 2160, y: 940}},
		{"bottom panel, centred", 1920 + 1280, 1430,
			placement{anchors: anchorBottom | anchorLeft, left: 1080, x: 1920 + 1080, y: 940}},
		{"top panel, tray at left", 1920 + 10, 5,
			placement{anchors: anchorTop | anchorLeft, left: 0, x: 1920, y: 0}},
		{"left panel", 1920 + 20, 700,
			placement{anchors: anchorLeft | anchorTop, top: 450, x: 1920, y: 450}},
		{"right panel near bottom", 1920 + 2550, 1300,
			placement{anchors: anchorRight | anchorTop, top: 940, x: 1920 + 2160, y: 940}},
		{"click off screen is clamped", 0, 99999,
			placement{anchors: anchorBottom | anchorLeft, left: 0, x: 1920, y: 940}},
	} {
		if got := popupPlacement(tc.cx, tc.cy, screen, w, h); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
