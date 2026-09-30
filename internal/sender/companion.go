package sender

// Companion protocol client: the Apple TV remote-control channel the Remote
// app and Siri Remote pairing use, here for launching apps and opening deep
// links. The wire format follows pyatv's pyatv/protocols/companion (MIT); see
// docs/10-companion.md.

import (
	"bufio"
	"context"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/opack"
	"golang.org/x/crypto/chacha20poly1305"
)

// Companion frame types (pyatv protocols/companion/connection.py FrameType).
const (
	companionFramePSStart = 3 // pair-setup, first message
	companionFramePSNext  = 4 // pair-setup, later messages and all replies
	companionFramePVStart = 5 // pair-verify, first message
	companionFramePVNext  = 6 // pair-verify, later messages and all replies
	companionFrameUOPACK  = 7 // unencrypted OPACK
	companionFrameEOPACK  = 8 // encrypted OPACK
	companionFramePOPACK  = 9 // "P" OPACK (pyatv handles it like E_OPACK)
	companionHeaderLength = 4 // frame type + 24-bit big-endian payload length
	companionMaxPayload   = 1<<24 - 1
)

// Companion message types (the "_t" key).
const (
	companionMessageEvent    = 1
	companionMessageRequest  = 2
	companionMessageResponse = 3
)

// Companion message keys and values.
const (
	companionKeyIdentifier  = "_i"
	companionKeyType        = "_t"
	companionKeyContent     = "_c"
	companionKeyXID         = "_x"
	companionKeyErrorMsg    = "_em"
	companionKeyErrorCode   = "_ec"
	companionKeyPairingData = "_pd"
	// companionKeyPasswordType is sent with pair-setup messages; 1 is a PIN.
	companionKeyPasswordType = "_pwTy"
	companionPasswordTypePIN = 1
	// companionKeyAuthType is sent with pair-verify V1; pyatv sends 4.
	companionKeyAuthType   = "_auTy"
	companionAuthTypeValue = 4

	companionCommandSystemInfo   = "_systemInfo"
	companionCommandSessionStart = "_sessionStart"
	companionCommandSessionStop  = "_sessionStop"
	companionCommandLaunchApp    = "_launchApp"
	companionCommandAppList      = "FetchLaunchableApplicationsEvent"
	companionKeyLaunchURL        = "_urlS"
	companionKeyLaunchBundleID   = "_bundleID"
	companionKeySessionID        = "_sid"
	companionKeyServiceType      = "_srvT"
	companionServiceTVRemote     = "com.apple.tvremoteservices"
)

// Session keys: HKDF-SHA512 of the pair-verify secret with an empty salt.
const (
	companionClientEncryptInfo = "ClientEncrypt-main"
	companionServerEncryptInfo = "ServerEncrypt-main"
)

// Values pyatv sends in _systemInfo (api.py system_info); their meaning is
// not documented.
const (
	companionSystemInfoModel   = "iPhone10,6"
	companionSystemInfoVersion = "170.18"
)

// companionExchangeTimeout bounds one request/response exchange when the
// context has no earlier deadline (pyatv DEFAULT_TIMEOUT).
const companionExchangeTimeout = 5 * time.Second

// CompanionCredentials is a Companion pairing: our controller identity and,
// when pair-setup proved it, the Apple TV's.
type CompanionCredentials struct {
	PairingID     string `json:"pairing_id"`
	Ed25519Public []byte `json:"ed25519_public"`
	Ed25519Seed   []byte `json:"ed25519_seed"`
	// ReceiverID and ReceiverPublic are the Apple TV's pairing identity from
	// pair-setup M6. When set, pair-verify checks the Apple TV's signature.
	ReceiverID     string `json:"receiver_id,omitempty"`
	ReceiverPublic []byte `json:"receiver_public,omitempty"`
}

// Valid reports whether c holds a usable controller identity.
func (c *CompanionCredentials) Valid() bool {
	return c != nil && c.PairingID != "" &&
		len(c.Ed25519Public) == ed25519.PublicKeySize &&
		len(c.Ed25519Seed) == ed25519.SeedSize
}

func (c *CompanionCredentials) privateKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(c.Ed25519Seed)
}

