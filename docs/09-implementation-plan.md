# Implementation Plan

## Guiding Principles

- Each phase produces independently testable, working code before the next begins
- No phase requires a physical Apple TV to validate
- Protocol trace logging is built in from day one, making failures self-describing
- Crypto and codec components are tested against known vectors from reference implementations before any network code is written

---

## Package Structure

```
cmd/foxCast/           main entry point (CLI flags, wires packages together)

internal/
  tlv8/                TLV8 encode/decode (used by HAP pairing)
  hap/                 HAP pair-setup, pair-verify, session encryption
  fairplay/            FairPlay v3 cipher (port from reference)
  mdns/                mDNS browser (_airplay._tcp), TXT record parsing, feature flags
  airplay/             AirPlay HTTP control (PTTH reverse channel, /play, /playback-info)
  fileserver/          Local HTTP file server for serving files to the Apple TV

testutil/
  receiver/            Minimal Go mock AirPlay receiver (see Test Fixtures section)
```

---

## Test Fixtures

### pyatv Fake Device (Integration Tests)

`postlund/pyatv` ships a purpose-built fake AirPlay server at `tests/fake_device/` that implements:
- `/pair-setup` and `/pair-verify` (HAP pairing with hardcoded test credentials)
- `/fp-setup` (FairPlay stub)
- `/play`, `/rate`, `/scrub`, `/stop`, `/playback-info`
- `/reverse` (PTTH event channel)
- Failure injection via `FakeAirPlayUseCases`

Integration tests start it as a subprocess, wait for it to advertise via mDNS (or connect directly by known port), run the sender against it, and assert on its stdout output or HTTP state.

```
TestMain → start pyatv fake_device subprocess → run test → kill subprocess
```

### Go Mock Receiver (Unit/Component Tests)

A thin `testutil/receiver` package implements just enough of the AirPlay receiver protocol to test individual sender components in-process. It grows incrementally as each phase is implemented — starting with just the handshake endpoints, then adding `/play`. Running in-process means:
- No subprocess lifecycle management
- Direct inspection of received bytes and parsed state
- Controlled failure injection without modifying external code
- Fast test execution

The mock receiver is not a correct AirPlay implementation — it only needs to accept what the sender sends and assert on it.

### Known-Vector Tests (Crypto/Codec)

For Phase 1, test vectors are extracted from reference implementations:
- SRP vectors: `ejurgensen/pair_ap` test suite, RFC 5054 Appendix B
- HKDF vectors: RFC 5869 Appendix A
- ChaCha20-Poly1305 session framing: `ejurgensen/pair_ap`, `openairplay/airplay2-receiver`
- FairPlay: extract a known fp-setup exchange from `FDH2/UxPlay` or packet capture

---

## Protocol Trace Logging

Every package logs sent and received bytes at `TRACE` level (controlled by env var `FOXCAST_TRACE=1`). Format:

```
[hap] >>> pair-verify M1 (34 bytes)
  0000  01 06 03 01 00 03 20 aa  bb cc dd ee ff 00 11 22  ......  .......
  ...
[hap] <<< pair-verify M2 (154 bytes)
  ...
```

This makes failures self-describing when running against a real Apple TV or the pyatv fake device — the exact bytes in flight are visible without a packet capture tool. Claude can read a failing test's output and identify where the handshake diverges from the spec.

---

## Phase 1 — Crypto and Codec Foundations

**Goal:** All cryptographic and encoding primitives tested with known vectors. No network code.

### `internal/tlv8`
- Encode and decode TLV8 byte streams
- Handle fragmentation of values > 255 bytes
- Test: round-trip encode/decode, known-vector from pyatv/pair_ap

### `internal/hap` — crypto primitives
- SRP-6a (3072-bit, SHA-512, g=5) with Apple's non-standard session key derivation
- X25519 key exchange
- Ed25519 sign/verify
- HKDF-SHA512 with all named salt/info pairs
- ChaCha20-Poly1305 HAP session frame encode/decode (length-prefixed, counter nonce)
- Test: known vectors for each primitive; specifically test Apple's `K = SHA512(S||0) || SHA512(S||1)` quirk

