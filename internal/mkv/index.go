package mkv

import (
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// blockHeaderPeek is enough bytes to read a block's track number (≤8-byte
// vint), relative timestamp and flags.
const blockHeaderPeek = 11

// Keyframes returns the keyframe index of a track in file order: the Cues for
// that track if present, otherwise a scan of every cluster's first keyframe
// of the track.
func (f *File) Keyframes(track uint64) ([]CuePoint, error) {
	var out []CuePoint
	for _, c := range f.Cues {
		if c.Track == track {
			out = append(out, c)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	return f.scanKeyframes(track)
}

// scanKeyframes walks clusters with header reads only, recording the first
// keyframe block of track in each cluster. Muxers start clusters at video
// keyframes, so this recovers a usable index without reading frame data.
func (f *File) scanKeyframes(track uint64) ([]CuePoint, error) {
	var out []CuePoint
	off := f.FirstCluster
	for off < f.SegmentEnd {
		id, size, hlen, err := readHeaderAt(f.r, off)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, err
		}
		if id != idCluster {
			if size == unknownSize {
				break
			}
			off += int64(hlen) + size
			continue
		}
		data := off + int64(hlen)
		end := f.SegmentEnd
		if size != unknownSize {
			end = data + size
		}
		next, cp, err := f.scanCluster(off, data, end, track)
		if err != nil {
			return nil, err
		}
		if cp != nil {
			out = append(out, *cp)
		}
		off = next
	}
	return out, nil
}

// scanCluster returns the offset after the cluster and its first keyframe of
// track, if any.
func (f *File) scanCluster(clusterPos, data, end int64, track uint64) (int64, *CuePoint, error) {
	var ts int64
	var found *CuePoint
	off := data
	for off < end {
		id, size, hlen, err := readHeaderAt(f.r, off)
		if err != nil {
			return end, found, nil
		}
		if isTopLevel(id) {
			return off, found, nil // end of an unknown-size cluster
		}
		if size == unknownSize {
			return end, found, errors.New("mkv: unknown-size cluster child")
		}
		body := off + int64(hlen)
		switch id {
		case idTimestamp:
			buf := make([]byte, size)
			if _, err := f.r.ReadAt(buf, body); err != nil {
				return end, found, err
			}
			ts = int64(readUint(buf))
		case idSimpleBlock:
			if found == nil {
				var hdr [blockHeaderPeek]byte
				n, _ := f.r.ReadAt(hdr[:], body)
				if num, vl, err := parseSize(hdr[:n]); err == nil && uint64(num) == track && n >= vl+3 && hdr[vl+2]&simpleBlockKeyframe != 0 {
					rel := int64(int16(binary.BigEndian.Uint16(hdr[vl:])))
					found = &CuePoint{
						Time:  time.Duration((ts + rel) * f.TimestampScale),
						Track: track,
						Pos:   Position{Cluster: clusterPos, Rel: off - data},
					}
				}
			}
		case idBlockGroup:
			if found == nil && size <= maxLoadedElement {
				buf := make([]byte, size)
				if _, err := f.r.ReadAt(buf, body); err != nil {
					return end, found, err
				}
				var block []byte
				hasRef := false
				_ = children(buf, func(e element) error {
					switch e.id {
					case idBlock:
						block = e.data
					case idReferenceBlock:
						hasRef = true
					}
					return nil
				})
				if num, vl, err := parseSize(block); err == nil && uint64(num) == track && !hasRef && len(block) >= vl+3 {
					rel := int64(int16(binary.BigEndian.Uint16(block[vl:])))
					found = &CuePoint{
						Time:  time.Duration((ts + rel) * f.TimestampScale),
						Track: track,
						Pos:   Position{Cluster: clusterPos, Rel: off - data},
					}
				}
			}
		}
		// A known-size cluster can be skipped as soon as it yielded a keyframe.
		if found != nil && end != f.SegmentEnd {
			return end, found, nil
		}
		off = body + size
	}
	return end, found, nil
}