// CompanionError is an error reply to a Companion request.
type CompanionError struct {
	Command string
	Code    int64
	Message string
}

func (e *CompanionError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("companion %s failed (code %d)", e.Command, e.Code)
	}
	return fmt.Sprintf("companion %s failed: %s", e.Command, e.Message)
}

// CompanionClient is one connection to a Companion service.
type CompanionClient struct {
	conn   net.Conn
	reader *bufio.Reader

	// Set once pair-verify completes. Nonces are 96-bit little-endian
	// counters per direction.
	writeAEAD, readAEAD   cipher.AEAD
	writeNonce, readNonce uint64

	xid uint64
	// localSID and remoteSID identify the _sessionStart session, if any.
	localSID, remoteSID uint64
	sessionStarted      bool
}

// DialCompanion connects to the Companion service at addr ("host:port").
func DialCompanion(ctx context.Context, addr string) (*CompanionClient, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial companion %s: %w", addr, err)
	}
	xid, err := rand.Int(rand.Reader, big.NewInt(1<<16))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("companion transaction ID: %w", err)
	}
	return &CompanionClient{conn: conn, reader: bufio.NewReader(conn), xid: xid.Uint64()}, nil
}

// Close ends the session, if one was started, and closes the connection.
func (c *CompanionClient) Close() error {
	if c.sessionStarted {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := c.StopSession(ctx); err != nil {
			dbg("[COMPANION] %v", err)
		}
		cancel()
	}
	return c.conn.Close()
}

// CompanionPairing is a pair-setup in progress: the Apple TV is showing a PIN.
type CompanionPairing struct {
	client *CompanionClient
	salt   []byte
	public []byte
}

// BeginPairing starts pair-setup, which makes the Apple TV display a PIN.
func (c *CompanionClient) BeginPairing(ctx context.Context) (*CompanionPairing, error) {
	m1 := tlv8EncodeOrdered([]tlv8Item{
		{Tag: tlvMethod, Value: []byte{0x00}},
		{Tag: tlvState, Value: []byte{0x01}},
	})
	m2, err := exchangePairSetupM1(ctx, func() ([]byte, error) {
		return c.exchangeAuth(ctx, companionFramePSStart, map[string]any{
			companionKeyPairingData:  m1,
			companionKeyPasswordType: companionPasswordTypePIN,
		})
	})
	if err != nil {
		return nil, err
	}
	salt, public := m2[tlvSalt], m2[tlvPublicKey]
	if salt == nil || public == nil {
		return nil, fmt.Errorf("M2: missing salt or public key")
	}
	return &CompanionPairing{client: c, salt: salt, public: public}, nil
}

// Finish completes pair-setup with the PIN the Apple TV shows. name is how
// the Apple TV lists this controller.
func (p *CompanionPairing) Finish(ctx context.Context, pin, name string) (*CompanionCredentials, error) {
	c := p.client
	srp, err := newSRPClientSession(pin, p.salt, p.public)
	if err != nil {
		return nil, err
	}
	m4, err := c.exchangeAuth(ctx, companionFramePSNext, map[string]any{
		companionKeyPairingData:  srp.m3(),
		companionKeyPasswordType: companionPasswordTypePIN,
	})
	if err != nil {
		return nil, fmt.Errorf("M3: %w", err)
	}
	if err := srp.checkM4(tlv8Decode(m4)); err != nil {
		return nil, err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate Ed25519 key: %w", err)
	}
	creds := &CompanionCredentials{
		PairingID:     generateUUID(),
		Ed25519Public: pub,
		Ed25519Seed:   priv.Seed(),
	}
	var extra []tlv8Item
	if name != "" {
		nameData, err := opack.Marshal(map[string]any{"name": name})
		if err != nil {
			return nil, err
		}
		extra = append(extra, tlv8Item{Tag: tlvName, Value: nameData})
	}
	m5, err := srp.m5([]byte(creds.PairingID), pub, priv, extra)
	if err != nil {
		return nil, err
	}
	m6, err := c.exchangeAuth(ctx, companionFramePSNext, map[string]any{
		companionKeyPairingData:  m5,
		companionKeyPasswordType: companionPasswordTypePIN,
	})
	if err != nil {
		return nil, fmt.Errorf("M5: %w", err)
	}
	m6TLV := tlv8Decode(m6)
	if errTLV, ok := m6TLV[tlvError]; ok && len(errTLV) > 0 {
		return nil, fmt.Errorf("pair-setup M6 error: %d", errTLV[0])
	}
	// pyatv does not check the Apple TV's M6 signature. Keep its identity only
	// when it verifies, so pair-verify never rejects a receiver whose M6 we
	// merely failed to understand.
	if id, key, err := srp.accessoryIdentity(m6TLV); err != nil {
		dbg("[COMPANION] not keeping the Apple TV's pairing identity: %v", err)
	} else {
		creds.ReceiverID, creds.ReceiverPublic = id, key
	}
	return creds, nil
}

