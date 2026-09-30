# Matroska → HLS Transmuxing

URL playback hands the receiver a URL and the receiver's own AVFoundation
player fetches and decodes it (see [05-video-url-playback.md](05-video-url-playback.md)).
AVFoundation has **no Matroska demuxer**, so an `.mkv` URL fails even when its
codecs are ones the Apple TV decodes in hardware. foxCast therefore rewraps
Matroska files as an HLS VOD presentation with fragmented-MP4 segments, on the
fly, **without re-encoding anything** (`internal/transmux`).

## What the receiver can play

AVFoundation on tvOS accepts, over a URL:

| Container | Notes |
|-----------|-------|
| MP4 / M4V / MOV (progressive) | Played directly; foxCast serves local files as-is |
| HLS, MPEG-TS segments | H.264 + AAC/AC-3/E-AC-3/MP3 only (no HEVC in TS) |
| HLS, fMP4/CMAF segments | Everything in the table below — **this is what foxCast emits** |
| Matroska / WebM, AVI, FLV, … | **Not supported** |

Codecs in HLS fMP4 (Apple's *HLS Authoring Specification for Apple Devices*,
and the codec list in its *General Authoring Requirements*):

| Kind | Supported | Not supported (would need transcoding) |
|------|-----------|-----------------------------------------|
| Video | H.264 (`avc1`), HEVC (`hvc1`, parameter sets in `hvcC`), Dolby Vision profile 5 and 8.x (`dvh1`) | VP9, AV1 (no hardware decoder before A17/M3; the AppleTV11,1 and AppleTV14,1 lack it), MPEG-2, VC-1, Dolby Vision profile 7 as such |
| Audio | AAC-LC/HE-AAC (`mp4a.40.x`), AC-3 (`ac-3`), E-AC-3 incl. Atmos/JOC (`ec-3`), FLAC (`fLaC`), ALAC (`alac`); Opus and MP3 unverified (see below) | **Dolby TrueHD, DTS / DTS-HD**, LPCM, Vorbis |
| Subtitles | WebVTT, IMSC1 text (as HLS subtitle renditions); foxCast converts SRT and ASS/SSA to WebVTT | PGS/VobSub/DVB (bitmap); IMSC1 image profile (verified: the receiver rejects the presentation) |

AAC, AC-3/E-AC-3 and HEVC/DV are the long-standing, safest choices. Apple's
spec does not list Opus for HLS, and documents MP3 for MPEG-TS segments only;
foxCast still offers both (as `Opus` and `mp4a.6B` fMP4 renditions), so treat
them as unverified until tested on hardware.

> **Hardware status:** a UHD Blu-ray remux (HEVC Main 10, Dolby Vision
> profile 7 played as its HDR10 base layer, open GOPs, AC-3 5.1 audio) plays
> on an AppleTV11,1 / tvOS 27, which confirms the fMP4 layout including
> negative composition offsets (`trun` v1). Still unverified on hardware: the
> `dvh1` sample entry for profile 5/8, H.264, and FLAC/Opus/MP3/ALAC audio.

## Architecture

```
source (local path / SMB mount, or HTTP(S) with Range)   internal/mediasource
   └─ Matroska demuxer: header, tracks, Cues, cluster reader   internal/mkv
        └─ segment plan + per-segment remux + HLS HTTP server   internal/transmux
             ├─ codec config records, codec strings, frame sizes  internal/codec
             └─ fMP4 init/media segment writer                    internal/fmp4
```

`foxCast play <file.mkv|https://…/file.mkv>` sniffs the EBML magic
(`1A 45 DF A3`), builds the session, serves it with the built-in HTTP server
(`internal/fileserver`; `-http-port` pins its port) and hands the receiver
`http://<local-ip>:<port>/master.m3u8` as the play-queue item (or in `/play`).
The receiver then pulls playlists and segments from foxCast, which pulls byte
ranges from the source. A remote URL whose server does not answer Range
requests is passed to the receiver untouched; `-transmux=false` disables
transmuxing. `foxCast probe` prints the track table without connecting;
`foxCast serve` serves the HLS presentation for testing with other players.

