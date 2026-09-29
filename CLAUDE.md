# foxCast — Claude Instructions

## Module and language

- Go module: `git.foxden.network/FoxDen/foxCast`
- Language: Go (no Rust, no CGo unless absolutely unavoidable — the optional `fdk_aac` and `gui` build tags are the exceptions)
- Minimum Go version: whatever is in `go.mod`
- License: LGPL-3.0-or-later (`LICENSE`, `COPYING.GPL`)

## Project layout

```
cmd/foxCast/                 CLI: discover, pair, play (URL/file), probe, serve, mirror
cmd/foxCast-test-receiver/   hardware-free AirPlay receiver for manual testing
internal/
  sender/                    AirPlay sender core: discovery, HAP/legacy pairing,
                             encrypted RTSP, FairPlay SAP, event channel,
                             URL playback (playback.go), screen mirroring,
                             audio, GStreamer capture, in-process test receiver
  fileserver/                local HTTP server for `play <file>` (file or transmux handler)
  mediasource/               random/streaming access to a local path or HTTP(S) Range URL
  mkv/                       Matroska demuxer (header, tracks, Cues, cluster reader)
  codec/                     bitstream parsing: codec config records, RFC 6381 strings, frame sizes
  fmp4/                      fragmented MP4 init/media segment writer
  transmux/                  Matroska → HLS (fMP4) VOD server used by `play`
  gui/                       tray app for `foxCast gui`: Qt 6 (miqt) UI, StatusNotifierItem +
                             dbusmenu over D-Bus, LayerShellQt popup placement; Qt files need
                             `-tags gui`, the model (model.go) is toolkit-free and tested untagged
contrib/                     desktop entry
docs/                        protocol research and implementation notes
```

`internal/sender` is derived from [doubletake](https://github.com/omarroth/doubletake)
(LGPL-3.0-or-later). Keep attribution when moving code around.

## Commands

```bash
go build ./...                          # build everything (without the GUI)
go build -tags gui ./cmd/foxCast        # with `foxCast gui` (needs Qt 6 dev files, CGo)
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
- No global mutable state outside of `main`. Known exceptions inherited from doubletake: `sender.SetDebugMode` and `sender.SetTargetLatency` (process-wide settings set once by the CLI).
- Credentials: `~/.config/foxcast/credentials.json` (one file, keyed by receiver device ID), or the system keyring with `-cred-backend keyring`.
- All protocol constants (HKDF salts, feature flag bits, etc.) live in a `const` block in the relevant package, not inline.

## Key dependencies

- `howett.net/plist` — binary plist encode/decode
- `github.com/grandcat/zeroconf` — mDNS browse
- `golang.org/x/crypto`, `github.com/aead/chacha20poly1305` — X25519, Ed25519, ChaCha20-Poly1305, HKDF
- `github.com/godbus/dbus/v5` — xdg-desktop-portal screencast (Wayland)
- `github.com/zalando/go-keyring` — optional keyring credential backend
- GStreamer (runtime, via `gst-launch-1.0` subprocess) — capture and encoding for mirroring
- `github.com/mappu/miqt` — Qt 6 bindings for the GUI (`gui` tag only); UI toolkit choices stay with Qt/KDE so Plasma theming applies

## Protocol documentation

All protocol details are in `docs/`. Read the relevant doc before implementing any protocol component. When in doubt about byte formats or crypto parameters, the docs are the source of truth — except where a doc says the doubletake/pyatv implementation supersedes it.
