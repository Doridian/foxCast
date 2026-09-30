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
    audioFormat:   0x40000,      // ALAC 44100 Hz 16-bit stereo (see bitmask below)
    audioMode:     "default",
    controlPort:   <client_udp_control_port>,
    ct:            2,            // 1=PCM, 2=ALAC, 4=AAC-LC, 8=AAC-ELD
    spf:           352,          // samples per frame (352=ALAC, 480=AAC-ELD)
    sr:            44100,
    latencyMin:    11025,
    latencyMax:    88200,
    shk:           <32 bytes>,   // stream key; foxCast sends a random one per stream
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

The same bit values are used in the receiver's `supportedFormats`
(`audioStream`, `screenStream`) masks. Bits below 18 are PCM variants.

| Bit | Value | Codec | Sample Rate | Bit Depth | Channels |
|-----|-------|-------|-------------|-----------|----------|
| **18** | **0x40000** | **ALAC** | **44100 Hz** | **16** | **2** |
| 19 | 0x80000 | ALAC | 44100 Hz | 24 | 2 |
| 20 | 0x100000 | ALAC | 48000 Hz | 16 | 2 |
| 21 | 0x200000 | ALAC | 48000 Hz | 24 | 2 |
| 22 | 0x400000 | AAC-LC | 44100 Hz | — | 2 |
| 23 | 0x800000 | AAC-LC | 48000 Hz | — | 2 |
| **24** | **0x1000000** | **AAC-ELD** | **44100 Hz** | — | **2** |
| 25 | 0x2000000 | AAC-ELD | 48000 Hz | — | 2 |

foxCast sends only the two bold formats: ALAC (`ct: 2`, `spf: 352`,
encoded locally as verbatim frames) and AAC-ELD (`ct: 8`, `spf: 480`, only in
builds with `-tags fdk_aac`). Only those two values are confirmed by foxCast's
code (`screenAudioFormat*` in `internal/sender/compatibility.go`); the other rows
follow the commonly published AirPlay 2 bit list and are unverified.

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

foxCast, like Apple's senders, never sets the marker bit (byte 1 is always
0x60). The first frame has sequence number 1 and a random 32-bit RTP epoch.

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

foxCast generates a random 32-byte key per stream and sends it as `shk`. The
appended nonce is a little-endian packet counter starting at 0. ALAC streams
without ChaCha20 (plain or legacy AES-CBC) also advertise `redundantAudio: 2` and send
each frame twice (an initial burst of 8, then every new frame is preceded by
a resend of the one 8 frames back); ChaCha20 streams send each frame once.

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

foxCast answers the receiver's 0xd2 requests from its boot-relative clock
(`CLOCK_BOOTTIME` with the 1900 epoch added). When a SETUP response names a
receiver `timingPort`, it also sends three 0xd2 probes of its own, 100 ms
apart: AirServer-style receivers wait for the sender to start the exchange.

### NTP Sync Packet (UDP, control port, type 0x54)

Sent once per second (TimeAnnounce) to synchronize RTP timestamps with wall clock:

```
Byte 0:     0x90 for the first packet or after a timeline reset (X=1), else 0x80
Byte 1:     0xD4 (type 212)
Bytes 2-3:  0x0004
Bytes 4-7:  RTP timestamp playing at the NTP time (the value below less the latency)
Bytes 8-15: NTP timestamp (64-bit, network byte order)
Bytes 16-19: RTP timestamp at which the receiver applies the mapping
(20 bytes total)
```

A PTP session sends the same packet as type 0xd7 (byte 1 = 0xD7, 28 bytes):
bytes 8-15 hold PTP time in nanoseconds and bytes 20-27 the receiver's
`ClockID`.

### PTP

