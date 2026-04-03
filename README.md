# foxCast

# THIS: https://air-display.github.io/airplay-internal/media_cast_service.html The connection order might matter???

foxCast is a project that aims to implement the current version of Apple's AirPlay protocol on the sender end.

In the end, it should be easy to stream any type of content to an Apple TV or other AirPlay receiver.

## MVP

The minimal end-to-end proof: instruct an Apple TV to play a URL. The Apple TV fetches and decodes the media itself — no RTP, no encoding, no timing sync required on the sender side.

- Discover Apple TV devices on the local network via mDNS
- Pair with an Apple TV (HAP pair-setup, persist credentials; pair-verify on each connection)
- Instruct an Apple TV to play a given URL (`POST /play`)
- Serve a local file over HTTP so it can be played from disk
- Runs on Linux
- Implemented in Go

## Post-MVP

- Playback controls for URL playback: play/pause (`/rate`), seek (`/scrub`), stop
- Event handling: read playback state notifications from the reverse (PTTH) channel
- Audio-only streaming via RAOP (required for non-Apple-TV AirPlay receivers, e.g. speakers)

## Documentation

Protocol research and implementation notes are in [`docs/`](docs/):

- [01 - Protocol Overview](docs/01-protocol-overview.md)
- [02 - Service Discovery](docs/02-discovery.md)
- [03 - Authentication & Pairing](docs/03-authentication.md)
- [04 - Audio Streaming (RAOP)](docs/04-audio-streaming.md)
- [05 - Video URL Playback](docs/05-video-url-playback.md)
- [06 - Screen Mirroring](docs/06-screen-mirroring.md)
- [07 - Data Formats](docs/07-data-formats.md)
- [08 - Cryptography](docs/08-cryptography.md)
- [09 - Implementation Plan](docs/09-implementation-plan.md)

## Sources

### Receiver Implementations (primary protocol references)

- https://github.com/FDH2/UxPlay — AirPlay 2 mirroring receiver (C); best source for mirror protocol, FairPlay v3, NTP timing, AES-CTR video key derivation
- https://github.com/openairplay/airplay2-receiver — AirPlay 2 audio receiver (Python); best source for HAP pairing, ChaCha20 RTP encryption, ALAC atom, PTP timing, buffered streams
- https://github.com/mikebrady/shairport-sync — Full-featured AirPlay 2 audio receiver (C); best documentation on AirPlay 2 buffered audio and PTP timing architecture
- https://github.com/mikebrady/nqptp — PTP subset daemon for AirPlay 2 multi-room sync (C); ports 319/320

### Sender Implementations

- https://github.com/postlund/pyatv — Apple TV client library (Python); best source for sender-side AirPlay 2 SETUP plist format, HAP session channel setup, MRP tunneling
- https://github.com/philippe44/RAOP-Player — AirPlay audio sender/RAOP client (C); shows RTSP ANNOUNCE SDP construction, sender-side auth

### Pairing Libraries

- https://github.com/ejurgensen/pair_ap — Clean HAP + legacy "fruit" pairing library (C); used by shairport-sync; best reference for TLV8 formats, ChaCha20 session encryption frame format, exact HKDF salt strings
- https://github.com/openairplay/open-airplay — Collection of misc AirPlay libraries (some outdated)

### Protocol Documentation

- https://emanuelecozzi.net/docs/airplay2 — Unofficial AirPlay 2 protocol docs; feature flags, encryption layer overview
- https://nto.github.io/AirPlay.html — AirPlay 1 HTTP API reference; `/play`, `/reverse`, `/scrub`, PTTH reverse connection, SDP format

## Later goals

- Screen mirroring (H.264/H.265 + AAC-ELD over TCP/UDP; requires real-time encoding)
- xdg-desktop-portal integration for app and window capture
- Runs on Windows as well
- Support for any AirPlay receiver type (not just Apple TV)