### Served paths

```
/master.m3u8                                  multivariant playlist
/video.m3u8   /video/init.mp4   /video/<n>.m4s
/audio/<track>.m3u8   /audio/<track>/init.mp4   /audio/<track>/<n>.m4s
/subs/<track>.m3u8    /subs/<track>/<n>.vtt
```

Every compatible audio track becomes an `EXT-X-MEDIA` rendition in one group,
so the Apple TV's audio menu lists them all (`AUTOSELECT=YES` lets it follow
the user's language preference). The first track flagged default is
`DEFAULT=YES`; `-audio 3,2` offers only the listed tracks, the first as the
default. An audio segment request for a stretch without audio is answered
with 404, since an empty fragment is invalid. The variant's `CODECS` is the union of the
video codec and all rendition codecs, which RFC 8216 §4.3.4.2 permits. Audio
is demuxed from video (separate playlists), as Apple recommends.

### Subtitles

Text subtitle tracks (SRT `S_TEXT/UTF8`, ASS/SSA, WebVTT) become
`EXT-X-MEDIA TYPE=SUBTITLES` renditions with WebVTT segments that share the
video's segment boundaries; the variant carries `SUBTITLES="subs"`.
Forced tracks get `FORCED=YES`, which tvOS shows automatically when the
language matches the audio (verified on AppleTV11,1 / tvOS 27: the receiver
fetched the `.vtt` segments of a forced English track unprompted).

- Cues are collected by block position in the same segment read as audio, so
  subtitles cost no extra source reads. A cue belongs to the segment its block
  is in; one that runs past a segment boundary is not repeated in the next
  segment, so starting playback in the middle of it does not show it.
- Each segment starts `WEBVTT` + `X-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000`;
  cue times are absolute, matching the fMP4 timeline (which starts at 0).
- SRT: `<i>`, `<b>`, `<u>` are kept, other tags (e.g. `<font>`) dropped, the
  rest escaped. ASS/SSA: the Text field of the block's Dialogue fields, with
  override blocks removed except italic/bold, drawings (`\p1`) skipped and
  `\N`/`\n` as line breaks. Positioning and styling are not carried.
- **Bitmap subtitles (PGS, VobSub, DVB) are not supported.** Apple's HLS
  accepts WebVTT and IMSC1 *text profile* (`stpp.ttml.im1t`) only. Tested on
  AppleTV11,1 / tvOS 27 (2026-09-25) with the same HLS stream carrying one
  forced IMSC1 subtitle rendition in fMP4 (`stpp`): declared as
  `stpp.ttml.im1t` it plays; declared as `stpp.ttml.im1i` (image profile,
  PNG in `smpte:image`) the receiver reads the playlists and abandons the
  whole presentation, so image subtitles cannot even be offered alongside
  video. PGS would need OCR to text, or burning into the video
  (re-encoding), neither of which foxCast does.

### Opening a file (small reads only)

1. EBML header → Segment → top-level elements until the first Cluster.
2. SeekHead(s) locate Info, Tracks and Cues (mkvmerge writes Cues at the end).
3. Cues give video keyframe positions as `(CueClusterPosition,
   CueRelativePosition)`. Without Cues, clusters are walked with header reads
   only, recording each cluster's first video keyframe.
4. For every audio track the first frame is read (AC-3/E-AC-3 need a syncframe
   for `dac3`/`dec3`; it also anchors the audio frame grid, below).

Over HTTP, small reads go through a 256 KiB chunk cache; opening an 87 GB file
reads well under a megabyte.

### Segment plan

Keyframes are grouped into segments of at least 6 s (Apple's recommendation).
A segment is **every block in file order from its keyframe's position up to the
next segment's keyframe position**, where a position is (cluster offset,
offset within the cluster). Assigning blocks by position rather than by
timestamp means every block, of every track, lands in exactly one segment;
interleaving quirks cannot duplicate or drop audio. The first segment starts
at the first cluster, since audio can precede the first keyframe.

