package sender

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// ErrPINRequired indicates that the receiver advertises one-time PIN pairing,
// so a caller must start the PIN display instead of probing transient setup.
var ErrPINRequired = errors.New("receiver requires PIN pairing")

// ErrPairingAuthentication indicates that the receiver rejected the SRP proof
// at pair-setup M4, i.e. the PIN, password or transient setup code was wrong.
var ErrPairingAuthentication = errors.New("pair-setup M4 error: authentication (2)")

// PairKeys holds the long-term and session keys from pairing.
type PairKeys struct {
	Ed25519Public  ed25519.PublicKey
	Ed25519Private ed25519.PrivateKey
	SharedSecret   []byte
	WriteKey       []byte
	ReadKey        []byte
	// AccessoryPublic is the receiver's long-term Ed25519 key from pair-setup
	// M6. HAP pair-verify checks the receiver's signature against it; it is
	// nil for transient pairings and credentials saved before it was kept.
	AccessoryPublic ed25519.PublicKey
	// MixFairPlayKey records the transformation negotiated by pair-verify.
	// HAP always mixes its shared secret. The raw protocol requests the same
	// transformation with X-Apple-PD unless the receiver advertises feature 27,
	// the empirical compatibility signal for the original unmixed key path.
	MixFairPlayKey bool
}

// TLV8 types for HomeKit-style pairing.
const (
	tlvMethod        = 0x00
	tlvIdentifier    = 0x01
	tlvSalt          = 0x02
	tlvPublicKey     = 0x03
	tlvProof         = 0x04
	tlvEncryptedData = 0x05
	tlvState         = 0x06
	tlvError         = 0x07
	tlvRetryDelay    = 0x08
	tlvSignature     = 0x0A
	tlvName          = 0x11 // Companion: OPACK {"name": ...} of the controller
	tlvACL           = 0x12
	tlvFlags         = 0x13
)

const (
	pairingErrorAuthentication = 2
	pairingErrorBackoff        = 3
	pairSetupBackoffRetries    = 3
	pairSetupMaximumRetryWait  = 30 * time.Second
)

// Pairing flags.
const (
	pairingFlagTransient = 0x00000010 // Bit 4: ephemeral/transient pairing
)

// transientPairingPIN is the fixed SRP password pyatv uses for transient
// pair-setup (pyatv/protocols/airplay/auth/hap_transient.py).
const transientPairingPIN = "3939"

// TransientSetupCodes returns the SRP passwords to try, in order, for
// transient pair-setup. doubletake uses an empty password and pyatv uses
// "3939"; a receiver with "Require Password" enabled rejects both at M4 with
// an authentication error, so its configured password is tried first.
func TransientSetupCodes(password string) []string {
	codes := make([]string, 0, 3)
	if password != "" && password != transientPairingPIN {
		codes = append(codes, password)
	}
	return append(codes, "", transientPairingPIN)
}

// X-Apple-HKP pairing types used by current Apple senders. Screen capture has
// its own system-pairing type and ACL; transient pairing is a separate type.
const (
	pairingTypeLegacy        = 3
	pairingTypeTransient     = 4
	pairingTypeScreenCapture = 5
)

// pairingProtocol is learned from the exchange, rather than receiver identity.
// Feature flags select the first probe; only a completed setup and verify pins
// the protocol for subsequent verification attempts on the same client.
type pairingProtocol uint8

const (
	pairingProtocolUnknown pairingProtocol = iota
	pairingProtocolHAP
	pairingProtocolRaw
)

// PairingProtocol reports the wire protocol which most recently completed on
// this client. Persist it with the long-term identity so a later PairVerify
// uses the same framing without relying on receiver identity or fingerprints.
func (c *AirPlayClient) PairingProtocol() PairingProtocol {
	switch c.pairingProtocol {
	case pairingProtocolHAP:
		return PairingProtocolHAP
	case pairingProtocolRaw:
		return PairingProtocolRaw
	default:
		return PairingProtocolUnknown
	}
}

const defaultPairingClientName = "foxCast device"

// OPACK encoding of {"com.apple.ScreenCapture": true}. Apple includes this
// access request in pair-setup M5 for X-Apple-HKP type 5, and current receivers
// reject a screen-capture identity that omits it.
const screenCaptureACL = "\xe1\x57com.apple.ScreenCapture\x01"

// pairingClientName is shown by the receiver while it asks the user to allow
// pairing. Prefer the machine's familiar hostname, but never put an empty or
// malformed value into the hand-built RTSP headers.
func pairingClientName() string {
	hostname, _ := os.Hostname()
	return sanitizePairingClientName(hostname)
}

func sanitizePairingClientName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) {
		return defaultPairingClientName
	}
	if strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return defaultPairingClientName
	}
	return name
}

// pairHeaders identifies this sender on Apple pairing requests.
func (c *AirPlayClient) pairHeaders() map[string]string {
	if c.effectivePairType() == pairingTypeLegacy {
		return map[string]string{"X-Apple-HKP": strconv.Itoa(pairingTypeLegacy)}
	}

	headers := map[string]string{
		"X-Apple-Client-Name": pairingClientName(),
		"X-Apple-HKP":         strconv.Itoa(c.effectivePairType()),
	}
	if c.PairingID != "" {
		headers["X-Apple-Client-ID"] = c.PairingID
	}
	return headers
}

// pairVerifyHeaders identifies a paired-device verification exchange.
func (c *AirPlayClient) pairVerifyHeaders() map[string]string {
	headers := c.pairHeaders()
	if c.effectivePairType() != pairingTypeLegacy {
		headers["X-Apple-PD"] = "1"
	}
	return headers
}