// Verify runs pair-verify with saved credentials and encrypts the connection.
func (c *CompanionClient) Verify(ctx context.Context, creds *CompanionCredentials) error {
	if !creds.Valid() {
		return errors.New("companion credentials contain no usable pairing identity")
	}
	verify, err := newHAPVerifySession()
	if err != nil {
		return err
	}
	v2, err := c.exchangeAuth(ctx, companionFramePVStart, map[string]any{
		companionKeyPairingData: verify.v1(),
		companionKeyAuthType:    companionAuthTypeValue,
	})
	if err != nil {
		return fmt.Errorf("V1: %w", err)
	}
	var receiverKey ed25519.PublicKey
	if len(creds.ReceiverPublic) == ed25519.PublicKeySize {
		receiverKey = creds.ReceiverPublic
	}
	v3, err := verify.v3(tlv8Decode(v2), []byte(creds.PairingID), creds.privateKey(), receiverKey)
	if err != nil {
		return err
	}
	v4, err := c.exchangeAuth(ctx, companionFramePVNext, map[string]any{
		companionKeyPairingData: v3,
	})
	if err != nil {
		return fmt.Errorf("V3: %w", err)
	}
	if errTLV, ok := tlv8Decode(v4)[tlvError]; ok && len(errTLV) > 0 {
		return fmt.Errorf("pair-verify V4 error: %d", errTLV[0])
	}

	writeKey := hkdfSHA512(verify.shared, nil, []byte(companionClientEncryptInfo), chacha20poly1305.KeySize)
	readKey := hkdfSHA512(verify.shared, nil, []byte(companionServerEncryptInfo), chacha20poly1305.KeySize)
	if c.writeAEAD, err = chacha20poly1305.New(writeKey); err != nil {
		return err
	}
	if c.readAEAD, err = chacha20poly1305.New(readKey); err != nil {
		return err
	}
	c.writeNonce, c.readNonce = 0, 0
	dbg("[COMPANION] pair-verify complete; connection encrypted")
	return nil
}

// CompanionSystemInfo describes this controller in _systemInfo.
type CompanionSystemInfo struct {
	Name string
	// PairingID is the controller's Companion pairing identifier.
	PairingID string
}

