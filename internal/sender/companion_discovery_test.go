package sender

import (
	"net"
	"reflect"
	"testing"

	"github.com/grandcat/zeroconf"
)

func TestParseCompanionEntry(t *testing.T) {
	entry := zeroconf.NewServiceEntry(`Living\ Room`, companionServiceType, "local.")
	entry.Port = 49153
	entry.AddrIPv4 = []net.IP{net.ParseIP("192.168.1.20")}
	entry.AddrIPv6 = []net.IP{net.ParseIP("fd00::20")}
	// rpfl values from pyatv's tests/protocols/companion/test_companion.py.
	entry.Text = []string{"rpfl=0x36782", "rpmd=AppleTV11,1"}

	got := parseCompanionEntry(entry)
	want := &CompanionService{
		Name:  "Living Room",
		IPs:   []string{"192.168.1.20", "fd00::20"},
		Port:  49153,
		Model: "AppleTV11,1",
		Flags: 0x36782,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCompanionEntry = %+v, want %+v", got, want)
	}
	if got.PairingDisabled() {
		t.Fatal("PairingDisabled for flags 0x36782")
	}
	entry.Text = []string{"rpfl=0x627B6"}
	if !parseCompanionEntry(entry).PairingDisabled() {
		t.Fatal("flags 0x627B6 not reported as pairing disabled")
	}
}