func (c *AirPlayClient) effectivePairType() int {
	if c.pairingProtocol == pairingProtocolRaw {
		return pairingTypeLegacy
	}
	if c.info.PrefersLegacyPairing() {
		return pairingTypeLegacy
	}
	if c.pairType == 0 {
		return pairingTypeScreenCapture
	}
	return c.pairType
}

func (c *AirPlayClient) pinStartHeaders() map[string]string {
	headers := c.pairHeaders()
	if c.effectivePairType() != pairingTypeLegacy {
		// Ask for a four-digit on-screen code.
		headers["X-Apple-SupportedPINLengths"] = "4"
	}
	return headers
}

func (c *AirPlayClient) transientPairingType() int {
	if c.info.PrefersLegacyPairing() {
		return pairingTypeLegacy
	}
	return pairingTypeTransient
}

func (c *AirPlayClient) pinPairingType() int {
	if c.info.PrefersLegacyPairing() {
		return pairingTypeLegacy
	}
	return pairingTypeScreenCapture
}

// SRP-6a parameters (3072-bit group from RFC 5054).
var (
	srpN, _ = new(big.Int).SetString(
		"FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD1"+
			"29024E088A67CC74020BBEA63B139B22514A08798E3404DD"+
			"EF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245"+
			"E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7ED"+
			"EE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3D"+
			"C2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F"+
			"83655D23DCA3AD961C62F356208552BB9ED529077096966D"+
			"670C354E4ABC9804F1746C08CA18217C32905E462E36CE3B"+
			"E39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9"+
			"DE2BCBF6955817183995497CEA956AE515D2261898FA0510"+
			"15728E5A8AAAC42DAD33170D04507A33A85521ABDF1CBA64"+
			"ECFB850458DBEF0A8AEA71575D060C7DB3970F85A6E1E4C7"+
			"ABF5AE8CDB0933D71E8C94E04A25619DCEE3D2261AD2EE6B"+
			"F12FFA06D98A0864D87602733EC86A64521F2B18177B200C"+
			"BBE117577A615D6C770988C0BAD946E208E24FA074E5AB31"+
			"43DB5BFCE0FD108E4B82D120A93AD2CAFFFFFFFFFFFFFFFF", 16)
	srpG = big.NewInt(5)
)

// pairTransient performs transient pairing (no PIN required).
func (c *AirPlayClient) pairTransient(ctx context.Context) error {
	if c.info != nil && c.info.RequiredPairingCredential() == PairingCredentialPIN {
		return ErrPINRequired
	}
	c.pairingProtocol = pairingProtocolUnknown
	c.pairType = c.transientPairingType()

	// Generate Ed25519 key pair for this session
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ed25519: %w", err)
	}

	c.PairKeys = &PairKeys{
		Ed25519Public:  pub,
		Ed25519Private: priv,
	}

	return c.performTransientSetupAndVerify(ctx)
}

// performTransientSetupAndVerify does transient (PIN-less) pair-setup + pair-verify.
func (c *AirPlayClient) performTransientSetupAndVerify(ctx context.Context) error {
	dbg("[PAIR] starting transient pair-setup")
	if c.info != nil && c.info.usesModernPairing() && c.info.SupportsTransientPairing() {
		dbg("[PAIR] receiver advertises modern transient pairing; using TLV8 directly")
		if err := c.pairSetupTransient(ctx); err != nil {
			if !isUnsupportedHAPPairSetup(err) {
				return fmt.Errorf("pair-setup: %w", err)
			}
			dbg("[PAIR] advertised HAP pair-setup is unsupported (%v); probing raw AirPlay pairing", err)
			if rawErr := c.performRawSetupAndVerify(ctx); rawErr != nil {
				return fmt.Errorf("pair-setup: advertised HAP protocol was rejected (%v); raw fallback: %w", err, rawErr)
			}
			return nil
		}
		if c.encrypted {
			return nil // M4-only transient setup already keyed the channel
		}
		if err := c.PairVerify(ctx); err != nil {
			return fmt.Errorf("pair-verify: %w", err)
		}
		c.pairingProtocol = pairingProtocolHAP
		dbg("[PAIR] pair-verify complete, channel is now encrypted")
		return nil
	}

	// Try the original raw binary AirPlay pair-setup first.
	// Send 32-byte Ed25519 public key, expect 32-byte server public key back.
	dbg("[PAIR] trying raw binary AirPlay pair-setup")
	serverPub, err := c.rawPairSetup(ctx)
	if err != nil {
		// Fall back to TLV8/HomeKit-style pair-setup (Apple TV)
		dbg("[PAIR] raw pair-setup failed (%v), trying TLV8 pair-setup", err)
		if err := c.pairSetupTransient(ctx); err != nil {
			return fmt.Errorf("pair-setup: %w", err)
		}
		if c.encrypted {
			return nil // M4-only transient setup already keyed the channel
		}
		dbg("[PAIR] transient pair-setup complete, starting HAP pair-verify")
		if err := c.PairVerify(ctx); err != nil {
			return fmt.Errorf("pair-verify: %w", err)
		}
		c.pairingProtocol = pairingProtocolHAP
		dbg("[PAIR] pair-verify complete, channel is now encrypted")
		return nil
	}

	// Raw pair-setup succeeded — store server's Ed25519 public key and use raw pair-verify
	dbg("[PAIR] raw pair-setup OK, server Ed25519 pub: %02x", serverPub[:8])
	return c.completeRawSetupAndVerify(ctx, serverPub)
}

// performRawSetupAndVerify probes the original binary AirPlay pairing
// protocol. It deliberately takes no PIN/password: a playback password belongs
// to HTTP Digest on receivers which advertise modern pairing.
func (c *AirPlayClient) performRawSetupAndVerify(ctx context.Context) error {
	serverPub, err := c.rawPairSetup(ctx)
	if err != nil {
		return fmt.Errorf("raw pair-setup: %w", err)
	}
	return c.completeRawSetupAndVerify(ctx, serverPub)
}

