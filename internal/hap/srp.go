package hap

import (
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"math/big"
)

// srpPrimeHex is the 3072-bit prime from RFC 5054 Appendix A.4, used for HAP pair-setup.
// srpPrimeHex is the 3072-bit MODP prime from RFC 5054 Appendix A.4.
// Verified against srptools.constants.PRIME_3072 (768 hex chars = 384 bytes).
const srpPrimeHex = "" +
	"FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74" +
	"020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F1437" +
	"4FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7ED" +
	"EE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF05" +
	"98DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB" +
	"9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3B" +
	"E39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF695581718" +
	"3995497CEA956AE515D2261898FA051015728E5A8AAAC42DAD33170D04507A33" +
	"A85521ABDF1CBA64ECFB850458DBEF0A8AEA71575D060C7DB3970F85A6E1E4C7" +
	"ABF5AE8CDB0933D71E8C94E04A25619DCEE3D2261AD2EE6BF12FFA06D98A0864" +
	"D87602733EC86A64521F2B18177B200CBBE117577A615D6C770988C0BAD946E2" +
	"08E24FA074E5AB3143DB5BFCE0FD108E4B82D120A93AD2CAFFFFFFFFFFFFFFFF"

var (
	srpN *big.Int // 3072-bit prime
	srpG *big.Int // generator = 5
	srpK *big.Int // k = H(N || PAD(g))
)

func init() {
	raw, err := hex.DecodeString(srpPrimeHex)
	if err != nil {
		panic("hap: invalid SRP prime hex: " + err.Error())
	}
	srpN = new(big.Int).SetBytes(raw)
	srpG = big.NewInt(5)
	srpK = srpComputeK()
}

// srpComputeK computes k = SHA-512(N_bytes || PAD_384(g_bytes)).
// Matches srptools.SRPContext.__init__ with prime=PRIME_3072, generator=5.
func srpComputeK() *big.Int {
	nBytes := srpN.Bytes() // 384 bytes (fills exactly for 3072-bit prime)
	gBytes := srpPad(srpG)
	h := sha512.New()
	h.Write(nBytes)
	h.Write(gBytes)
	return new(big.Int).SetBytes(h.Sum(nil))
}

// srpPad zero-pads val to 384 bytes (the byte length of N).
func srpPad(val *big.Int) []byte {
	b := val.Bytes()
	if len(b) == 384 {
		return b
	}
	padded := make([]byte, 384)
	copy(padded[384-len(b):], b)
	return padded
}

// srpHash hashes one or more byte slices concatenated together using SHA-512,
// returning the digest as a big.Int (matching srptools' hash() with as_bytes=False).
func srpHash(parts ...[]byte) *big.Int {
	h := sha512.New()
	for _, p := range parts {
		h.Write(p)
	}
	return new(big.Int).SetBytes(h.Sum(nil))
}

// srpHashBytes is like srpHash but returns the raw 64-byte digest.
func srpHashBytes(parts ...[]byte) []byte {
	h := sha512.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// SRPClient is a client-side SRP-6a session for HAP pair-setup.
//
// Parameters: 3072-bit prime (RFC 5054 A.4), generator g=5, SHA-512.
// Session key K = SHA-512(S) — single hash, matching srptools/pyatv.
type SRPClient struct {
	username string
	password string
	a        *big.Int // private key
	A        *big.Int // public key A = g^a mod N

	salt []byte
	B    *big.Int

	sessionKey []byte // K = SHA-512(S), 64 bytes
	proof      []byte // M1
	proofHash  []byte // M2 (expected from server)
}

// NewSRPClient creates a new SRP client for HAP pair-setup.
// username is always "Pair-Setup"; password is the numeric PIN as a string (e.g. "1111").
// a is the client's private key as a big.Int; pass nil to generate a random one.
func NewSRPClient(username, password string, a *big.Int) *SRPClient {
	if a == nil {
		// Generate a random private key of 1024 bits.
		rawA := make([]byte, 128)
		if _, err := randFill(rawA); err != nil {
			panic("hap: failed to generate SRP private key: " + err.Error())
		}
		a = new(big.Int).SetBytes(rawA)
	}
	A := new(big.Int).Exp(srpG, a, srpN)
	return &SRPClient{
		username: username,
		password: password,
		a:        a,
		A:        A,
	}
}

// PublicKeyBytes returns the client's public key A as minimal big-endian bytes,
// ready to be sent to the server in TLV8 tag 0x03.
func (c *SRPClient) PublicKeyBytes() []byte {
	return c.A.Bytes()
}

// Process computes the shared secret and session proofs from the server's salt and
// public key B. Both are raw bytes as received from the server's TLV8 response.
// Must be called before Proof(), SessionKey(), or VerifyServerProof().
func (c *SRPClient) Process(salt, serverPublicB []byte) error {
	c.salt = salt
	c.B = new(big.Int).SetBytes(serverPublicB)

	if new(big.Int).Mod(c.B, srpN).Sign() == 0 {
		return errors.New("hap/srp: server public key B is zero mod N")
	}

	// x = SHA-512(salt || SHA-512("username:password"))
	innerHash := srpHashBytes([]byte(c.username + ":" + c.password))
	x := srpHash(salt, innerHash)

	// u = SHA-512(PAD(A) || PAD(B))
	u := srpHash(srpPad(c.A), srpPad(c.B))

	// v = g^x mod N
	v := new(big.Int).Exp(srpG, x, srpN)

	// S = (B - k*v)^(a + u*x) mod N
	// Use modular arithmetic to keep intermediate values in [0,N).
	kv := new(big.Int).Mul(srpK, v)
	kv.Mod(kv, srpN)

	base := new(big.Int).Sub(c.B, kv)
	base.Mod(base, srpN)

	exp := new(big.Int).Mul(u, x)
	exp.Add(exp, c.a)

	S := new(big.Int).Exp(base, exp, srpN)

	// K = SHA-512(S) — single hash, matching srptools.get_common_session_key
	c.sessionKey = srpHashBytes(S.Bytes())

	// M1 = SHA-512(bytes(H(N) XOR H(g)) || bytes(H(username)) || salt || A || B || K)
	// H(N), H(g) are big.Ints; XOR → minimal bytes (matching srptools int_to_bytes).
	hN := srpHash(srpN.Bytes())
	hG := srpHash(srpG.Bytes())
	hNxorG := new(big.Int).Xor(hN, hG)

	hI := srpHash([]byte(c.username))

	c.proof = srpHashBytes(
		hNxorG.Bytes(),
		hI.Bytes(),
		salt,
		c.A.Bytes(),
		c.B.Bytes(),
		c.sessionKey,
	)

	// M2 = SHA-512(A || M1 || K)
	c.proofHash = srpHashBytes(c.A.Bytes(), c.proof, c.sessionKey)

	return nil
}

// Proof returns the client proof M1, sent to the server in TLV8 tag 0x04.
func (c *SRPClient) Proof() []byte { return c.proof }

// SessionKey returns K = SHA-512(S), the 64-byte SRP session key.
// This is used as IKM for subsequent HKDF derivations.
func (c *SRPClient) SessionKey() []byte { return c.sessionKey }

// VerifyServerProof checks the server's proof M2 (TLV8 tag 0x04 from M4 response).
func (c *SRPClient) VerifyServerProof(serverProofM2 []byte) bool {
	if len(serverProofM2) != len(c.proofHash) {
		return false
	}
	for i := range serverProofM2 {
		if serverProofM2[i] != c.proofHash[i] {
			return false
		}
	}
	return true
}
