# RTMP Ingest: Streaming to a Receiver from OBS

`foxCast rtmp` turns a receiver into an RTMP destination. It connects and
starts a screen mirroring session straight away, which shows a "Waiting for a
stream…" card; the URL to publish to (`rtmp://127.0.0.1:1935/live` by
default) is logged. Anything a local client publishes there then plays on the
receiver. When the client stops or drops, the
card comes back and the session stays up for the next client.

```bash
foxCast rtmp -target 192.168.1.20          # listens on 127.0.0.1:1935
foxCast rtmp -listen 127.0.0.1:1936 ...    # another port (loopback only)
```

In OBS: Settings → Stream → Service "Custom…", Server
`rtmp://127.0.0.1:1935/live`, any stream key. The key and app are logged but
not checked, because the listener only accepts loopback addresses. RTMP has no
authentication, so `-listen` rejects anything else.

## Design

- **Session:** this is the Wayland mirror session's compositor pipeline
  (`capture_mixer.go`), with no display involved (`PrepareIngestCapture`). The
  encoder, its parameter sets and the receiver's decoder run once for the whole
  session.
- **Placeholder:** it stays on the compositor's first pad all the time. A
  playing stream is a second pad above it at full alpha, with no fade. So
  whenever no stream is on top, the receiver shows the card rather than black.
- **One decode pipeline per connection** (`ingest.go`):
  `fdsrc ! flvdemux ! decodebin ! videoscale ! intervideosink` for video, plus
  `decodebin ! pulsesink` into the receiver's virtual output device for audio
  (a `fakesink` when there is no session audio or no `pulsesink`).
  The session reads that output device's monitor, as for `-audio-source sink`.
  The device is created without becoming the desktop's default output. The
  session shows the frames through an `intervideosrc` pad, which is added once
  the first frame has decoded. A malformed stream (a decoder error) ends only
  its own pipeline. The AirPlay session never restarts, so clients can
  reconnect indefinitely.
- **Sync:** neither decode sink syncs to a clock. The publisher paces the
  stream in real time, the compositor takes the newest frame, and audio goes
  through PulseAudio as it would during desktop mirroring. A client that sends
  faster than real time (ffmpeg without `-re`) therefore plays fast.
- **Replacement:** the newest publisher wins. A client that reconnects while
  its old connection is still open replaces it at once. A client that sends
  nothing for 10 s is dropped.

## RTMP subset (`internal/rtmp`)

Written from the Adobe RTMP 1.0 specification, the AMF0 specification, and
FLV 10.1 annex E. It has been tested against ffmpeg's RTMP client
(`TestFFmpegPublish`), not against OBS itself.

- **Handshake:** the simple handshake. S1 carries a zero version field, which
  makes librtmp (OBS's client) skip its FP9 digest check and use the simple
  handshake too. S2 echoes C1, and C2 is ignored.
- **Chunk stream:** header types 0–3, 1–3 byte basic headers, and extended
  timestamps, including their repetition on type 3 continuations.
  Set Chunk Size, Abort, Window Acknowledgement Size (acknowledged per window)
  and ping requests are handled.
- **Commands:**
  - `connect` is answered with Window Ack Size, Set Peer Bandwidth,
    Set Chunk Size 4096 and `_result` `NetConnection.Connect.Success`.
  - `createStream` returns stream ID 1.
  - `publish` gets StreamBegin and `onStatus` `NetStream.Publish.Start`.
  - `releaseStream` and `FCPublish` need no answer.
  - `play` is refused.
  - `FCUnpublish`, `deleteStream` and `closeStream` end the stream.
- **Media:** audio (8) and video (9) messages become FLV tags with their
  absolute timestamps. `@setDataFrame` metadata is dropped, since flvdemux
  needs only the sequence headers.
- **Codecs:** whatever flvdemux and decodebin handle. H.264/AAC (OBS's default)
  is tested. Enhanced-RTMP HEVC/AV1 depends on the installed GStreamer's
  flvdemux.

An AVC end-of-sequence tag (sent when a client stops) makes flvdemux end its
stream, which ends that decode pipeline normally.
