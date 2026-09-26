package transmux

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/fmp4"
	"git.foxden.network/FoxDen/foxCast/internal/mediasource"
)

const fixtures = "../mkv/testdata/"

func newTestSession(t *testing.T, name string, opts Options) (*Session, *httptest.Server) {
	t.Helper()
	src, err := mediasource.Open(context.Background(), fixtures+name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	if !IsMatroska(src) {
		t.Fatal("fixture not detected as Matroska")
	}
	s, err := NewSession(context.Background(), src, opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

func get(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s: %s", url, resp.Status, body)
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Fatalf("GET %s: Content-Length %s, body %d", url, cl, len(body))
	}
	return body
}

// child returns the payload of the first box of typ in b.
func child(t *testing.T, b []byte, typ string) []byte {
	t.Helper()
	for len(b) >= 8 {
		size := int(binary.BigEndian.Uint32(b))
		if size < 8 || size > len(b) {
			t.Fatalf("bad box size %d", size)
		}
		if string(b[4:8]) == typ {
			return b[8:size]
		}
		b = b[size:]
	}
	t.Fatalf("no %s box", typ)
	return nil
}

type parsedFragment struct {
	tfdt    uint64
	samples []fmp4.Sample
}

func parseFragment(t *testing.T, seg []byte) parsedFragment {
	t.Helper()
	traf := child(t, child(t, seg, "moof"), "traf")
	tfdt := child(t, traf, "tfdt")
	trun := child(t, traf, "trun")
	flags := binary.BigEndian.Uint32(trun) & 0xFFFFFF
	n := int(binary.BigEndian.Uint32(trun[4:]))
	p := trun[12:]
	mdat := child(t, seg, "mdat")
	f := parsedFragment{tfdt: binary.BigEndian.Uint64(tfdt[4:])}
	for i := 0; i < n; i++ {
		var s fmp4.Sample
		s.Duration = binary.BigEndian.Uint32(p)
		size := binary.BigEndian.Uint32(p[4:])
		s.Flags = binary.BigEndian.Uint32(p[8:])
		p = p[12:]
		if flags&0x800 != 0 {
			s.CTO = int32(binary.BigEndian.Uint32(p))
			p = p[4:]
		}
		s.Data, mdat = mdat[:size], mdat[size:]
		f.samples = append(f.samples, s)
	}
	if len(mdat) != 0 {
		t.Fatalf("%d trailing mdat bytes", len(mdat))
	}
	return f
}

var extinf = regexp.MustCompile(`#EXTINF:([0-9.]+),\n(\S+)`)

// fetchTrack downloads every segment listed in a media playlist.
func fetchTrack(t *testing.T, base, playlist string) (init []byte, frags []parsedFragment, total float64) {
	t.Helper()
	pl := string(get(t, base+"/"+playlist))
	dir := playlist[:strings.LastIndex(playlist, "/")+1]
	if !strings.Contains(pl, "#EXT-X-ENDLIST") || !strings.Contains(pl, "#EXT-X-PLAYLIST-TYPE:VOD") {
		t.Fatalf("not a VOD playlist:\n%s", pl)
	}
	m := regexp.MustCompile(`#EXT-X-MAP:URI="([^"]+)"`).FindStringSubmatch(pl)
	init = get(t, base+"/"+dir+m[1])
	for _, e := range extinf.FindAllStringSubmatch(pl, -1) {
		d, _ := strconv.ParseFloat(e[1], 64)
		total += d
		frags = append(frags, parseFragment(t, get(t, base+"/"+dir+e[2])))
	}
	return init, frags, total
}

func countSamples(frags []parsedFragment) int {
	n := 0
	for _, f := range frags {
		n += len(f.samples)
	}
	return n
}

// checkContinuity verifies each fragment starts where the previous one ended
// (within tolerance ticks).
func checkContinuity(t *testing.T, name string, frags []parsedFragment, tolerance int64) {
	t.Helper()
	for i := 1; i < len(frags); i++ {
		end := frags[i-1].tfdt
		for _, s := range frags[i-1].samples {
			end += uint64(s.Duration)
		}
		if gap := int64(frags[i].tfdt) - int64(end); gap < -tolerance || gap > tolerance {
			t.Errorf("%s: fragment %d starts at %d, previous ended at %d", name, i, frags[i].tfdt, end)
		}
	}
}

func TestH264AAC(t *testing.T) {
	opts := DefaultOptions()
	opts.SegmentDuration = time.Second
	s, srv := newTestSession(t, "h264_aac.mkv", opts)

	master := string(get(t, srv.URL+s.MasterPath()))
	for _, want := range []string{
		`CODECS="avc1.64000A,mp4a.40.2"`, `RESOLUTION=64x48`, `FRAME-RATE=24.000`, `VIDEO-RANGE=SDR`,
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="Track 2 (AAC Mono)",DEFAULT=YES`, `URI="audio/2.m3u8"`,
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master playlist lacks %s:\n%s", want, master)
		}
	}

	_, video, vdur := fetchTrack(t, srv.URL, "video.m3u8")
	if len(video) != 3 {
		t.Errorf("%d video segments, want 3", len(video))
	}
	if got := countSamples(video); got != 72 {
		t.Errorf("%d video samples, want 72", got)
	}
	for i, f := range video {
		if f.samples[0].Flags != fmp4.SampleFlagsSync {
			t.Errorf("video segment %d does not start with a sync sample", i)
		}
	}
	// Timestamps are whole milliseconds: allow 1 ms (90 ticks).
	checkContinuity(t, "video", video, 90)

	_, audio, adur := fetchTrack(t, srv.URL, "audio/2.m3u8")
	if got := countSamples(audio); got != 131 {
		t.Errorf("%d audio samples, want 131", got)
	}
	checkContinuity(t, "audio", audio, 0)
	if vdur < 2.9 || vdur > 3.1 || vdur != adur {
		t.Errorf("playlist durations: video %.3f, audio %.3f", vdur, adur)
	}
}

func TestHEVCMultiAudio(t *testing.T) {
	opts := DefaultOptions()
	opts.SegmentDuration = 400 * time.Millisecond // a segment per keyframe
	s, srv := newTestSession(t, "hevc_multi.mkv", opts)

	master := string(get(t, srv.URL+s.MasterPath()))
	// PQ comes from the SPS VUI: mkvmerge dropped the container colour.
	if !strings.Contains(master, `CODECS="hvc1.2.4.L30.90,ac-3,ec-3,fLaC,opus"`) || !strings.Contains(master, "VIDEO-RANGE=PQ") {
		t.Errorf("master playlist:\n%s", master)
	}

	_, video, _ := fetchTrack(t, srv.URL, "video.m3u8")
	if len(video) != 4 {
		t.Errorf("%d video segments, want 4", len(video))
	}
	if got := countSamples(video); got != 48 {
		t.Errorf("%d video samples, want 48", got)
	}
	// Open GOP with B-frames: decode times are the sorted presentation
	// times, so they stay continuous across segments.
	checkContinuity(t, "video", video, 90)
	negative := false
	for _, f := range video {
		for _, smp := range f.samples {
			negative = negative || smp.CTO < 0
		}
	}
	if !negative {
		t.Error("expected negative composition offsets for reordered frames")
	}

	for _, tc := range []struct {
		track, frames int
		entry         string
	}{
		{2, 63, "ac-3"}, {3, 63, "ec-3"}, {4, 94, "fLaC"}, {5, 101, "Opus"},
	} {
		init, frags, _ := fetchTrack(t, srv.URL, fmt.Sprintf("audio/%d.m3u8", tc.track))
		if !strings.Contains(string(init), tc.entry) {
			t.Errorf("track %d init segment lacks %s entry", tc.track, tc.entry)
		}
		if got := countSamples(frags); got != tc.frames {
			t.Errorf("track %d: %d samples, want %d", tc.track, got, tc.frames)
		}
		checkContinuity(t, tc.entry, frags, 0)
	}
}

func TestAudioSelection(t *testing.T) {
	opts := DefaultOptions()
	opts.AudioTracks = []uint64{3, 2}
	s, srv := newTestSession(t, "hevc_multi.mkv", opts)
	master := string(get(t, srv.URL+s.MasterPath()))
	first := strings.Index(master, `URI="audio/3.m3u8"`)
	second := strings.Index(master, `URI="audio/2.m3u8"`)
	if first < 0 || second < first || strings.Contains(master, "audio/4.m3u8") {
		t.Errorf("unexpected renditions:\n%s", master)
	}
	if !strings.Contains(master, `NAME="Track 3 (Dolby Digital Plus 5.1)",DEFAULT=YES`) {
		t.Errorf("track 3 should be the default:\n%s", master)
	}

	src, err := mediasource.Open(context.Background(), fixtures+"hevc_multi.mkv")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	bad := DefaultOptions()
	bad.AudioTracks = []uint64{1}
	if _, err := NewSession(context.Background(), src, bad); err == nil {
		t.Error("selecting a video track as audio should fail")
	}
}

func TestRemoteSource(t *testing.T) {
	data, err := os.ReadFile(fixtures + "h264_aac.mkv")
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "movie.mkv", time.Time{}, strings.NewReader(string(data)))
	}))
	defer origin.Close()

	src, err := mediasource.Open(context.Background(), origin.URL+"/movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := NewSession(context.Background(), src, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	local, _ := newTestSession(t, "h264_aac.mkv", DefaultOptions())
	if remote.masterPlaylist() != local.masterPlaylist() || remote.mediaPlaylist("", true) != local.mediaPlaylist("", true) {
		t.Error("remote and local sources produce different playlists")
	}
	srv := httptest.NewServer(remote)
	defer srv.Close()
	_, video, _ := fetchTrack(t, srv.URL, "video.m3u8")
	if got := countSamples(video); got != 72 {
		t.Errorf("%d video samples over HTTP, want 72", got)
	}
}

func TestLanguages(t *testing.T) {
	for _, tc := range []struct{ in, hls, mdhd, name string }{
		{"eng", "en", "eng", "English"},
		{"fre", "fr", "fra", "French"},
		{"fr", "fr", "fra", "French"},
		{"pt-BR", "pt-BR", "por", "Portuguese"},
		{"und", "", "und", ""},
		{"tlh", "tlh", "und", ""},
	} {
		if got := hlsLanguage(tc.in); got != tc.hls {
			t.Errorf("hlsLanguage(%s) = %q, want %q", tc.in, got, tc.hls)
		}
		if got := mdhdLanguage(tc.in); got != tc.mdhd {
			t.Errorf("mdhdLanguage(%s) = %q, want %q", tc.in, got, tc.mdhd)
		}
		if got := languageName(tc.in); got != tc.name {
			t.Errorf("languageName(%s) = %q, want %q", tc.in, got, tc.name)
		}
	}
}