func (c *AirPlayClient) completeRawSetupAndVerify(ctx context.Context, serverPub []byte) error {
	if c.info == nil {
		c.info = &ReceiverInfo{}
	}
	c.info.PK = serverPub
	dbg("[PAIR] starting raw pair-verify (no HAP encryption)")
	if err := c.rawPairVerify(ctx); err != nil {
		return fmt.Errorf("raw pair-verify: %w", err)
	}
	dbg("[PAIR] raw pair-verify complete (connection stays plaintext)")
	return nil
}

// isUnsupportedHAPPairSetup recognizes transport responses which mean the
// receiver rejected the HAP wire format before returning a TLV8 M2. It is kept
// intentionally narrow: authentication, rate limiting, HomeKit backoff, and
// all errors after setup must surface to the caller instead of changing
// protocols.
func isUnsupportedHAPPairSetup(err error) bool {
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	switch statusErr.StatusCode {
	case 400, 404, 405, 415, 455, 500, 501, 505:
		return true
	default:
		return false
	}
}

// rawPairSetup sends a 32-byte Ed25519 public key to /pair-setup and expects
// a 32-byte server Ed25519 public key back. This is the original AirPlay
// transient pair-setup protocol.
func (c *AirPlayClient) rawPairSetup(ctx context.Context) ([]byte, error) {
	resp, err := c.httpRequest("POST", "/pair-setup", "application/octet-stream", c.PairKeys.Ed25519Public)
	if err != nil {
		return nil, fmt.Errorf("pair-setup: %w", err)
	}
	if len(resp) != 32 {
		return nil, fmt.Errorf("pair-setup: expected 32 bytes, got %d", len(resp))
	}
	return resp, nil
}

// pairSetupTransient performs a transient (ephemeral, no on-screen PIN)
// pair-setup keyed by c.transientSetupCode.
func (c *AirPlayClient) pairSetupTransient(ctx context.Context) error {
	// Transient M1: method=0, state=1, flags=transient
	flags := make([]byte, 4)
	binary.LittleEndian.PutUint32(flags, pairingFlagTransient)

	m1 := tlv8EncodeOrdered([]tlv8Item{
		{Tag: tlvMethod, Value: []byte{0x00}},
		{Tag: tlvState, Value: []byte{0x01}},
		{Tag: tlvFlags, Value: flags},
	})

	m2, err := exchangePairSetupM1(ctx, func() ([]byte, error) {
		return c.httpRequest("POST", "/pair-setup", "application/octet-stream", m1, c.pairHeaders())
	})
	if err != nil {
		return err
	}

	serverPub := m2[tlvPublicKey]
	serverSalt := m2[tlvSalt]
	if serverPub == nil {
		return fmt.Errorf("M2: missing server public key")
	}

	// pyatv's fixed-code transient flow ends at M4; so does a password-protected
	// Apple TV, which drops the connection on M5. doubletake's empty-code flow
	// continues through M5/M6 and pair-verify.
	m4Only := c.transientSetupCode != ""
	return c.completeSRPExchange(ctx, c.transientSetupCode, serverSalt, serverPub, m4Only)
}

// StartPINDisplay triggers the PIN display on the Apple TV.
// Call this before prompting the user so the PIN is visible when they're asked.
func (c *AirPlayClient) StartPINDisplay() error {
	c.pairType = c.pinPairingType()
	if _, err := c.httpRequest("POST", "/pair-pin-start", "", nil, c.pinStartHeaders()); err != nil {
		var statusErr *HTTPStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == 453 {
			dbg("[PAIR] receiver accepted PIN request asynchronously (HTTP 453)")
			return nil
		}
		return fmt.Errorf("pair-pin-start: %w", err)
	}
	return nil
}

// pairWithPIN performs PIN-based pairing.
func (c *AirPlayClient) pairWithPIN(ctx context.Context, pin string) error {
	c.pairingProtocol = pairingProtocolUnknown
	c.pairType = c.pinPairingType()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ed25519: %w", err)
	}

	c.PairKeys = &PairKeys{
		Ed25519Public:  pub,
		Ed25519Private: priv,
	}

	return c.performPairSetupAndVerify(ctx, pin)
}

func (c *AirPlayClient) performPairSetupAndVerify(ctx context.Context, pin string) error {
	if err := c.pairSetup(ctx, pin); err != nil {
		return fmt.Errorf("pair-setup: %w", err)
	}
	if err := c.PairVerify(ctx); err != nil {
		return fmt.Errorf("pair-verify: %w", err)
	}
	c.pairingProtocol = pairingProtocolHAP
	return nil
}

// pairSetup implements the SRP6a pair-setup exchange (PIN-based).
func (c *AirPlayClient) pairSetup(ctx context.Context, pin string) error {
	// M1: Send pairing method + state
	m1 := tlv8EncodeOrdered([]tlv8Item{
		{Tag: tlvMethod, Value: []byte{0x00}},
		{Tag: tlvState, Value: []byte{0x01}},
	})

	m2, err := exchangePairSetupM1(ctx, func() ([]byte, error) {
		return c.httpRequest("POST", "/pair-setup", "application/octet-stream", m1, c.pairHeaders())
	})
	if err != nil {
		return err
	}

	salt := m2[tlvSalt]
	serverPubB := m2[tlvPublicKey]
	if salt == nil || serverPubB == nil {
		return fmt.Errorf("M2: missing salt or public key")
	}

	return c.completeSRPExchange(ctx, pin, salt, serverPubB, false)
}

