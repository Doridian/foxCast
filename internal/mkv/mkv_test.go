package mkv

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func openFixture(t *testing.T, name string) (*File, *os.File) {
	t.Helper()
	fh, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fh.Close() })
	st, err := fh.Stat()
	if err != nil {
		t.Fatal(err)
	}
	f, err := Open(fh, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	return f, fh
}

// readAll returns every frame of the file, by track.
func readAll(t *testing.T, f *File, fh *os.File) map[uint64][]*Frame {
	t.Helper()
	cr := f.NewClusterReader(io.NewSectionReader(fh, f.FirstCluster, f.Size()-f.FirstCluster), f.FirstCluster)
	out := map[uint64][]*Frame{}
	for {
		fr, err := cr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[fr.Track] = append(out[fr.Track], fr)
	}
}

func TestParseVints(t *testing.T) {
	for _, tc := range []struct {
		in   []byte
		size int64
		n    int
	}{
		{[]byte{0x81}, 1, 1},
		{[]byte{0x40, 0x01}, 1, 2},
		{[]byte{0x10, 0x00, 0x00, 0x7F}, 127, 4},
		{[]byte{0xFF}, unknownSize, 1},
		{[]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, unknownSize, 8},
	} {
		size, n, err := parseSize(tc.in)
		if err != nil || size != tc.size || n != tc.n {
			t.Errorf("parseSize(%x) = %d, %d, %v; want %d, %d", tc.in, size, n, err, tc.size, tc.n)
		}
	}
	id, n, err := parseID([]byte{0x1F, 0x43, 0xB6, 0x75})
	if err != nil || id != idCluster || n != 4 {
		t.Errorf("parseID(Cluster) = %X, %d, %v", id, n, err)
	}
	if _, _, err := parseSize([]byte{0x00}); err == nil {
		t.Error("parseSize(0x00) should fail")
	}
}

func TestUnlace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lacing byte
		in     []byte
		want   []string
	}{
		// 3 frames, Xiph sizes 2 and 256 (0xFF 0x01), remainder 1.
		{"xiph", lacingXiph, append(append([]byte{2, 2, 0xFF, 0x01}, "ab"...), append(bytes.Repeat([]byte("x"), 256), 'z')...),
			[]string{"ab", string(bytes.Repeat([]byte("x"), 256)), "z"}},
		// 3 frames, EBML sizes 3 (0x83) then delta −1 (0xBE: 0x3E − 63).
		{"ebml", lacingEBML, append([]byte{2, 0x83, 0xBE}, "abcdeZ"...), []string{"abc", "de", "Z"}},
		{"fixed", lacingFixed, append([]byte{1}, "abcd"...), []string{"ab", "cd"}},
		{"none", lacingNone, []byte("abc"), []string{"abc"}},
	} {
		got, err := unlace(tc.in, tc.lacing)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: %d frames, want %d", tc.name, len(got), len(tc.want))
			continue
		}
		for i := range got {
			if string(got[i]) != tc.want[i] {
				t.Errorf("%s frame %d = %q, want %q", tc.name, i, got[i], tc.want[i])
			}
		}
	}
	if _, err := unlace([]byte{1, 0x05, 'a'}, lacingXiph); err == nil {
		t.Error("overrunning Xiph lace should fail")
	}
}

func TestHeaderStripping(t *testing.T) {
	tr := &Track{stripped: []byte{0x0B, 0x77}}
	got, err := tr.decodeFrame([]byte{0x01, 0x02})
	if err != nil || !bytes.Equal(got, []byte{0x0B, 0x77, 0x01, 0x02}) {
		t.Fatalf("decodeFrame = %x, %v", got, err)
	}
}

