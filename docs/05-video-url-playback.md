# Video URL Playback

This mode instructs an Apple TV to fetch and play a remote URL. The Apple TV downloads the video itself; the sender only sends control commands. This is the simplest way to play video on an Apple TV and is the recommended first implementation target.

## Connection Setup

Requires **two persistent TCP connections** to port 7000:

1. **Main connection** — sender sends commands to receiver
2. **Reverse (event) connection** — receiver sends event notifications back to sender

Both connections go through HAP session encryption after pair-verify.

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

## Event Notifications (Reverse Channel)

The receiver sends `POST /event` requests to the sender over the reverse PTTH connection:

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

The Apple TV's own media player handles format support. Recommended formats for broad compatibility:

- HLS (`.m3u8`) — best support
- MP4/M4V with H.264
- MOV

Local files must be served over HTTP since the Apple TV fetches the URL directly. The sender may need to run a local HTTP server to serve local files.