// exchangePairSetupM1 sends an unchanged pair-setup M1 until the receiver
// returns M2 or its bounded HomeKit backoff cannot be honored. Backoff happens
// before SRP uses the PIN/password, so reporting it as a bad credential is both
// misleading and prevents current Apple receivers from recovering.
func exchangePairSetupM1(ctx context.Context, send func() ([]byte, error)) (map[byte][]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for retry := 0; ; retry++ {
		m2Bytes, err := send()
		if err != nil {
			return nil, fmt.Errorf("M1: %w", err)
		}
		m2 := tlv8Decode(m2Bytes)
		errorValue := m2[tlvError]
		if len(errorValue) == 0 {
			return m2, nil
		}

		code := int(errorValue[0])
		if code != pairingErrorBackoff {
			return nil, fmt.Errorf("pair-setup M2 error: %s (%d)", pairingErrorName(code), code)
		}
		if retry >= pairSetupBackoffRetries {
			return nil, fmt.Errorf("pair-setup M2 error: backoff (3) persisted after %d retries", retry)
		}
		delay, err := pairingRetryDelay(m2[tlvRetryDelay])
		if err != nil {
			return nil, fmt.Errorf("pair-setup M2 backoff: %w", err)
		}
		dbg("[PAIR] receiver requested pair-setup backoff for %v (retry %d/%d)", delay, retry+1, pairSetupBackoffRetries)
		if delay == 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, fmt.Errorf("pair-setup M2 backoff: %w", ctx.Err())
		}
	}
}

func pairingRetryDelay(value []byte) (time.Duration, error) {
	if len(value) == 0 || len(value) > 8 {
		return 0, fmt.Errorf("invalid RetryDelay length %d", len(value))
	}
	var seconds uint64
	for index, octet := range value {
		seconds |= uint64(octet) << (8 * index)
	}
	if seconds > uint64(pairSetupMaximumRetryWait/time.Second) {
		return 0, fmt.Errorf("RetryDelay %ds exceeds %v limit", seconds, pairSetupMaximumRetryWait)
	}
	return time.Duration(seconds) * time.Second, nil
}

func pairingErrorName(code int) string {
	switch code {
	case 1:
		return "unknown"
	case 2:
		return "authentication"
	case pairingErrorBackoff:
		return "backoff"
	case 4:
		return "maximum peers"
	case 5:
		return "maximum tries"
	case 6:
		return "unavailable"
	case 7:
		return "busy"
	default:
		return "unknown"
	}
}

// completeSRPExchange finishes SRP from M3 onward (shared by PIN and transient
// flows). With m4Only, the exchange stops after M4 and the SRP session key
// directly keys the encrypted control channel, replacing pair-verify.
func (c *AirPlayClient) completeSRPExchange(ctx context.Context, pin string, salt, serverPubB []byte, m4Only bool) error {
	srp, err := newSRPClientSession(pin, salt, serverPubB)
	if err != nil {
		return err
	}

	// M3: Send client public key + proof
	m4Bytes, err := c.httpRequest("POST", "/pair-setup", "application/octet-stream", srp.m3(), c.pairHeaders())
	if err != nil {
		return fmt.Errorf("M3: %w", err)
	}
	if err := srp.checkM4(tlv8Decode(m4Bytes)); err != nil {
		return err
	}
	if m4Only {
		dbg("[PAIR] M4-only transient pair-setup complete; enabling encryption from SRP key")
		return c.enableHAPEncryption(srp.key)
	}

	// M5: Exchange Ed25519 keys over encrypted channel
	var extra []tlv8Item
	if c.effectivePairType() == pairingTypeScreenCapture {
		extra = append(extra, tlv8Item{Tag: tlvACL, Value: []byte(screenCaptureACL)})
	}
	m5, err := srp.m5([]byte(c.PairingID), c.PairKeys.Ed25519Public, c.PairKeys.Ed25519Private, extra)
	if err != nil {
		return err
	}
	m6Bytes, err := c.httpRequest("POST", "/pair-setup", "application/octet-stream", m5, c.pairHeaders())
	if err != nil {
		return fmt.Errorf("M5: %w", err)
	}

	m6 := tlv8Decode(m6Bytes)
	if errTLV, ok := m6[tlvError]; ok {
		return fmt.Errorf("pair-setup M6 error: %d", errTLV[0])
	}
	c.PairKeys.SharedSecret = srp.key
	if pin == "" && len(m6[tlvEncryptedData]) == 0 {
		// doubletake's empty-code transient flow proves nothing about the
		// receiver, and its keys are never saved.
		return nil
	}
	_, accessoryKey, err := srp.accessoryIdentity(m6)
	if err != nil {
		return err
	}
	c.PairKeys.AccessoryPublic = accessoryKey
	return nil
}

// srpClientSession is the controller side of an SRP-6a pair-setup from M2
// onward, independent of the transport carrying the TLV8 messages.
type srpClientSession struct {
	clientPublic []byte
	proof        []byte
	// key is the SRP session key K = H(S).
	key []byte
	// serverProof is the M4 proof the accessory must return.
	serverProof []byte
}

