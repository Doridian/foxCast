package hap

// TLV8 tag codes used in HAP pair-setup and pair-verify messages.
// These are standardized by the HAP specification.
const (
	TLVMethod        = uint8(0x00) // Pairing method (0 = pair-setup)
	TLVIdentifier    = uint8(0x01) // Pairing identifier string
	TLVSalt          = uint8(0x02) // SRP salt (16 bytes)
	TLVPublicKey     = uint8(0x03) // SRP or X25519 public key
	TLVProof         = uint8(0x04) // SRP proof (M1 or M2)
	TLVEncryptedData = uint8(0x05) // ChaCha20-Poly1305 encrypted payload
	TLVState         = uint8(0x06) // Message state (M1–M6)
	TLVError         = uint8(0x07) // Error code
	TLVSignature     = uint8(0x0A) // Ed25519 signature
	TLVFlags         = uint8(0x13) // Pairing flags (Apple internal)

	// FlagsTransientPairing requests a transient session (no stored credentials).
	// Used in pair-setup M1 when X-Apple-HKP: 4.  PIN is always "3939".
	FlagsTransientPairing = uint8(0x10)
)
