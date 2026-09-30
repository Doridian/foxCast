# foxCast

foxCast is a project that aims to implement the current version of Apple's AirPlay protocol on the sender end.

In the end, it should be easy to stream any type of content to an Apple TV or other AirPlay receiver.

## Status

| Feature | State |
|---------|-------|
| mDNS discovery | ✅ |
| Pairing (transient, PIN, password/Digest; saved credentials) | ✅ |
| Video URL playback (`play <url>`), incl. local files via built-in HTTP server (tvOS 26+ play-queue protocol) | ✅ hardware-tested (AppleTV11,1, tvOS 27) |
| Opening YouTube (and other app) links in the Apple TV's own app (`play <youtube url>`, Companion protocol) | ✅ against the test receiver; not yet hardware-tested |
| MKV playback: on-the-fly Matroska → HLS/fMP4 transmux (no re-encoding), local or HTTP(S) | ✅ hardware-tested (UHD HEVC HDR10 + AC-3 remux, AppleTV11,1) |
| Screen mirroring (`mirror`, H.264/HEVC + audio, Wayland/X11) | ✅ against the test receiver; hardware-tested upstream in doubletake (AppleTV11,1/14,1, tvOS 27) |
| System tray app (`gui`, Qt 6; Plasma-style popup, native pairing dialogs) | ✅ against the test receiver |
| Playback controls in the CLI (pause/seek) | API exists (`PlaybackSession.Rate/Scrub`), not yet exposed |
| Audio-only streaming to AirPlay 2 speakers (`mirror -audio-only`, automatic for speakers) | ✅ against the test receiver; not yet hardware-tested |
| Receiver groups (`group`: several speakers in step, stereo pairs, quad/5.1/7.1 routing) | ✅ against the test receiver; not yet hardware-tested |
| Audio streaming to AirPlay 1-only (RAOP ANNOUNCE) speakers | ⏳ |

## Installation

foxCast links GStreamer and Qt 6 with CGo, so building needs their development files
(GStreamer, qt6-base, layer-shell-qt). The first build of miqt takes a few minutes.

```sh
go build ./cmd/foxCast
install -Dm644 contrib/foxcast.desktop ~/.local/share/applications/foxcast.desktop
```

Mirroring and audio also need the GStreamer base/good/bad/ugly/libav plugins at runtime and, on
Wayland, xdg-desktop-portal.

## Usage

### Tray app

`foxCast gui` (or the foxCast entry in the application menu) puts an icon in the system tray.
Clicking it opens a popup against the panel, like Plasma's Networks applet: paired receivers are
listed first, the others below. Click a receiver to mirror the screen, play a file (native file
picker) or paste a URL, play the computer's sound on it (Play Sound — the only action for
speakers), or to forget its pairing. PIN and password prompts appear as dialogs. The `mirror`,
`play` and credential flags work here too and apply to every session.

It follows the Plasma style, colours and icons. The tray icon is a StatusNotifierItem (Plasma,
and other desktops with an SNI host). On Wayland the popup is a layer-shell surface anchored to
the panel. Without a tray host, the receiver list opens as an ordinary window.

### Command line

The same features are available as subcommands:

```sh
foxCast discover                                  # list receivers
foxCast pair  -target 10.0.0.5                    # PIN pairing, saves credentials
foxCast play  -target 10.0.0.5 https://example.com/video.m3u8
foxCast play  -target 10.0.0.5 ./movie.mp4        # served from this machine
foxCast play  -target 10.0.0.5 /mnt/nas/movie.mkv # MKV: remuxed to HLS on the fly
foxCast play  -target 10.0.0.5 https://nas.example/movie.mkv  # needs HTTP Range support
foxCast play  -target 10.0.0.5 https://youtu.be/dQw4w9WgXcQ  # opens in the YouTube app
foxCast probe /mnt/nas/movie.mkv                  # show which tracks will play
foxCast mirror -target 10.0.0.5                   # screen sharing (needs GStreamer)
                                                  # audio: adds a "<receiver> (foxCast)" output device
                                                  # and makes it the default while mirroring
foxCast mirror -target 10.0.0.5 -test -no-audio   # synthetic source
foxCast mirror -target 10.0.0.7                   # a speaker (e.g. HomePod): sound only
foxCast mirror -target 10.0.0.5 -audio-only       # use a TV as a speaker
```

