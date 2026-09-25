# Screen Mirroring

Screen mirroring streams H.264 or H.265 video (with AAC-ELD audio) from the sender's display to an AirPlay receiver. This is more complex than video URL playback and requires real-time encoding on the sender side.

> **Reference implementation:** [omarroth/doubletake](https://github.com/omarroth/doubletake) is a working Go mirroring sender (tested on AppleTV11,1 / tvOS 27) and is the source of truth for anything below that disagrees with it. Notably, modern receivers require a FairPlay SAP handshake (`fp-setup`) whose output wraps the stream key (`ekey`/`eiv`), and the SETUP is split into a control-only SETUP followed by per-stream SETUPs (with one media-first fallback if the control-only form is rejected).

## Overview

| Component | Transport | Port |
|-----------|-----------|------|
| Video (H.264/H.265) | TCP stream | Negotiated via SETUP (typically 7100) |
| Audio (AAC-ELD) | UDP RTP | Negotiated via SETUP |
| Session control | RTSP | 7000 |

---

## Session Setup

Mirroring uses two concurrent streams set up via a single RTSP SETUP request: type 110 (video) and type 96 (audio).

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
0-3     4     Payload size (uint32, big-endian)
4-5     2     Packet type:
              0x0000 = Video data (AES-CTR encrypted H.264/H.265 NAL units)
              0x0001 = Codec parameters (SPS/PPS or VPS/SPS/PPS) — unencrypted
              0x0002 = Keep-alive — unencrypted, no payload
              0x0005 = Performance metrics (binary plist) — unencrypted
6-7     2     Options:
              0x0056 / 0x005E = pause
              0x0016 / 0x001E = resume
8-15    8     NTP timestamp (nanoseconds since boot, sender wall-clock)
16-63   48    Video dimension/metadata fields (width, height, etc.)
64-127  64    Reserved/padding (zeros)
```

### Codec Parameter Packet (type 0x0001)

Sent once at stream start. Contains raw SPS/PPS (H.264) or VPS/SPS/PPS (H.265) parameter sets. Must be sent before any video data packets.

### Video Data Packet (type 0x0000)

Payload is AES-128-CTR encrypted NAL units. NAL units use 4-byte length-prefix format (not Annex B). Standard decoders expect Annex B (`0x00 0x00 0x00 0x01` start code); convert after decryption if needed.

---

## Video Encryption (AES-128-CTR)

Key and IV are derived using SHA-1 from the audio AES key and the `streamConnectionID`:

```
key_material = SHA1("AirPlayStreamKey" + str(streamConnectionID) + audio_aes_key)
iv_material  = SHA1("AirPlayStreamIV"  + str(streamConnectionID) + audio_aes_key)

video_aes_key = key_material[0:16]  // first 16 bytes
video_aes_iv  = iv_material[0:16]   // first 16 bytes
```

Where `audio_aes_key` is the 16-byte AES key established during FairPlay or HAP authentication.

AES-128-CTR decryption does not change payload size.

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

## Mirror Audio

- Codec: AAC-ELD, 44100 Hz, 2 channels
- Samples per frame (spf): 480
- Transported as standard RAOP audio (UDP RTP, type 96)
- Same ChaCha20-Poly1305 encryption as standalone audio streams
- Set up in the same SETUP request as the video stream (type 110)

---

## NTP Timestamp in Video Packets

The 8-byte NTP timestamp in the video packet header uses the sender's wall clock (nanoseconds since boot), not synchronized receiver time. Receivers use this to match video frames with the audio stream for A/V sync. The NTP synchronization exchange (on `timingPort`) establishes the relationship between sender and receiver clocks.