### `internal/fairplay`
- FairPlay v3 two-round handshake and AES key derivation (port from `openairplay/airplay2-receiver`)
- Test: known fp-setup exchange bytes from reference implementation

**Phase complete when:** All tests pass with no network calls.

---

## Phase 2 — mDNS Discovery

**Goal:** Discover a receiver on the local network and parse its advertisement.

### `internal/mdns`
- Browse `_airplay._tcp` using `github.com/grandcat/zeroconf`
- Parse TXT records into a typed struct
- Decode the 64-bit feature flags bitmask
- Test: start pyatv fake_device (or `openairplay/airplay2-receiver`) as subprocess; assert discovery returns expected `deviceID`, `pk`, `features`

**CLI:** `foxCast discover` — prints found receivers and exits.

**Phase complete when:** `foxCast discover` lists a locally-running fake receiver.

---

## Phase 3 — HAP Pairing

**Goal:** Complete pair-setup and pair-verify against a test receiver. Persist credentials.

### `internal/hap` — pairing
- pair-setup: M1–M6 exchange over HTTP; store receiver LTPK and client LTSK to disk
- pair-verify: M1–M4 exchange; derive write/read session keys
- HAP session wrapper: wraps a `net.Conn`, transparently encrypts/decrypts all reads and writes using the ChaCha20 frame format from Phase 1
- `GET /info` request/response parsing
- Credential store: JSON file at `~/.config/foxcast/credentials/<deviceID>.json`

**Test strategy:**
1. Unit test pair-setup and pair-verify state machines against the Go mock receiver with hardcoded test keys (fast, in-process)
2. Integration test against pyatv fake_device subprocess (validates against a real protocol implementation)

**CLI:** `foxCast pair <receiver-address>` — runs pair-setup and saves credentials.

**Phase complete when:** `foxCast pair` completes against pyatv fake_device and persists credentials that survive process restart.

---

## Phase 4 — AirPlay Control Protocol

**Goal:** Send a URL to a receiver and have it "play" (acknowledged by the fake receiver).

### `internal/airplay`
- `GET /info` over HAP-encrypted connection
- FairPlay fp-setup (if feature flags require it)
- RTSP-style request/response framing over the HAP session wrapper
- `POST /reverse` — open PTTH reverse channel, read incoming event POST requests asynchronously
- `POST /play` with binary plist body
- `GET /playback-info`

**Test strategy:**
1. Unit test request/response serialization with the Go mock receiver
2. Integration test the full `/play` flow against pyatv fake_device: assert the fake device logs receipt of the play command with the correct URL

**CLI:** `foxCast play <receiver-address> <url>` — pairs if needed, then plays the URL.

**Phase complete when:** `foxCast play` sends a URL to pyatv fake_device and the fake device confirms receipt.

---

## Phase 5 — Local File Server and End-to-End Integration

**Goal:** Play a local file on a discovered Apple TV (or fake receiver).

### `internal/fileserver`
- Start an HTTP server on a random available port
- Serve a single local file at a predictable path
- Determine the local outbound IP for the URL passed to `/play`
- Shut down cleanly after playback ends or on interrupt

**CLI:** `foxCast cast <file>` — discovers a receiver via mDNS, starts the file server, plays the URL, streams progress from the event channel, shuts down on completion.

**Test strategy:**
1. Unit test file server (serve a file, fetch it, assert byte-for-byte match)
2. End-to-end integration test: `foxCast cast` against pyatv fake_device

**Phase complete when:** `foxCast cast` plays a local file against pyatv fake_device end-to-end.

---

## Iteration Notes for Claude

- Each package has its own `_test.go` file(s). Run `go test ./internal/<pkg>/` to test a single package.
- `FOXCAST_TRACE=1 go test ./...` emits full protocol traces on all tests, including passing ones — useful for diffing against reference captures.
- The pyatv integration test fixture is guarded by a build tag `//go:build integration` so it doesn't run by default (requires Python + pyatv installed). Run with `go test -tags integration ./...`.
- Reference packet captures (from Wireshark against a real Apple TV) can be checked into `testdata/captures/` and replayed against the mock receiver for regression testing.
