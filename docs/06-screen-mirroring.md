# Screen Mirroring

Screen mirroring streams H.264 or H.265 video (with ALAC or AAC-ELD audio) from the sender's display to an AirPlay receiver. This is more complex than video URL playback and requires real-time encoding on the sender side.

> **Reference implementation:** [omarroth/doubletake](https://github.com/omarroth/doubletake) is a working Go mirroring sender (tested on AppleTV11,1 / tvOS 27) and is the source of truth for anything below that disagrees with it. Notably, modern receivers require a FairPlay SAP handshake (`fp-setup`) whose output wraps the stream key (`ekey`/`eiv`), and the SETUP is split into a control-only SETUP followed by per-stream SETUPs (with one media-first fallback if the control-only form is rejected).

## Overview

| Component | Transport | Port |
|-----------|-----------|------|
| Video (H.264/H.265) | TCP stream | Negotiated via SETUP (typically 7100) |
| Audio (ALAC or AAC-ELD) | UDP RTP | Negotiated via SETUP |
| Session control | RTSP | 7000 |

---

## Session Setup

Mirroring uses two concurrent streams: type 110 (video) and type 96 (audio). Older sources set both up in a single RTSP SETUP request, as in the example below.

foxCast (`setupMirrorSession` in `mirror.go`, from doubletake) sends separate requests instead:

1. A control-only SETUP with the session keys (`deviceID`, `sessionUUID`, `timingProtocol` with `timingPort` or `timingPeerInfo`, …) and `combinedGetInfoWithControlSetup`. Its response may carry an `info` dictionary with the session's display size; without a usable one, foxCast asks `GET /info` again. RECORD follows unless the response says `skipRecord`.
2. The audio stream SETUP (type 96), on the audio `streamConnectionID` URI.
3. The video stream SETUP (type 110), on a URI with its own `streamConnectionID`, carrying `shk`/`shiv` and `latencyMs`.

If the receiver rejects the control-only SETUP (400, 405, 406, 415, 455 or 501), foxCast switches once to media-first order: the audio SETUP carries the session keys, the video SETUP follows, then RECORD. The encoder starts once the receiver's canvas is known (after the control SETUP, or after the audio SETUP in media-first order), so the codec and size come from the session's display.

### Mirror Stream SETUP

```
POST /setup RTSP/1.0
Content-Type: application/x-apple-binary-plist

{
  streams: [
    {
      type:              110,
      streamConnectionID: <uint64>,    // used for AES-CTR key/IV derivation
      timingPort:        <ntp_udp_port>
    },
    {
      type:        96,                  // realtime audio alongside mirror
      audioFormat: 0x800,              // AAC-ELD 44100 Hz
      ct:          8,                  // AAC-ELD
      spf:         480,
      sr:          44100,
      controlPort: <client_audio_control_port>,
      shk:         <32 bytes HAP session key>
    }
  ]
}

← 200 OK
{
  streams: [
    {type: 110, dataPort: <tcp_port>},
    {type: 96, controlPort: <port>, dataPort: <udp_port>}
  ]
}
```

After SETUP, open a TCP connection to the `dataPort` for video data.

---

## Video Packet Format

Each video TCP packet has a 128-byte header followed by the payload:

```
Offset  Size  Description
0-3     4     Payload size (uint32, little-endian)
4       1     Packet type:
              0x00 = Video data (encrypted H.264/H.265 NAL units)
              0x01 = Codec parameters (SPS/PPS or VPS/SPS/PPS) — unencrypted
              0x02 = Keep-alive — unencrypted, no payload
              0x05 = Performance metrics (binary plist) — unencrypted
5       1     0x10 on a keyframe (IDR) video packet, else 0x00
6-7     2     Options:
              0x0056 / 0x005E = pause
              0x0016 / 0x001E = resume
8-15    8     Presentation time (NTP 32.32 fixed point, little-endian)
16-63   48    Video dimension/metadata fields (width, height, etc.)
64-127  64    Reserved/padding (zeros)
```

What foxCast sends (`sendFrame`, `sendCodecFrame`, `dataHeartbeatLoop` in `mirror.go`): codec packets use options `0x16 0x01` (H.264) or `0x1e 0x01` (HEVC) and carry the encoded size as float32 at offsets 16, 40 and 56. Video packets have zero options and, in a PTP session, the receiver's timeline (clock) ID at offset 40. A keep-alive (type 0x02, options `0x1e 0x00`) goes out every second once the first frame is sent. With ChaCha20-Poly1305 the payload size includes the 16-byte tag.

### Codec Parameter Packet (type 0x0001)

Carries an avcC decoder configuration record (H.264) or a complete `hvc1` sample description with its `hvcC` box (H.265). Must be sent before any video data packets. foxCast sends it with the first IDR and again whenever the encoder's parameter sets change, not before every IDR.

### Video Data Packet (type 0x0000)

Payload is encrypted NAL units (AES-128-CTR, or ChaCha20-Poly1305; see below). NAL units use 4-byte length-prefix format (not Annex B). Standard decoders expect Annex B (`0x00 0x00 0x00 0x01` start code); convert after decryption if needed.

---

## Video Encryption (AES-128-CTR)

Key and IV are derived using SHA-512 from the stream key and the video `streamConnectionID` (`deriveVideoKeys`, per UxPlay):

```
key_material = SHA512("AirPlayStreamKey" + str(streamConnectionID) + shk)
iv_material  = SHA512("AirPlayStreamIV"  + str(streamConnectionID) + shk)

video_aes_key = key_material[0:16]  // first 16 bytes
video_aes_iv  = iv_material[0:16]   // first 16 bytes
```

Where `shk` is the 16-byte stream key sent (with `shiv`) in the video stream descriptor: the FairPlay SAP key when `fp-setup` ran, otherwise one taken from the HAP session. `-direct-key` uses `shk`/`shiv` as the AES key and IV without derivation; `-no-encrypt` sends frames unencrypted.

AES-128-CTR decryption does not change payload size.

### ChaCha20-Poly1305 (encrypted HAP sessions)

When pair-verify set up an encrypted session, foxCast encrypts video with ChaCha20-Poly1305 instead (`deriveChaChaKey`):

```
key   = HKDF-SHA512(IKM  = pair-verify X25519 shared secret (FairPlay AES key as fallback),
                    salt = "DataStream-Salt" + str(streamConnectionID),
                    info = "DataStream-Output-Encryption-Key")[0:32]
nonce = 4 zero bytes + LE64(frame counter)
AAD   = the 128-byte packet header
```

---

## Codec Details

### H.264

- NAL unit types: 1 (non-IDR), 5 (IDR), 6 (SEI), 7 (SPS), 8 (PPS)
- Parameter sets are in type 0x0001 packets
- Video data is length-prefixed NAL format
- Feature bit 7 (ScreenMirroring) must be set by receiver

### H.265 / HEVC

- Three parameter sets: VPS + SPS + PPS
- NAL type = `(payload[0] & 0x7E) >> 1`
- Signaled by "hvc1" signature in codec parameters
- Feature bit 42 (ScreenMultiCodec) must be set by receiver

---

### Sender-side video capture (Wayland)

foxCast asks xdg-desktop-portal's ScreenCast interface for one source.
`SelectSources` requests `types = MONITOR (1) | WINDOW (2)`, masked by the
portal's `AvailableSourceTypes`, so the picker offers individual windows as
well as whole screens. With `types = 1` alone, KDE's portal shows only the
monitor list.

By default `persist_mode` is `0` (do not persist) and no restore token is
sent, so the picker appears for every mirror session, as on macOS.
`-remember-source` restores the old behaviour: `persist_mode = 2`
(persistent), and the `restore_token` returned by `Start` is saved per
receiver in the credential store and sent with the next request, which skips
the picker. Both options need ScreenCast portal version 4 or newer.

As on macOS, foxCast pairs and completes SETUP before asking what to share.
A Wayland session (`CaptureConfig.DeferSource`) runs one GStreamer pipeline
for its whole life: `compositor ! caps ! videorate ! queue ! encoder`. What is
shared is a source bin on a compositor pad. First comes a "Choosing what to
share…" placeholder (an embedded PNG from `internal/sender/placeholders`,
decoded by `pngdec ! imagefreeze`, or a plain card without those elements),
and the portal picker
opens once the receiver shows it. This also keeps the picker outside the
receiver's first-frame deadline. Dismissing the first picker ends the session.

