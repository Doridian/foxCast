// Package fileserver serves a single local file over HTTP so an AirPlay
// receiver can fetch it directly. The server listens on a random available
// port and exposes the local outbound IP for building the playback URL.
package fileserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

// Server serves one file over HTTP.
type Server struct {
	listener net.Listener
	srv      *http.Server
	path     string // absolute path to the file being served
	filename string // base name used in the URL path
}

// Start creates a new HTTP server on a random local port serving the file at path.
// Call URL(remoteAddr) to get the URL to pass to the AirPlay receiver, then
// call Shutdown when playback is done.
func Start(ctx context.Context, path string) (*Server, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("fileserver: resolve path: %w", err)
	}
	if _, err := os.Stat(absPath); err != nil {
		return nil, fmt.Errorf("fileserver: stat %s: %w", absPath, err)
	}

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("fileserver: listen: %w", err)
	}

	filename := filepath.Base(absPath)
	mux := http.NewServeMux()
	s := &Server{
		listener: ln,
		path:     absPath,
		filename: filename,
	}
	mux.HandleFunc("/"+filename, s.serveFile)
	s.srv = &http.Server{Handler: mux}

	go func() {
		_ = s.srv.Serve(ln)
	}()

	// Shut down when the context is cancelled.
	go func() {
		<-ctx.Done()
		_ = s.srv.Shutdown(context.Background())
	}()

	return s, nil
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, s.path)
}

// Port returns the port the server is listening on.
func (s *Server) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

// URL returns the HTTP URL the AirPlay receiver should use to fetch the file.
// receiverAddr is the receiver's address (used to determine the correct local IP).
func (s *Server) URL(receiverAddr string) (string, error) {
	localIP, err := outboundIP(receiverAddr)
	if err != nil {
		return "", fmt.Errorf("fileserver: determine local IP: %w", err)
	}
	return fmt.Sprintf("http://%s:%d/%s", localIP, s.Port(), s.filename), nil
}

// Shutdown stops the server.
func (s *Server) Shutdown() error {
	return s.srv.Shutdown(context.Background())
}

// outboundIP determines the local IP address used to reach addr.
func outboundIP(addr string) (string, error) {
	// Dial UDP (no actual connection is made) to find the outbound interface.
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	conn, err := net.Dial("udp", host+":80")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String(), nil
}
