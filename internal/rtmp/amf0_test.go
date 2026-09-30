package rtmp

import (
	"bytes"
	"reflect"
	"testing"
)

func TestEncodeAMF0KnownVectors(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  []byte
	}{
		{"number", 1.0, []byte{0x00, 0x3f, 0xf0, 0, 0, 0, 0, 0, 0}},
		{"int", 2, []byte{0x00, 0x40, 0x00, 0, 0, 0, 0, 0, 0}},
		{"true", true, []byte{0x01, 0x01}},
		{"string", "live", []byte{0x02, 0x00, 0x04, 'l', 'i', 'v', 'e'}},
		{"null", nil, []byte{0x05}},
		{"undefined", undefined{}, []byte{0x06}},
		{"object", object{{"a", 0}}, []byte{0x03, 0x00, 0x01, 'a', 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0x00, 0x00, 0x09}},
	}
	for _, tt := range tests {
		if got := encodeAMF0(nil, tt.value); !bytes.Equal(got, tt.want) {
			t.Errorf("%s: got % x, want % x", tt.name, got, tt.want)
		}
	}
}

// TestDecodeAMF0Connect decodes a connect command as OBS sends it (trimmed).
func TestDecodeAMF0Connect(t *testing.T) {
	payload := []byte{
		0x02, 0x00, 0x07, 'c', 'o', 'n', 'n', 'e', 'c', 't',
		0x00, 0x3f, 0xf0, 0, 0, 0, 0, 0, 0,
		0x03,
		0x00, 0x03, 'a', 'p', 'p', 0x02, 0x00, 0x04, 'l', 'i', 'v', 'e',
		0x00, 0x04, 't', 'y', 'p', 'e', 0x02, 0x00, 0x0a, 'n', 'o', 'n', 'p', 'r', 'i', 'v', 'a', 't', 'e',
		0x00, 0x00, 0x09,
		// An ECMA array and a strict array, which some clients add.
		0x08, 0, 0, 0, 1, 0x00, 0x01, 'x', 0x01, 0x00, 0x00, 0x00, 0x09,
		0x0a, 0, 0, 0, 2, 0x05, 0x02, 0x00, 0x01, 'y',
	}
	got, err := decodeAMF0(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		"connect", 1.0,
		map[string]any{"app": "live", "type": "nonprivate"},
		map[string]any{"x": false},
		[]any{nil, "y"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestDecodeAMF0RoundTrip(t *testing.T) {
	enc := encodeAMF0(nil, "_result", 4.0, nil, object{{"code", "ok"}, {"n", 3}, {"b", true}})
	got, err := decodeAMF0(enc)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{"_result", 4.0, nil, map[string]any{"code": "ok", "n": 3.0, "b": true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestDecodeAMF0Truncated(t *testing.T) {
	full := encodeAMF0(nil, "connect", 1.0, object{{"app", "live"}})
	for n := 1; n < len(full); n++ {
		if values, err := decodeAMF0(full[:n]); err == nil && len(values) == 3 {
			t.Errorf("decoding %d of %d bytes gave all values", n, len(full))
		}
	}
}
