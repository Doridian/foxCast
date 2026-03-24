// Package fairplay implements the FairPlay v3 fp-setup handshake required by
// AirPlay 2 receivers before HAP pair-setup and pair-verify.
package fairplay

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"

	"git.foxden.network/FoxDen/foxCast/internal/hap"
)

const (
	fpVersion = 0x03
	fpType    = 0x01 // setup

	fpSeqRound1 = 0x01
	fpSeqRound2 = 0x03

	fpMode = 0x00 // use mode 0; selects which static 142-byte server reply is returned

	// round1PayloadLen is the 4-byte payload after the 12-byte FPLY header.
	round1PayloadLen = 4
	// round2PayloadLen is 152 bytes (128 cipher + 20 nonce + 4 tag bytes).
	round2PayloadLen = 152
	// round2NonceLen is the nonce appended at the end of the round 2 message.
	round2NonceLen = 20
	// round2CipherLen is the opaque cipher payload in the round 2 message.
	round2CipherLen = 128
)

// FPSetup performs the two-round FairPlay v3 fp-setup handshake against addr
// using sess. This must be called on the plaintext session before HAP
// pair-setup or pair-verify. If the device does not require FairPlay (i.e.
// /fp-setup returns a non-200 status), FPSetup returns nil and is a no-op.
func FPSetup(sess *hap.Session, addr string) error {
	// Round 1: send 16-byte challenge, receive 142-byte static reply.
	round1 := buildRound1()
	resp1, err := doFP(sess, addr, round1)
	if err == ErrNotSupported {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fp-setup round1: %w", err)
	}
	if len(resp1) != 142 {
		return fmt.Errorf("fp-setup round1: unexpected response length %d (want 142)", len(resp1))
	}

	// Round 2: send 164-byte message, receive 32-byte echo response.
	nonce, round2, err := buildRound2()
	if err != nil {
		return fmt.Errorf("fp-setup round2 build: %w", err)
	}
	resp2, err := doFP(sess, addr, round2)
	if err != nil {
		return fmt.Errorf("fp-setup round2: %w", err)
	}
	// Server responds with a 12-byte FPLY header followed by the 20-byte nonce echo.
	if len(resp2) < 12+round2NonceLen {
		return fmt.Errorf("fp-setup round2: response too short (%d bytes)", len(resp2))
	}
	if !bytes.Equal(resp2[12:12+round2NonceLen], nonce) {
		return fmt.Errorf("fp-setup round2: nonce echo mismatch")
	}
	return nil
}

// buildRound1 constructs the 16-byte fp-setup round 1 challenge message.
//
// Frame layout:
//
//	[0-3]  FPLY magic
//	[4]    version (0x03)
//	[5]    type (0x01 = setup)
//	[6]    sequence (0x01 = round 1)
//	[7-10] zeros
//	[11]   payload length (0x04)
//	[12]   0x02 (tag byte, observed in captures)
//	[13]   0x00
//	[14]   mode (0x00–0x03)
//	[15]   0x00
func buildRound1() []byte {
	msg := make([]byte, 12+round1PayloadLen)
	copy(msg[0:4], []byte{'F', 'P', 'L', 'Y'})
	msg[4] = fpVersion
	msg[5] = fpType
	msg[6] = fpSeqRound1
	// [7-10] already zero
	msg[11] = round1PayloadLen
	msg[12] = 0x02
	msg[13] = 0x00
	msg[14] = fpMode
	msg[15] = 0x00
	return msg
}

// buildRound2 constructs the 164-byte fp-setup round 2 message.
// Returns the 20-byte nonce (for echo verification) and the full message.
//
// Frame layout:
//
//	[0-3]    FPLY magic
//	[4]      version (0x03)
//	[5]      type (0x01 = setup)
//	[6]      sequence (0x03 = round 2)
//	[7-10]   zeros
//	[11]     payload length (0x98 = 152)
//	[12]     mode (same as round 1)
//	[13-15]  0x8F 0x1A 0x9C (fixed tag bytes observed in captures)
//	[16-143] 128-byte cipher payload (zeros; session key not needed for video)
//	[144-163] 20-byte random nonce (echoed back by server)
func buildRound2() (nonce []byte, msg []byte, err error) {
	msg = make([]byte, 12+round2PayloadLen)
	copy(msg[0:4], []byte{'F', 'P', 'L', 'Y'})
	msg[4] = fpVersion
	msg[5] = fpType
	msg[6] = fpSeqRound2
	// [7-10] already zero
	msg[11] = round2PayloadLen
	msg[12] = fpMode
	msg[13] = 0x8F
	msg[14] = 0x1A
	msg[15] = 0x9C
	// [16-143] cipher payload — random bytes
	// [144-163] random nonce (echoed back by server for verification)
	if _, err = io.ReadFull(rand.Reader, msg[16:164]); err != nil {
		return nil, nil, fmt.Errorf("fp-setup round2: generate random payload: %w", err)
	}
	nonce = msg[144:164]
	return nonce, msg, nil
}

// doFP sends a POST to /fp-setup with the given raw body.
// Returns (body, nil) on HTTP 200, or (nil, error) otherwise.
// A 404 response is treated as ErrNotSupported.
func doFP(sess *hap.Session, addr string, body []byte) ([]byte, error) {
	req, err := http.NewRequest("POST", "http://"+addr+"/fp-setup", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("User-Agent", "AirPlay/550.10")
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	resp, respBody, err := sess.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 404 {
		return nil, ErrNotSupported
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return respBody, nil
}

// ErrNotSupported is returned by FPSetup when the device does not expose /fp-setup.
var ErrNotSupported = fmt.Errorf("fp-setup not supported by device")