With `timingProtocol: "PTP"` the session runs on the receiver's PTP timeline
(its `ClockID`) instead of the sender's NTP clock. Apple uses it for
multi-room (buffered type 103) streams, and HomePods require it for realtime
speaker streams too. How foxCast follows a receiver's PTP clock is described
under [PTP Timing in foxCast](#ptp-timing-in-foxcast-following-a-receivers-clock).

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
[0x80, 0xD5]
[request_seq: uint16 big-endian]
[start_seq_no: uint16 big-endian]
[count: uint16 big-endian]
```

### Retransmit Response (type 0xD6, from sender)

Standard RTP packet with 4 bytes prepended, one response per requested packet:
```
[0x80, 0xD6]
[request_seq: uint16 big-endian]   // echoed from the request
[... original RTP packet ...]
```

foxCast keeps the last 512 packets and resends them byte for byte (same
ciphertext and nonce). For the first requested packet it no longer has, it
sends the 8-byte form `[0x80, 0xD6, request_seq, missing_seq]` instead and
stops, telling the receiver that retrying is futile.

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
mirroring (feature bit 7), or with `-audio-only`, as does the tray app's
audio-only action (`foxCast gui`). `AirPlayClient.SetupAudioOnly`
(`internal/sender/audio_session.go`) runs an AirPlay 2 realtime session with
the same control-first ordering as screen mirroring:

1. Session SETUP (no `streams`): the Initial SETUP above with
   `isScreenMirroringSession: false`, `isMultiSelectAirPlay: false`,
   `senderSupportsRelay: false` (as listed above). Timing is chosen as for
   screen mirroring: `timingProtocol: "PTP"` with `timingPeerInfo` /
   `timingPeerList` when the receiver offers PTP (feature 41, SourceVersion
   ≥ 354.54.6, encrypted session), otherwise `"NTP"` with the sender's
   `timingPort`. A PTP session runs on the receiver's clock (`ClockID` from
   the SETUP response), followed as described under
   [PTP Timing in foxCast](#ptp-timing-in-foxcast-following-a-receivers-clock).
   FairPlay root fields are added under the same rules as mirroring. A PTP
   receiver that returns a `ClockID` but no clock headers gets a timeline
   anchored at the sender's boot clock until PTP samples arrive.

   HomePods need PTP. On HomePod mini (AudioAccessory5,1, AirTunes 980.77.2,
   observed 2026-09-28) an NTP session SETUP succeeds (`skipRecord: true`),
   but the audio stream SETUP that follows gets `400 Bad Request` with an
   empty body, in both descriptor forms and whatever the other fields are.
   pyatv's own `stream_file` gets the same 400 on this device. OwnTone
   (`src/outputs/airplay.c`) uses PTP on receivers that support it. With
   PTP, the same stream SETUP succeeds with either descriptor form.
2. Event channel to `eventPort`, then RECORD (unless `skipRecord`).
3. Audio stream SETUP: one type 96 stream as above with `isMedia: true` (no
   `usingScreen`), `latencyMin: 11025`, `latencyMax: 88200`, and the ALAC or
   AAC-ELD descriptor chosen from `supportedFormats.audioStream` (ALAC when the
   mask is absent; `redundantAudio: 2` as above for ALAC without ChaCha20).
   The descriptor layout (`controlPort` vs `streamConnections`,
   feature 59), its one-shot alternate on rejection, and the ChaCha20 `shk` /
   legacy AES keys are shared with mirror audio.
4. RTP audio, TimeAnnounce sync packets (every second) and `/feedback` every
   2 s, exactly as for mirror audio, then TEARDOWN. With the PTP ports open, a
   PTP session first waits up to 3 s for the receiver's PTP clock (see below).

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

A member is `RECEIVER[=CH[,CH]][@DELAY]`: a discovered name, a device ID, or
an address (`host[:port]`). Without `=CH` it plays `FL,FR`. Members connect
one at a time (so pairing prompts do not interleave) and each gets its own
speaker session as above.

### Options considered for keeping receivers in step

| Approach | What foxCast would need | Verdict |
|---|---|---|
| Apple multi-room: buffered (type 103) streams, PTP timing, `SETRATEANCHORTIME` | Take part in PTP on UDP 319/320 (privileged ports, as nqptp does for shairport-sync) as grandmaster, or keep every receiver's `timingPeerList` pointing at each other and track the elected grandmaster through RTSP clock headers | Tightest sync, but a PTP stack is a project of its own |
| Group leader relay (`senderSupportsRelay`, `groupContainsGroupLeader`) | Undocumented relay protocol; only Apple devices lead groups | Not viable |
| Home app stereo pair | Nothing: the pair is one AirPlay receiver that takes a stereo stream | Works as a group member today (`Pair=RL,RR`) |
| **Independent realtime sessions sharing one clock (chosen)** | One realtime session per receiver (as for a single speaker), all fed from one capture | Uses only what is already implemented and tested |

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
TimeAnnounce.

Receivers that need PTP (HomePods, see above) are not timed from foxCast's
clock. Each of those sessions maps the sender's clock onto that receiver's
PTP timeline (`mediaClock`), so the same PTS still becomes the same instant
on every receiver. How closely depends on how well each mapping tracks its
receiver: well under a millisecond of wander when foxCast takes part in PTP,
several milliseconds when it falls back to the receivers' clock headers (see
[PTP Timing in foxCast](#ptp-timing-in-foxcast-following-a-receivers-clock)).
Every HomePod in one home reported its own `ClockID` (six HomePods,
2026-09-28; the two set up as Den TV's home theater speakers share an
`HTGroupUUID` and `TightSyncUUID` but not a clock), so there is no shared
timeline to use instead. Group sync between PTP receivers has not been
measured by ear. When capture timestamps are unavailable (no GStreamer
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
foxCast creates a null sink with that channel map and makes it the default
output until it exits (`-keep-default-sink` leaves the default alone;
`-audio-source NAME` records a PulseAudio source instead of creating a
sink). GStreamer records it
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
alignment PTP gives Apple's own groups. PTP receivers are bounded by how
well foxCast follows each of their clocks (see the PTP section below), and
by how symmetric each receiver's network path is. That is fine for rooms and loose
surround, and audible as smearing on a tight stereo image; there a Home app
stereo pair (one receiver) does better. Not verified on hardware yet. The
tests check the defining property: two outputs of one group capture announce
the same network time for the same audio, offset only by their delays
(`TestGroupOutputsAnnounceTheSameClock`). They also stream to four
in-process receivers at once.

---

## PTP Timing in foxCast (following a receiver's clock)

A PTP session is timed on the receiver's clock: audio TimeAnnounce packets
(0xd7) and video frames carry times on its timeline. foxCast does not serve
PTP; it follows the receiver (usually its own grandmaster, see below) and
maps local monotonic time onto that timeline (`mediaClock`,
`internal/sender/media_clock.go`). The mapping slews toward each new
estimate at up to 500 ppm and never runs backwards; it only jumps, forwards,
when more than 20 ms behind.

### What a HomePod sends

Observed on HomePod mini (AudioAccessory5,1, AirTunes 980.77.2, 2026-09-28).
Once a session SETUP lists the sender's address in `timingPeerInfo`, the
HomePod sends IEEE 802.1AS flavoured PTPv2 over UDP/IPv4, unicast to that
address (flags `0x0408`: unicast, PTP timescale):

| Message | Port | Rate | Notes |
|---|---|---|---|
| Sync (two-step) | 319 | 8/s (log −3) | |
| Follow_Up | 320 | 8/s | 802.1AS Follow_Up information TLV (OUI `00-80-C2`) and an Apple TLV (OUI `00-0D-93`) carrying the ClockID |
| Announce | 320 | 4/s (log −2) | grandmaster: the HomePod's own ClockID (priority1 248), or the home's grandmaster, see below |
| Signaling | 320 | 4/s | Apple TLVs (OUI `00-0D-93`) |

The first header byte carries transportSpecific 1 (majorSdoId, 802.1AS).
The HomePod answers a `Delay_Req` only when it carries transportSpecific 1
too, with a `Delay_Resp` to port 320; it ignores `Pdelay_Req`. Its
preciseOriginTimestamp and Delay_Resp receiveTimestamp both sit on its
125 ms Sync schedule, with the correctionField (up to about 100 ms, in
nanoseconds scaled by 2^16) making up the difference. Origin plus
correction gives a consistent Sync departure time. The Delay_Resp
receiveTimestamp does not: with or without its correction, the path delays
it implies are negative and vary by milliseconds from run to run. foxCast
therefore uses only the Delay_Resp's arrival, to time the round trip itself.

A HomePod need not be its own grandmaster. The two HomePods set up as Den
TV's home theater speakers both announced grandmaster `0x9c3e539f34dc0008`
(presumably the Apple TV) and sent Syncs on its time, each under its own
ClockID (the one in its SETUP response, which foxCast uses as the timeline
ID). foxCast's two estimates of that shared time agreed within about
0.1-0.4 ms over a minute.

The receiver's `X-Apple-RequestReceivedTimestamp` header (milliseconds) is
on the receiver's own clock. That is the PTP timeline only when the
receiver is its own grandmaster: on Dori Office HomePod they agreed to a
few milliseconds, but the two Den HomePods' headers were 16½ and 4¾
minutes off the grandmaster time.

### Following it

`PTPListener` (`internal/sender/ptp.go`) binds UDP 319 and 320 once per
process (`foxCast mirror`, `rtmp`, `group` and `gui` open it), takes kernel receive
timestamps (`SO_TIMESTAMPNS`), and per followed receiver:

1. pairs each Sync's arrival with its Follow_Up's origin + correction;
2. keeps 60 s of Syncs, takes the least-transit Sync of each 1 s bin (the
   lower envelope: the Syncs that met the least queueing), and fits a
   least-squares line through those minima for the receiver clock's rate
   against the local clock, picking the minima a second time against that
   line;
3. estimates the receiver's time as the mean of the latest 8 minima at that
   rate, plus the one-way delay: half the fastest of the last 64
   Delay_Req round trips (one a second), taking the path to be symmetric;
4. steers the session's `mediaClock` with that time once 8 Syncs (one bin)
   and 3 round trips are in (Delay_Reqs go 4/s for the first 8), and with
   the rate once the minima span 4 s.

Until the first PTP sample, the clock headers of SETUP responses and
`/feedback` time the session; after it, they are ignored, as they may be on
another clock. If the first PTP estimate is more than 20 ms from the header
one, the headers were on another clock and the timeline restarts on PTP
time, stepping back if need be; otherwise the difference is slewed out like
any correction. A speaker session waits up to 3 s for that first sample
before it starts streaming (about 1 s on the HomePods above), so audio
starts on PTP time.

Rate tracking is needed even between two healthy clocks: the local
monotonic clock is steered by NTP (systemd-timesyncd's kernel PLL was
slewing out a 3 ms offset during these tests), and the Go runtime's
monotonic clock follows that frequency adjustment. With the rate held at 1,
the gap between the header and PTP estimates drifted by 0.3 ms a second.

The rate comes from the long window and the offset from the recent bins
because the Wi-Fi link is noisy: during these tests Delay_Req round trips
ranged from 6 to 74 ms (median 36 ms) and Syncs arrived up to 24 ms late. A
16 s window extrapolated to its newest end swung the rate by hundreds of ppm
and the offset by milliseconds. Replaying two minutes of recorded Syncs, the
split estimator stayed within about ±0.6 ms of a fit over the whole
recording; live, its corrections settle to tens of microseconds per Sync,
while the header estimate wanders by ±8 ms around it.

### Falling back to the clock headers

Without the PTP ports, or when a receiver sends no Syncs, a PTP session is
timed from the receiver's RTSP clock headers alone (`clockEstimator`, `internal/sender/clock_estimator.go`):
each request/response exchange gives four timestamps; the estimate
averages the midpoints of the lowest-delay exchanges of the last 8, which
also averages out the headers' millisecond truncation. Its error is bounded
by half the fastest round trip, 5 ms on the HomePod above, where RTSP
round trips never went below 10 ms. The first 5 `/feedback` requests of a
PTP session go 200 ms apart to fill the estimator. It is right only for a
receiver that is its own grandmaster: one following another device's clock
(the Den HomePods above) is timed minutes off, which such a receiver can
only drop or hold. foxCast needs the PTP ports for those.

Ports 319 and 320 are privileged: foxCast needs `CAP_NET_BIND_SERVICE`
(`setcap cap_net_bind_service=+ep`) or `net.ipv4.ip_unprivileged_port_start`
at 319 or below, and a firewall must let UDP 319-320 in. Another PTP
daemon on the host (ptp4l, nqptp) holds the ports and also means the
fallback.
