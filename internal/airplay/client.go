// Package airplay implements the AirPlay video URL playback control protocol.
//
// After HAP pair-verify, commands are sent over an HTTP/1.1 connection that is
// transparently encrypted by the hap.Session. The receiver fetches and plays
// the URL itself; no media data is streamed through the sender.
package airplay

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"git.foxden.network/FoxDen/foxCast/internal/hap"
	"git.foxden.network/FoxDen/foxCast/internal/mdns"
	"howett.net/plist"
)

const userAgent = "AirPlay/550.10"

// InfoResponse holds the parsed response body of GET /info.
type InfoResponse struct {
	DeviceID        string `plist:"deviceID"`
	Name            string `plist:"name"`
	Model           string `plist:"model"`
	Features        uint64 `plist:"features"`
	StatusFlags     uint32 `plist:"statusFlags"`
	ProtocolVersion string `plist:"protocolVersion"`
	SourceVersion   string `plist:"sourceVersion"`
}

// Client manages a single AirPlay connection to a receiver.
// It handles pairing (if needed), pair-verify on every connect, and
// the video URL playback control commands (play, stop, rate, scrub).
type Client struct {
	dev       *mdns.Device
	sess      *hap.Session
	eventSess *hap.Session // separate encrypted connection for the PTTH /reverse event channel
	sessionID string       // X-Apple-Session-ID UUID, consistent across the session
	password  string       // AirPlay device password for HTTP Digest auth (empty if none)
}

// Connect dials the receiver, performs pair-setup if no credentials are saved,
// then runs pair-verify to establish the encrypted session.
func Connect(dev *mdns.Device, pin string) (*Client, error) {
	conn, err := net.Dial("tcp", dev.Addr())
	if err != nil {
		return nil, fmt.Errorf("airplay: dial %s: %w", dev.Addr(), err)
	}
	sess := hap.NewSession(conn)
	sessionID := newSessionID()
	client := &Client{dev: dev, sess: sess, sessionID: sessionID}

	// If we do not have feature flags yet, fetch /info on the plaintext connection.
	if dev.Features == 0 {
		if info, err := client.GetInfo(); err == nil {
			dev.Features = mdns.FeatureFlags(info.Features)
			if dev.DeviceID == dev.Addr() && info.DeviceID != "" {
				dev.DeviceID = info.DeviceID
			}
		}
	}

	// Standard HAP pairing.
	creds, err := hap.LoadCredentials(dev.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("airplay: load credentials: %w", err)
	}

	if creds == nil {
		// First time connecting: run pair-setup.
		creds, err = hap.PairSetup(sess, dev.Addr(), dev.DeviceID, dev.PublicKey, pin)
		if err != nil {
			sess.Close()
			return nil, fmt.Errorf("airplay: pair-setup: %w", err)
		}
	}

	if err := hap.PairVerify(sess, dev.Addr(), creds); err != nil {
		sess.Close()
		return nil, fmt.Errorf("airplay: pair-verify: %w", err)
	}

	// Set up the reverse (PTTH) event channel on a separate TCP connection.
	// The protocol requires two connections: the main command connection (sess)
	// and an event connection that transitions to a receiver-driven PTTH channel
	// after POST /reverse. They cannot share the same socket.
	eventSess, err := dialEventSession(dev, creds, sessionID, creds.Password)
	if err != nil {
		sess.Close()
		return nil, err
	}

	go runEventLoop(eventSess)

	client.eventSess = eventSess
	client.password = creds.Password
	return client, nil
}

// runEventLoop reads POST /event requests sent by the receiver over the reversed
// PTTH connection and acknowledges each one with HTTP/1.1 200 OK. Without these
// acknowledgements the receiver stalls in the loading state indefinitely.
func runEventLoop(sess *hap.Session) {
	ok200 := []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	for {
		req, err := http.ReadRequest(sess.Reader())
		if err != nil {
			return
		}
		io.Copy(io.Discard, req.Body)
		req.Body.Close()
		if _, err := sess.Write(ok200); err != nil {
			return
		}
	}
}

