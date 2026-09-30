# Video URL Playback

This mode instructs an Apple TV to fetch and play a remote URL. The Apple TV downloads the video itself; the sender only sends control commands. foxCast implements it in `internal/sender/playback.go` (`AirPlayClient.PlayURL`), driven by `foxCast play <url|file>` (`cmd/foxCast/play.go`; `-start` sets the start position). Links to sites with a known Apple TV app (e.g. YouTube) are opened in that app over the Companion protocol instead (`-app auto|always|never`).

## Play-queue flow (tvOS 26/27) — what foxCast uses

> **Hardware-verified** on AppleTV11,1, tvOS 27 (build 24J361), 2026-09-25,
> following pyatv PR #2899 (measured by its author on tvOS 26.5).

On current tvOS the `POST /play` flow below is accepted (every request gets
200 and a spinner appears) but the item is never fetched. pyatv 0.18 has the
same failure. The receiver instead expects:

| # | Step | Notes |
|---|------|-------|
| 1 | pair-verify, encrypt the control connection | as before |
| 2 | `SETUP rtsp://<local-ip>/<id>` with a **PTP** video session body | `timingProtocol: "PTP"`, `timingPeerInfo`/`timingPeerList` = `{ID, Addresses: [local-ip], DeviceType: 0, SupportsClockPortMatchingOverride: true}`, `sessionUUID`, `sessionCorrelationUUID`, `updateSessionRequest: false`, `isMultiSelectAirPlay: false`, plus the usual device fields. No PTP daemon is needed. With NTP the item plays but no events arrive and the session dies after ~20 s. |
| 3 | Connect the event channel | Receives `playbackState` and `notification` events (below) |
| 4 | `RECORD` | |
| 5 | `GET /info` (HTTP/1.1, on the encrypted connection) | read `psi` |
| 6 | `SETUP` with `streams: [{type: 130, controlType: 1, channelID: "<psi>-RCS-1", clientUUID, clientTypeUUID: "A6B27562-B43A-4F2D-B75F-82391E250194"}]` | Registers a remote control session; the response has `streamID` and no `dataPort`. Enables `/command`. |
| 7 | `POST /command HTTP/1.1`, header `X-Apple-StreamID: <streamID>`, body `{params: {data: <binary plist>}}` | inner: `{type: "insertPlayQueueItem", item: {uuid, Content-Location, mediaType: "file", IsTLSEnabled, playbackRestrictions: 0, referenceRestrictions: 2, supportsIntegratedTimeline: false, snapTimeToPausePlayback: false, clientBundleID, clientProcName, playerLoggingID (**≤ 6 chars**, else the connection is dropped), playerItemLoggingID, Start-Position: {flags: 1, value: <s>, epoch: 0, timescale: 1}}}` |
| 8 | `/command` `{type: "setProperty", property: "isInterestedInDateRange", value: true, item: {uuid}}`, then `{type: "setRate", rate: 1.0}` | Starts paused without `setRate` |
| 9 | Start `POST /feedback` every 2 s | Only **after** the commands: an RTSP feedback in flight alongside `/command` makes the receiver drop the connection |

`GET /playback-info` answers 500 in this mode. State arrives on the event
channel wrapped as `{params: {data: <binary plist>}}`: `{type: "playbackState",
name: "loading"|"playing"|"paused"|"stopped"}` and `{type: "notification",
name: …}` with names such as `currentItemChanged`, `timeJumped`,
`playbackLikelyToKeepUp`, `loadedTimeRangesChanged`, `playbackBufferFull`,
`itemPlayedToEnd`. foxCast treats `itemPlayedToEnd`, a `stopped` state after
playback started, or the receiver closing the connection after playback
started (tvOS does that when playback is stopped on the Apple TV) as the end of
playback. Stopping from foxCast sends only `TEARDOWN` (no `/stop`).

