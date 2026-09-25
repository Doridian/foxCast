package sender

import (
	"bytes"
	"crypto/sha512"
	"testing"
)

// The stream master key is either the raw FairPlay key or its SHA-512 mixture
// with the pair-verify secret. Physical AppleTV3 hardware takes the raw branch;
// HAP receivers and UxPlay take the mixed branch even though UxPlay leaves its
// control channel plaintext.
func TestDeriveStreamMasterKey(t *testing.T) {
	raw := bytes.Repeat([]byte{0xa5}, 16)
	secret := bytes.Repeat([]byte{0x5a}, 32)

	h := sha512.New()
	h.Write(raw)
	h.Write(secret)
	mixed := h.Sum(nil)[:16]

	for _, tc := range []struct {
		name      string
		secret    []byte
		mixSecret bool
		want      []byte
	}{
		// The case issue #17 was about: rawPairVerify leaves a shared secret
		// behind, but the receiver only ever saw ekey, which wraps the raw key.
		{"physical legacy receiver, secret present", secret, false, raw},
		{"physical legacy receiver, no secret", nil, false, raw},
		{"HAP or UxPlay pairing", secret, true, mixed},
		{"mix requested but no secret", nil, true, raw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveStreamMasterKey(raw, tc.secret, tc.mixSecret)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got %x, want %x", got, tc.want)
			}
		})
	}
}

// The two branches must not coincide, or the test above proves nothing.
func TestDeriveStreamMasterKeyBranchesDiffer(t *testing.T) {
	raw := bytes.Repeat([]byte{0xa5}, 16)
	secret := bytes.Repeat([]byte{0x5a}, 32)
	if bytes.Equal(
		deriveStreamMasterKey(raw, secret, false),
		deriveStreamMasterKey(raw, secret, true),
	) {
		t.Fatal("legacy and HAP derivations produce the same key")
	}
}

// rawPairVerify must keep leaving the channel unencrypted. Receiver policy may
// independently request the UxPlay key mixture; physical AppleTV3 hardware
// still relies on this transport flag remaining false to select its raw path.
func TestRawPairVerifyDoesNotEnableHAPEncryption(t *testing.T) {
	if !bytes.Contains(readSource(t, "pairing.go"), []byte("c.PairKeys.SharedSecret = shared")) {
		t.Skip("pairing.go no longer stores a shared secret in the expected form")
	}
	src := readSource(t, "pairing.go")
	start := bytes.Index(src, []byte("func (c *AirPlayClient) rawPairVerify"))
	if start < 0 {
		t.Skip("rawPairVerify not found")
	}
	body := src[start:]
	if end := bytes.Index(body, []byte("\nfunc ")); end > 0 {
		body = body[:end]
	}
	if bytes.Contains(body, []byte("c.encrypted = true")) {
		t.Error("rawPairVerify now enables HAP encryption; physical AppleTV3 " +
			"receivers would switch to the mixed key (see issue #17)")
	}
}