// dialEventSession opens a second encrypted connection to the receiver, runs
// pair-verify, then sends POST /reverse to establish the PTTH event channel.
func dialEventSession(dev *mdns.Device, creds *hap.Credentials, sessionID, password string) (*hap.Session, error) {
	conn, err := net.Dial("tcp", dev.Addr())
	if err != nil {
		return nil, fmt.Errorf("airplay: event dial %s: %w", dev.Addr(), err)
	}
	eventSess := hap.NewSession(conn)

	if err := hap.PairVerify(eventSess, dev.Addr(), creds); err != nil {
		eventSess.Close()
		return nil, fmt.Errorf("airplay: event pair-verify: %w", err)
	}

	reverseReq, err := http.NewRequest("POST", "http://"+dev.Addr()+"/reverse", http.NoBody)
	if err != nil {
		eventSess.Close()
		return nil, fmt.Errorf("airplay: build /reverse request: %w", err)
	}
	reverseReq.Header.Set("User-Agent", userAgent)
	reverseReq.Header.Set("Upgrade", "PTTH/1.0")
	reverseReq.Header.Set("Connection", "Upgrade")
	reverseReq.Header.Set("X-Apple-Purpose", "event")
	reverseReq.Header.Set("X-Apple-Session-ID", sessionID)
	reverseReq.Header.Set("Content-Length", "0")

	resp, _, err := eventSess.Do(reverseReq)
	if err != nil {
		eventSess.Close()
		return nil, fmt.Errorf("airplay: /reverse: %w", err)
	}

	// Handle Digest auth challenge: parse nonce/realm and retry once with credentials.
	if resp.StatusCode == http.StatusUnauthorized {
		realm, nonce := parseDigestChallenge(resp.Header.Get("Www-Authenticate"))
		if realm == "" || nonce == "" {
			eventSess.Close()
			return nil, fmt.Errorf("airplay: /reverse: 401 without parseable Digest challenge")
		}
		reverseReq.Header.Set("Authorization", buildDigestAuth("POST", "/reverse", realm, nonce, password))
		resp, _, err = eventSess.Do(reverseReq)
		if err != nil {
			eventSess.Close()
			return nil, fmt.Errorf("airplay: /reverse (auth): %w", err)
		}
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		eventSess.Close()
		return nil, fmt.Errorf("airplay: /reverse: unexpected status %d", resp.StatusCode)
	}

	return eventSess, nil
}

// newSessionID generates a random UUID v4 for use as X-Apple-Session-ID.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("airplay: generate session ID: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// Close closes the underlying connections.
func (c *Client) Close() error {
	_ = c.eventSess.Close()
	return c.sess.Close()
}

// GetInfo fetches the receiver's capability information.
func (c *Client) GetInfo() (*InfoResponse, error) {
	_, body, err := c.doCmd("GET", "/info", "", nil)
	if err != nil {
		return nil, fmt.Errorf("airplay: GET /info: %w", err)
	}

	var info InfoResponse
	if _, err := plist.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("airplay: parse /info response: %w", err)
	}
	return &info, nil
}

// Play instructs the receiver to fetch and play url.
// startPos is a normalized position from 0.0 (start) to 1.0 (end).
func (c *Client) Play(url string, startPos float64) error {
	payload := map[string]interface{}{
		"Content-Location": url,
		"Start-Position":   startPos,
	}
	body, err := plist.Marshal(payload, plist.BinaryFormat)
	if err != nil {
		return fmt.Errorf("airplay: marshal /play body: %w", err)
	}

	_, _, err = c.doCmd("POST", "/play", "application/x-apple-binary-plist", body)
	return err
}

// Stop instructs the receiver to stop playback.
func (c *Client) Stop() error {
	_, _, err := c.doCmd("POST", "/stop", "", nil)
	return err
}

