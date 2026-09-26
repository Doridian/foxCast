# Matroska test fixtures

Generated with FFmpeg 8 and mkvmerge v100 (the mkvmerge pass gives real-world
cues, lacing, and an empty ContentEncoding on the AC-3 track):

```sh
# h264_aac.mkv — 64x48 H.264 High (keyint 12, 2 B-frames) + mono AAC, 3 s
ffmpeg -f lavfi -i testsrc=size=64x48:rate=24 -f lavfi -i sine=frequency=440:sample_rate=44100 -t 3 \
  -c:v libx264 -profile:v high -x264-params keyint=12:min-keyint=12:bframes=2:scenecut=0 -pix_fmt yuv420p \
  -c:a aac -b:a 32k -ac 1 raw1.mkv
mkvmerge -o h264_aac.mkv raw1.mkv

# hevc_multi.mkv — 64x64 HEVC Main 10, PQ/BT.2020 VUI, open GOP (keyint 12, 3 B-frames)
#   + AC-3 5.1, E-AC-3 5.1, FLAC mono, Opus stereo, 2 s
ffmpeg -f lavfi -i testsrc2=size=64x64:rate=24 -f lavfi -i sine=frequency=300:sample_rate=48000 -t 2 \
  -map 0:v -map 1:a -map 1:a -map 1:a -map 1:a -c:v libx265 -pix_fmt yuv420p10le -crf 40 \
  -x265-params "keyint=12:min-keyint=12:bframes=3:open-gop=1:scenecut=0:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc" \
  -c:a:0 ac3 -ac:a:0 6 -b:a:0 192k -c:a:1 eac3 -ac:a:1 6 -b:a:1 192k \
  -c:a:2 flac -ac:a:2 1 -sample_fmt:a:2 s16 -c:a:3 libopus -ac:a:3 2 -b:a:3 32k raw2.mkv
mkvmerge -o hevc_multi.mkv --compression 1:analyze_header_removal raw2.mkv
```

Reference frame counts (from `ffprobe -count_packets` on the pre-mkvmerge
files): h264_aac 72 video / 131 audio; hevc_multi 48 video, 63 AC-3, 63 E-AC-3,
94 FLAC, 101 Opus. The `dac3`/`dec3`/`dfLa`/`dOps` vectors in
`internal/codec/codec_test.go` come from FFmpeg's MP4 muxer on these streams.
