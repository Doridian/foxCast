# foxCast

foxCast is a project that aims to implement the current version of Apple's AirPlay protocol on the sender end.

In the end, it should be easy to stream any type of content to an Apple TV or other AirPlay receiver.

## Status

| Feature | State |
|---------|-------|
| mDNS discovery | ✅ |
| Pairing (transient, PIN, password/Digest; saved credentials) | ✅ |
| Video URL playback (`play <url>`), incl. local files via built-in HTTP server (tvOS 26+ play-queue protocol) | ✅ hardware-tested (AppleTV11,1, tvOS 27) |
| MKV playback: on-the-fly Matroska → HLS/fMP4 transmux (no re-encoding), local or HTTP(S) | ✅ hardware-tested (UHD HEVC HDR10 + AC-3 remux, AppleTV11,1) |
| Screen mirroring (`mirror`, H.264/HEVC + audio, Wayland/X11) | ✅ against the test receiver; hardware-tested upstream in doubletake (AppleTV11,1/14,1, tvOS 27) |
| System tray app (`gui`, Qt 6; Plasma-style popup, native pairing dialogs) | ✅ against the test receiver |
| Playback controls in the CLI (pause/seek) | API exists (`PlaybackSession.Rate/Scrub`), not yet exposed |
| Audio-only streaming to AirPlay 2 speakers (`mirror -audio-only`, automatic for speakers) | ✅ against the test receiver; not yet hardware-tested |
| Audio streaming to AirPlay 1-only (RAOP ANNOUNCE) speakers | ⏳ |

## Usage

```sh
foxCast discover                                  # list receivers
foxCast pair  -target 10.0.0.5                    # PIN pairing, saves credentials
foxCast play  -target 10.0.0.5 https://example.com/video.m3u8
foxCast play  -target 10.0.0.5 ./movie.mp4        # served from this machine
foxCast play  -target 10.0.0.5 /mnt/nas/movie.mkv # MKV: remuxed to HLS on the fly
foxCast play  -target 10.0.0.5 https://nas.example/movie.mkv  # needs HTTP Range support
foxCast probe /mnt/nas/movie.mkv                  # show which tracks will play
foxCast mirror -target 10.0.0.5                   # screen sharing (needs GStreamer)
                                                  # audio: adds a "<receiver> (foxCast)" output device
                                                  # and makes it the default while mirroring
foxCast mirror -target 10.0.0.5 -test -no-audio   # synthetic source
foxCast mirror -target 10.0.0.7                   # a speaker (e.g. HomePod): sound only
foxCast mirror -target 10.0.0.5 -audio-only       # use a TV as a speaker
```

Receivers without screen mirroring are used as speakers: `mirror` streams the computer's sound
to them the same way it does alongside the screen (the `-audio-source` flags apply), with no
video. The receiver's volume is left as it is. The playout lead is 500 ms by default;
`-target-latency-ms` changes it within 250–2000 ms.

Matroska files are rewrapped as HLS with fMP4 segments while playing; video and audio are
copied, not re-encoded. H.264/HEVC video (incl. HDR10, HLG, Dolby Vision 5/8; profile 7 plays
its HDR10 base layer) and AAC/AC-3/E-AC-3/FLAC/Opus/ALAC/MP3 audio work. Every compatible audio
track is selectable on the Apple TV; `-audio 3,5` picks tracks. Text subtitles (SRT, ASS/SSA,
WebVTT) become selectable WebVTT subtitles. TrueHD/DTS audio and bitmap subtitles (PGS, VobSub)
are skipped. See [docs/09](docs/09-matroska-transmuxing.md).

The receiver fetches local and transmuxed media from foxCast's HTTP server, so a host firewall
must let it in: pin the port with `-http-port 7020` (and `-port-range` for timing) and open them.

Omit `-target` to pick from discovered receivers. Pass a PIN/password with `$FOXCAST_CODE`
(preferred over `-code`). `-debug` or `FOXCAST_TRACE=1` enables protocol logging.

Mirroring needs GStreamer (`gst-launch-1.0` with base/good/bad/ugly/libav plugins) and, on
Wayland, xdg-desktop-portal. On Wayland foxCast connects first, shows "Choosing what to share…" on
the receiver, and then the portal asks for a screen or window; `-remember-source` reuses the last
choice for that receiver instead. The receiver probes a local UDP timing port during SETUP; use
`-port-range MIN-MAX` to pin the ports if a firewall is in the way.

### Tray app

