package hap

import (
	"crypto/sha512"
	"io"

	"golang.org/x/crypto/hkdf"
)

// DeriveKey derives a 32-byte key using HKDF-SHA512 with the given salt and info strings,
// as used throughout HAP pair-setup and pair-verify.
func DeriveKey(ikm []byte, salt, info string) []byte {
	r := hkdf.New(sha512.New, ikm, []byte(salt), []byte(info))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		panic("hap: HKDF read failed: " + err.Error())
	}
	return key
}
