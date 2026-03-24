// Package mdns discovers AirPlay receivers on the local network using mDNS/Bonjour.
package mdns

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/grandcat/zeroconf"
)

// FeatureFlags is the 64-bit AirPlay capability bitmask from the mDNS TXT record.
type FeatureFlags uint64

// Key feature flag bits relevant to a sender.
const (
	FeatureVideo              FeatureFlags = 1 << 0
	FeatureScreenMirroring    FeatureFlags = 1 << 7
	FeatureAudio              FeatureFlags = 1 << 9
	FeatureFairPlay           FeatureFlags = 1 << 12 // FPS-AP v2.5 — fp-setup required
	FeatureMFiSoftFairPlay    FeatureFlags = 1 << 14 // MFi/FairPlay — fp-setup required
	FeatureLegacyPairing      FeatureFlags = 1 << 27
	FeatureUnifiedAdvertising FeatureFlags = 1 << 30
	FeatureBufferedAudio      FeatureFlags = 1 << 40
	FeaturePTP                FeatureFlags = 1 << 41
	FeatureHomeKitPairing     FeatureFlags = 1 << 46
	FeatureAirPlayVideoV2     FeatureFlags = 1 << 49
)

// Device holds the parsed advertisement for a discovered AirPlay receiver.
type Device struct {
	Name      string
	Host      string // IP address or hostname
	Port      int
	DeviceID  string       // MAC address from TXT "deviceid"
	PublicKey []byte       // Ed25519 LTPK from TXT "pk" (32 bytes)
	Features  FeatureFlags // Decoded 64-bit bitmask
	Model     string       // e.g. "AppleTV6,2"
	PairingID string       // TXT "pi" — pairing identifier UUID
}

// Addr returns "host:port" for use as a dial address.
func (d *Device) Addr() string {
	return fmt.Sprintf("%s:%d", d.Host, d.Port)
}

// NeedsFairPlay reports whether the device requires FairPlay fp-setup.
func (d *Device) NeedsFairPlay() bool {
	return d.Features&(FeatureFairPlay|FeatureMFiSoftFairPlay) != 0
}

// NeedsHAPPairing reports whether the device requires HAP pair-setup/verify.
func (d *Device) NeedsHAPPairing() bool {
	return d.Features&FeatureHomeKitPairing != 0
}

// Discover browses for _airplay._tcp receivers until ctx is cancelled.
// Found devices are sent to the returned channel; the channel is closed when
// browsing stops.
func Discover(ctx context.Context) (<-chan *Device, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("mdns: new resolver: %w", err)
	}

	entries := make(chan *zeroconf.ServiceEntry)
	out := make(chan *Device, 8)

	go func() {
		defer close(out)
		if err := resolver.Browse(ctx, "_airplay._tcp", "local.", entries); err != nil {
			return
		}
		for {
			select {
			case entry, ok := <-entries:
				if !ok {
					return
				}
				dev, err := parseEntry(entry)
				if err != nil {
					continue
				}
				select {
				case out <- dev:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

func parseEntry(entry *zeroconf.ServiceEntry) (*Device, error) {
	dev := &Device{
		Name: entry.ServiceInstanceName(),
		Port: entry.Port,
	}

	// Prefer IPv4; fall back to IPv6 or hostname.
	if len(entry.AddrIPv4) > 0 {
		dev.Host = entry.AddrIPv4[0].String()
	} else if len(entry.AddrIPv6) > 0 {
		dev.Host = "[" + entry.AddrIPv6[0].String() + "]"
	} else {
		dev.Host = strings.TrimSuffix(entry.HostName, ".")
	}

	txt := parseTXT(entry.Text)
	dev.DeviceID = txt["deviceid"]
	dev.Model = txt["model"]
	dev.PairingID = txt["pi"]

	if pkHex, ok := txt["pk"]; ok && pkHex != "" {
		pk, err := hex.DecodeString(pkHex)
		if err != nil {
			return nil, fmt.Errorf("mdns: invalid pk %q: %w", pkHex, err)
		}
		dev.PublicKey = pk
	}

	if feats, ok := txt["features"]; ok {
		f, err := ParseFeatures(feats)
		if err != nil {
			return nil, fmt.Errorf("mdns: invalid features %q: %w", feats, err)
		}
		dev.Features = f
	}

	if dev.DeviceID == "" {
		return nil, fmt.Errorf("mdns: entry %q missing deviceid", dev.Name)
	}

	return dev, nil
}

// parseTXT converts a slice of "key=value" strings into a map.
func parseTXT(txt []string) map[string]string {
	m := make(map[string]string, len(txt))
	for _, kv := range txt {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		m[kv[:idx]] = kv[idx+1:]
	}
	return m
}

// ParseFeatures decodes the comma-separated hex pair from the TXT "features" field.
// Format: "0x4A7FDFD5,0x038BCB46" — low 32 bits first, high 32 bits second.
func ParseFeatures(s string) (FeatureFlags, error) {
	parts := strings.SplitN(s, ",", 2)
	lo, err := strconv.ParseUint(strings.TrimPrefix(parts[0], "0x"), 16, 32)
	if err != nil {
		return 0, fmt.Errorf("low word %q: %w", parts[0], err)
	}
	if len(parts) == 1 {
		return FeatureFlags(lo), nil
	}
	hi, err := strconv.ParseUint(strings.TrimPrefix(parts[1], "0x"), 16, 32)
	if err != nil {
		return 0, fmt.Errorf("high word %q: %w", parts[1], err)
	}
	return FeatureFlags(lo | (hi << 32)), nil
}