`foxCast group` plays one multichannel output device on several receivers in step, each taking
a channel or a left/right pair: `foxCast group -layout quad A=FL B=FR C=RL D=RR`, or
`foxCast group Kitchen Den` for the same stereo in two rooms. `-test` walks a beep around the
layout to check the placement. See [docs/04](docs/04-audio-streaming.md#receiver-groups-in-foxcast-stereo-pairs-surround).

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

Links to YouTube, Apple TV, Disney+, Hulu and Pluto TV open in the Apple TV's own app
instead of AirPlay's player. This uses the Apple TV's remote-control (Companion) service, which
pairs separately: the first time, the Apple TV shows a PIN. `-app never` plays through
AirPlay; `-app always` hands any URL to the Apple TV to route. Twitch deep links do not work on
tvOS. See [docs/10](docs/10-companion.md).

The receiver fetches local and transmuxed media from foxCast's HTTP server, so a host firewall
must let it in: pin the port with `-http-port 7020` (and `-port-range` for timing) and open them.

Omit `-target` to pick from discovered receivers. Pass a PIN/password with `$FOXCAST_CODE`
(preferred over `-code`). `-debug` or `FOXCAST_TRACE=1` enables protocol logging.

On Wayland, mirroring connects first, shows "Choosing what to share…" on the receiver, and then
the portal asks for a screen or window; `-remember-source` reuses the last
choice for that receiver instead. The receiver probes a local UDP timing port during SETUP; use
`-port-range MIN-MAX` to pin the ports if a firewall is in the way.

HomePods and current Apple TVs time sessions with PTP. foxCast follows their clocks on UDP
ports 319/320, which needs `sudo setcap cap_net_bind_service=+ep` on the binary (or
`net.ipv4.ip_unprivileged_port_start=319`) and UDP 319-320 open in the firewall. Without them
those receivers are timed from their RTSP clock headers: a few milliseconds less precisely, and
minutes off for a HomePod that follows another device's clock (a home theater's speakers). See [docs/04](docs/04-audio-streaming.md).

For hardware-free testing, run `go run ./cmd/foxCast-test-receiver -profile modern -auth none -listen 127.0.0.1:7000`
and point foxCast at `-target 127.0.0.1`.

> **Note:** for headless machines, `go build -tags nogui ./cmd/foxCast` builds foxCast without
> the tray app (`foxCast gui`), so only the GStreamer development files are needed.

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
- [10 - Companion Protocol: Opening Links in Apple TV Apps](docs/10-companion.md)

## Sources

### Receiver Implementations (primary protocol references)

- https://github.com/FDH2/UxPlay — AirPlay 2 mirroring receiver (C); best source for mirror protocol, FairPlay v3, NTP timing, AES-CTR video key derivation
- https://github.com/openairplay/airplay2-receiver — AirPlay 2 audio receiver (Python); best source for HAP pairing, ChaCha20 RTP encryption, ALAC atom, PTP timing, buffered streams
- https://github.com/mikebrady/shairport-sync — Full-featured AirPlay 2 audio receiver (C); best documentation on AirPlay 2 buffered audio and PTP timing architecture
- https://github.com/mikebrady/nqptp — PTP subset daemon for AirPlay 2 multi-room sync (C); ports 319/320

### Sender Implementations

- https://github.com/omarroth/doubletake — AirPlay screen mirroring sender for Linux (Go, LGPL-3.0-or-later); pure-Go FairPlay SAP, encrypted RTSP, mirror/audio streams; hardware-tested on AppleTV11,1 / tvOS 27. Basis for foxCast's mirroring support
- https://github.com/akustikrausch/airplay2-sender-cpp — AirPlay 2 realtime audio sender (C++); documents the exact AP2 handshake order (encrypted control channel, SETUP → event channel → RECORD, event-channel keep-alive)

- https://github.com/postlund/pyatv — Apple TV client library (Python); best source for sender-side AirPlay 2 SETUP plist format, HAP session channel setup, MRP tunneling, and the Companion protocol (OPACK, `_launchApp`) behind app deep links
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
