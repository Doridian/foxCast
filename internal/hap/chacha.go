package hap

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// SealMsg encrypts plaintext using ChaCha20-Poly1305 with an 8-byte ASCII nonce string.
// Used for pair-setup (PS-Msg05/06) and pair-verify (PV-Msg02/03) message bodies.
// The 8-byte nonce is padded to 12 bytes by prepending 4 zero bytes, matching pyatv's
// Chacha20Cipher._pad_nonce behavior.
func SealMsg(key []byte, nonce8 string, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := msgNonce(nonce8)
	return aead.Seal(nil, nonce, plaintext, nil), nil
}

// OpenMsg decrypts a ChaCha20-Poly1305 ciphertext using an 8-byte ASCII nonce string.
func OpenMsg(key []byte, nonce8 string, ciphertext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := msgNonce(nonce8)
	return aead.Open(nil, nonce, ciphertext, nil)
}

// EncryptFrame encrypts plaintext into a single HAP session frame.
// plaintext must be ≤ 1024 bytes.
// Returns: [uint16_LE length][ChaCha20-Poly1305 ciphertext + 16-byte tag]
// AAD is the 2-byte length prefix, matching HAPSession in pyatv.
func EncryptFrame(key []byte, counter uint64, plaintext []byte) ([]byte, error) {
	if len(plaintext) > 1024 {
		return nil, fmt.Errorf("hap: frame plaintext too large (%d > 1024)", len(plaintext))
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	length := make([]byte, 2)
	binary.LittleEndian.PutUint16(length, uint16(len(plaintext)))
	nonce := frameNonce(counter)
	ct := aead.Seal(nil, nonce, plaintext, length)
	return append(length, ct...), nil
}

// DecryptFrame decrypts a HAP session frame.
// frame must start with the 2-byte LE length followed by the ciphertext+tag.
func DecryptFrame(key []byte, counter uint64, frame []byte) ([]byte, error) {
	if len(frame) < 2 {
		return nil, fmt.Errorf("hap: frame too short (%d bytes)", len(frame))
	}
	length := frame[:2]
	payloadLen := int(binary.LittleEndian.Uint16(length))
	ciphertext := frame[2:]
	if len(ciphertext) != payloadLen+chacha20poly1305.Overhead {
		return nil, fmt.Errorf("hap: frame length mismatch: header says %d, got %d ciphertext bytes",
			payloadLen, len(ciphertext))
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := frameNonce(counter)
	return aead.Open(nil, nonce, ciphertext, length)
}

// msgNonce pads an 8-byte ASCII nonce string to the required 12 bytes by prepending
// 4 zero bytes: [0x00,0x00,0x00,0x00, nonce8[0..7]].
func msgNonce(nonce8 string) []byte {
	nonce := make([]byte, 12)
	copy(nonce[4:], []byte(nonce8))
	return nonce
}

// frameNonce encodes a uint64 counter as a 12-byte ChaCha20 nonce:
// [0x00,0x00,0x00,0x00, counter_LE8], matching pyatv's Chacha20Cipher with nonce_length=8.
func frameNonce(counter uint64) []byte {
	nonce := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonce[4:], counter)
	return nonce
}
