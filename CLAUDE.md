# foxCast — Claude Instructions

## Module and language

- Go module: `git.foxden.network/FoxDen/foxCast`
- Language: Go (no Rust). CGo is used for GStreamer (`internal/gst`, always) and Qt (`internal/gui`, unless built with `-tags nogui`), and for the optional `fdk_aac` tag; don't add other C dependencies unless absolutely unavoidable
- Minimum Go version: whatever is in `go.mod`
- License: LGPL-3.0-or-later (`LICENSE`, `COPYING.GPL`)

## Project layout

```
cmd/foxCast/                 CLI: discover, pair, play (URL/file, or app link), probe, serve, mirror,
                             rtmp, group, gui
cmd/foxCast-test-receiver/   hardware-free AirPlay receiver for manual testing
internal/
  sender/                    AirPlay sender core: discovery, HAP/legacy pairing,
                             encrypted RTSP, FairPlay SAP, event channel,
                             URL playback (playback.go), screen mirroring,
                             audio (speakers, receiver groups), PTP clock following,
                             GStreamer capture (in-process pipelines, gst_command.go;
                             Wayland session compositor with fades, capture_mixer.go;
                             RTMP ingest onto that compositor, ingest.go),
                             in-process test receiver, Companion protocol client
                             (Apple TV app launching, companion.go) and test service
  gst/                       thin CGo binding to GStreamer: parse-launch pipelines, bus,
                             properties, adding/removing bins while playing
  opack/                     Apple's OPACK serialization (Companion messages)
  applink/                   web URL → Apple TV app deep link (YouTube, …)
  rtmp/                      minimal RTMP ingest server (publish only) producing FLV, for `rtmp`
  fileserver/                local HTTP server for `play <file>` (file or transmux handler)
  mediasource/               random/streaming access to a local path or HTTP(S) Range URL
  mkv/                       Matroska demuxer (header, tracks, Cues, cluster reader)
  codec/                     bitstream parsing: codec config records, RFC 6381 strings, frame sizes
  fmp4/                      fragmented MP4 init/media segment writer
  transmux/                  Matroska → HLS (fMP4) VOD server used by `play`
  gui/                       tray app for `foxCast gui`: Qt 6 (miqt) UI, StatusNotifierItem +
                             dbusmenu over D-Bus, LayerShellQt popup placement; `-tags nogui`
                             drops the Qt files, the model (model.go) is toolkit-free
contrib/                     desktop entry
docs/                        protocol research and implementation notes
```

`internal/sender` is derived from [doubletake](https://github.com/omarroth/doubletake)
(LGPL-3.0-or-later). Keep attribution when moving code around.

## Commands

```bash
go build ./...                          # build everything (needs GStreamer and Qt 6 dev files, CGo)
go build -tags nogui ./cmd/foxCast      # without `foxCast gui` (GStreamer dev files only)
go test ./...                           # all tests (includes end-to-end tests against the in-process receiver)
FOXCAST_TRACE=1 go run ./cmd/foxCast …  # CLI with verbose protocol logging (same as -debug)
go run ./cmd/foxCast -- <args>          # run the CLI
go run ./cmd/foxCast-test-receiver -profile modern -auth none -listen 127.0.0.1:7000
```

## Testing conventions

- Unit tests live alongside the code they test (`foo_test.go` in the same package).
- Protocol flows are tested end to end against `sender.ReceiverServer` (the in-process test receiver) using `newReceiverServerTestPair`. Extend the receiver when adding sender features (see `receiver_playback.go`).
- All crypto and codec code must have known-vector unit tests before any network code uses it.
- There is no hardware in CI. Real-device behaviour must come from a documented source (pyatv, doubletake, airplay2-sender-cpp, captures) and be noted in `docs/`.

## Code conventions

- Errors: return them, don't log-and-swallow. Log at the call site where context is available.
- No `init()` functions.
- No global mutable state outside of `main`. Known exceptions inherited from doubletake: `sender.SetDebugMode` and `sender.SetTargetLatency` (process-wide settings set once by the CLI). `gst.Init` guards `gst_init`, which is process-wide by nature. The sender also memoizes per-process GStreamer probes (`automaticHEVCProbeResults`, `audioTimestampFallbackWarning`).
- Credentials: `$XDG_CONFIG_HOME/foxcast/credentials.json` (default `~/.config`; one file, keyed by receiver AirPlay device ID, with the receiver password and Companion pairing in the same entry), or the system keyring with `-cred-backend keyring`.
- All protocol constants (HKDF salts, feature flag bits, etc.) live in a `const` block in the relevant package, not inline.

## Key dependencies

- `howett.net/plist` — binary plist encode/decode
- `github.com/grandcat/zeroconf` — mDNS browse
- `golang.org/x/crypto`, `github.com/aead/chacha20poly1305` — X25519, Ed25519, ChaCha20-Poly1305, HKDF
- `golang.org/x/sys` — Linux packet receive timestamps, CLOCK_BOOTTIME, pipes to in-process GStreamer
- `github.com/godbus/dbus/v5` — xdg-desktop-portal screencast (Wayland); the tray icon (StatusNotifierItem, dbusmenu) and notifications
- `github.com/zalando/go-keyring` — optional keyring credential backend
- GStreamer (linked through `internal/gst`) — capture and encoding for mirroring and audio. Pipelines are still written as gst-launch argument lists and run in-process by `gstCommand`
- `github.com/mappu/miqt` — Qt 6 bindings for the GUI (dropped by `-tags nogui`); UI toolkit choices stay with Qt/KDE so Plasma theming applies

## Protocol documentation

All protocol details are in `docs/`. Read the relevant doc before implementing any protocol component. When in doubt about byte formats or crypto parameters, the docs are the source of truth — except where a doc says the doubletake/pyatv implementation supersedes it.