func TestOpenH264AAC(t *testing.T) {
	f, fh := openFixture(t, "h264_aac.mkv")
	if f.DocType != "matroska" || f.TimestampScale != 1_000_000 {
		t.Errorf("DocType %q, scale %d", f.DocType, f.TimestampScale)
	}
	if f.Duration < 2900*time.Millisecond || f.Duration > 3100*time.Millisecond {
		t.Errorf("Duration = %v", f.Duration)
	}
	if len(f.Tracks) != 2 {
		t.Fatalf("%d tracks", len(f.Tracks))
	}
	v, a := f.Tracks[0], f.Tracks[1]
	if v.Type != TrackVideo || v.CodecID != "V_MPEG4/ISO/AVC" || v.Video.PixelWidth != 64 || v.Video.PixelHeight != 48 || len(v.CodecPrivate) == 0 {
		t.Errorf("video track = %+v", v)
	}
	if a.Type != TrackAudio || a.CodecID != "A_AAC" || a.Audio.SamplingFrequency != 44100 || a.Audio.Channels != 1 {
		t.Errorf("audio track = %+v / %+v", a, a.Audio)
	}

	// keyint=12 at 24 fps: a keyframe every 500 ms.
	kf, err := f.Keyframes(v.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(kf) != 6 {
		t.Fatalf("%d keyframes, want 6", len(kf))
	}
	for i, k := range kf {
		if k.Time != time.Duration(i)*500*time.Millisecond {
			t.Errorf("keyframe %d at %v", i, k.Time)
		}
	}

	frames := readAll(t, f, fh)
	if len(frames[v.Number]) != 72 || len(frames[a.Number]) != 131 {
		t.Errorf("frames: video %d (want 72), audio %d (want 131)", len(frames[v.Number]), len(frames[a.Number]))
	}
	// Every cue must point at a keyframe with the cue's timestamp.
	for _, k := range kf {
		found := false
		for _, fr := range frames[v.Number] {
			if fr.Pos == k.Pos {
				found = fr.Keyframe && fr.PTS == k.Time
			}
		}
		if !found {
			t.Errorf("cue %+v does not match a keyframe", k)
		}
	}
}

func TestScanKeyframesMatchesCues(t *testing.T) {
	f, _ := openFixture(t, "h264_aac.mkv")
	cues, err := f.Keyframes(1)
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := f.scanKeyframes(1)
	if err != nil {
		t.Fatal(err)
	}
	// The scan only finds the first keyframe per cluster; every one of them
	// must be a cue point.
	if len(scanned) == 0 {
		t.Fatal("scan found no keyframes")
	}
	for _, s := range scanned {
		ok := false
		for _, c := range cues {
			ok = ok || (c.Pos == s.Pos && c.Time == s.Time)
		}
		if !ok {
			t.Errorf("scanned keyframe %+v is not a cue point", s)
		}
	}
}

func TestOpenHEVCMulti(t *testing.T) {
	f, fh := openFixture(t, "hevc_multi.mkv")
	want := []struct {
		codec  string
		frames int
	}{
		{"V_MPEGH/ISO/HEVC", 48}, {"A_AC3", 63}, {"A_EAC3", 63}, {"A_FLAC", 94}, {"A_OPUS", 101},
	}
	if len(f.Tracks) != len(want) {
		t.Fatalf("%d tracks", len(f.Tracks))
	}
	frames := readAll(t, f, fh)
	for i, w := range want {
		tr := f.Tracks[i]
		if tr.CodecID != w.codec {
			t.Errorf("track %d codec %s, want %s", tr.Number, tr.CodecID, w.codec)
		}
		// The AC-3 track carries an empty ContentEncoding (no-op).
		if err := tr.Supported(); err != nil {
			t.Errorf("track %d: %v", tr.Number, err)
		}
		if got := len(frames[tr.Number]); got != w.frames {
			t.Errorf("track %d: %d frames, want %d", tr.Number, got, w.frames)
		}
	}
	for _, fr := range frames[2] {
		if !bytes.HasPrefix(fr.Data, []byte{0x0B, 0x77}) {
			t.Fatalf("AC-3 frame at %v lacks sync word: %x", fr.PTS, fr.Data[:4])
		}
	}
	// The AC-3 track is laced (8 frames per block): timestamps must still
	// advance by the 32 ms DefaultDuration.
	ac3 := frames[2]
	for i := 1; i < len(ac3); i++ {
		if d := ac3[i].PTS - ac3[i-1].PTS; d != 32*time.Millisecond {
			t.Fatalf("AC-3 frame %d delta %v", i, d)
		}
	}
	// mkvmerge drops FFmpeg's colour description (the bitstream VUI still has
	// it), like real remuxes; transmux falls back to the SPS for that.
	if c := f.Tracks[0].Video.Colour; c != nil && c.TransferCharacteristics != 0 {
		t.Errorf("fixture unexpectedly has container colour %+v", c)
	}
}

func TestStopAt(t *testing.T) {
	f, fh := openFixture(t, "h264_aac.mkv")
	kf, err := f.Keyframes(1)
	if err != nil {
		t.Fatal(err)
	}
	all := readAll(t, f, fh)
	total := len(all[1]) + len(all[2])
	// Reading every keyframe interval separately must yield each frame once.
	// The first interval starts at the first cluster: blocks can precede the
	// first keyframe.
	kf[0].Pos = Position{Cluster: f.FirstCluster}
	seen := 0
	for i := range kf {
		cr := f.NewClusterReader(io.NewSectionReader(fh, kf[i].Pos.Cluster, f.Size()), kf[i].Pos.Cluster)
		if i+1 < len(kf) {
			cr.StopAt = &kf[i+1].Pos
		}
		for {
			fr, err := cr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if fr.Pos.Less(kf[i].Pos) {
				continue
			}
			seen++
		}
	}
	if seen != total {
		t.Errorf("saw %d frames across intervals, want %d", seen, total)
	}
}
