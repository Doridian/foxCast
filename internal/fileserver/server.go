// Package fileserver serves a single local file (or any handler, such as a
// transmuxed HLS presentation) over HTTP so an AirPlay receiver can fetch it
// directly. The server listens on a given port (or a random one) and
// exposes the local outbound IP for building the playback URL.
package fileserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

// Server serves an http.Handler to the receiver.
type Server struct {
	listener net.Listener
	srv      *http.Server
}

// FileHandler serves the file at path under /<base name>. It returns the
// handler and its URL path.
func FileHandler(path string) (http.Handler, string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("fileserver: resolve path: %w", err)
	}
	if _, err := os.Stat(absPath); err != nil {
		return nil, "", fmt.Errorf("fileserver: stat %s: %w", absPath, err)
	}
	urlPath := "/" + url.PathEscape(filepath.Base(absPath))
	mux := http.NewServeMux()
	mux.HandleFunc(urlPath, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, absPath)
	})
	return mux, urlPath, nil
}

// Start serves handler on the given TCP port (0 = random) until ctx is
// cancelled or Shutdown is called.
func Start(ctx context.Context, handler http.Handler, port int) (*Server, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("fileserver: listen: %w", err)
	}
	s := &Server{listener: ln, srv: &http.Server{Handler: handler}}

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

// Port returns the port the server is listening on.
func (s *Server) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

// URL returns the HTTP URL of urlPath that the AirPlay receiver should use.
// receiverAddr is the receiver's address (used to determine the correct local IP).
func (s *Server) URL(receiverAddr, urlPath string) (string, error) {
	localIP, err := outboundIP(receiverAddr)
	if err != nil {
		return "", fmt.Errorf("fileserver: determine local IP: %w", err)
	}
	return "http://" + net.JoinHostPort(localIP, strconv.Itoa(s.Port())) + urlPath, nil
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
