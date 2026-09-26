// Package transmux serves a Matroska file as an HLS VOD presentation with
// fragmented-MP4 segments, so that AirPlay receivers (whose AVFoundation
// player has no Matroska demuxer) can play it. Frames are copied, never
// re-encoded: segment boundaries come from the file's keyframe index and each
// segment is remuxed on demand from a byte range of the source.
package transmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/mediasource"
	"git.foxden.network/FoxDen/foxCast/internal/mkv"
)

const (
	// DefaultSegmentDuration follows Apple's HLS authoring recommendation.
	DefaultSegmentDuration = 6 * time.Second
	// DefaultCachedSegments bounds memory: each cached segment holds its
	// video and audio frames (≈50 MB for a UHD Blu-ray remux). Apple TV
	// fetches audio up to ~4 segments ahead of video while starting, so a
	// smaller cache reads those segments from the source twice.
	DefaultCachedSegments = 5
	// firstFrameScanLimit bounds the read used to find each audio track's
	// first frame.
	firstFrameScanLimit = 64 << 20

	matroskaMagic = 0x1A45DFA3
)

// Options configures a Session.
type Options struct {
	// SegmentDuration is the minimum segment length (keyframes permitting).
	SegmentDuration time.Duration
	// AudioTracks selects the Matroska audio track numbers to offer, in
	// order; the first is the default. Empty offers every compatible track.
	AudioTracks []uint64
	// DolbyVision keeps Dolby Vision profile 5/8 metadata. When false, or
	// for profile 7, the HDR10/SDR/HLG base layer is played instead.
	DolbyVision bool
	// CachedSegments bounds the segment cache.
	CachedSegments int
	// Logf receives diagnostic messages; nil discards them.
	Logf func(format string, args ...any)
}

// DefaultOptions returns the recommended options.
func DefaultOptions() Options {
	return Options{SegmentDuration: DefaultSegmentDuration, DolbyVision: true, CachedSegments: DefaultCachedSegments}
}

// IsMatroska reports whether src starts with an EBML header.
func IsMatroska(src io.ReaderAt) bool {
	var b [4]byte
	if _, err := src.ReadAt(b[:], 0); err != nil {
		return false
	}
	return uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]) == matroskaMagic
}

// TrackSummary describes a source track for display.
type TrackSummary struct {
	Number   uint64
	Type     string
	Codec    string
	Language string
	Name     string
	Channels int
	// Status is "video", "audio" (offered), "default audio", or why the
	// track is not used.
	Status string
}

// Session is a transmuxed presentation of one Matroska source.
type Session struct {
	ctx    context.Context
	src    mediasource.Source
	file   *mkv.File
	opts   Options
	video  *videoTrack
	audio  []*audioTrack
	subs   []*subtitleTrack
	segs   []segment
	tracks map[uint64]bool
	cache  *segmentCache
	notes  []string
	logf   func(format string, args ...any)

	summary []TrackSummary
}

// NewSession parses src and prepares the HLS presentation. ctx bounds all
// background reads of the source.
func NewSession(ctx context.Context, src mediasource.Source, opts Options) (*Session, error) {
	if opts.SegmentDuration <= 0 {
		opts.SegmentDuration = DefaultSegmentDuration
	}
	if opts.CachedSegments <= 0 {
		opts.CachedSegments = DefaultCachedSegments
	}
	s := &Session{ctx: ctx, src: src, opts: opts, logf: opts.Logf, tracks: map[uint64]bool{}}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	f, err := mkv.Open(src, src.Size())
	if err != nil {
		return nil, err
	}
	s.file = f

	var videoSrc *mkv.Track
	for _, t := range f.Tracks {
		if t.Type == mkv.TrackVideo && t.Enabled {
			videoSrc = t
			break
		}
	}
	if videoSrc == nil {
		return nil, errors.New("no video track")
	}
	s.video, err = newVideoTrack(videoSrc, opts)
	if err != nil {
		return nil, err
	}
	if s.video.note != "" {
		s.notes = append(s.notes, s.video.note)
	}
	s.tracks[videoSrc.Number] = true

	if err := s.selectAudio(); err != nil {
		return nil, err
	}

	keyframes, err := f.Keyframes(videoSrc.Number)
	if err != nil {
		return nil, fmt.Errorf("keyframe index: %w", err)
	}
	if len(f.Cues) == 0 {
		s.notes = append(s.notes, "file has no cue index; keyframes were found by scanning clusters")
	}
	s.segs = planSegments(f, keyframes, opts.SegmentDuration)
	if len(s.segs) == 0 {
		return nil, errors.New("no video keyframes found")
	}
	s.cache = newSegmentCache(opts.CachedSegments, s.loadSegment)
	s.logf("transmux: %d segments, %d audio and %d subtitle renditions", len(s.segs), len(s.audio), len(s.subs))
	return s, nil
}

