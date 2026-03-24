// Package airplay implements the AirPlay video URL playback control protocol.
//
// After HAP pair-verify, commands are sent over an HTTP/1.1 connection that is
// transparently encrypted by the hap.Session. The receiver fetches and plays
// the URL itself; no media data is streamed through the sender.
package airplay

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"

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
	dev  *mdns.Device
	sess *hap.Session
}

// Connect dials the receiver, performs pair-setup if no credentials are saved,
// then runs pair-verify to establish the encrypted session.
//
// If dev.Features is zero (e.g. a manually specified host:port), Connect
// fetches /info first to discover the receiver's capabilities.
func Connect(dev *mdns.Device, pin string) (*Client, error) {
	conn, err := net.Dial("tcp", dev.Addr())
	if err != nil {
		return nil, fmt.Errorf("airplay: dial %s: %w", dev.Addr(), err)
	}
	sess := hap.NewSession(conn)

	// If we don't have feature flags yet, fetch /info on the plaintext connection.
	if dev.Features == 0 {
		if info, err := fetchInfoUnencrypted(sess, dev.Addr()); err == nil {
			dev.Features = info.Features
			if dev.DeviceID == dev.Addr() && info.DeviceID != "" {
				dev.DeviceID = info.DeviceID
			}
		}
	}

	// Transient pairing: no credentials stored; session keys come from SRP.
	if dev.SupportsTransientPairing() {
		if err := hap.PairSetupTransient(sess, dev.Addr()); err != nil {
			sess.Close()
			return nil, fmt.Errorf("airplay: transient pair-setup: %w", err)
		}
		return &Client{dev: dev, sess: sess}, nil
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

	return &Client{dev: dev, sess: sess}, nil
}

// fetchInfoUnencrypted does a plaintext GET /info before pairing to discover
// the receiver's feature flags and device ID.
func fetchInfoUnencrypted(sess *hap.Session, addr string) (*infoResponse, error) {
	req, err := http.NewRequest("GET", "http://"+addr+"/info", http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	_, body, err := sess.Do(req)
	if err != nil {
		return nil, err
	}
	var info infoResponse
	if _, err := plist.Unmarshal(body, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// infoResponse holds just the fields from /info we need before pairing.
type infoResponse struct {
	DeviceID string               `plist:"deviceID"`
	Features mdns.FeatureFlags    `plist:"features"`
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	return c.sess.Close()
}

// GetInfo fetches the receiver's capability information.
func (c *Client) GetInfo() (*InfoResponse, error) {
	req, err := http.NewRequest("GET", "http://"+c.dev.Addr()+"/info", http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	_, body, err := c.sess.Do(req)
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
	Duration   float64 `plist:"duration"`
	Position   float64 `plist:"position"`
	Rate       float64 `plist:"rate"`
	ReadyToPlay bool   `plist:"readyToPlay"`
}

// PlaybackInfo queries current playback state from the receiver.
func (c *Client) PlaybackInfo() (*PlaybackInfoResponse, error) {
	req, err := http.NewRequest("GET", "http://"+c.dev.Addr()+"/playback-info", http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	_, body, err := c.sess.Do(req)
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
func (c *Client) doCmd(method, path, contentType string, body []byte) (*http.Response, []byte, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	} else {
		bodyReader = http.NoBody
	}

	req, err := http.NewRequest(method, "http://"+c.dev.Addr()+path, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, respBody, err := c.sess.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("airplay: %s %s: %w", method, path, err)
	}
	return resp, respBody, nil
}
