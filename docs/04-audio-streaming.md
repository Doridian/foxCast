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

---

## Audio-Only Sessions in foxCast (speakers)

`foxCast mirror` uses a receiver as a speaker when it does not advertise screen
mirroring (feature bit 7), or with `-audio-only`. `AirPlayClient.SetupAudioOnly`
(`internal/sender/audio_session.go`) runs an AirPlay 2 realtime session with
the same control-first ordering as screen mirroring:

1. Session SETUP (no `streams`): the Initial SETUP above with
   `isScreenMirroringSession: false`, `isMultiSelectAirPlay: false`,
   `senderSupportsRelay: false` (as listed above), and `timingProtocol: "NTP"` with the sender's
   `timingPort`. NTP is used even on receivers that offer PTP, as pyatv does
   for audio. FairPlay root fields are added under the same rules as mirroring.
2. Event channel to `eventPort`, then RECORD (unless `skipRecord`).
3. Audio stream SETUP: one type 96 stream as above with `isMedia: true` (no
   `usingScreen`), `latencyMin: 11025`, `latencyMax: 88200`, and the ALAC or
   AAC-ELD descriptor chosen from `supportedFormats.audioStream` (ALAC when the
   mask is absent). The descriptor layout (`controlPort` vs `streamConnections`,
   feature 59), its one-shot alternate on rejection, and the ChaCha20 `shk` /
   legacy AES keys are shared with mirror audio.
4. RTP audio, TimeAnnounce sync packets and `/feedback` every 2 s, exactly as for
   mirror audio, then TEARDOWN.

Receivers that reject the control-only SETUP get the media-first form once:
the session keys and the stream in one SETUP, followed by RECORD.

Differences from mirror audio, and why:

- **No video gate.** Streaming starts at once instead of waiting for the first
  video frame.
- **Playout lead 500 ms** (TimeAnnounce latency), not the 85 ms screen-audio
  lead: there is no video to keep in step with, and speakers on Wi-Fi need room
  for retransmits. `-target-latency-ms` overrides it, kept within
  `latencyMin`–`latencyMax` (250 ms–2 s).
- **Volume is not set.** Mirroring sends `volume: 0` (full scale), which would
  play a speaker at maximum volume.

Not yet verified on hardware; the flow is tested end to end against the
in-process receiver (`-audio-only` on `foxCast-test-receiver`) for every
receiver profile. AirPlay 1-only speakers (RAOP ANNOUNCE/SDP, e.g. older
AirPort Express firmware) are not supported.

---

## Receiver Groups in foxCast (stereo pairs, surround)

`foxCast group` plays one multichannel output device on several receivers at
once, each receiver playing one channel (on both of its sides) or a
left/right pair:

```
foxCast group -layout quad Kitchen=FL Den=FR Hall=RL Office=RR
foxCast group -layout quad "Front Pair=FL,FR" "Back Pair=RL,RR"
```

### Options considered for keeping receivers in step

| Approach | What foxCast would need | Verdict |
|---|---|---|
| Apple multi-room: buffered (type 103) streams, PTP timing, `SETRATEANCHORTIME` | Take part in PTP on UDP 319/320 (privileged ports, as nqptp does for shairport-sync) as grandmaster, or keep every receiver's `timingPeerList` pointing at each other and track the elected grandmaster through RTSP clock headers | Tightest sync, but a PTP stack is a project of its own |
| Group leader relay (`senderSupportsRelay`, `groupContainsGroupLeader`) | Undocumented relay protocol; only Apple devices lead groups | Not viable |
| Home app stereo pair | Nothing: the pair is one AirPlay receiver that takes a stereo stream | Works as a group member today (`Pair=RL,RR`) |
| **Independent realtime sessions sharing one clock (chosen)** | One NTP realtime session per receiver (as for a single speaker), all fed from one capture | Uses only what is already implemented and tested |

### How the chosen approach stays in sync

A realtime NTP receiver does not follow the arrival of packets. It syncs its
clock to the sender's NTP timing port and plays each RTP sample at the network
time given by the sender's TimeAnnounce packets (control port, payload
type 0xd4). foxCast derives those times from the capture timestamp (PTS) of
each audio frame plus the playout lead. So N receivers play in step when:

1. **One clock.** Every session answers NTP from the same host clock (the
   sender's boot-relative clock with the 1900 epoch; `audioClockAt`).
2. **One capture timeline.** `StartGroupAudioCapture` records the
   multichannel source once. A fan-out gives each receiver its own stereo
   stream, keeping every chunk's PTS and source sample position unchanged.
   Each receiver's RTP epoch differs, but its RTP-to-time mapping comes from
   the same PTS, so sample *n* of the source is announced at the same instant
   everywhere. Setup order and connection time do not matter.
3. **One playout lead.** Every session uses the same speaker latency
   (500 ms by default, `-target-latency-ms`).

foxCast therefore stays involved as the timing authority for the whole
session: it is every receiver's NTP server, and it sends every
TimeAnnounce. When capture timestamps are unavailable (no GStreamer
RTP/ONVIF elements), the fan-out gives frames a sample-counted clock
anchored at the first read, which all outputs share in the same way.

A per-receiver delay (`Name=FL@15ms`) adds to that receiver's PTS, which
moves its TimeAnnounce later. That trims for distance or for devices with
more output latency. Negative delays are not accepted: delay the other
receivers instead.

A receiver that falls behind (full 0.5 s queue) loses chunks rather than
stalling the others. Its StreamAudio sees the gap as a source discontinuity
and carries on from the next chunk. A receiver that drops out leaves the
others playing.

### Channel layout and routing

`-layout` accepts `stereo`, `quad`, `5.1`, `7.1` or a list (`FL,FR,RC`).
foxCast creates a null sink with that channel map, and GStreamer records it
with a matching `channel-mask`. Without a mask, more than two channels count
as unpositioned and `audioconvert` would mix them. Channels are indexed in
GStreamer's interleaving order (ascending `GstAudioChannelPosition`), and
the sink's channel map is written in that same order. Getting stereo
applications into the rear channels is the sound server's job: PipeWire
only upmixes when `channelmix.upmix` is enabled. Multichannel sources
(games, video players set to 4.0/5.1 output) fill the channels directly.

`-test` replaces recording with a beep that walks through the channels in
layout order (600 ms slots), for matching speakers to positions by ear.

### Accuracy and limits

Sync precision is bounded by how well each receiver tracks the sender's NTP
clock over Wi-Fi: typically a few milliseconds, not the sub-millisecond
alignment PTP gives Apple's own groups. That is fine for rooms and loose
surround, and audible as smearing on a tight stereo image; there a Home app
stereo pair (one receiver) does better. Not verified on hardware yet. The
tests check the defining property: two outputs of one group capture announce
the same network time for the same audio, offset only by their delays
(`TestGroupOutputsAnnounceTheSameClock`). They also stream to four
in-process receivers at once.