// selectAudio maps the requested (or all compatible) audio tracks.
func (s *Session) selectAudio() error {
	f := s.file
	requested := map[uint64]int{}
	for i, n := range s.opts.AudioTracks {
		t := f.Track(n)
		if t == nil || t.Type != mkv.TrackAudio {
			return fmt.Errorf("track %d is not an audio track", n)
		}
		requested[n] = i
	}

	var candidates []*mkv.Track
	for _, t := range f.Tracks {
		if t.Type != mkv.TrackAudio {
			continue
		}
		if len(requested) > 0 {
			if _, ok := requested[t.Number]; !ok {
				continue
			}
		}
		if audioCodecSupported(t.CodecID) && t.Supported() == nil {
			candidates = append(candidates, t)
		}
	}
	first, err := s.firstFrames(candidates)
	if err != nil {
		return err
	}

	status := map[uint64]string{}
	for _, t := range candidates {
		a, err := newAudioTrack(t, first[t.Number])
		if err != nil {
			status[t.Number] = err.Error()
			continue
		}
		s.audio = append(s.audio, a)
		s.tracks[t.Number] = true
	}
	if len(requested) > 0 {
		// Keep the requested order; the first becomes the default.
		ordered := make([]*audioTrack, len(s.opts.AudioTracks))
		for _, a := range s.audio {
			ordered[requested[a.src.Number]] = a
		}
		s.audio = s.audio[:0]
		for i, a := range ordered {
			if a == nil {
				n := s.opts.AudioTracks[i]
				reason := status[n]
				if reason == "" {
					reason = fmt.Sprintf("codec %s is not supported by Apple TV", f.Track(n).CodecID)
				}
				return fmt.Errorf("audio track %d: %s", n, reason)
			}
			s.audio = append(s.audio, a)
		}
	} else {
		// The default is the first compatible track flagged default.
		for i, a := range s.audio {
			if a.src.Default {
				s.audio = append([]*audioTrack{a}, slices.Delete(slices.Clone(s.audio), i, i+1)...)
				break
			}
		}
	}
	if len(s.audio) == 0 {
		s.notes = append(s.notes, "no audio track Apple TV can decode; playing video only")
	}
	s.nameRenditions()
	s.selectSubtitles(status)

	for _, t := range f.Tracks {
		ts := TrackSummary{Number: t.Number, Type: t.Type.String(), Codec: t.CodecID, Language: t.Language, Name: t.Name}
		if t.Audio != nil {
			ts.Channels = int(t.Audio.Channels)
		}
		switch {
		case s.video.src == t:
			ts.Status = "video"
		case len(s.audio) > 0 && s.audio[0].src == t:
			ts.Status = "default audio"
		case s.tracks[t.Number] && t.Type == mkv.TrackSubtitle:
			ts.Status = "subtitles (WebVTT)"
		case s.tracks[t.Number]:
			ts.Status = "audio"
		case status[t.Number] != "":
			ts.Status = "unused: " + status[t.Number]
		case t.Type == mkv.TrackAudio && !audioCodecSupported(t.CodecID):
			ts.Status = "unused: Apple TV cannot decode " + codecDisplayName(t.CodecID)
		default:
			ts.Status = "unused"
		}
		s.summary = append(s.summary, ts)
	}
	return nil
}