`CaptureSwitcher.Switch` handles every source change, the first one included.
It opens a new portal session (a mid-session change always leaves out the
restore token). It then adds a `pipewiresrc ! [vapostproc !] videoscale add-borders ! capsfilter`
bin for the new node on a new pad at alpha 0, fades the current pad's
`alpha` to 0 over ½ s, and fades the new pad in once it has delivered a
frame. The old bin is removed in the background, because a pipewiresrc that
has failed takes about 30 s to reach NULL. The encoder never restarts, so the
receiver sees no new parameter sets and no forced keyframe. The tray app
offers mid-session changes ("Change…" next to "Stop"); dismissing that picker
keeps the current source.

A fade costs a blend only while it runs. At alpha 1 the compositor copies the
frame, so steady state is as cheap as a plain pipeline. The compositor's
output caps pin the encoder's raw format, so a new input never renegotiates
the encoder. Two GStreamer behaviours shape the pipeline:

- A compositor left with no pads restarts its output timeline, so the new
  source is added before the old one is removed.
- Adding a pad still makes the compositor emit one frame stamped at the start
  of its timeline; `videorate drop-only=true` drops it.

openh264 in screen-content mode codes most fade frames as keyframes, whatever
`scene-change-detection` says; rate control keeps them small. x264 gets
`scenecut=0`.

