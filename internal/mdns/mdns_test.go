package mdns

import (
	"encoding/hex"
	"testing"
)

func TestParseFeatures(t *testing.T) {
	tests := []struct {
		input string
		want  FeatureFlags
	}{
		// Fake receiver features from fake_receiver.py
		{"0x4A7FDFD5,0x038BCB46", 0x038BCB464A7FDFD5},
		// Single word (no comma)
		{"0x4A7FDFD5", 0x4A7FDFD5},
		// Zero
		{"0x0,0x0", 0},
	}

	for _, tc := range tests {
		got, err := ParseFeatures(tc.input)
		if err != nil {
			t.Errorf("ParseFeatures(%q) error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseFeatures(%q) = 0x%016x, want 0x%016x", tc.input, uint64(got), uint64(tc.want))
		}
	}
}

func TestParseFeaturesError(t *testing.T) {
	_, err := ParseFeatures("not-a-hex")
	if err == nil {
		t.Fatal("expected error for invalid hex")
	}
}

func TestFeatureFlagBits(t *testing.T) {
	// Verify specific bits using the fake receiver's features.
	f, err := ParseFeatures("0x4A7FDFD5,0x038BCB46")
	if err != nil {
		t.Fatal(err)
	}

	if f&FeatureHomeKitPairing == 0 {
		t.Error("expected HomeKitPairing bit to be set")
	}
	if f&FeatureTransientPairing == 0 {
		t.Error("expected TransientPairing bit to be set")
	}
	if f&FeatureAudio == 0 {
		t.Error("expected Audio bit to be set")
	}
}

func TestParseTXT(t *testing.T) {
	txt := []string{
		"deviceid=AA:BB:CC:DD:EE:FF",
		"features=0x4A7FDFD5,0x038BCB46",
		"model=AppleTV6,2",
		"pk=0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20",
		"novalue",      // no '=' — should be skipped
		"empty=",       // empty value is fine
	}
	m := parseTXT(txt)

	if m["deviceid"] != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("deviceid = %q", m["deviceid"])
	}
	if m["model"] != "AppleTV6,2" {
		t.Errorf("model = %q", m["model"])
	}
	if m["empty"] != "" {
		t.Errorf("empty = %q", m["empty"])
	}
	if _, ok := m["novalue"]; ok {
		t.Error("novalue should not appear in map")
	}

	// Check pk decoding path manually
	pkHex := m["pk"]
	pk, err := hex.DecodeString(pkHex)
	if err != nil {
		t.Fatalf("pk decode: %v", err)
	}
	if len(pk) != 32 {
		t.Errorf("pk length = %d, want 32", len(pk))
	}
}
