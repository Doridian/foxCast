package tlv8

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeSingle(t *testing.T) {
	in := map[uint8][]byte{0x06: {0x01}}
	want := []byte{0x06, 0x01, 0x01}

	got := Encode(in)
	if !bytes.Equal(got, want) {
		t.Fatalf("Encode single: got %x want %x", got, want)
	}

	dec, err := Decode(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec[0x06], []byte{0x01}) {
		t.Fatalf("Decode single: got %x", dec[0x06])
	}
}

func TestEncodeDecodeMultiple(t *testing.T) {
	// HAP pair-setup M1: {State=M1, Method=pair-setup}
	records := []Record{
		{Tag: 0x06, Value: []byte{0x01}}, // State = M1
		{Tag: 0x00, Value: []byte{0x00}}, // Method = pair-setup
	}
	encoded := EncodeOrdered(records)
	want := []byte{0x06, 0x01, 0x01, 0x00, 0x01, 0x00}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("EncodeOrdered: got %x want %x", encoded, want)
	}

	dec, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec[0x06], []byte{0x01}) {
		t.Fatalf("State tag: got %x", dec[0x06])
	}
	if !bytes.Equal(dec[0x00], []byte{0x00}) {
		t.Fatalf("Method tag: got %x", dec[0x00])
	}
}

func TestFragmentation(t *testing.T) {
	// Value of 300 bytes should be split into 255 + 45 with the same tag.
	value := make([]byte, 300)
	for i := range value {
		value[i] = byte(i)
	}
	encoded := EncodeOrdered([]Record{{Tag: 0x03, Value: value}})

	// First fragment: tag=0x03, length=255, 255 bytes
	// Second fragment: tag=0x03, length=45, 45 bytes
	wantLen := 2 + 255 + 2 + 45
	if len(encoded) != wantLen {
		t.Fatalf("fragmented encoded len: got %d want %d", len(encoded), wantLen)
	}
	if encoded[0] != 0x03 || encoded[1] != 255 {
		t.Fatalf("first fragment header wrong: %x %x", encoded[0], encoded[1])
	}
	if encoded[2+255] != 0x03 || encoded[2+255+1] != 45 {
		t.Fatalf("second fragment header wrong")
	}

	dec, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec[0x03], value) {
		t.Fatal("de-fragmented value does not match original")
	}
}

func TestDecodeError(t *testing.T) {
	// Truncated record (tag present, length claims 10 bytes, only 3 available).
	bad := []byte{0x01, 0x0a, 0x00, 0x01, 0x02}
	_, err := Decode(bad)
	if err == nil {
		t.Fatal("expected error for truncated record, got nil")
	}
}

func TestRoundTrip(t *testing.T) {
	original := map[uint8][]byte{
		0x01: []byte("identifier-string"),
		0x03: make([]byte, 512), // forces two-fragment encoding
		0x06: {0x03},
	}
	for i := range original[0x03] {
		original[0x03][i] = byte(i % 251)
	}

	encoded := Encode(original)
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for tag, want := range original {
		got, ok := decoded[tag]
		if !ok {
			t.Fatalf("tag 0x%02x missing after round-trip", tag)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("tag 0x%02x: round-trip mismatch", tag)
		}
	}
}