Each segment maps to one contiguous byte range, fetched with a single request
(or one `SectionReader` for files). `EXTINF` durations come from cue times; the
last segment ends at the Info duration.

Segments are loaded once and shared by the concurrent video and audio
requests for them. The next segment is prefetched when a video segment is
served, and the five most recent are cached (roughly 50 MB each for a UHD
Blu-ray remux). Only the served tracks' data is read into memory: SimpleBlocks
of unused tracks (TrueHD, DTS, bitmap subtitles, …) are skipped without being
copied.

### Video timing

Matroska stores presentation timestamps only. Within a segment the decode
timestamps are the **sorted presentation timestamps**; composition offsets are
`pts − dts` and may be negative, so `trun` is version 1 (as CMAF allows). This
keeps decode time continuous across segments even for **open GOPs** (HEVC CRA
with RASL leading pictures, as in UHD Blu-ray streams): the leading pictures'
PTS fall between the previous segment's last PTS and the CRA's. The last frame
of a segment gets the track's DefaultDuration. Timescale is 90 kHz.

FFmpeg's MP4 demuxer shifts PTS by the largest negative offset, so ffprobe
reports keyframe PTS one or two frames late; that is a display artefact.

### Audio timing

Matroska timestamps are rounded (1 ms by default), which would leave
sub-millisecond gaps or overlaps between fragments. For codecs with a constant
frame length (AAC 1024/960, AC-3 1536, E-AC-3 from `numblkscod`, MP3, Opus from
the first TOC, FLAC fixed-blocksize streams) each fragment's `tfdt` is
snapped to the frame grid anchored at the track's first frame, so consecutive
fragments line up sample-exactly. Per-sample durations come from the codec
(frame headers), not from timestamps. Timescale is the codec sample rate.

### HDR and Dolby Vision

`VIDEO-RANGE` (PQ/HLG/SDR) and the `colr` box come from the Matroska Colour
element or, if absent (mkvmerge often drops it), from the HEVC SPS VUI.
`mdcv`/`clli` are written when the container has mastering metadata; the
bitstream SEI carries it regardless.

Dolby Vision configuration is read from the track's BlockAdditionMapping
(`dvcC`/`dvvC`):

| Profile | Handling |
|---------|----------|
| 5 | `dvh1` + `hvcC` + `dvcC`, `CODECS=dvh1.05.LL`, PQ |
| 8.x | `dvh1` + `hvcC` + `dvvC`, `CODECS=dvh1.08.LL` |
| 7 (UHD Blu-ray, dual layer) | Not playable on Apple TV. RPU (NAL 62) and enhancement layer (NAL 63) are stripped and the HDR10 base layer is played as `hvc1` |
| any, with `-dolby-vision=false` | As profile 7 (profile 5 is refused: its base layer is not HDR10) |

Converting profile 7 to 8.1 would require rewriting the RPU (as `dovi_tool`
does) and is not implemented.

### Matroska details handled

- Xiph, EBML and fixed lacing (mkvmerge laces AC-3 and other audio)
- Header-removal compression (prepended back) and zlib content compression;
  an empty ContentEncoding (written by mkvmerge when header-removal analysis
  finds nothing) is treated as a no-op, whereas FFmpeg rejects the track
- SimpleBlock and BlockGroup (keyframe = no ReferenceBlock)
- Unknown-size clusters (live-muxed files)
- Legacy `A_AAC/MPEG4/LC`-style codec IDs without CodecPrivate (an
  AudioSpecificConfig is synthesized)

## Not (yet) supported

- Bitmap subtitles (PGS/VobSub/DVB), see above.
- TrueHD/DTS audio: would need audio transcoding (e.g. to E-AC-3 or AAC). Most
  UHD/Blu-ray remuxes also carry an AC-3 track, which is offered instead.
- Chapters, attachments, and video codecs other than H.264/HEVC.
