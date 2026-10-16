# Now Playing and Handoff: Continuing an Apple TV Video on the Computer

The inverse of [10](10-companion.md): read what an Apple TV is playing, pause
it, and open the same video in a local browser at the same position
(`foxCast handoff`, and "Continue on This Computer" in the tray app).
`foxCast nowplaying [-raw]` shows the state read-only.

Companion cannot report playback state. It comes from **MediaRemote (MRP)**,
which tvOS 15+ carries as protobuf inside an AirPlay 2 *data stream*
(pyatv: `protocols/airplay/ap2_session.py`, `channels.py`,
`protocols/mrp/protobuf`; MIT). Verified against an Apple TV 4K (AppleTV11,1,
`AirTunes/980.77.2`) in October 2026, read only, while it played YouTube.

## Setting up the stream

On a paired (pair-verify or transient) encrypted RTSP connection:

1. `SETUP` (binary plist): `isRemoteControlOnly: true`, `osName`,
   `sourceVersion: "550.10"`, `timingProtocol: "None"`, `sessionUUID`, `name`.
   The reply has an `eventPort`; the sender connects to it (as for URL
   playback; nothing is read from it). Then `RECORD`.
2. `SETUP` with `streams: [{type: 130, controlType: 2, channelID: <UUID>,
   seed: <random u64>, clientUUID, wantsDedicatedSocket: true,
   clientTypeUUID: "1910A70F-DBC0-4242-AF95-115DB30604E1"}]`. The reply's
   `streams[0].dataPort` is a plain TCP port.

Keys: HKDF-SHA512 of the pair-verify shared secret, salt
`"DataStream-Salt" + decimal(seed)`; the sender encrypts with info
`DataStream-Output-Encryption-Key` and decrypts with
`DataStream-Input-Encryption-Key`. Framing is HAP's: 2-byte LE plaintext
length (also the AAD, at most 1024 per chunk), ciphertext, 16-byte tag, nonce
a per-direction counter in bytes 4–11.

## Data frames

Inside the encrypted stream, big-endian:

| Offset | Size | Field |
|--------|------|-------|
| 0 | 4 | total size, header included |
| 4 | 12 | type: `sync` or `rply`, zero padded |
| 16 | 4 | command: `comm` in a `sync`, zeros in an `rply` |
| 20 | 8 | sequence number (a `rply` repeats the request's) |
| 28 | 4 | zero |

The payload is a binary plist `{params: {data: <bytes>}}`; `data` holds
protobuf `ProtocolMessage`s, each prefixed with a varint length. Every `sync`
frame from the Apple TV is answered with an empty `rply`. The sender's first
sequence number is random in `[2³², 2³³)`.

## Handshake

`ProtocolMessage`: `type` (1), `identifier` (2; a reply repeats the request's),
`errorCode` (4); each message lives in an extension field:

| Message | type | field | Notes |
|---------|------|-------|-------|
| DEVICE_INFO | 15 | 20 | sent first; the reply is the Apple TV's own |
| SET_CONNECTION_STATE | 38 | 42 | `state = 2` (connected); no reply |
| CLIENT_UPDATES_CONFIG | 16 | 21 | no reply (see below) |
| SET_STATE | 4 | 9 | pushed by the Apple TV |
| SEND_COMMAND | 1 | 6 | `command`: 1 play, 2 pause, 3 toggle |

DEVICE_INFO is pyatv's (an "iPhone" running com.apple.TVRemote). With
CLIENT_UPDATES_CONFIG set to `nowPlayingUpdates = false` and everything else
`true` (pyatv's defaults), the Apple TV pushes its state right away: a few
dozen messages (types 65, 17, 64, 72, 105, 46, 55, 4, 104, …). With every
flag `false` it pushes **nothing** — the first attempt got silence.

## Now-playing state

SET_STATE (`SetStateMessage`): `displayName` (5), `playbackState` (6; 1 playing,
2 paused, 3 stopped, 4 interrupted, 5 seeking), `playbackQueue` (3: `location`
and `contentItems`, each `ContentItem{identifier, metadata}`), `playerPath`
(9: `client{bundleIdentifier (2), displayName (7)}`). `ContentItemMetadata`:
`title` (1), `trackArtistName` (7; a video's channel), `duration` (14),
`elapsedTime` (35), `playbackRate` (39), `contentIdentifier` (44),
`elapsedTimeTimestamp` (74). Updates are partial: one message carries the
item, another only the state, so foxCast merges them per app.

`elapsedTimeTimestamp` is in seconds since 2001-01-01 (Apple's reference date)
and says when `elapsedTime` was true; the position now is
`elapsedTime + rate × (now − timestamp)` while playing. Cross-checked against
the Apple TV: `elapsedTime` 1.46 at 18:15:54 gave 571 s at 18:25:24.

## The video ID is not in there

For YouTube (bundle `com.google.ios.youtube`) the Apple TV reports title,
channel, duration and position, but **no video ID or URL**: `contentIdentifier`
is empty, `nowPlayingInfoData` (field 67) is an archived empty dictionary,
and none of the other messages carries a link. foxCast therefore looks the
video up (`internal/ytsearch`): it searches YouTube for "title channel" and
scrapes `ytInitialData` from the results page (`videoRenderer`: `videoId`,
title, `ownerText`, `lengthText`), accepting a result whose title equals the
reported one (ignoring case) and whose length is within 2 s of the duration,
preferring one from the same channel. That page is not a documented
interface; if YouTube changes it, `handoff` fails with a clear error and
leaves the Apple TV playing (the lookup runs before the pause). Videos whose
title is not unique among same-length results may match the wrong one.

## Pause and open

`handoff` sends SEND_COMMAND pause, waits (up to 3 s) for the paused SET_STATE
to get the exact position, and opens
`https://www.youtube.com/watch?v=<id>&t=<seconds>s` with `xdg-open`.
Other apps are refused with a message naming what is playing.

## Hardware testing

Reading state was verified read only against an Apple TV 4K playing YouTube.
`handoff` (SEND_COMMAND pause, then opening the browser) was confirmed working
end to end on that device by the maintainer.
