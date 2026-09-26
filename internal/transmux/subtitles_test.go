package transmux

import (
	"strings"
	"testing"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/mkv"
)

func TestSRTToVTT(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Hello <i>world</i>", "Hello <i>world</i>"},
		{`<font color="red">red</font> & <B>bold</B>`, "red &amp; <b>bold</b>"},
		{"a < b > c", "a &lt; b &gt; c"},
		{"x <unclosed", "x &lt;unclosed"},
		{"Déjà vu", "Déjà vu"},
	} {
		if got := srtToVTT(tc.in); got != tc.want {
			t.Errorf("srtToVTT(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestASSToVTT(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// ReadOrder,Layer,Style,Name,MarginL,MarginR,MarginV,Effect,Text
		{`0,0,Default,,0,0,0,,{\an8\i1}Bonjour{\i0}, le monde\Nligne deux`, "<i>Bonjour</i>, le monde\nligne deux"},
		{`1,0,Default,,0,0,0,,{\p1}m 0 0 l 100 0{\p0}Visible, 1 < 2`, "Visible, 1 &lt; 2"},
		{`2,0,Default,,0,0,0,,{\b1}unclosed\hbold`, "<b>unclosed bold</b>"},
		{`2,0,Default,,0,0,0,,Déjà`, "Déjà"},
		{`too,few,fields`, ""},
	} {
		if got := assToVTT(tc.in); got != tc.want {
			t.Errorf("assToVTT(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWebVTTSegment(t *testing.T) {
	st := &subtitleTrack{format: subtitleSRT}
	got := string(st.webVTT([]*mkv.Frame{
		{PTS: 3723*time.Second + 4*time.Millisecond, Duration: 1500 * time.Millisecond, Data: []byte("one\r\n\r\ntwo")},
		{PTS: time.Second, Data: []byte("   ")}, // empty cue is dropped
	}))
	want := webVTTHeaderTimeMap + "\n01:02:03.004 --> 01:02:04.504\none\ntwo\n"
	if got != want {
		t.Errorf("webVTT =\n%q\nwant\n%q", got, want)
	}
	if got := string(st.webVTT(nil)); got != webVTTHeaderTimeMap {
		t.Errorf("empty segment = %q", got)
	}
}

func TestSubtitleRenditions(t *testing.T) {
	opts := DefaultOptions()
	opts.SegmentDuration = time.Second
	s, srv := newTestSession(t, "subs.mkv", opts)

	master := string(get(t, srv.URL+s.MasterPath()))
	for _, want := range []string{
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",DEFAULT=NO,AUTOSELECT=YES,FORCED=NO,URI="subs/2.m3u8"`,
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="French (Forced)",LANGUAGE="fr",DEFAULT=NO,AUTOSELECT=YES,FORCED=YES,URI="subs/3.m3u8"`,
		`,SUBTITLES="subs"`,
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master playlist lacks %s:\n%s", want, master)
		}
	}

	collect := func(track string) string {
		pl := string(get(t, srv.URL+"/subs/"+track+".m3u8"))
		if strings.Contains(pl, "EXT-X-MAP") {
			t.Errorf("subtitle playlist has an init segment:\n%s", pl)
		}
		var all strings.Builder
		for _, e := range extinf.FindAllStringSubmatch(pl, -1) {
			seg := string(get(t, srv.URL+"/subs/"+e[2]))
			if !strings.HasPrefix(seg, webVTTHeaderTimeMap) {
				t.Errorf("segment %s lacks the WebVTT header: %q", e[2], seg)
			}
			all.WriteString(strings.TrimPrefix(seg, webVTTHeaderTimeMap))
		}
		return all.String()
	}

	en := collect("2")
	for _, want := range []string{
		"00:00:00.200 --> 00:00:00.900\nHello <i>world</i> &amp; friends\n",
		"00:00:01.100 --> 00:00:01.800\nDéjà vu,\nsecond line\n",
		"00:00:02.300 --> 00:00:02.700\nLast cue\n",
	} {
		if !strings.Contains(en, want) {
			t.Errorf("English cues lack %q:\n%s", want, en)
		}
	}
	if n := strings.Count(en, "-->"); n != 3 {
		t.Errorf("%d English cues, want 3 (each exactly once)", n)
	}

	fr := collect("3")
	for _, want := range []string{
		"00:00:00.400 --> 00:00:01.400\n<i>Bonjour</i>, le monde\nligne deux\n",
		"00:00:01.500 --> 00:00:02.000\nVisible, 1 &lt; 2\n",
	} {
		if !strings.Contains(fr, want) {
			t.Errorf("French cues lack %q:\n%s", want, fr)
		}
	}

	var subStatus []string
	for _, ts := range s.Tracks() {
		if ts.Type == "subtitle" {
			subStatus = append(subStatus, ts.Status)
		}
	}
	if len(subStatus) != 2 || subStatus[0] != "subtitles (WebVTT)" || subStatus[1] != "subtitles (WebVTT)" {
		t.Errorf("subtitle statuses = %v", subStatus)
	}
}

func TestBitmapSubtitlesRejected(t *testing.T) {
	_, err := newSubtitleTrack(&mkv.Track{CodecID: codecPGS})
	if err == nil || !strings.Contains(err.Error(), "PGS") {
		t.Errorf("PGS track: %v", err)
	}
}
