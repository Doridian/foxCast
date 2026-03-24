# Service Discovery (mDNS/Bonjour)

AirPlay receivers advertise themselves via mDNS (Multicast DNS / DNS-SD / Bonjour). A sender must perform mDNS browsing to discover available receivers on the local network.

## Service Types

Two mDNS service types are published by a receiver:

| Service Type | Registration Format | Port |
|--------------|---------------------|------|
| `_airplay._tcp` | `<DeviceName>` | 7000 |
| `_raop._tcp` | `<MACADDR>@<DeviceName>` | 7000 |

For AirPlay 2 video and audio control, query `_airplay._tcp`. The `_raop._tcp` service is primarily for audio-only (RAOP) streaming.

## TXT Record Fields — `_airplay._tcp`

| Key | Example | Description |
|-----|---------|-------------|
| `deviceid` | `AA:BB:CC:DD:EE:FF` | Hardware MAC address (used as unique device identifier) |
| `features` | `0x4A7FDFD5,0x38BCB46` | 64-bit capability bitmask (two 32-bit hex values, comma-separated) |
| `flags` | `0x4` | Status flags |
| `model` | `AppleTV6,2` | Hardware model string |
| `pk` | (128 hex chars) | Ed25519 device long-term public key (for HAP pairing) |
| `pi` | (UUID string) | Pairing identifier |
| `srcvers` | `366.0` | AirPlay server version |
| `vv` | `2` | AirPlay protocol version |
| `acl` | `0` | Access control: 0=open, 1=HomeKit users only, 2=admin only |

## TXT Record Fields — `_raop._tcp`

| Key | Example | Description |
|-----|---------|-------------|
| `ch` | `2` | Audio channel count |
| `cn` | `0,1,2,3` | Supported compression types: 0=PCM, 1=ALAC, 2=AAC-LC, 3=AAC-ELD |
| `et` | `0,3,5` | Encryption types supported |
| `ft` | (hex) | Feature flags (same bitmask as `features` above) |
| `pk` | (hex) | Ed25519 public key |
| `pw` | `false` | Whether a password is required |
| `sf` | (hex) | Security flags |
| `sr` | `44100` | Sample rate |
| `ss` | `16` | Sample size (bits) |
| `tp` | `UDP` | Transport protocol preference |
| `vs` | `366.0` | Server version |
| `vn` | `65537` | |

## Feature Flag Bitmask

The `features` field is a 64-bit integer encoded as two 32-bit hex values. Key bits relevant to a sender:

| Bit | Name | Relevance to Sender |
|-----|------|---------------------|
| 0 | Video | AirPlay video v1 supported |
| 7 | ScreenMirroring | Mirror streaming supported |
| 9 | Audio | General audio streaming supported |
| 11 | AudioRedundant | RFC 2198 audio redundancy supported |
| 12 | FPSAPv2pt5_AES_GCM | FairPlay secure auth required |
| 14 | MFiSoft_FairPlay | MFi/FairPlay audio auth required |
| 18 | ReceiveAudioPCM | PCM audio accepted |
| 19 | ReceiveAudioALAC | ALAC audio accepted |
| 20 | ReceiveAudioAAC_LC | AAC-LC audio accepted |
| 27 | LegacyPairing | Legacy SRP pairing supported |
| 30 | UnifiedAdvertisingInfo | Unified mDNS format in use |
| 40 | BufferedAudio | AirPlay 2 buffered (multi-room) audio supported |
| 41 | PTP | PTP clock synchronization supported (multi-room) |
| 43 | SystemPairing | System-level pairing required |
| 46 | HomeKitPairing | HAP (HomeKit) pairing required |
| 48 | TransientPairing | Transient pairing (no stored credentials) supported |
| 49 | AirPlayVideoV2 | Video v2 supported |
| 51 | MFiPairSetup | MFi pair-setup supported |
| 59 | StreamConnectionSetup | Stream connection setup supported |

**Minimum bitmask for AirPlay 2 multi-room:** bits 9, 11, 30, 40, 41, 51 → `0x40000a00,0x80300`

## GET /info

After TCP connection, send this before authentication to learn the receiver's capabilities:

```
GET /info RTSP/1.0
CSeq: 0
User-Agent: AirPlay/366.0

← 200 OK
Content-Type: application/x-apple-binary-plist

{
  deviceID: "AA:BB:CC:DD:EE:FF",
  name: "Living Room",
  model: "AppleTV6,2",
  manufacturer: "Apple Inc.",
  features: <uint64>,
  statusFlags: <uint32>,
  protocolVersion: "1.1",
  sourceVersion: "366.0",
  pk: <data, 32 bytes Ed25519 pub key>,
  audioLatencies: [
    {inputLatencyMicros: 0, outputLatencyMicros: 79000}
  ],
  timingPeerInfo: { ... }   // present if PTP enabled
}
```

The `sourceVersion` field affects protocol behavior:
- `>= 355` — PTP + buffered streams available
- `> 360` — full MRP remote control tunneling available
