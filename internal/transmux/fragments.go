package transmux

import (
	"math"
	"slices"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/fmp4"
	"git.foxden.network/FoxDen/foxCast/internal/mkv"
)

// fallbackFrameTicks is used for the last video frame of a segment when the
// track has no DefaultDuration and only one frame (≈ 1/30 s at 90 kHz).
const fallbackFrameTicks = 3000

// segment is one HLS media segment: every block from From (a video keyframe)
// up to, but excluding, To.
type segment struct {
	Start, End time.Duration
	From       mkv.Position
	To         *mkv.Position // nil for the last segment
	ByteStart  int64
	ByteEnd    int64
}

// planSegments groups keyframes into segments of at least target duration.
func planSegments(f *mkv.File, keyframes []mkv.CuePoint, target time.Duration) []segment {
	var segs []segment
	for _, kf := range keyframes {
		if len(segs) > 0 {
			last := &segs[len(segs)-1]
			if kf.Time <= last.Start || kf.Time-last.Start < target {
				continue
			}
			to := kf.Pos
			last.To = &to
			last.End = kf.Time
		}
		segs = append(segs, segment{Start: kf.Time, From: kf.Pos})
	}
	if len(segs) == 0 {
		return nil
	}
	// Blocks (typically audio) can precede the first keyframe.
	segs[0].From = mkv.Position{Cluster: f.FirstCluster}
	// A tiny trailing segment is merged into its predecessor.
	last := &segs[len(segs)-1]
	last.End = f.Duration
	if len(segs) > 1 && last.End-last.Start < target/2 {
		prev := &segs[len(segs)-2]
		prev.To = nil
		segs = segs[:len(segs)-1]
		last = prev
		last.End = f.Duration
	}
	if last.End <= last.Start {
		last.End = last.Start + target
	}
	for i := range segs {
		s := &segs[i]
		s.ByteStart = s.From.Cluster
		if s.To != nil {
			// Enough to read the next segment's cluster header and first
			// block header, so the reader can see where to stop.
			const headers = 2 * 12
			s.ByteEnd = s.To.Cluster + s.To.Rel + headers
		} else {
			s.ByteEnd = f.NextTopLevelAfter(s.From.Cluster)
		}
	}
	return segs
}

// videoFragment builds the video fragment for a segment's frames (in decode
// order). Matroska stores presentation timestamps only; decode timestamps are
// the sorted presentation timestamps, which keeps decode time continuous
// across segments even for open GOPs.
func (v *videoTrack) fragment(seq uint32, frames []*mkv.Frame) *fmp4.Fragment {
	n := len(frames)
	pts := make([]int64, n)
	for i, fr := range frames {
		pts[i] = toTimescale(fr.PTS, videoTimescale)
	}
	dts := slices.Clone(pts)
	slices.Sort(dts)

	lastDur := int64(v.defaultDuration)
	if lastDur == 0 {
		lastDur = fallbackFrameTicks
		if n > 1 {
			lastDur = dts[n-1] - dts[n-2]
		}
	}
	base := dts[0]
	if base < 0 {
		base = 0
	}
	samples := make([]fmp4.Sample, n)
	for i, fr := range frames {
		dur := lastDur
		if i+1 < n {
			dur = dts[i+1] - dts[i]
		}
		flags := uint32(fmp4.SampleFlagsNonSync)
		if fr.Keyframe {
			flags = fmp4.SampleFlagsSync
		}
		data := fr.Data
		if v.filter != nil {
			data = v.filter(data)
		}
		samples[i] = fmp4.Sample{
			Duration: uint32(dur),
			CTO:      int32(pts[i] - dts[i]),
			Flags:    flags,
			Data:     data,
		}
	}
	return &fmp4.Fragment{Sequence: seq, BaseDecodeTime: uint64(base), Samples: samples}
}

// fragment builds an audio fragment. Matroska timestamps are rounded (usually
// to 1 ms), so for constant-frame-size codecs the start time is snapped to
// the frame grid anchored at the track's first frame; consecutive fragments
// then line up sample-exactly instead of leaving sub-millisecond gaps.
func (a *audioTrack) fragment(seq uint32, frames []*mkv.Frame) *fmp4.Fragment {
	start := toTimescale(frames[0].PTS, a.timescale)
	if a.fixedSamples > 0 {
		anchor := toTimescale(a.firstPTS, a.timescale)
		k := math.Round(float64(start-anchor) / float64(a.fixedSamples))
		start = anchor + int64(k)*int64(a.fixedSamples)
	}
	if start < 0 {
		start = 0
	}
	samples := make([]fmp4.Sample, len(frames))
	var prev uint32
	for i, fr := range frames {
		dur, err := a.frameSamples(fr.Data)
		if err != nil || dur == 0 {
			// Fall back to the timestamp delta, then the previous length.
			dur = prev
			if i+1 < len(frames) {
				if d := toTimescale(frames[i+1].PTS-fr.PTS, a.timescale); d > 0 {
					dur = uint32(d)
				}
			}
		}
		prev = dur
		samples[i] = fmp4.Sample{Duration: dur, Flags: fmp4.SampleFlagsSync, Data: fr.Data}
	}
	return &fmp4.Fragment{Sequence: seq, BaseDecodeTime: uint64(start), Samples: samples}
}
