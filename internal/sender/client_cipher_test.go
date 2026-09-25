package sender

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

func TestMirrorCipherPreservesCachedKeystreamAcrossSmallFrames(t *testing.T) {
	key := bytes.Repeat([]byte{0x31}, 16)
	iv := bytes.Repeat([]byte{0x72}, 16)
	frames := [][]byte{
		{0x01},
		{0x02, 0x03},
		{0x04, 0x05, 0x06, 0x07},
		{0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		{0x11, 0x12, 0x13},
	}

	mirror, err := newMirrorCipher(key, iv)
	if err != nil {
		t.Fatal(err)
	}
	var got, plaintext []byte
	for _, frame := range frames {
		got = append(got, mirror.EncryptFrame(frame)...)
		plaintext = append(plaintext, frame...)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, len(plaintext))
	cipher.NewCTR(block, iv).XORKeyStream(want, plaintext)
	if !bytes.Equal(got, want) {
		t.Fatalf("fragmented mirror ciphertext = %x, want continuous CTR prefix %x", got, want)
	}
}