// newSRPClientSession computes the M3 values for pin from the accessory's M2
// salt and public key.
func newSRPClientSession(pin string, salt, serverPubB []byte) (*srpClientSession, error) {
	username := []byte("Pair-Setup")
	password := []byte(pin)

	// x = H(salt, H(username, ":", password))
	innerHash := sha512.Sum512(append(append(username, ':'), password...))
	xInput := append(append([]byte{}, salt...), innerHash[:]...)
	xHash := sha512.Sum512(xInput)
	x := new(big.Int).SetBytes(xHash[:])

	// k = H(N, pad(g))
	padN := padTo(srpN.Bytes(), 384)
	padG := padTo(srpG.Bytes(), 384)
	kHash := sha512.Sum512(append(padN, padG...))
	k := new(big.Int).SetBytes(kHash[:])

	aBytes := make([]byte, 32)
	if _, err := rand.Read(aBytes); err != nil {
		return nil, fmt.Errorf("generate SRP private key: %w", err)
	}
	a := new(big.Int).SetBytes(aBytes)
	if a.Sign() == 0 {
		a.SetInt64(1)
	}
	A := new(big.Int).Exp(srpG, a, srpN)
	clientPublic := A.Bytes()

	B := new(big.Int).SetBytes(serverPubB)
	if B.Sign() <= 0 || B.Cmp(srpN) >= 0 {
		return nil, fmt.Errorf("M2: invalid server public key")
	}
	serverPublic := B.Bytes()

	uHash := sha512.Sum512(append(padTo(clientPublic, 384), padTo(serverPublic, 384)...))
	u := new(big.Int).SetBytes(uHash[:])

	// S = (B - k * g^x mod N)^(a + u*x) mod N
	gx := new(big.Int).Exp(srpG, x, srpN)
	kgx := new(big.Int).Mul(k, gx)
	kgx.Mod(kgx, srpN)
	diff := new(big.Int).Sub(B, kgx)
	if diff.Sign() < 0 {
		diff.Add(diff, srpN)
	}
	exp := new(big.Int).Mul(u, x)
	exp.Add(exp, a)
	S := new(big.Int).Exp(diff, exp, srpN)

	// K = H(S) — S uses natural (unpadded) byte representation
	sHash := sha512.Sum512(S.Bytes())
	K := sHash[:]

	// M1 proof = H(H(N) XOR H(g), H(I), s, A, B, K)
	// Per SRP-6a: proof uses natural (unpadded) byte representations
	hnHash := sha512.Sum512(srpN.Bytes())
	hgHash := sha512.Sum512(srpG.Bytes())
	hxor := make([]byte, 64)
	for i := range hxor {
		hxor[i] = hnHash[i] ^ hgHash[i]
	}
	huHash := sha512.Sum512(username)

	proofInput := bytes.Join([][]byte{
		hxor, huHash[:], salt,
		clientPublic, serverPublic, K,
	}, nil)
	m1Proof := sha512.Sum512(proofInput)

	// Server proof: H(A, M1, K) — A unpadded
	m2Proof := sha512.Sum512(bytes.Join([][]byte{clientPublic, m1Proof[:], K}, nil))

	return &srpClientSession{
		clientPublic: clientPublic,
		proof:        m1Proof[:],
		key:          K,
		serverProof:  m2Proof[:],
	}, nil
}

// m3 returns the M3 TLV8 message: the client public key and proof.
func (s *srpClientSession) m3() []byte {
	return tlv8EncodeOrdered([]tlv8Item{
		{Tag: tlvState, Value: []byte{0x03}},
		{Tag: tlvPublicKey, Value: padTo(s.clientPublic, 384)},
		{Tag: tlvProof, Value: s.proof},
	})
}

// checkM4 checks the accessory's M4 for an error and, when it sends one, its
// proof.
func (s *srpClientSession) checkM4(m4 map[byte][]byte) error {
	if errTLV, ok := m4[tlvError]; ok {
		if len(errTLV) > 0 && errTLV[0] == pairingErrorAuthentication {
			return ErrPairingAuthentication
		}
		return fmt.Errorf("pair-setup M4 error: %d", errTLV[0])
	}
	if serverProof, ok := m4[tlvProof]; ok {
		if !bytes.Equal(serverProof, s.serverProof) {
			return fmt.Errorf("server proof mismatch")
		}
	}
	return nil
}

// encryptKey is the ChaCha20-Poly1305 key of the M5/M6 sub-TLVs.
func (s *srpClientSession) encryptKey() []byte {
	return hkdfSHA512(s.key, []byte("Pair-Setup-Encrypt-Salt"), []byte("Pair-Setup-Encrypt-Info"), 32)
}

// m5 returns the M5 TLV8 message, which hands the accessory the controller's
// long-term identity. extra items go into the encrypted sub-TLV after the
// identity and signature.
func (s *srpClientSession) m5(pairingID []byte, pub ed25519.PublicKey, priv ed25519.PrivateKey, extra []tlv8Item) ([]byte, error) {
	sigKey := hkdfSHA512(s.key, []byte("Pair-Setup-Controller-Sign-Salt"), []byte("Pair-Setup-Controller-Sign-Info"), 32)
	sigInput := bytes.Join([][]byte{sigKey, pairingID, pub}, nil)
	signature := ed25519.Sign(priv, sigInput)

	subTLVItems := append([]tlv8Item{
		{Tag: tlvIdentifier, Value: pairingID},
		{Tag: tlvPublicKey, Value: pub},
		{Tag: tlvSignature, Value: signature},
	}, extra...)
	subTLV := tlv8EncodeOrdered(subTLVItems)

	aead, err := chacha20poly1305.New(s.encryptKey())
	if err != nil {
		return nil, fmt.Errorf("chacha20: %w", err)
	}
	nonce := make([]byte, 12)
	copy(nonce[4:], "PS-Msg05")
	encrypted := aead.Seal(nil, nonce, subTLV, nil)

	return tlv8EncodeOrdered([]tlv8Item{
		{Tag: tlvEncryptedData, Value: encrypted},
		{Tag: tlvState, Value: []byte{0x05}},
	}), nil
}

