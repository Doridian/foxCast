# Companion Protocol: Opening Links in Apple TV Apps

AirPlay can only hand the Apple TV a media URL for its own player. A YouTube
watch page is not media, and even a stream URL extracted from it would bypass
the YouTube app (accounts, recommendations, quality selection). To open a
video *in the app*, foxCast uses the **Companion protocol** instead: the
channel the iOS Remote app, Control Center's remote and Siri Remote pairing
use. Its `_launchApp` command opens an app by bundle ID, or hands a URL to
tvOS, which routes it to the app that claims it (a custom URL scheme or a
universal link).

Everything here comes from pyatv (`pyatv/protocols/companion`,
`pyatv/support/opack.py`, `pyatv/auth/hap_srp.py`; MIT), which has
implemented it against real Apple TVs since 2020, and from Home Assistant
community reports on which deep links work. **foxCast's implementation has
been tested against its in-process receiver only, not against hardware.**

Only Apple TVs (tvOS) run a Companion service. HomePods advertise one too, but
have no apps to open; third-party AirPlay receivers have none.

## Discovery

DNS-SD service `_companion-link._tcp`, on a dynamic port (often 49152+). It
is a separate service from `_airplay._tcp`; foxCast matches the two by IP
address. TXT keys used:

| Key | Meaning |
|-----|---------|
| `rpmd` | Model, e.g. `AppleTV11,1` |
| `rpfl` | Flags, hex (`0x36782`). `0x4`: pairing disabled; `0x4000`: PIN pairing supported |

## Framing

Every message is a frame:

```
+------+-------------------+-------------------+
| type | length (24-bit BE) | payload            |
+------+-------------------+-------------------+
```

| Type | Name | Use |
|------|------|-----|
| 3 | PS_Start | first pair-setup message |
| 4 | PS_Next | later pair-setup messages; **every** pair-setup reply |
| 5 | PV_Start | first pair-verify message |
| 6 | PV_Next | later pair-verify messages; every pair-verify reply |
| 7 | U_OPACK | unencrypted OPACK |
| 8 | E_OPACK | encrypted OPACK (requests, replies, events) |
| 9 | P_OPACK | handled like E_OPACK by pyatv |

The payload is an OPACK dictionary. After pair-verify, a non-empty payload is
ChaCha20-Poly1305 encrypted: the header is the associated data, its length
includes the 16-byte tag, and the 12-byte nonce is a per-direction counter
starting at 0, **little-endian over all 12 bytes** (pyatv
`Chacha20Cipher(nonce_length=12)`). This differs from AirPlay's HAP framing,
which puts the counter at offset 4.

Keys are HKDF-SHA512 of the pair-verify X25519 secret, empty salt:

| Direction | Info |
|-----------|------|
| controller → Apple TV | `ClientEncrypt-main` |
| Apple TV → controller | `ServerEncrypt-main` |

## OPACK

Apple's compact binary serialization (CoreUtils). Type byte first; integers
and lengths little-endian.

| Byte(s) | Value |
|---------|-------|
| `01` / `02` | true / false |
| `03` | terminator for endless arrays/dictionaries |
| `04` | null |
| `05` + 16 | UUID |
| `06` + 8 | absolute time (pyatv reads it as an integer) |
| `08`–`2F` | integers 0–39 |
| `30`–`33` | integer in 1, 2, 4, 8 bytes |
| `35` / `36` | float32 / float64 |
| `40`–`60` | UTF-8 string of 0–32 bytes |
| `61`–`64` | string with a 1, 2, 3, 4-byte length |
| `70`–`90` | data of 0–32 bytes |
| `91`–`94` | data with a 1, 2, 4, 8-byte length |
| `A0`–`C0` | reference to object 0–32 |
| `C1`–`C4` | reference with a 1, 2, 3, 4-byte index |
| `D0`–`DE` | array of 0–14 elements; `DF` endless, ends at `03` |
| `E0`–`EE` | dictionary of 0–14 pairs; `EF` endless, ends at `03` |

References name earlier values in first-seen order: every string, data,
UUID, float and non-small integer, each distinct value once. Booleans, null,
small integers, arrays and dictionaries are not referable. foxCast decodes
references and never emits them. `internal/opack` carries pyatv's test
vectors.

## Pairing

HAP pair-setup and pair-verify (see [03](03-authentication.md)), with each TLV8
message in the `_pd` field of an OPACK dictionary instead of an HTTP body.

**Pair-setup** (once; the Apple TV shows a 4-digit PIN when M1 arrives):

| Frame | Content |
|-------|---------|
| PS_Start | `{_pd: TLV(Method=0, State=1), _pwTy: 1, _x}` |
| PS_Next ← | `{_pd: TLV(State=2, Salt, PublicKey)}` |
| PS_Next | `{_pd: TLV(State=3, PublicKey, Proof), _pwTy: 1, _x}` |
| PS_Next ← | `{_pd: TLV(State=4, Proof)}` |
| PS_Next | `{_pd: TLV(State=5, EncryptedData), _pwTy: 1, _x}` |
| PS_Next ← | `{_pd: TLV(State=6, EncryptedData)}` |

