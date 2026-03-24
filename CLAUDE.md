# foxCast — Claude Instructions

## Module and language

- Go module: `git.foxden.network/FoxDen/foxCast`
- Language: Go (no Rust, no CGo unless absolutely unavoidable)
- Minimum Go version: whatever is in `go.mod`

## Project layout

```
cmd/foxCast/        main entry point
internal/           all library code (not exported outside the module)
  tlv8/             TLV8 codec
  hap/              HAP pairing and session encryption
  fairplay/         FairPlay v3 cipher
  mdns/             mDNS discovery
  airplay/          AirPlay HTTP control protocol
  fileserver/       local HTTP file server
testfixtures/       Python fake AirPlay receiver (uv venv, for integration tests)
docs/               protocol research and implementation notes
```

## Commands

```bash
go build ./...                          # build everything
go test ./...                           # unit tests only
go test -tags integration ./...         # unit + integration tests (requires Python/uv)
FOXCAST_TRACE=1 go test ./...           # with full protocol hex traces
go run ./cmd/foxCast -- <args>          # run the CLI
```

## Testing conventions

- Unit tests live alongside the code they test (`foo_test.go` in the same package).
- Tests that require the Python fake receiver use build tag `//go:build integration` and start `testfixtures/fake_receiver.py` as a subprocess via `uv run`.
- Integration tests parse the fake receiver's stdout as newline-delimited JSON to assert on protocol outcomes.
- All crypto and codec code must have known-vector unit tests before any network code uses it.
- Use `FOXCAST_TRACE=1` to enable per-package hex dumps of all bytes sent and received. Tests should log at trace level unconditionally; the env var controls output visibility.

## Code conventions

- Errors: return them, don't log-and-swallow. Log at the call site where context is available.
- No `init()` functions.
- No global mutable state outside of `main`.
- Credential files: `~/.config/foxcast/credentials/<deviceID>.json`.
- All protocol constants (HKDF salts, feature flag bits, etc.) live in a `const` block in the relevant package, not inline.

## Key dependencies

- `howett.net/plist` — binary plist encode/decode
- `github.com/grandcat/zeroconf` — mDNS browse
- `golang.org/x/crypto` — X25519, Ed25519, ChaCha20-Poly1305, HKDF

## Protocol documentation

All protocol details are in `docs/`. Read the relevant doc before implementing any protocol component. When in doubt about byte formats or crypto parameters, the docs are the source of truth.
