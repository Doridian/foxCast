# Audio Streaming (RAOP)

AirPlay audio streaming uses RTSP for session negotiation and RTP for media transport. AirPlay 2 adds a buffered (TCP) mode for multi-room synchronization alongside the realtime (UDP) mode.

## Stream Types

| Type | Mode | Transport | Use case |
|------|------|-----------|----------|
| 96 (0x60) | Realtime | UDP | Single-device, low-latency audio |
| 103 (0x67) | Buffered | TCP | Multi-room synchronized audio (AirPlay 2) |

For an MVP sender targeting a single Apple TV, use type 96 (realtime UDP).

---

## Session Setup (RTSP POST /setup)

AirPlay 2 uses binary plist bodies for RTSP, not SDP ANNOUNCE. Two SETUP calls are required.

### Initial SETUP (Session Establishment)

```
POST /setup RTSP/1.0
Content-Type: application/x-apple-binary-plist

{
  deviceID:        "AA:BB:CC:DD:EE:FF",
  macAddress:      "AA:BB:CC:DD:EE:FF",
  sessionUUID:     "<UUID>",
  timingProtocol:  "NTP",         // use "NTP" for single-device; "PTP" for multi-room
  timingPort:      <client_ntp_udp_port>,
  model:           "AppleTV3,2",
  name:            "foxCast",
  osVersion:       "14.0",
  sourceVersion:   "366.0",
  isMultiSelectAirPlay: false,
  senderSupportsRelay: false
}

← 200 OK
{
  eventPort:  <port>,   // connect a TCP event channel here
  timingPort: <port>    // NTP timing port on receiver
}
```

Open a second TCP connection to `eventPort` immediately after. This channel uses HAP encryption with the Events-Salt keys.

### Audio Stream SETUP

```
POST /setup RTSP/1.0
Content-Type: application/x-apple-binary-plist

{
  streams: [{
    type:          96,           // realtime UDP audio
    audioFormat:   0x800,        // AAC-ELD 44100 Hz (see bitmask below)
    audioMode:     "default",
    controlPort:   <client_udp_control_port>,
    ct:            2,            // 1=PCM, 2=ALAC, 4=AAC-LC, 8=AAC-ELD
    spf:           352,          // samples per frame (352=ALAC, 480=AAC-ELD)
    sr:            44100,
    latencyMin:    11025,
    latencyMax:    88200,
    shk:           <32 bytes>,   // shared HAP key (from pair-verify session)
    isMedia:       true,
    supportsDynamicStreamID: false,
    streamConnectionID: <uint64>
  }]
}

← 200 OK
{
  streams: [{
    type:        96,
    controlPort: <server_udp_control_port>,
    dataPort:    <server_udp_data_port>
  }],
  eventPort: <port>
}
```

### Audio Format Bitmask (`audioFormat` field)

| Value | Codec | Sample Rate | Bit Depth | Channels |
|-------|-------|-------------|-----------|----------|
| 0x001 | PCM | 44100 Hz | 16 | 2 |
| 0x002 | PCM | 44100 Hz | 24 | 2 |
| 0x004 | PCM | 48000 Hz | 16 | 2 |
| 0x008 | PCM | 48000 Hz | 24 | 2 |
| 0x020 | ALAC | 44100 Hz | 16 | 2 |
| 0x040 | ALAC | 44100 Hz | 24 | 2 |
| 0x080 | ALAC | 48000 Hz | 16 | 2 |
| 0x100 | ALAC | 48000 Hz | 24 | 2 |
| 0x200 | AAC-LC | 44100 Hz | — | 2 |
| 0x400 | AAC-LC | 48000 Hz | — | 2 |
| **0x800** | **AAC-ELD** | **44100 Hz** | — | **2** |

### Begin Streaming

```
RECORD rtsp://... RTSP/1.0
RTP-Info: seq=<initial_seq>;rtptime=<initial_rtp_time>
Range: npt=0-
Session: <session_id>

← 200 OK
Audio-Latency: <samples>
```

---

## RTP Audio Packet Format

### Header (12 bytes, standard RTP)

```
Byte 0:    0x80 (V=2, P=0, X=0, CC=0)
Byte 1:    0x60 (M=0, PT=96)  or  0xE0 for the first packet (M=1)
Bytes 2-3: sequence number (uint16, big-endian)
Bytes 4-7: RTP timestamp (uint32, big-endian; increments by spf per packet)
Bytes 8-11: SSRC = 0x00000000
```

