package transmux

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"git.foxden.network/FoxDen/foxCast/internal/fmp4"
)

const (
	masterPlaylist = "master.m3u8"
	videoPlaylist  = "video.m3u8"
	initSegment    = "init.mp4"
	segmentSuffix  = ".m4s"
	audioGroupID   = "audio"
	hlsVersion     = 7

	contentTypePlaylist = "application/vnd.apple.mpegurl"
	contentTypeVideo    = "video/mp4"
	contentTypeAudio    = "audio/mp4"
)

// ServeHTTP serves the playlists, init segments and media segments:
//
//	/master.m3u8
//	/video.m3u8   /video/init.mp4   /video/<n>.m4s
//	/audio/<track>.m3u8   /audio/<track>/init.mp4   /audio/<track>/<n>.m4s
func (s *Session) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.logf("transmux: %s %s", r.Method, r.URL.Path)
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == masterPlaylist:
		s.writeBody(w, r, contentTypePlaylist, []byte(s.masterPlaylist()))
	case len(parts) == 1 && parts[0] == videoPlaylist:
		s.writeBody(w, r, contentTypePlaylist, []byte(s.mediaPlaylist("video/")))
	case len(parts) == 2 && parts[0] == "video":
		s.serveTrackFile(w, r, nil, parts[1])
	case len(parts) == 2 && parts[0] == "audio" && strings.HasSuffix(parts[1], ".m3u8"):
		if a := s.audioTrack(strings.TrimSuffix(parts[1], ".m3u8")); a != nil {
			s.writeBody(w, r, contentTypePlaylist, []byte(s.mediaPlaylist(fmt.Sprintf("%d/", a.src.Number))))
			return
		}
		http.NotFound(w, r)
	case len(parts) == 3 && parts[0] == "audio":
		if a := s.audioTrack(parts[1]); a != nil {
			s.serveTrackFile(w, r, a, parts[2])
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Session) audioTrack(num string) *audioTrack {
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return nil
	}
	for _, a := range s.audio {
		if a.src.Number == n {
			return a
		}
	}
	return nil
}

func (s *Session) writeBody(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// serveTrackFile serves an init or media segment; audio is nil for video.
func (s *Session) serveTrackFile(w http.ResponseWriter, r *http.Request, audio *audioTrack, name string) {
	contentType := contentTypeVideo
	if audio != nil {
		contentType = contentTypeAudio
	}
	if name == initSegment {
		init := s.video.init
		if audio != nil {
			init = audio.init
		}
		s.writeBody(w, r, contentType, init)
		return
	}
	i, err := strconv.Atoi(strings.TrimSuffix(name, segmentSuffix))
	if err != nil || !strings.HasSuffix(name, segmentSuffix) || i < 0 || i >= len(s.segs) {
		http.NotFound(w, r)
		return
	}
	data, err := s.cache.get(r.Context(), i)
	if err != nil {
		s.logf("transmux: segment %d: %v", i, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if audio == nil && i+1 < len(s.segs) {
		s.cache.prefetch(i + 1)
	}

	seq := uint32(i + 1)
	var frag *fmp4.Fragment
	if audio == nil {
		frag = s.video.fragment(seq, data.video)
	} else {
		frames := data.audio[audio.src.Number]
		if len(frames) == 0 {
			// Audio can be absent from a stretch of the file, and an empty
			// fragment is invalid, so report the gap.
			http.Error(w, "no audio in segment", http.StatusNotFound)
			return
		}
		frag = audio.fragment(seq, frames)
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(frag.Size(), 10))
	if r.Method == http.MethodHead {
		return
	}
	if _, err := frag.WriteTo(w); err != nil {
		s.logf("transmux: write segment %d: %v", i, err)
	}
}

func (s *Session) masterPlaylist() string {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:%d\n#EXT-X-INDEPENDENT-SEGMENTS\n", hlsVersion)
	codecs := []string{s.video.codecs}
	for i, a := range s.audio {
		def := "NO"
		if i == 0 {
			def = "YES"
		}
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=%q,NAME=%q,", audioGroupID, a.name)
		if lang := hlsLanguage(a.src.Language); lang != "" {
			fmt.Fprintf(&b, "LANGUAGE=%q,", lang)
		}
		fmt.Fprintf(&b, "DEFAULT=%s,AUTOSELECT=YES,CHANNELS=\"%d\",URI=\"audio/%d.m3u8\"\n", def, a.channels, a.src.Number)
		if !contains(codecs, a.codecs) {
			codecs = append(codecs, a.codecs)
		}
	}
	peak, avg := s.bandwidth()
	fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,CODECS=%q,RESOLUTION=%dx%d",
		peak, avg, strings.Join(codecs, ","), s.video.width, s.video.height)
	if s.video.frameRate > 0 {
		fmt.Fprintf(&b, ",FRAME-RATE=%.3f", s.video.frameRate)
	}
	fmt.Fprintf(&b, ",VIDEO-RANGE=%s", s.video.videoRange)
	if len(s.audio) > 0 {
		fmt.Fprintf(&b, ",AUDIO=%q", audioGroupID)
	}
	fmt.Fprintf(&b, "\n%s\n", videoPlaylist)
	return b.String()
}

// mediaPlaylist lists every segment; prefix is the directory of the
// segments relative to the playlist.
func (s *Session) mediaPlaylist(prefix string) string {
	target := 1
	for _, seg := range s.segs {
		if d := int(math.Round((seg.End - seg.Start).Seconds())); d > target {
			target = d
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:%d\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:1\n", hlsVersion, target)
	fmt.Fprintf(&b, "#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-MAP:URI=\"%s%s\"\n", prefix, initSegment)
	for i, seg := range s.segs {
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n%s%d%s\n", (seg.End - seg.Start).Seconds(), prefix, i, segmentSuffix)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// bandwidth estimates peak and average bit rates from segment byte ranges.
// The ranges include tracks that are not served, so this overestimates.
func (s *Session) bandwidth() (peak, avg int64) {
	var bytes int64
	for _, seg := range s.segs {
		n := seg.ByteEnd - seg.ByteStart
		bytes += n
		if d := (seg.End - seg.Start).Seconds(); d > 0 {
			if r := int64(float64(n*8) / d); r > peak {
				peak = r
			}
		}
	}
	if d := s.Duration().Seconds(); d > 0 {
		avg = int64(float64(bytes*8) / d)
	}
	if peak < avg {
		peak = avg
	}
	return peak, avg
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
