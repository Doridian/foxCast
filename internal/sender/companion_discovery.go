package sender

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/grandcat/zeroconf"
)

// companionServiceType is the Companion protocol's DNS-SD service.
const companionServiceType = "_companion-link._tcp"

// Companion TXT record flags (rpfl), from pyatv protocols/companion.
const (
	companionFlagPairingDisabled = 0x04
	companionFlagPINPairing      = 0x4000
)

// CompanionService is an advertised Companion service.
type CompanionService struct {
	Name string
	// IPs are the addresses the service resolved to.
	IPs  []string
	Port int
	// Model is the rpmd TXT value, e.g. AppleTV11,1.
	Model string
	// Flags is the rpfl TXT value (companionFlag*).
	Flags uint64
}

// PairingDisabled reports whether the device refuses Companion pairing.
func (s *CompanionService) PairingDisabled() bool {
	return s.Flags&companionFlagPairingDisabled != 0
}

// ErrNoCompanionService means no Companion service was advertised for the
// receiver: it is not an Apple TV, or it is asleep and not answering mDNS.
var ErrNoCompanionService = errors.New("receiver advertises no Companion service (only Apple TVs can open apps)")

// FindCompanionService browses for the Companion service of the host at ip
// until one is found or ctx is done.
func FindCompanionService(ctx context.Context, ip string) (*CompanionService, error) {
	want := net.ParseIP(ip)
	if want == nil {
		return nil, fmt.Errorf("companion lookup: %q is not an IP address", ip)
	}
	browseCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var match *CompanionService
	err := browseMDNS(browseCtx, companionServiceType, func(entry *zeroconf.ServiceEntry) {
		if match != nil {
			return
		}
		service := parseCompanionEntry(entry)
		if slices.ContainsFunc(service.IPs, func(s string) bool { return net.ParseIP(s).Equal(want) }) {
			match = service
			cancel()
		}
	})
	if err != nil {
		return nil, err
	}
	if match == nil {
		return nil, ErrNoCompanionService
	}
	return match, nil
}

func parseCompanionEntry(entry *zeroconf.ServiceEntry) *CompanionService {
	service := &CompanionService{Name: unescapeDNSName(entry.Instance), Port: entry.Port}
	for _, ip := range entry.AddrIPv4 {
		service.IPs = append(service.IPs, ip.String())
	}
	for _, ip := range entry.AddrIPv6 {
		service.IPs = append(service.IPs, ip.String())
	}
	txt := parseTXT(entry.Text)
	service.Model = txt["rpmd"]
	// pyatv reads rpfl as hex, with or without its usual 0x prefix.
	if flags := strings.TrimPrefix(strings.ToLower(txt["rpfl"]), "0x"); flags != "" {
		service.Flags, _ = strconv.ParseUint(flags, 16, 64)
	}
	return service
}