For HEVC without `-target-latency-ms`, the capture latency probe measures the
placeholder encoder rather than the screen.

A window source changes size when the window is resized. The fixed-size
`videoscale add-borders=true` stage in front of the encoder letterboxes it
into the receiver canvas, so the encoded size stays the same.

## Mirror Audio

- Codec: ALAC (spf 352) or AAC-ELD (spf 480), 44100 Hz, 2 channels
- Transported as standard RAOP audio (UDP RTP, type 96)
- Same encryption as standalone audio streams: ChaCha20-Poly1305 with a
  random key sent as `shk` on encrypted sessions, else AES-128-CBC with the
  FairPlay key
- Set up in its own SETUP request, before the video stream (type 110)
- The codec follows `supportedFormats.screenStream`: ALAC (`0x40000`) when the
  receiver lists it, else AAC-ELD (`0x1000000`, needs the `fdk_aac` build).
  AppleTV11,1 on tvOS 27 advertises `0x1440000` and gets ALAC.

### Sender-side capture (foxCast)

By default `mirror` creates an output device for the receiver, a
`module-null-sink` named `foxcast_<device id>` and described as
"<receiver name> (foxCast)". It is loaded with `pactl`, which covers both
PulseAudio and pipewire-pulse. The sink is 44.1 kHz stereo so no resampling
happens before encoding. Its monitor source is recorded with `pulsesrc`.

- The sink becomes the default output while mirroring, like macOS routing
  system audio to the TV. On exit the previous default is restored first,
  then the module is unloaded, so streams move back to the previous device
  and not to a fallback the sound server picks. `-keep-default-sink` keeps
  the default output unchanged; apps then have to be routed to the device
  by hand.
- A killed session leaves the module loaded. The next run unloads any
  `module-null-sink` that has the same `sink_name` before loading a new one.
- `-audio-source monitor` records the default output's monitor (the old
  behaviour, where audio also plays locally). Any other value is taken as a
  PulseAudio source name.
- A PipeWire-native sink (`pipewiresrc` with `media.class=Audio/Sink`)
  registers and links, but on GStreamer 1.28 / PipeWire 1.6 it delivered only
  a few thousand silent samples. It is not used.

---

## NTP Timestamp in Video Packets

The 8-byte timestamp in the video packet header is the frame's presentation time: its capture PTS plus the negotiated video playout lead (`latencyMs`), as NTP 32.32 fixed point. In an NTP session it is on the sender's boot-relative clock (no epoch), and the NTP exchange (on `timingPort`) establishes the relationship between sender and receiver clocks. In a PTP session foxCast converts it to the receiver's PTP timeline and puts that timeline's ID at offset 40 (`frameTimeAt` in `mirror.go`). Receivers use this to match video frames with the audio stream for A/V sync.
