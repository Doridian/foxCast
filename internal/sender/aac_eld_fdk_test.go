//go:build cgo && fdk_aac

package sender

import (
	"bytes"
	"testing"
)

func TestFDKAACELDEncodesRawFrame(t *testing.T) {
	encoder, err := newELDEncoder()
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()

	output := make([]byte, 8192)
	n, err := encoder.Encode(make([]byte, 480*2*2), output)
	if err != nil {
		t.Fatal(err)
	}
	if n <= 0 || n > len(output) {
		t.Fatalf("encoded AAC-ELD length = %d", n)
	}
	if bytes.HasPrefix(output[:n], []byte{0xff, 0xf1}) || bytes.HasPrefix(output[:n], []byte{0xff, 0xf9}) {
		t.Fatalf("AAC-ELD output unexpectedly contains an ADTS header: %x", output[:min(n, 8)])
	}
}
