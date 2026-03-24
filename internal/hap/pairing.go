package hap

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"

	"git.foxden.network/FoxDen/foxCast/internal/tlv8"
	"golang.org/x/crypto/curve25519"
)

const (
	userAgent = "AirPlay/550.10"

	// methodPairSetup is the HAP pair-setup method byte.
	methodPairSetup = uint8(0x00)
)

// PairSetup runs the full HAP pair-setup M1–M6 exchange against the receiver
// at sess. pin is the numeric PIN string displayed on the receiver (e.g. "1234").
// deviceID is the receiver's MAC address used as the credential store key.
// deviceLTPK is the receiver's Ed25519 public key from the mDNS TXT record.
//
// On success, credentials are saved to disk and returned.
func PairSetup(sess *Session, addr, deviceID string, deviceLTPK []byte, pin string) (*Credentials, error) {
	// Trigger PIN display on receiver.
	req, _ := http.NewRequest("POST", "http://"+addr+"/pair-pin-start", http.NoBody)
	req.Header.Set("User-Agent", userAgent)
	req.Header["X-Apple-HKP"] = []string{"3"}
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Content-Length", "0")
	if _, _, err := sess.Do(req); err != nil {
		// Non-fatal: some receivers don't implement this endpoint.
		_ = err
	}

	// --- M1: Send Method + State ---
	m1 := tlv8.EncodeOrdered([]tlv8.Record{
		{Tag: TLVMethod, Value: []byte{methodPairSetup}},
		{Tag: TLVState, Value: []byte{0x01}},
	})
	resp, body, err := doTLV8(sess, addr, "/pair-setup", m1)
	if err != nil {
		return nil, fmt.Errorf("pair-setup M1: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("pair-setup M1: HTTP %d body=%x", resp.StatusCode, body)
	}

	// --- Parse M2: Salt + ServerPublicKey ---
	m2, err := tlv8.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("pair-setup M2 decode: %w", err)
	}
	if errCode, ok := m2[TLVError]; ok {
		return nil, fmt.Errorf("pair-setup M2 error: %d", errCode)
	}
	salt := m2[TLVSalt]
	serverB := m2[TLVPublicKey]
	if len(salt) == 0 || len(serverB) == 0 {
		return nil, fmt.Errorf("pair-setup M2: missing salt or server public key")
	}

	// --- M3: SRP client proof ---
	srp := NewSRPClient("Pair-Setup", pin, nil)
	if err := srp.Process(salt, serverB); err != nil {
		return nil, fmt.Errorf("pair-setup SRP: %w", err)
	}
	m3 := tlv8.EncodeOrdered([]tlv8.Record{
		{Tag: TLVState, Value: []byte{0x03}},
		{Tag: TLVPublicKey, Value: srp.PublicKeyBytes()},
		{Tag: TLVProof, Value: srp.Proof()},
	})
	_, body, err = doTLV8(sess, addr, "/pair-setup", m3)
	if err != nil {
		return nil, fmt.Errorf("pair-setup M3: %w", err)
	}

	// --- Parse M4: Verify server proof ---
	m4, err := tlv8.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("pair-setup M4 decode: %w", err)
	}
	if errCode, ok := m4[TLVError]; ok {
		return nil, fmt.Errorf("pair-setup M4 error: %d (wrong PIN?)", errCode)
	}
	if !srp.VerifyServerProof(m4[TLVProof]) {
		return nil, fmt.Errorf("pair-setup M4: server proof verification failed")
	}

	// Derive the session encryption key for M5/M6.
	sessionKey := DeriveKey(srp.SessionKey(), "Pair-Setup-Encrypt-Salt", "Pair-Setup-Encrypt-Info")

	// Generate our long-term Ed25519 key pair and pairing ID.
	clientID, clientLTPK, clientLTSK, err := GenerateClientKeys()
	if err != nil {
		return nil, err
	}

	// Derive the signing material for our Ed25519 signature.
	signSalt := DeriveKey(srp.SessionKey(),
		"Pair-Setup-Controller-Sign-Salt",
		"Pair-Setup-Controller-Sign-Info",
	)
	// signMsg = HKDF_output || clientID || clientLTPK
	signMsg := append(append(signSalt, []byte(clientID)...), []byte(clientLTPK)...)
	sig := ed25519.Sign(clientLTSK, signMsg)

	// Build and encrypt the M5 inner payload.
	inner := tlv8.EncodeOrdered([]tlv8.Record{
		{Tag: TLVIdentifier, Value: []byte(clientID)},
		{Tag: TLVPublicKey, Value: clientLTPK},
		{Tag: TLVSignature, Value: sig},
	})
	encInner, err := SealMsg(sessionKey, "PS-Msg05", inner)
	if err != nil {
		return nil, fmt.Errorf("pair-setup M5 encrypt: %w", err)
	}

	// --- M5: Send encrypted payload ---
	m5 := tlv8.EncodeOrdered([]tlv8.Record{
		{Tag: TLVState, Value: []byte{0x05}},
		{Tag: TLVEncryptedData, Value: encInner},
	})
	_, body, err = doTLV8(sess, addr, "/pair-setup", m5)
	if err != nil {
		return nil, fmt.Errorf("pair-setup M5: %w", err)
	}

	// --- Parse M6: Server credentials ---
	m6, err := tlv8.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("pair-setup M6 decode: %w", err)
	}
	if errCode, ok := m6[TLVError]; ok {
		return nil, fmt.Errorf("pair-setup M6 error: %d", errCode)
	}
	encM6Data := m6[TLVEncryptedData]
	if len(encM6Data) == 0 {
		return nil, fmt.Errorf("pair-setup M6: missing encrypted data")
	}
	m6Inner, err := OpenMsg(sessionKey, "PS-Msg06", encM6Data)
	if err != nil {
		return nil, fmt.Errorf("pair-setup M6 decrypt: %w", err)
	}
	m6Fields, err := tlv8.Decode(m6Inner)
	if err != nil {
		return nil, fmt.Errorf("pair-setup M6 inner decode: %w", err)
	}

	serverID := string(m6Fields[TLVIdentifier])
	serverLTPK := m6Fields[TLVPublicKey]
	serverSig := m6Fields[TLVSignature]

	if len(serverLTPK) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pair-setup M6: invalid server LTPK length %d", len(serverLTPK))
	}

	// Verify server's signature using either the mDNS pk or the one from M6.
	// If mDNS pk was provided, use it; otherwise trust the M6 key.
	verifyKey := ed25519.PublicKey(serverLTPK)
	if len(deviceLTPK) == ed25519.PublicKeySize {
		verifyKey = ed25519.PublicKey(deviceLTPK)
	}

	serverSignSalt := DeriveKey(srp.SessionKey(),
		"Pair-Setup-Accessory-Sign-Salt",
		"Pair-Setup-Accessory-Sign-Info",
	)
	serverSignMsg := append(append(serverSignSalt, []byte(serverID)...), serverLTPK...)
	if !ed25519.Verify(verifyKey, serverSignMsg, serverSig) {
		return nil, fmt.Errorf("pair-setup M6: server signature verification failed")
	}

	creds := &Credentials{
		DeviceID:        deviceID,
		DevicePairingID: serverID,
		DeviceLTPK:      serverLTPK,
		ClientID:        clientID,
		ClientLTPK:      clientLTPK,
		ClientLTSK:      clientLTSK,
	}
	if err := SaveCredentials(creds); err != nil {
		return nil, fmt.Errorf("pair-setup: save credentials: %w", err)
	}
	return creds, nil
}

