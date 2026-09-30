package fileserver

import (
	"net"
	"testing"
)

func TestOutboundIPAcceptsIPv6(t *testing.T) {
	for _, addr := range []string{"::1", "[::1]:7000"} {
		ip, err := outboundIP(addr)
		if err != nil {
			t.Fatalf("outboundIP(%q): %v", addr, err)
		}
		if parsed := net.ParseIP(ip); parsed == nil || !parsed.IsLoopback() {
			t.Fatalf("outboundIP(%q) = %q, want a loopback address", addr, ip)
		}
	}
}