SRP-6a with the 3072-bit group, SHA-512 and username `Pair-Setup`, as for
AirPlay. The M5 sub-TLV carries Identifier, PublicKey and Signature plus
tag `0x11` (Name): OPACK `{"name": "<controller name>"}`, which the Apple TV
lists under its remotes. M6 carries the Apple TV's Identifier, PublicKey and
Signature (and, in pyatv's fake device, a tag `0x11` with device info).
pyatv does not verify M6's signature; foxCast checks it and keeps the Apple
TV's key only when it verifies, then requires it at pair-verify.

**Pair-verify** (every connection):

| Frame | Content |
|-------|---------|
| PV_Start | `{_pd: TLV(State=1, PublicKey), _auTy: 4, _x}` |
| PV_Next ← | `{_pd: TLV(State=2, PublicKey, EncryptedData)}` |
| PV_Next | `{_pd: TLV(State=3, EncryptedData), _x}` |
| PV_Next ← | `{_pd: TLV(State=4)}` |

Encryption starts with the next frame. pyatv verifies on a new connection
after pair-setup; foxCast does the same.

Companion credentials are separate from AirPlay's: pyatv pairs each protocol
on its own. foxCast stores them under the receiver's AirPlay device ID, in the
`companion` field of its credentials entry.

## Messages

Requests are `{_i: <command>, _t: 2, _c: {<content>}, _x: <xid>}`; the reply
is `{_t: 3, _x: <same xid>, _c: {...}}`, or carries `_em` (error message)
and/or `_ec` (error code). `_t: 1` is an event (`_i` names it). pyatv starts
`_x` at a random value below 2¹⁶ and increments it per message.

Before commands, pyatv sends:

1. `_systemInfo` — `{_bf: 0, _cf: 512, _clFl: 128, _i: <rp_id>, _idsID:
   <pairing id>, _pubID: <MAC-like id>, _sf: 256, _sv: "170.18", model:
   "iPhone10,6", name: <name>}`. The values are pyatv's; their meaning is
   unknown. A null `_i` stops the Apple TV from pushing power-state events.
   foxCast derives `_i` and `_pubID` from its Companion pairing ID.
2. `_sessionStart` — `{_srvT: "com.apple.tvremoteservices", _sid: <random
   u32>}`; the reply's `_c._sid` is the Apple TV's half of the session ID,
   `(remote << 32) | local`, which `_sessionStop` takes.

Then:

| Command | Content | Reply |
|---------|---------|-------|
| `_launchApp` | `{_urlS: <url>}` or `{_bundleID: <id>}` | empty |
| `FetchLaunchableApplicationsEvent` | `{}` | `{<bundle id>: <name>, ...}` |

pyatv sends `_urlS` when the string has a URL scheme, `_bundleID` otherwise.

## Deep links

The link format is up to each app, and apps change it. Reports from the Home
Assistant thread *"AppleTV Integration Deep Link URLs – Which Are Working?"*
(community.home-assistant.io/t/592862, as of late 2025):

| App | Link | Status |
|-----|------|--------|
| YouTube | `youtube://www.youtube.com/watch?v=<id>` | works (Home Assistant's docs also give `youtube://watch/<id>`) |
| Apple TV | `https://tv.apple.com/...` (universal link, from the Share menu) | works |
| Disney+ | `https://www.disneyplus.com/video/<id>` | works |
| Hulu | `hulu://watch/<id>`, `hulu://series/<id>` | works |
| Pluto TV | `https://pluto.tv/us/live-tv/<id>` | works |
| Spotify | URI with `play=true` | albums, playlists, artists only |
| Netflix | — | stopped working with Netflix's September 2025 update |
| Paramount+ | — | stopped working |
| Twitch | `twitch://stream/<name>`, `twitch://open?stream=<name>`, web URLs | **none work** (community.home-assistant.io/t/665787); they do on Android TV |

`internal/applink` maps web URLs to these: YouTube's `/watch?v=`, `/shorts/`,
`/live/`, `/embed/` and `youtu.be` forms become the `youtube://` link (the
timestamp and playlist are dropped, since no source documents parameters
beyond `v`), Hulu watch/series pages become `hulu://`, and Apple TV, Disney+ and
Pluto TV links pass through unchanged. `foxCast play -app always` hands any
other URL to the Apple TV unchanged, for trying apps not in the table.

## In foxCast

- `foxCast play <url>` opens mapped links in the app (`-app auto`, the
  default); `-app never` plays through AirPlay as before. The first time,
  the Apple TV shows a PIN to enter; later launches reuse the pairing.
  `foxCast pair -companion` pairs (again) without opening anything.
- The tray app does the same for pasted URLs.
- `-companion-port` skips the mDNS lookup (e.g. for the test receiver's
  `-companion-listen`).
