package hap

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
)

// Session manages a TCP connection with optional HAP frame encryption.
//
// Before Upgrade is called, Read and Write pass through to the underlying conn.
// After Upgrade, Write encrypts data into ChaCha20-Poly1305 frames and Read
// decrypts incoming frames, both using the keys derived from pair-verify.
//
// A single *bufio.Reader (reader) buffers all raw conn reads. A second
// *bufio.Reader (encReader) is created by Upgrade to buffer decrypted data
// for callers of http.ReadResponse after encryption is enabled. Using
// persistent readers ensures no bytes are lost at the mode transition.
type Session struct {
	conn   net.Conn
	reader *bufio.Reader // always buffers raw conn bytes

	encrypted    bool
	writeKey     []byte
	readKey      []byte
	writeCounter uint64
	readCounter  uint64

	decBuf    bytes.Buffer  // decrypted bytes waiting to be consumed by Read
	encReader *bufio.Reader // buffers decrypted bytes after Upgrade; nil before
}

// NewSession wraps conn in a Session. The session starts in plaintext mode.
func NewSession(conn net.Conn) *Session {
	return &Session{
		conn:   conn,
		reader: bufio.NewReaderSize(conn, 4096),
	}
}

// Upgrade switches the session to HAP frame encryption.
// writeKey encrypts data sent to the receiver; readKey decrypts data received.
// Both keys are 32 bytes derived via HKDF-SHA512 during pair-verify.
// After Upgrade, use Reader() to get the correct *bufio.Reader for http.ReadResponse.
func (s *Session) Upgrade(writeKey, readKey []byte) {
	s.writeKey = append([]byte(nil), writeKey...)
	s.readKey = append([]byte(nil), readKey...)
	s.encrypted = true
	s.writeCounter = 0
	s.readCounter = 0
	// Wrap s.Read so that http.ReadResponse sees a stream of decrypted bytes.
	// This reader must be created once and reused across all post-upgrade responses.
	s.encReader = bufio.NewReaderSize(s, 4096)
}

// Reader returns the *bufio.Reader appropriate for the current mode.
// Pass this to http.ReadResponse.
func (s *Session) Reader() *bufio.Reader {
	if s.encrypted {
		return s.encReader
	}
	return s.reader
}

// Write sends p to the connection. In encrypted mode, p is split into frames
// of at most 1024 bytes and each frame is ChaCha20-Poly1305 encrypted.
func (s *Session) Write(p []byte) (int, error) {
	if !s.encrypted {
		return s.conn.Write(p)
	}
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > 1024 {
			chunk = p[:1024]
		}
		frame, err := EncryptFrame(s.writeKey, s.writeCounter, chunk)
		if err != nil {
			return total, err
		}
		s.writeCounter++
		if _, err := s.conn.Write(frame); err != nil {
			return total, err
		}
		total += len(chunk)
		p = p[len(chunk):]
	}
	return total, nil
}

// Read returns decrypted data in encrypted mode, or raw conn data otherwise.
// This satisfies io.Reader for use by encReader.
func (s *Session) Read(p []byte) (int, error) {
	if !s.encrypted {
		return s.reader.Read(p)
	}
	if s.decBuf.Len() > 0 {
		return s.decBuf.Read(p)
	}
	plain, err := s.readFrame()
	if err != nil {
		return 0, err
	}
	s.decBuf.Write(plain)
	return s.decBuf.Read(p)
}

// readFrame reads and decrypts one HAP frame from the raw conn reader.
// Frame format: [uint16_LE plaintext_length][ChaCha20-Poly1305 ciphertext][16-byte tag]
func (s *Session) readFrame() ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(s.reader, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("hap: read frame length: %w", err)
	}
	payloadLen := int(binary.LittleEndian.Uint16(lenBuf[:]))
	// Assemble the full frame buffer for DecryptFrame: length prefix + ciphertext + tag.
	frameBuf := make([]byte, 2+payloadLen+16)
	copy(frameBuf[:2], lenBuf[:])
	if _, err := io.ReadFull(s.reader, frameBuf[2:]); err != nil {
		return nil, fmt.Errorf("hap: read frame body: %w", err)
	}
	plain, err := DecryptFrame(s.readKey, s.readCounter, frameBuf)
	if err != nil {
		return nil, fmt.Errorf("hap: decrypt frame %d: %w", s.readCounter, err)
	}
	s.readCounter++
	return plain, nil
}

// Do sends req and reads the full response body, returning it as []byte.
// Works in both plaintext and encrypted modes.
func (s *Session) Do(req *http.Request) (*http.Response, []byte, error) {
	if err := req.Write(s); err != nil {
		return nil, nil, fmt.Errorf("hap: write request %s %s: %w", req.Method, req.URL.Path, err)
	}
	resp, err := http.ReadResponse(s.Reader(), req)
	if err != nil {
		return nil, nil, fmt.Errorf("hap: read response %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("hap: read body %s %s: %w", req.Method, req.URL.Path, err)
	}
	return resp, body, nil
}

// Close closes the underlying connection.
func (s *Session) Close() error {
	return s.conn.Close()
}

// RemoteAddr returns the remote address of the underlying connection.
func (s *Session) RemoteAddr() net.Addr {
	return s.conn.RemoteAddr()
}
