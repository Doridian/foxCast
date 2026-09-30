# AirPlay 2 Protocol Overview

AirPlay 2 is Apple's proprietary wireless streaming protocol. No official specification exists; this documentation is based on community reverse engineering. It is built on top of standard protocols: mDNS/DNS-SD for discovery, RTSP/HTTP for session management, RTP for media transport, and a HomeKit-derived (HAP) pairing/encryption layer.

## Use Cases

Three distinct modes of operation exist from a sender's perspective:

| Mode | Description | Port |
|------|-------------|------|
| **Audio streaming (RAOP)** | Stream audio to an Apple TV or AirPlay speaker; uses RTSP + RTP over UDP (realtime) or TCP (buffered) | 7000 |
| **Screen mirroring** | Stream H.264/H.265 video + ALAC/AAC-ELD audio from sender's screen; RTSP session setup + proprietary TCP stream for video | 7000 (video on a SETUP-negotiated port, typically 7100) |
| **Video URL playback** | Instruct an Apple TV to fetch and play a remote URL directly (Apple TV downloads the video itself) | 7000 |

## AirPlay 2 vs AirPlay 1 Differences

| Feature | AirPlay 1 | AirPlay 2 |
|---------|-----------|-----------|
| Pairing | RSA challenge / SRP PIN | HAP (HomeKit) SRP + X25519 + Ed25519 |
| Control channel encryption | None (or HTTP digest auth) | ChaCha20-Poly1305 (HAP session) |
| Stream setup | RTSP ANNOUNCE + SDP | RTSP SETUP + binary plist |
| Audio encryption | AES-128-CBC (key from SDP) | ChaCha20-Poly1305 (key from HAP session) |
| Audio buffering | Realtime only (UDP) | Realtime (UDP) + Buffered (TCP) |
| Timing synchronization | NTP (custom UDP protocol) | PTP (IEEE 1588); NTP still used by many sessions |
| Multi-room | No | Yes (via PTP sync + buffered streams) |
| Audio codecs | ALAC, AAC-ELD | PCM, ALAC, AAC-LC, AAC-ELD, Opus |
| Video mirroring | Port 7100, AES-CTR, NTP timing | Same mechanism, optionally H.265 |

Modern Apple TVs (tvOS 10.2+) require AirPlay 2. AirPlay 1 is not accepted.

## High-Level Connection Flow

```
1. Discover receiver via mDNS (_airplay._tcp)
2. TCP connect to the advertised port (usually 7000)
3. GET /info — learn capabilities, get receiver's Ed25519 public key
4. Authenticate:
   a. Pair-setup: transient (every connection) or PIN/password (once per
      device, stores long-term credentials)
   b. Pair-verify with stored credentials — HAP establishes the ChaCha20
      session; the raw legacy variant leaves the channel plaintext
   c. FairPlay fp-setup (if receiver requires it)
5. RTSP SETUP — negotiate session and stream parameters (binary plist)
6. Stream media (RTP audio, mirrored video, or HTTP commands for URL playback)
7. TEARDOWN when done
```

foxCast's concrete choices at steps 3–4 (which pairing it tries, in what order, and when FairPlay runs) are in [03-authentication.md](03-authentication.md#foxcasts-pairing-flow).

## Key RFCs and Standards Referenced

- RFC 4566 — SDP (used by AirPlay 1 audio)
- RFC 3550 — RTP
- RFC 3711 — SRTP (encryption model, though AirPlay uses custom encryption)
- RFC 5054 — SRP for TLS (used as basis for HAP pair-setup)
- RFC 6234 — SHA-2
- RFC 7748 — X25519 (Curve25519 ECDH)
- RFC 8032 — Ed25519
- RFC 7539 — ChaCha20-Poly1305
- RFC 5869 — HKDF
- IEEE 1588 — PTP (used for AirPlay 2 multi-room timing)
