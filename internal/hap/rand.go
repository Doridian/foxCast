package hap

import "crypto/rand"

// randFill fills b with cryptographically random bytes.
func randFill(b []byte) (int, error) {
	return rand.Read(b)
}