If the receiver rejects the PTP SETUP with an HTTP error status, foxCast runs
the whole legacy flow below instead. If only the remote control session fails
(no `psi` in `/info`, or its SETUP is rejected), it keeps the PTP session and
sends `POST /play` + `setProperty` + `/rate` (legacy steps 10–12) on it.

A multivariant playlist without `FRAME-RATE`/`AVERAGE-BANDWIDTH` and with
`BANDWIDTH=80000000` for a 4K HDR HEVC variant was fetched but never played
(tvOS 27); the same variant with `FRAME-RATE=23.976`, `AVERAGE-BANDWIDTH`
and a measured `BANDWIDTH` played. foxCast's transmuxer always sets
`BANDWIDTH` and `AVERAGE-BANDWIDTH`, and `FRAME-RATE` whenever the video track
has a DefaultDuration.

The receiver connects back to the sender (UDP timing port for NTP sessions,
and the HTTP server for local/transmuxed media), so a host firewall must allow
them: `-port-range` and `-http-port` pin the ports.

## Legacy connection setup (POST /play, pre-tvOS 26)

> **This replaces the AirPlay 1 `/reverse` flow for HAP-paired receivers.** Sending
> `/reverse` + `/play` straight after pair-verify does not start playback on modern tvOS. The order below is taken
> from pyatv's `AirPlayV2.play_url` (`pyatv/protocols/raop/protocols/airplayv2.py`),
> cross-checked against the AP2 sender recipe in `akustikrausch/airplay2-sender-cpp`
> and the session handling in `omarroth/doubletake` (tested on AppleTV11,1 / tvOS 27).

All steps use **one** TCP connection to port 7000 (the "control connection"),
plus one outbound TCP connection to the receiver's `eventPort`. The control
connection must stay open for the whole playback: closing it stops the video.

| # | Step | Notes |
|---|------|-------|
| 1 | `GET /info` (plaintext, optional) | Feature flags, `pk`, display info |
| 2 | `POST /pair-verify` M1–M4 (pair-setup first if no stored credentials) | See 03-authentication.md |
| 3 | **Encrypt the control connection immediately** | ChaCha20-Poly1305, `Control-Salt` / `Control-Write/Read-Encryption-Key`. The receiver drops the connection ~1 ms after pair-verify if the next request is plaintext. |
| 4 | Start a local UDP NTP timing responder | Its port goes in the SETUP body as `timingPort` |
| 5 | `SETUP rtsp://<local-ip>/<session-id> RTSP/1.0` with the session plist | **Must be RTSP `SETUP`**, not `POST /setup` (404). Response contains `eventPort`. |
| 6 | Connect TCP to `eventPort`, encrypt it | HKDF salt `Events-Salt`; keys are **swapped** relative to control: we *read* with `Events-Write-Encryption-Key` and *write* with `Events-Read-Encryption-Key` |
| 7 | Serve the event channel | Receiver pushes `POST /command` (e.g. `updateInfo`). Reply `RTSP/1.0 200 OK` with only `Server` + `CSeq` headers. **This is the keep-alive** — unanswered, the session is torn down after ~25–30 s. |
| 8 | Start `POST /feedback RTSP/1.0` every 2 s on the control connection | Best effort; does not replace step 7 |
| 9 | `RECORD rtsp://<local-ip>/<session-id> RTSP/1.0` | Must come after the event channel is up (otherwise `RECORD` → 500) |
| 10 | `POST /play` (binary plist, see below) | Retry on HTTP 500 (pyatv retries up to 3× with 1 s delay) |
| 11 | `PUT /setProperty?isInterestedInDateRange` `{value: true}`, `PUT /setProperty?actionAtItemEnd` `{value: 0}` | |
| 12 | `POST /rate?value=1.000000` | **Required** — otherwise the item loads paused |
| 13 | Poll `GET /playback-info` every 1 s until `duration` disappears | Also surfaces `error.code` / `error.domain`. foxCast treats 500 before the item exists as "not ready yet" and gives up after 15 polls without a `duration` |