// accessoryIdentity opens the M6 sub-TLV and checks the accessory's
// signature, returning its pairing identifier and long-term public key.
func (s *srpClientSession) accessoryIdentity(m6 map[byte][]byte) (string, ed25519.PublicKey, error) {
	if errTLV, ok := m6[tlvError]; ok && len(errTLV) > 0 {
		return "", nil, fmt.Errorf("pair-setup M6 error: %d", errTLV[0])
	}
	aead, err := chacha20poly1305.New(s.encryptKey())
	if err != nil {
		return "", nil, fmt.Errorf("chacha20: %w", err)
	}
	nonce := make([]byte, 12)
	copy(nonce[4:], "PS-Msg06")
	plain, err := aead.Open(nil, nonce, m6[tlvEncryptedData], nil)
	if err != nil {
		return "", nil, fmt.Errorf("decrypt M6: %w", err)
	}
	sub := tlv8Decode(plain)
	identifier, publicKey, signature := sub[tlvIdentifier], sub[tlvPublicKey], sub[tlvSignature]
	if len(identifier) == 0 || len(publicKey) != ed25519.PublicKeySize {
		return "", nil, fmt.Errorf("M6: missing accessory identity")
	}
	signKey := hkdfSHA512(s.key, []byte("Pair-Setup-Accessory-Sign-Salt"), []byte("Pair-Setup-Accessory-Sign-Info"), 32)
	if !ed25519.Verify(publicKey, bytes.Join([][]byte{signKey, identifier, publicKey}, nil), signature) {
		return "", nil, fmt.Errorf("M6: accessory signature mismatch")
	}
	return string(identifier), ed25519.PublicKey(publicKey), nil
}

// PairVerify verifies the stored long-term identity: HAP pair-verify (X25519 +
// Ed25519, then an encrypted control channel), or raw pair-verify when that
// is the protocol this client last completed or restored.
func (c *AirPlayClient) PairVerify(ctx context.Context) error {
	if c.pairingProtocol == pairingProtocolRaw {
		return c.rawPairVerify(ctx)
	}
	return c.hapPairVerify(ctx)
}

func (c *AirPlayClient) hapPairVerify(ctx context.Context) error {
	verify, err := newHAPVerifySession()
	if err != nil {
		return err
	}

	// V1: Send our ephemeral X25519 public key only.
	// The Ed25519 long-term key was already exchanged during pair-setup M5.
	dbg("[PAIR-VERIFY] V1: sending %d-byte X25519 public key", len(verify.public))
	v2Bytes, err := c.httpRequest("POST", "/pair-verify", "application/octet-stream", verify.v1(), c.pairVerifyHeaders())
	if err != nil {
		return fmt.Errorf("V1: %w", err)
	}

	// V3: Send our encrypted proof
	v3, err := verify.v3(tlv8Decode(v2Bytes), []byte(c.PairingID), c.PairKeys.Ed25519Private, c.PairKeys.AccessoryPublic)
	if err != nil {
		return err
	}
	dbg("[PAIR-VERIFY] V3: sending encrypted proof")
	v4Bytes, err := c.httpRequest("POST", "/pair-verify", "application/octet-stream", v3, c.pairVerifyHeaders())
	if err != nil {
		return fmt.Errorf("V3: %w", err)
	}
	// Check V4 response for TLV errors
	if len(v4Bytes) > 0 {
		v4 := tlv8Decode(v4Bytes)
		if errTLV, ok := v4[tlvError]; ok {
			return fmt.Errorf("pair-verify V4 error: %d", errTLV[0])
		}
		dbg("[PAIR-VERIFY] V4: response %d bytes, no error", len(v4Bytes))
	} else {
		dbg("[PAIR-VERIFY] V4: empty response (OK)")
	}

	return c.enableHAPEncryption(verify.shared)
}

// hapVerifySession is the controller side of a HAP pair-verify, independent
// of the transport carrying the TLV8 messages.
type hapVerifySession struct {
	private, public []byte
	// shared is the X25519 secret, set by v3.
	shared []byte
}

func newHAPVerifySession() (*hapVerifySession, error) {
	private := make([]byte, curve25519.ScalarSize)
	if _, err := rand.Read(private); err != nil {
		return nil, fmt.Errorf("generate X25519 private key: %w", err)
	}
	public, err := curve25519.X25519(private, curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("x25519: %w", err)
	}
	return &hapVerifySession{private: private, public: public}, nil
}

// v1 returns the V1 TLV8 message: our ephemeral X25519 public key.
func (v *hapVerifySession) v1() []byte {
	return tlv8EncodeOrdered([]tlv8Item{
		{Tag: tlvState, Value: []byte{0x01}},
		{Tag: tlvPublicKey, Value: v.public},
	})
}

// v3 processes the accessory's V2 and returns the V3 TLV8 message. When
// accessoryKey is set, the accessory's signature must verify against it;
// otherwise V2's encrypted data is only checked to decrypt.
func (v *hapVerifySession) v3(v2 map[byte][]byte, pairingID []byte, priv ed25519.PrivateKey, accessoryKey ed25519.PublicKey) ([]byte, error) {
	if errTLV, ok := v2[tlvError]; ok {
		return nil, fmt.Errorf("pair-verify V2 error: %d", errTLV[0])
	}

	serverKeyData := v2[tlvPublicKey]
	serverEncrypted := v2[tlvEncryptedData]
	dbg("[PAIR-VERIFY] V2: server pubkey=%d bytes, encrypted=%d bytes", len(serverKeyData), len(serverEncrypted))

	if len(serverKeyData) < 32 {
		return nil, fmt.Errorf("V2: server public key too short")
	}
	serverPublic := serverKeyData[:32]

	shared, err := curve25519.X25519(v.private, serverPublic)
	if err != nil {
		return nil, fmt.Errorf("x25519: %w", err)
	}

	// Derive session encryption key
	verifyKey := hkdfSHA512(shared, []byte("Pair-Verify-Encrypt-Salt"), []byte("Pair-Verify-Encrypt-Info"), 32)
	aead, err := chacha20poly1305.New(verifyKey)
	if err != nil {
		return nil, fmt.Errorf("chacha20: %w", err)
	}

	// Decrypt and verify server's response if encrypted data present
	if len(serverEncrypted) > 0 || accessoryKey != nil {
		nonce := make([]byte, 12)
		copy(nonce[4:], "PV-Msg02")
		plain, err := aead.Open(nil, nonce, serverEncrypted, nil)
		if err != nil {
			return nil, fmt.Errorf("decrypt V2: %w", err)
		}
		if accessoryKey != nil {
			sub := tlv8Decode(plain)
			signed := bytes.Join([][]byte{serverPublic, sub[tlvIdentifier], v.public}, nil)
			if !ed25519.Verify(accessoryKey, signed, sub[tlvSignature]) {
				return nil, fmt.Errorf("V2: accessory signature mismatch")
			}
		}
	}

	// HAP spec: sign(clientX25519Public || pairingID || serverX25519Public)
	sigInput := bytes.Join([][]byte{v.public, pairingID, serverPublic}, nil)
	signature := ed25519.Sign(priv, sigInput)
	dbg("[PAIR-VERIFY] V3: sig input = clientPub(%d) || pairingID(%d) || serverPub(%d) = %d bytes",
		len(v.public), len(pairingID), len(serverPublic), len(sigInput))

	subTLV := tlv8EncodeOrdered([]tlv8Item{
		{Tag: tlvIdentifier, Value: pairingID},
		{Tag: tlvSignature, Value: signature},
	})
	nonce := make([]byte, 12)
	copy(nonce[4:], "PV-Msg03")
	encrypted := aead.Seal(nil, nonce, subTLV, nil)

	v.shared = shared
	return tlv8EncodeOrdered([]tlv8Item{
		{Tag: tlvState, Value: []byte{0x03}},
		{Tag: tlvEncryptedData, Value: encrypted},
	}), nil
}