// firstFrames reads from the first cluster until every track has a frame.
func (s *Session) firstFrames(tracks []*mkv.Track) (map[uint64]*mkv.Frame, error) {
	out := map[uint64]*mkv.Frame{}
	if len(tracks) == 0 {
		return out, nil
	}
	want := map[uint64]bool{}
	for _, t := range tracks {
		want[t.Number] = true
	}
	start := s.file.FirstCluster
	length := s.file.SegmentEnd - start
	if length > firstFrameScanLimit {
		length = firstFrameScanLimit
	}
	rc, err := s.src.OpenRange(s.ctx, start, length)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	cr := s.file.NewClusterReader(rc, start)
	cr.Tracks = want
	for len(out) < len(want) {
		fr, err := cr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, err
		}
		if _, ok := out[fr.Track]; !ok {
			out[fr.Track] = fr
		}
	}
	return out, nil
}

// selectSubtitles offers every enabled text subtitle track; status records
// why others are skipped.
func (s *Session) selectSubtitles(status map[uint64]string) {
	seen := map[string]int{}
	for _, t := range s.file.Tracks {
		if t.Type != mkv.TrackSubtitle || !t.Enabled {
			continue
		}
		st, err := newSubtitleTrack(t)
		if err != nil {
			status[t.Number] = err.Error()
			continue
		}
		name := t.Name
		if name == "" {
			name = languageName(t.Language)
			if name == "" {
				name = fmt.Sprintf("Track %d", t.Number)
			}
			if t.Forced {
				name += " (Forced)"
			}
		}
		seen[name]++
		if seen[name] > 1 {
			name = fmt.Sprintf("%s %d", name, seen[name])
		}
		st.name = name
		s.subs = append(s.subs, st)
		s.tracks[t.Number] = true
	}
}

// nameRenditions gives every audio rendition a unique display name.
func (s *Session) nameRenditions() {
	seen := map[string]int{}
	for _, a := range s.audio {
		t := a.src
		name := t.Name
		if name == "" {
			name = languageName(t.Language)
			if name == "" {
				name = fmt.Sprintf("Track %d", t.Number)
			}
			name = fmt.Sprintf("%s (%s %s)", name, codecDisplayName(t.CodecID), channelLayoutName(a.channels))
		}
		seen[name]++
		if seen[name] > 1 {
			name = fmt.Sprintf("%s %d", name, seen[name])
		}
		a.name = name
	}
}

// Tracks describes every source track and how it is used.
func (s *Session) Tracks() []TrackSummary { return s.summary }

// Notes lists adaptations the user should know about.
func (s *Session) Notes() []string { return s.notes }

// Duration is the presentation duration.
func (s *Session) Duration() time.Duration { return s.segs[len(s.segs)-1].End }

// MasterPath is the URL path of the multivariant playlist.
func (s *Session) MasterPath() string { return "/" + masterPlaylist }

// segmentData is the demuxed content of one segment.
type segmentData struct {
	video []*mkv.Frame
	// other holds audio and subtitle frames by track number.
	other map[uint64][]*mkv.Frame
}

func (s *Session) loadSegment(i int) (*segmentData, error) {
	seg := s.segs[i]
	started := time.Now()
	rc, err := s.src.OpenRange(s.ctx, seg.ByteStart, seg.ByteEnd-seg.ByteStart)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	cr := s.file.NewClusterReader(rc, seg.ByteStart)
	cr.Tracks = s.tracks
	cr.StopAt = seg.To
	d := &segmentData{other: map[uint64][]*mkv.Frame{}}
	videoNum := s.video.src.Number
	for {
		fr, err := cr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, fmt.Errorf("segment %d: %w", i, err)
		}
		if fr.Pos.Less(seg.From) {
			continue // belongs to the previous segment
		}
		if fr.Track == videoNum {
			if len(d.video) == 0 && !fr.Keyframe {
				continue // undecodable leading frames at the start of the file
			}
			d.video = append(d.video, fr)
		} else {
			d.other[fr.Track] = append(d.other[fr.Track], fr)
		}
	}
	if len(d.video) == 0 {
		return nil, fmt.Errorf("segment %d: no video frames", i)
	}
	if !d.video[0].Keyframe {
		s.logf("transmux: segment %d does not start with a keyframe", i)
	}
	s.logf("transmux: segment %d (%v–%v, %.1f MB) read in %v", i, seg.Start, seg.End,
		float64(seg.ByteEnd-seg.ByteStart)/1e6, time.Since(started).Round(time.Millisecond))
	return d, nil
}