pyatv's RTSP requests carry `CSeq`, `DACP-ID`, `Active-Remote`,
`Client-Instance`, and `User-Agent: AirPlay/550.10`. foxCast (`internal/sender/playback.go`)
sends `CSeq`, doubletake's `User-Agent: AirPlay/935.7.1` and `Content-Length`,
plus random per-session `DACP-ID`/`Active-Remote`/`Client-Instance` headers on
SETUP, RECORD, `/feedback`, TEARDOWN, `GET /info` and the `/rate`, `/scrub`,
`/stop`, `setProperty` and `/playback-info` requests (not on `/play` or
`/command`). Its event channel replies carry `CSeq` and `Content-Length: 0`
(no `Server`). `/play`, `/command`, `GET /info` and `GET /playback-info` are
sent with an `HTTP/1.1` request line, everything else with `RTSP/1.0`, all on
the same encrypted connection. After `/rate` (step 12) foxCast also sets
`forwardEndTime` and `reverseEndTime` to an empty CMTime, best effort.

### Session SETUP body (step 5)

```
{
  deviceID:                 "AA:BB:CC:DD:EE:FF",   // sender MAC-style ID
  sessionUUID:              <UUID, upper-case>,
  timingPort:               <local UDP port>,
  timingProtocol:           "NTP",
  isMultiSelectAirPlay:     true,
  groupContainsGroupLeader: false,
  macAddress:               "AA:BB:CC:DD:EE:FF",
  model:                    "iPhone14,3",
  name:                     <sender hostname>,
  osBuildVersion:           "20F66",
  osName:                   "iPhone OS",
  osVersion:                "16.5",
  senderSupportsRelay:      false,
  sourceVersion:            "690.7.1",
  statsCollectionEnabled:   false
}
```

### `/play` body (step 10)

Headers: `Content-Type: application/x-apple-binary-plist`,
`X-Apple-ProtocolVersion: 1`, `X-Apple-Stream-ID: 1`, `X-Apple-Session-ID: <UUID>`.

```
{
  Content-Location:       <url>,
  Start-Position-Seconds: <float>,
  uuid:                   <UUID>,
  streamType:             1,
  mediaType:              "file",
  rate:                   1.0,
  volume:                 1.0,
  playbackRestrictions:   0,
  referenceRestrictions:  3,
  mightSupportStorePastisKeyRequests: true,
  SenderMACAddress:       "AA:BB:CC:DD:EE:FF",
  model:                  "iPhone14,3",
  clientBundleID:         <reverse-DNS id>,
  clientProcName:         <reverse-DNS id>,
  osBuildVersion:         "20F66"
  // pyatv also sends timing fields (secureConnectionMs, infoMs, connectMs,
  // authMs, bonjourMs, postAuthMs); believed optional.
}
```

---

## Legacy Connection Setup (AirPlay 1 receivers)

> foxCast does not implement this AirPlay 1 flow (`/reverse`, `POST /event`,
> FCUP below); it is documented for reference.

Older receivers use **two persistent TCP connections** to port 7000:

1. **Main connection** — sender sends commands to receiver
2. **Reverse (event) connection** — receiver sends event notifications back to sender

### Reverse Connection (PTTH)

```
POST /reverse HTTP/1.1
Host: <receiver_ip>:7000
Upgrade: PTTH/1.0
Connection: Upgrade
X-Apple-Purpose: event
X-Apple-Session-ID: <UUID>
User-Agent: MediaControl/1.0
Content-Length: 0

← HTTP/1.1 101 Switching Protocols
Upgrade: PTTH/1.0
Connection: Upgrade
```

After the 101 response, this connection is "reversed": the **receiver** sends HTTP POST requests (specifically `POST /event`) to the sender over this persistent TCP connection. The sender reads these asynchronously.

---

## Playback Commands

All commands use HTTP/1.1 with a consistent `X-Apple-Session-ID` header.

### Play