// enableHAPEncryption switches the control channel to HAP framing, keyed from
// the pair-verify X25519 secret or an M4-only transient SRP session key.
func (c *AirPlayClient) enableHAPEncryption(shared []byte) error {
	c.PairKeys.SharedSecret = shared
	c.encWriteKey = hkdfSHA512(shared, []byte("Control-Salt"), []byte("Control-Write-Encryption-Key"), 32)
	c.encReadKey = hkdfSHA512(shared, []byte("Control-Salt"), []byte("Control-Read-Encryption-Key"), 32)
	c.PairKeys.WriteKey = c.encWriteKey
	c.PairKeys.ReadKey = c.encReadKey

	dbg("[PAIR-VERIFY] shared secret: %s...", hex.EncodeToString(shared[:16]))
	dbg("[PAIR-VERIFY] writeKey: %s...", hex.EncodeToString(c.encWriteKey[:8]))
	dbg("[PAIR-VERIFY] readKey:  %s...", hex.EncodeToString(c.encReadKey[:8]))

	writeCipher, err := chacha20poly1305.New(c.encWriteKey)
	if err != nil {
		return fmt.Errorf("write cipher: %w", err)
	}
	c.encCipher = writeCipher
	c.encWriteNonce = 0
	c.encReadNonce = 0
	c.encrypted = true
	c.PairKeys.MixFairPlayKey = true
	c.pairingProtocol = pairingProtocolHAP
	dbg("[PAIR-VERIFY] encryption ENABLED (HAP framing, nonces at 0)")

	return nil
}

// TLV8 encoding/decoding

// tlv8Item is an ordered tag-value pair for deterministic encoding.
type tlv8Item struct {
	Tag   byte
	Value []byte
}

// tlv8EncodeOrdered encodes TLV8 items in the order given.
func tlv8EncodeOrdered(items []tlv8Item) []byte {
	var buf bytes.Buffer
	for _, item := range items {
		value := item.Value
		if len(value) == 0 {
			buf.WriteByte(item.Tag)
			buf.WriteByte(0)
			continue
		}
		for len(value) > 0 {
			chunk := value
			if len(chunk) > 255 {
				chunk = chunk[:255]
			}
			buf.WriteByte(item.Tag)
			buf.WriteByte(byte(len(chunk)))
			buf.Write(chunk)
			value = value[len(chunk):]
		}
	}
	return buf.Bytes()
}

func tlv8Decode(data []byte) map[byte][]byte {
	result := make(map[byte][]byte)
	for len(data) >= 2 {
		tag := data[0]
		length := int(data[1])
		data = data[2:]
		if length > len(data) {
			break
		}
		result[tag] = append(result[tag], data[:length]...)
		data = data[length:]
	}
	return result
}

// hkdfSHA512 derives a key using HKDF-SHA-512.
func hkdfSHA512(secret, salt, info []byte, length int) []byte {
	r := hkdf.New(sha512.New, secret, salt, info)
	key := make([]byte, length)
	if _, err := io.ReadFull(r, key); err != nil {
		panic(fmt.Sprintf("hkdf read: %v", err))
	}
	return key
}

// padTo pads data with leading zeros to the specified length.
func padTo(data []byte, size int) []byte {
	if len(data) >= size {
		return data
	}
	padded := make([]byte, size)
	copy(padded[size-len(data):], data)
	return padded
}

// nonceBytes converts a uint64 nonce to a 12-byte nonce for ChaCha20.
func nonceBytes(n uint64) []byte {
	nonce := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonce[4:], n)
	return nonce
}