// PairVerify runs the HAP pair-verify M1–M4 exchange. On success, the session
// is upgraded to encrypted mode using the derived session keys.
func PairVerify(sess *Session, addr string, creds *Credentials) error {
	// Generate an ephemeral X25519 key pair.
	var clientScalar [32]byte
	if _, err := randFill(clientScalar[:]); err != nil {
		return fmt.Errorf("pair-verify: generate X25519 key: %w", err)
	}
	clientPub, err := curve25519.X25519(clientScalar[:], curve25519.Basepoint)
	if err != nil {
		return fmt.Errorf("pair-verify: X25519 public key: %w", err)
	}

	// --- M1: Send ephemeral public key ---
	m1 := tlv8.EncodeOrdered([]tlv8.Record{
		{Tag: TLVState, Value: []byte{0x01}},
		{Tag: TLVPublicKey, Value: clientPub},
	})
	_, body, err := doTLV8(sess, addr, "/pair-verify", m1)
	if err != nil {
		return fmt.Errorf("pair-verify M1: %w", err)
	}

	// --- Parse M2 ---
	m2, err := tlv8.Decode(body)
	if err != nil {
		return fmt.Errorf("pair-verify M2 decode: %w", err)
	}
	if errCode, ok := m2[TLVError]; ok {
		return fmt.Errorf("pair-verify M2 error: %d", errCode)
	}
	serverPub := m2[TLVPublicKey]
	encM2 := m2[TLVEncryptedData]
	if len(serverPub) != 32 {
		return fmt.Errorf("pair-verify M2: invalid server public key length %d", len(serverPub))
	}

	// Compute X25519 shared secret.
	shared, err := curve25519.X25519(clientScalar[:], serverPub)
	if err != nil {
		return fmt.Errorf("pair-verify: X25519 ECDH: %w", err)
	}

	// Derive verify key for M2/M3 inner payloads.
	verifyKey := DeriveKey(shared, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info")

	// Decrypt and verify server's M2 inner payload.
	m2Inner, err := OpenMsg(verifyKey, "PV-Msg02", encM2)
	if err != nil {
		return fmt.Errorf("pair-verify M2 decrypt: %w", err)
	}
	m2Fields, err := tlv8.Decode(m2Inner)
	if err != nil {
		return fmt.Errorf("pair-verify M2 inner decode: %w", err)
	}
	serverID := string(m2Fields[TLVIdentifier])
	serverSig := m2Fields[TLVSignature]
	if len(serverSig) == 0 {
		return fmt.Errorf("pair-verify M2: missing server signature")
	}

	// Verify server's Ed25519 signature: server_X25519_pub || serverID || client_X25519_pub
	serverSignMsg := append(append(serverPub, []byte(serverID)...), clientPub...)
	if !ed25519.Verify(ed25519.PublicKey(creds.DeviceLTPK), serverSignMsg, serverSig) {
		return fmt.Errorf("pair-verify M2: server signature verification failed")
	}

	// --- M3: Send our signed identity ---
	clientLTSK := ed25519.PrivateKey(creds.ClientLTSK)
	// clientSignMsg = client_X25519_pub || clientID || server_X25519_pub
	clientSignMsg := append(append(clientPub, []byte(creds.ClientID)...), serverPub...)
	clientSig := ed25519.Sign(clientLTSK, clientSignMsg)

	inner := tlv8.EncodeOrdered([]tlv8.Record{
		{Tag: TLVIdentifier, Value: []byte(creds.ClientID)},
		{Tag: TLVSignature, Value: clientSig},
	})
	encInner, err := SealMsg(verifyKey, "PV-Msg03", inner)
	if err != nil {
		return fmt.Errorf("pair-verify M3 encrypt: %w", err)
	}
	m3 := tlv8.EncodeOrdered([]tlv8.Record{
		{Tag: TLVState, Value: []byte{0x03}},
		{Tag: TLVEncryptedData, Value: encInner},
	})
	resp, _, err := doTLV8(sess, addr, "/pair-verify", m3)
	if err != nil {
		return fmt.Errorf("pair-verify M3: %w", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("pair-verify M3: HTTP %d", resp.StatusCode)
	}
	// M4 is an empty TLV8 {State: 0x04}; no need to parse further.

	// Derive HAP session keys from the shared X25519 secret.
	writeKey := DeriveKey(shared, "Control-Salt", "Control-Write-Encryption-Key")
	readKey := DeriveKey(shared, "Control-Salt", "Control-Read-Encryption-Key")
	sess.Upgrade(writeKey, readKey)
	return nil
}

// doTLV8 sends a POST request with a TLV8 body to path and returns the response.
func doTLV8(sess *Session, addr, path string, body []byte) (*http.Response, []byte, error) {
	req, err := http.NewRequest("POST", "http://"+addr+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("User-Agent", userAgent)
	req.Header["X-Apple-HKP"] = []string{"3"}
	req.Header.Set("Connection", "keep-alive")
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return sess.Do(req)
}