```
POST /play HTTP/1.1
Content-Type: application/x-apple-binary-plist
X-Apple-Session-ID: <UUID>

{
  Content-Location: "http://example.com/video.m3u8",
  Start-Position: 0.0      // 0.0–1.0 normalized position
  // or: Start-Position-Seconds: 30.5
}
```

### Rate (Play/Pause)

```
POST /rate?value=1.000000 HTTP/1.1
X-Apple-Session-ID: <UUID>

// value: 0.0 = pause, 1.0 = play normal speed
```

### Seek

```
POST /scrub?position=30.5 HTTP/1.1
X-Apple-Session-ID: <UUID>

// position: seconds from start
```

### Stop

```
POST /stop HTTP/1.1
X-Apple-Session-ID: <UUID>
```

### Query Playback State

```
GET /playback-info HTTP/1.1
X-Apple-Session-ID: <UUID>

← 200 OK
Content-Type: application/x-apple-binary-plist

{
  duration:            3600.0,
  position:            30.5,
  rate:                1.0,
  readyToPlay:         true,
  playbackBufferEmpty: false,
  playbackBufferFull:  false,
  playbackLikelyToKeepUp: true,
  loadedTimeRanges:    [{duration: 60.0, start: 0.0}],
  seekableTimeRanges:  [{duration: 3600.0, start: 0.0}]
}
```

### Property Access

```
GET  /getProperty?<key> HTTP/1.1
PUT  /setProperty?<key> HTTP/1.1
Content-Type: application/x-apple-binary-plist
{value: <property_value>}
```

---

## Event Notifications (Legacy Reverse Channel)

On AirPlay 2 receivers events arrive on the encrypted `eventPort` channel
instead (see step 7 above). On AirPlay 1 receivers, the receiver sends `POST /event` requests to the sender over the reverse PTTH connection:

```
POST /event HTTP/1.0
Content-Type: text/x-apple-plist+xml
Content-Length: ...

<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" ...>
<plist version="1.0">
<dict>
  <key>category</key>    <string>video</string>
  <key>state</key>       <string>playing</string>    <!-- or: loading, paused, stopped -->
  <key>duration</key>    <real>3600.0</real>
  <key>position</key>    <real>30.5</real>
</dict>
</plist>
```

The sender must write HTTP responses back to the receiver over this connection:
```
HTTP/1.1 200 OK
```

---

## FCUP (URL Forwarding)

When the Apple TV encounters a URL it cannot handle during HLS playback, it sends a `POST /event` of type `unhandledURLRequest` via the reverse channel:

```xml
<dict>
  <key>sessionID</key>   <integer>1</integer>
  <key>type</key>        <string>unhandledURLRequest</string>
  <key>request</key>
  <dict>
    <key>FCUP_Response_RequestID</key>  <integer>1</integer>
    <key>FCUP_Response_URL</key>        <string>http://...</string>
    <key>FCUP_Response_ClientRef</key>  <integer>1</integer>
  </dict>
</dict>
```

The sender responds by fetching the URL and sending back the response via `POST /fp-frizz`:

```
POST /fp-frizz HTTP/1.1
Content-Type: application/x-apple-binary-plist
X-Apple-Session-ID: <UUID>

{
  FCUP_Response_RequestID: <int>,
  FCUP_Response_Data:      <fetched data>,
  FCUP_Response_Status:    200,
  FCUP_Response_Headers:   {Content-Type: "..."}
}
```

This is primarily needed for DRM-protected or custom HLS manifests. For direct video URLs, FCUP is typically not needed.

---

## Supported Formats

The Apple TV's own media player (AVFoundation) handles format support:
progressive MP4/M4V/MOV, and HLS with MPEG-TS or fMP4 segments. It has no
Matroska demuxer, so foxCast transmuxes MKV/WebM files to HLS on the fly
(without re-encoding); see [09-matroska-transmuxing.md](09-matroska-transmuxing.md)
for the codec matrix and design.

Local files must be served over HTTP since the Apple TV fetches the URL directly;
foxCast runs a local HTTP server for them.