### Payload Encryption (AirPlay 2, ChaCha20-Poly1305)

```
[12-byte RTP header]
[ciphertext                        ]
[16-byte Poly1305 authentication tag]
[ 8-byte nonce (appended)           ]

AAD  = RTP header bytes 4-11 (timestamp + SSRC)
Key  = HKDF-SHA512(X25519_shared, "Control-Salt", "Control-Write-Encryption-Key", 32)
      or the `shk` field from SETUP response
Nonce: 8 bytes appended to packet; prepend 4 zero bytes to get 12-byte ChaCha nonce
```

### ALAC QuickTime Atom (36 bytes, included in SETUP or ANNOUNCE)

```
Offset  Size  Description
0-3     4     Atom size = 36
4-7     4     Atom type = 'alac'
8-11    4     Version = 0
12-15   4     Samples per frame (352 or 1024)
16      1     Compatible version = 0
17      1     Sample size (16 or 24)
18      1     History multiplier = 40
19      1     Initial history = 10
20      1     Rice parameter limit = 14
21      1     Channel count (2 for stereo)
22-23   2     Max run = 255
24-27   4     Max coded frame size = 0 (variable)
28-31   4     Average bit rate = 0
32-35   4     Sample rate (44100 or 48000)
```

---

## Timing Synchronization (NTP)

For single-device realtime streaming, NTP (custom Apple variant) synchronizes audio playback timing.

### Timing Exchange (UDP, on timingPort)

Request and response are both 32 bytes:

```
Bytes 0-1:  {0x80, 0xD2}
Bytes 2-3:  {0x00, 0x07}
Bytes 4-7:  {0x00, 0x00, 0x00, 0x00}
Bytes 8-15: reference transmit time (t0, NTP 64-bit timestamp)
Bytes 16-23: received time (t1)
Bytes 24-31: transmit time (t2)
```

Timing exchange runs every 3 seconds. Use an 8-sample circular buffer and weight samples by round-trip delay. This establishes the clock offset between sender and receiver.

### NTP Sync Packet (UDP, control port, type 0x54)

Sent periodically to synchronize RTP timestamps with wall clock:

```
Byte 0:     0x80
Byte 1:     0xD4 (type 212)
Bytes 2-3:  padding
Bytes 4-7:  current RTP timestamp (uint32)
Bytes 8-15: NTP timestamp (64-bit, network byte order)
Bytes 16-19: next RTP timestamp value
(32 bytes total)
```

### PTP (Multi-Room)

For multi-room (buffered type 103 streams), use PTP instead of NTP. The SETUP plist sets `timingProtocol: "PTP"`. PTP requires a companion daemon (`nqptp`) on ports 319/320. For single-device MVP, use `"NTP"`.

---

## SETRATEANCHORTIME (Buffered Mode)

For buffered (AirPlay 2 multi-room) streams, use this instead of NTP sync:

```
SETRATEANCHORTIME rtsp://... RTSP/1.0
Content-Type: application/x-apple-binary-plist

{
  rate:        1.0,
  rtpTime:     <uint32>,
  networkTime: <uint64 NTP nanoseconds>
}
```

---

## Keepalive and Control

### FEEDBACK (every 2 seconds)

```
FEEDBACK rtsp://... RTSP/1.0
← 200 OK (optional stream list in response)
```

### FLUSHBUFFERED (buffered streams)

```
FLUSHBUFFERED rtsp://... RTSP/1.0
Content-Type: application/x-apple-binary-plist

{flushUntilSeq: <uint32>, flushUntilRTPTime: <uint32>}
```

### Volume Control

```
SET_PARAMETER rtsp://... RTSP/1.0
Content-Type: text/parameters

volume: -20.0
```

Volume range: -144 (mute) to 0.0 (maximum).

### Now Playing Metadata

```
SET_PARAMETER rtsp://... RTSP/1.0
Content-Type: application/x-dmap-tagged

[DMAP binary data with album, artist, title, duration]
```

---

## Control Port (UDP) — Retransmission

### Retransmit Request (type 0xD5, from receiver)

```
[0x80, 0xD5, 0x00, 0x01]
[start_seq_no: uint16 big-endian]
[count: uint16 big-endian]
```

### Retransmit Response (type 0xD6, from sender)

Standard RTP packet with 4 bytes prepended:
```
[0x80, 0xD6, 0x00, 0x00]
[... original RTP packet ...]
```

---

## Session Teardown

```
TEARDOWN rtsp://... RTSP/1.0
Session: <session_id>

← 200 OK
```