// StartSession announces this controller and opens a TV remote session, as
// pyatv does before sending commands.
func (c *CompanionClient) StartSession(ctx context.Context, info CompanionSystemInfo) error {
	// pyatv sends a random stable "_i" (rp_id) and a MAC-like "_pubID"; derive
	// both from the pairing ID so they stay stable without more state.
	id := sha256.Sum256([]byte(info.PairingID))
	rpID := fmt.Sprintf("%x", id[:6])
	pubID := strings.ToUpper(fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", id[6], id[7], id[8], id[9], id[10], id[11]))
	if _, err := c.Request(ctx, companionCommandSystemInfo, map[string]any{
		"_bf":    0,
		"_cf":    512,
		"_clFl":  128,
		"_i":     rpID,
		"_idsID": info.PairingID,
		"_pubID": pubID,
		"_sf":    256,
		"_sv":    companionSystemInfoVersion,
		"model":  companionSystemInfoModel,
		"name":   info.Name,
	}); err != nil {
		return err
	}

	sid, err := rand.Int(rand.Reader, big.NewInt(1<<32))
	if err != nil {
		return fmt.Errorf("companion session ID: %w", err)
	}
	c.localSID = sid.Uint64()
	resp, err := c.Request(ctx, companionCommandSessionStart, map[string]any{
		companionKeyServiceType: companionServiceTVRemote,
		companionKeySessionID:   c.localSID,
	})
	if err != nil {
		return err
	}
	remote, ok := asUint64(resp[companionKeySessionID])
	if !ok {
		return fmt.Errorf("companion %s: reply has no session ID", companionCommandSessionStart)
	}
	c.remoteSID = remote
	c.sessionStarted = true
	dbg("[COMPANION] session 0x%X started", c.remoteSID<<32|c.localSID)
	return nil
}

// StopSession ends the session StartSession opened.
func (c *CompanionClient) StopSession(ctx context.Context) error {
	if !c.sessionStarted {
		return nil
	}
	c.sessionStarted = false
	_, err := c.Request(ctx, companionCommandSessionStop, map[string]any{
		companionKeyServiceType: companionServiceTVRemote,
		companionKeySessionID:   c.remoteSID<<32 | c.localSID,
	})
	return err
}

// LaunchApp opens target on the Apple TV: a URL (deep link or universal
// link) is handed to the app that claims it, anything else is taken as an
// app bundle ID.
func (c *CompanionClient) LaunchApp(ctx context.Context, target string) error {
	key := companionKeyLaunchBundleID
	if u, err := url.Parse(target); err == nil && u.Scheme != "" {
		key = companionKeyLaunchURL
	}
	_, err := c.Request(ctx, companionCommandLaunchApp, map[string]any{key: target})
	return err
}

// Apps returns the Apple TV's launchable apps, bundle ID to name.
func (c *CompanionClient) Apps(ctx context.Context) (map[string]string, error) {
	resp, err := c.Request(ctx, companionCommandAppList, map[string]any{})
	if err != nil {
		return nil, err
	}
	apps := make(map[string]string, len(resp))
	for id, name := range resp {
		if s, ok := name.(string); ok {
			apps[id] = s
		}
	}
	return apps, nil
}

// Request sends a command and returns the content of its reply.
func (c *CompanionClient) Request(ctx context.Context, command string, content map[string]any) (map[string]any, error) {
	if c.writeAEAD == nil {
		return nil, errors.New("companion connection is not verified")
	}
	stop := c.watch(ctx)
	defer stop()

	xid := c.nextXID()
	if err := c.writeMessage(companionFrameEOPACK, map[string]any{
		companionKeyIdentifier: command,
		companionKeyType:       companionMessageRequest,
		companionKeyContent:    content,
		companionKeyXID:        xid,
	}); err != nil {
		return nil, fmt.Errorf("companion %s: %w", command, err)
	}
	for {
		frameType, message, err := c.readMessage()
		if err != nil {
			return nil, fmt.Errorf("companion %s: %w", command, companionContextError(ctx, err))
		}
		if frameType != companionFrameEOPACK && frameType != companionFrameUOPACK && frameType != companionFramePOPACK {
			dbg("[COMPANION] ignoring frame type %d", frameType)
			continue
		}
		if t, _ := asUint64(message[companionKeyType]); t != companionMessageResponse {
			dbg("[COMPANION] ignoring message %v (%v)", message[companionKeyIdentifier], message[companionKeyType])
			continue
		}
		if x, _ := asUint64(message[companionKeyXID]); x != xid {
			dbg("[COMPANION] ignoring reply to transaction %v", message[companionKeyXID])
			continue
		}
		if err := companionReplyError(command, message); err != nil {
			return nil, err
		}
		reply, _ := message[companionKeyContent].(map[string]any)
		if reply == nil {
			reply = map[string]any{}
		}
		return reply, nil
	}
}

func companionReplyError(command string, message map[string]any) error {
	msg, hasMsg := message[companionKeyErrorMsg].(string)
	code, hasCode := asUint64(message[companionKeyErrorCode])
	if !hasMsg && (!hasCode || code == 0) {
		return nil
	}
	return &CompanionError{Command: command, Code: int64(code), Message: msg}
}

// exchangeAuth sends a pair-setup or pair-verify message and returns the
// TLV8 pairing data of the reply.
func (c *CompanionClient) exchangeAuth(ctx context.Context, frameType byte, message map[string]any) ([]byte, error) {
	stop := c.watch(ctx)
	defer stop()

	message[companionKeyXID] = c.nextXID()
	if err := c.writeMessage(frameType, message); err != nil {
		return nil, err
	}
	// Replies to *_Start come as *_Next.
	want := byte(companionFramePSNext)
	if frameType == companionFramePVStart || frameType == companionFramePVNext {
		want = companionFramePVNext
	}
	for {
		gotType, reply, err := c.readMessage()
		if err != nil {
			return nil, companionContextError(ctx, err)
		}
		if gotType != want {
			dbg("[COMPANION] ignoring frame type %d during pairing", gotType)
			continue
		}
		if err := companionReplyError("pairing", reply); err != nil {
			return nil, err
		}
		data, ok := reply[companionKeyPairingData].([]byte)
		if !ok {
			return nil, errors.New("reply has no pairing data")
		}
		return data, nil
	}
}

func (c *CompanionClient) nextXID() uint64 {
	x := c.xid
	c.xid++
	return x
}

// watch applies ctx to the connection until the returned function is called:
// its deadline (or companionExchangeTimeout) and its cancellation.
func (c *CompanionClient) watch(ctx context.Context) func() {
	deadline := time.Now().Add(companionExchangeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Now()) })
	return func() {
		stop()
		_ = c.conn.SetDeadline(time.Time{})
	}
}

func companionContextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (c *CompanionClient) writeMessage(frameType byte, message map[string]any) error {
	dbg("[COMPANION] >> frame %d: %v", frameType, message)
	payload, err := opack.Marshal(message)
	if err != nil {
		return err
	}
	frame, err := sealCompanionFrame(c.writeAEAD, &c.writeNonce, frameType, payload)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(frame)
	return err
}

func (c *CompanionClient) readMessage() (byte, map[string]any, error) {
	frameType, payload, err := readCompanionFrame(c.reader, c.readAEAD, &c.readNonce)
	if err != nil {
		return 0, nil, err
	}
	if len(payload) == 0 {
		return frameType, map[string]any{}, nil
	}
	value, err := opack.Unmarshal(payload)
	if err != nil {
		return 0, nil, err
	}
	message, ok := value.(map[string]any)
	if !ok {
		return 0, nil, fmt.Errorf("companion frame holds %T, not a dictionary", value)
	}
	dbg("[COMPANION] << frame %d: %v", frameType, message)
	return frameType, message, nil
}

// sealCompanionFrame builds a frame; with aead set, a non-empty payload is
// encrypted with the header (whose length counts the tag) as associated data.
func sealCompanionFrame(aead cipher.AEAD, nonce *uint64, frameType byte, payload []byte) ([]byte, error) {
	length := len(payload)
	if aead != nil && length > 0 {
		length += aead.Overhead()
	}
	if length > companionMaxPayload {
		return nil, fmt.Errorf("companion frame of %d bytes is too large", length)
	}
	header := []byte{frameType, byte(length >> 16), byte(length >> 8), byte(length)}
	if aead == nil || len(payload) == 0 {
		return append(header, payload...), nil
	}
	sealed := aead.Seal(header, companionNonce(*nonce), payload, header)
	*nonce++
	return sealed, nil
}

func readCompanionFrame(r io.Reader, aead cipher.AEAD, nonce *uint64) (byte, []byte, error) {
	header := make([]byte, companionHeaderLength)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}
	length := int(header[1])<<16 | int(header[2])<<8 | int(header[3])
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if aead == nil || length == 0 {
		return header[0], payload, nil
	}
	plain, err := aead.Open(payload[:0], companionNonce(*nonce), payload, header)
	if err != nil {
		return 0, nil, fmt.Errorf("decrypt companion frame: %w", err)
	}
	*nonce++
	return header[0], plain, nil
}

// companionNonce is the 96-bit little-endian frame counter (pyatv
// Chacha20Cipher with nonce_length=12), unlike HAP's counter at offset 4.
func companionNonce(n uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.LittleEndian.PutUint64(nonce, n)
	return nonce
}

func asUint64(v any) (uint64, bool) {
	switch v := v.(type) {
	case int64:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	case uint64:
		return v, true
	}
	return 0, false
}