// rawPairVerify performs the original non-HAP pair-verify, which keeps the
// control connection in plaintext.
//
// Protocol (raw binary, NOT TLV8):
//
//	V1 (client→server, 68 bytes): \x01\x00\x00\x00 + X25519_pub(32) + Ed25519_pub(32)
//	V2 (server→client, 96 bytes): server_X25519_pub(32) + AES-CTR(server_sig, offset=0)(64)
//	V3 (client→server, 68 bytes): \x00\x00\x00\x00 + AES-CTR(client_sig, offset=64)(64)
//	V4 (server→client, 0 bytes): empty 200 OK
//
// AES-CTR key derivation (SHA-512, NOT HKDF):
//
//	key = SHA-512("Pair-Verify-AES-Key" || X25519_shared_secret)[:16]
//	iv  = SHA-512("Pair-Verify-AES-IV"  || X25519_shared_secret)[:16]
func (c *AirPlayClient) rawPairVerify(ctx context.Context) error {
	// X-Apple-PD asks the receiver to apply the pair-verify shared secret to
	// FairPlay key derivation. Apple's senders advertise it unconditionally, but
	// observed raw receivers split on feature 27: those advertising the original
	// legacy-pairing capability require the unmixed key path, while those without
	// it require PD mixing. Keep this empirical exception capability-based.
	mixFairPlayKey := c.info == nil || !c.info.SupportsLegacyPairing()
	var verifyHeaders map[string]string
	if mixFairPlayKey {
		verifyHeaders = map[string]string{"X-Apple-PD": "1"}
	}

	// Generate ephemeral X25519 key pair
	var clientPrivate [32]byte
	rand.Read(clientPrivate[:])
	var clientPublic [32]byte
	curve25519.ScalarBaseMult(&clientPublic, &clientPrivate)

	// V1: flags(4) + X25519_pub(32) + Ed25519_pub(32) = 68 bytes
	v1 := make([]byte, 68)
	v1[0] = 0x01 // flags: auth type=1
	copy(v1[4:36], clientPublic[:])
	copy(v1[36:68], c.PairKeys.Ed25519Public)

	dbg("[RAW-PV] V1: sending 68 bytes (X25519 pub + Ed25519 pub)")
	dbg("[RAW-PV] V1 hex: %02x", v1)
	v2, err := c.rawRequest("POST", "/pair-verify", "application/octet-stream", v1, verifyHeaders)
	if err != nil {
		return fmt.Errorf("V1: %w", err)
	}
	dbg("[RAW-PV] V2: received %d bytes", len(v2))

	if len(v2) != 96 {
		return fmt.Errorf("V2: expected 96 bytes, got %d", len(v2))
	}

	// Extract server's X25519 public key (first 32 bytes)
	var serverPublic [32]byte
	copy(serverPublic[:], v2[:32])
	encryptedServerSig := v2[32:96]

	// Compute X25519 shared secret
	shared, err := curve25519.X25519(clientPrivate[:], serverPublic[:])
	if err != nil {
		return fmt.Errorf("x25519: %w", err)
	}

	// Derive AES-128-CTR key and IV from shared secret using SHA-512
	aesKey := sha512DeriveKey("Pair-Verify-AES-Key", shared)
	aesIV := sha512DeriveKey("Pair-Verify-AES-IV", shared)

	// Decrypt server's signature (at AES-CTR offset 0)
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return fmt.Errorf("aes cipher: %w", err)
	}
	serverSig := make([]byte, 64)
	cipher.NewCTR(block, aesIV).XORKeyStream(serverSig, encryptedServerSig)

	// Verify server's Ed25519 signature over (server_X25519 || client_X25519)
	serverSigMsg := make([]byte, 64)
	copy(serverSigMsg[:32], serverPublic[:])
	copy(serverSigMsg[32:], clientPublic[:])

	// Use the server's Ed25519 public key from /info
	if c.info == nil || len(c.info.PK) < 32 {
		return fmt.Errorf("server Ed25519 public key not available (call GetInfo first)")
	}
	serverEd25519Pub := ed25519.PublicKey(c.info.PK[:32])
	if !ed25519.Verify(serverEd25519Pub, serverSigMsg, serverSig) {
		return fmt.Errorf("server signature verification failed")
	}
	dbg("[RAW-PV] server signature verified OK")

	// Sign our proof: Ed25519_sign(client_X25519 || server_X25519)
	clientSigMsg := make([]byte, 64)
	copy(clientSigMsg[:32], clientPublic[:])
	copy(clientSigMsg[32:], serverPublic[:])
	clientSig := ed25519.Sign(c.PairKeys.Ed25519Private, clientSigMsg)

	// Encrypt client signature at AES-CTR offset 64 (skip first 64 bytes)
	block2, _ := aes.NewCipher(aesKey)
	ctr := cipher.NewCTR(block2, aesIV)
	skip := make([]byte, 64)
	ctr.XORKeyStream(skip, skip) // advance CTR by 64 bytes
	encryptedClientSig := make([]byte, 64)
	ctr.XORKeyStream(encryptedClientSig, clientSig)

	// V3: flags(4) + encrypted_client_sig(64) = 68 bytes
	v3 := make([]byte, 68)
	// v3[0:4] = 0x00000000 (already zero)
	copy(v3[4:68], encryptedClientSig)

	dbg("[RAW-PV] V3: sending encrypted proof (68 bytes)")
	v4, err := c.rawRequest("POST", "/pair-verify", "application/octet-stream", v3, verifyHeaders)
	if err != nil {
		return fmt.Errorf("V3: %w", err)
	}

	if len(v4) != 0 {
		dbg("[RAW-PV] V4: unexpected %d bytes in response", len(v4))
	}
	dbg("[RAW-PV] pair-verify complete (connection stays PLAINTEXT)")

	// Store shared secret for potential stream key derivation,
	// but do NOT enable HAP encryption on the control channel
	c.PairKeys.SharedSecret = shared
	c.PairKeys.MixFairPlayKey = mixFairPlayKey
	c.pairType = pairingTypeLegacy
	c.pairingProtocol = pairingProtocolRaw
	return nil
}

// sha512DeriveKey derives a 16-byte key: SHA-512(salt || secret)[:16]
func sha512DeriveKey(salt string, secret []byte) []byte {
	h := sha512.New()
	h.Write([]byte(salt))
	h.Write(secret)
	return h.Sum(nil)[:16]
}