`foxCast gui` puts an icon in the system tray. Clicking it opens a popup against the panel,
like Plasma's Networks applet: paired receivers are listed first, the others below. Click a
receiver to mirror the screen, play a file (native file picker) or paste a URL, play the
computer's sound on it (Play Sound — the only action for speakers), or to forget its pairing. PIN and password prompts appear as dialogs. The `mirror`, `play` and credential flags
work here too and apply to every session.

The GUI uses Qt 6 through [miqt](https://github.com/mappu/miqt), so it needs CGo and the Qt 6
development files, and is behind a build tag:

```sh
go build -tags gui ./cmd/foxCast            # needs Qt 6 (qt6-base) and, for Wayland, layer-shell-qt
install -Dm644 contrib/foxcast.desktop ~/.local/share/applications/foxcast.desktop
```

It follows the Plasma style, colours and icons. The tray icon is a StatusNotifierItem (Plasma,
and other desktops with an SNI host). On Wayland the popup is a layer-shell surface anchored to
the panel. Without a tray host, the receiver list opens as an ordinary window. The first build of
miqt takes a few minutes.

For hardware-free testing, run `go run ./cmd/foxCast-test-receiver -profile modern -auth none -listen 127.0.0.1:7000`
and point foxCast at `-target 127.0.0.1`.

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
- [09 - Matroska → HLS Transmuxing](docs/09-matroska-transmuxing.md)

## Sources

### Receiver Implementations (primary protocol references)

- https://github.com/FDH2/UxPlay — AirPlay 2 mirroring receiver (C); best source for mirror protocol, FairPlay v3, NTP timing, AES-CTR video key derivation
- https://github.com/openairplay/airplay2-receiver — AirPlay 2 audio receiver (Python); best source for HAP pairing, ChaCha20 RTP encryption, ALAC atom, PTP timing, buffered streams
- https://github.com/mikebrady/shairport-sync — Full-featured AirPlay 2 audio receiver (C); best documentation on AirPlay 2 buffered audio and PTP timing architecture
- https://github.com/mikebrady/nqptp — PTP subset daemon for AirPlay 2 multi-room sync (C); ports 319/320

### Sender Implementations

- https://github.com/omarroth/doubletake — AirPlay screen mirroring sender for Linux (Go, LGPL-3.0-or-later); pure-Go FairPlay SAP, encrypted RTSP, mirror/audio streams; hardware-tested on AppleTV11,1 / tvOS 27. Basis for foxCast's mirroring support
- https://github.com/akustikrausch/airplay2-sender-cpp — AirPlay 2 realtime audio sender (C++); documents the exact AP2 handshake order (encrypted control channel, SETUP → event channel → RECORD, event-channel keep-alive)

- https://github.com/postlund/pyatv — Apple TV client library (Python); best source for sender-side AirPlay 2 SETUP plist format, HAP session channel setup, MRP tunneling
- https://github.com/philippe44/RAOP-Player — AirPlay audio sender/RAOP client (C); shows RTSP ANNOUNCE SDP construction, sender-side auth

### Pairing Libraries

- https://github.com/ejurgensen/pair_ap — Clean HAP + legacy "fruit" pairing library (C); used by shairport-sync; best reference for TLV8 formats, ChaCha20 session encryption frame format, exact HKDF salt strings
- https://github.com/openairplay/open-airplay — Collection of misc AirPlay libraries (some outdated)

### Protocol Documentation

- https://emanuelecozzi.net/docs/airplay2 — Unofficial AirPlay 2 protocol docs; feature flags, encryption layer overview
- https://nto.github.io/AirPlay.html — AirPlay 1 HTTP API reference; `/play`, `/reverse`, `/scrub`, PTTH reverse connection, SDP format
- https://air-display.github.io/airplay-internal/ — receiver-side notes incl. a full media-cast request log (`/info` → `fp-setup` → `SETUP` → pairing → `/reverse` → `/play`)

## Later goals

- Screen mirroring (H.264/H.265 + AAC-ELD over TCP/UDP; requires real-time encoding)
- Runs on Windows as well
- Support for any AirPlay receiver type (not just Apple TV)

## License

foxCast is licensed under the [GNU Lesser General Public License v3.0 or later](LICENSE) (`LGPL-3.0-or-later`). See [COPYING.GPL](COPYING.GPL) for the incorporated GPLv3 terms. Portions are derived from [doubletake](https://github.com/omarroth/doubletake) (LGPL-3.0-or-later).