// Rate sets the playback rate. Use 0.0 to pause and 1.0 to play.
func (c *Client) Rate(value float64) error {
	path := fmt.Sprintf("/rate?value=%f", value)
	_, _, err := c.doCmd("POST", path, "", nil)
	return err
}

// Scrub seeks to position seconds from the start.
func (c *Client) Scrub(position float64) error {
	path := fmt.Sprintf("/scrub?position=%f", position)
	_, _, err := c.doCmd("POST", path, "", nil)
	return err
}

// PlaybackInfoResponse holds the parsed response body of GET /playback-info.
type PlaybackInfoResponse struct {
	Duration    float64 `plist:"duration"`
	Position    float64 `plist:"position"`
	Rate        float64 `plist:"rate"`
	ReadyToPlay bool    `plist:"readyToPlay"`
}

// PlaybackInfo queries current playback state from the receiver.
func (c *Client) PlaybackInfo() (*PlaybackInfoResponse, error) {
	_, body, err := c.doCmd("GET", "/playback-info", "", nil)
	if err != nil {
		return nil, fmt.Errorf("airplay: GET /playback-info: %w", err)
	}

	var info PlaybackInfoResponse
	if _, err := plist.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("airplay: parse /playback-info: %w", err)
	}
	return &info, nil
}

// doCmd sends an HTTP command to the receiver and returns the response.
// If the receiver responds with a Digest auth challenge, it retries once with credentials.
func (c *Client) doCmd(method, path, contentType string, body []byte) (*http.Response, []byte, error) {
	buildReq := func(authHeader string) (*http.Request, error) {
		var bodyReader io.Reader
		if len(body) > 0 {
			bodyReader = bytes.NewReader(body)
		} else {
			bodyReader = http.NoBody
		}
		req, err := http.NewRequest(method, "http://"+c.dev.Addr()+path, bodyReader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("X-Apple-Session-ID", c.sessionID)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		return req, nil
	}

	req, err := buildReq("")
	if err != nil {
		return nil, nil, err
	}
	resp, respBody, err := c.sess.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("airplay: %s %s: %w", method, path, err)
	}

	// Handle Digest auth challenge: parse nonce/realm and retry once with credentials.
	if resp.StatusCode == http.StatusUnauthorized {
		realm, nonce := parseDigestChallenge(resp.Header.Get("Www-Authenticate"))
		if realm != "" && nonce != "" {
			req, err = buildReq(buildDigestAuth(method, path, realm, nonce, c.password))
			if err != nil {
				return nil, nil, err
			}
			resp, respBody, err = c.sess.Do(req)
			if err != nil {
				return nil, nil, fmt.Errorf("airplay: %s %s (auth): %w", method, path, err)
			}
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("airplay: %s %s: unexpected status %d", method, path, resp.StatusCode)
	}
	return resp, respBody, nil
}

// parseDigestChallenge extracts realm and nonce from a WWW-Authenticate: Digest header value.
func parseDigestChallenge(challenge string) (realm, nonce string) {
	challenge = strings.TrimPrefix(challenge, "Digest ")
	for part := range strings.SplitSeq(challenge, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.TrimSpace(k) {
		case "realm":
			realm = v
		case "nonce":
			nonce = v
		}
	}
	return
}

// buildDigestAuth computes an Authorization header value for HTTP Digest auth (RFC 2617, no qop).
// AirPlay uses username "airplay" and the device password (empty if none is set).
func buildDigestAuth(method, uri, realm, nonce, password string) string {
	ha1 := md5Hex("airplay:" + realm + ":" + password)
	ha2 := md5Hex(method + ":" + uri)
	response := md5Hex(ha1 + ":" + nonce + ":" + ha2)
	return fmt.Sprintf(`Digest username="airplay", realm="%s", nonce="%s", uri="%s", response="%s"`,
		realm, nonce, uri, response)
}

func md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}
