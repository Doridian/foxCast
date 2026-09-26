// Package mkv is a read-only Matroska/WebM demuxer built for random access:
// it loads the header, track list and cue index with small reads, then
// streams frames from any cluster position. It does not decode codecs.
package mkv

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

// Element IDs (Matroska specification, RFC 9559).
const (
	idEBML        = 0x1A45DFA3
	idDocType     = 0x4282
	idSegment     = 0x18538067
	idSeekHead    = 0x114D9B74
	idSeek        = 0x4DBB
	idSeekID      = 0x53AB
	idSeekPos     = 0x53AC
	idInfo        = 0x1549A966
	idTimestScale = 0x2AD7B1
	idDuration    = 0x4489
	idTitle       = 0x7BA9
	idTracks      = 0x1654AE6B
	idCues        = 0x1C53BB6B
	idCluster     = 0x1F43B675
	idChapters    = 0x1043A770
	idTags        = 0x1254C367
	idAttachments = 0x1941A469

	idTrackEntry      = 0xAE
	idTrackNumber     = 0xD7
	idTrackUID        = 0x73C5
	idTrackType       = 0x83
	idFlagEnabled     = 0xB9
	idFlagDefault     = 0x88
	idFlagForced      = 0x55AA
	idName            = 0x536E
	idLanguage        = 0x22B59C
	idLanguageBCP47   = 0x22B59D
	idCodecID         = 0x86
	idCodecPrivate    = 0x63A2
	idDefaultDuration = 0x23E383
	idCodecDelay      = 0x56AA
	idSeekPreRoll     = 0x56BB
	idBlockAddMapping = 0x41E4
	idBlockAddIDType  = 0x41E7
	idBlockAddIDExtra = 0x41ED

	idVideo           = 0xE0
	idPixelWidth      = 0xB0
	idPixelHeight     = 0xBA
	idDisplayWidth    = 0x54B0
	idDisplayHeight   = 0x54BA
	idDisplayUnit     = 0x54B2
	idColour          = 0x55B0
	idMatrixCoeffs    = 0x55B1
	idRange           = 0x55B9
	idTransferChar    = 0x55BA
	idPrimaries       = 0x55BB
	idMaxCLL          = 0x55BC
	idMaxFALL         = 0x55BD
	idMasteringMeta   = 0x55D0
	idPrimaryRX       = 0x55D1
	idPrimaryRY       = 0x55D2
	idPrimaryGX       = 0x55D3
	idPrimaryGY       = 0x55D4
	idPrimaryBX       = 0x55D5
	idPrimaryBY       = 0x55D6
	idWhitePointX     = 0x55D7
	idWhitePointY     = 0x55D8
	idLuminanceMax    = 0x55D9
	idLuminanceMin    = 0x55DA
	idAudio           = 0xE1
	idSamplingFreq    = 0xB5
	idOutputSampling  = 0x78B5
	idChannels        = 0x9F
	idBitDepth        = 0x6264
	idContentEncs     = 0x6D80
	idContentEnc      = 0x6240
	idContentEncScope = 0x5032
	idContentEncType  = 0x5033
	idContentComp     = 0x5034
	idContentCompAlgo = 0x4254
	idContentCompSet  = 0x4255

	idCuePoint     = 0xBB
	idCueTime      = 0xB3
	idCueTrackPos  = 0xB7
	idCueTrack     = 0xF7
	idCueClusterPo = 0xF1
	idCueRelPos    = 0xF0

	idTimestamp      = 0xE7
	idSimpleBlock    = 0xA3
	idBlockGroup     = 0xA0
	idBlock          = 0xA1
	idBlockDuration  = 0x9B
	idReferenceBlock = 0xFB

	defaultTimestampScale = 1_000_000 // ns per tick

	// maxLoadedElement bounds header elements loaded into memory.
	maxLoadedElement = 64 << 20

	contentCompZlib     = 0
	contentCompStripped = 3
	contentEncScopeData = 1
)

// TrackType is the Matroska TrackType value.
type TrackType uint8

// Track types.
const (
	TrackVideo    TrackType = 1
	TrackAudio    TrackType = 2
	TrackSubtitle TrackType = 17
)

func (t TrackType) String() string {
	switch t {
	case TrackVideo:
		return "video"
	case TrackAudio:
		return "audio"
	case TrackSubtitle:
		return "subtitle"
	}
	return fmt.Sprintf("type %d", t)
}

// Track describes one TrackEntry.
type Track struct {
	Number       uint64
	UID          uint64
	Type         TrackType
	CodecID      string
	CodecPrivate []byte
	Name         string
	// Language is the ISO 639-2 code, or the BCP 47 tag when one is present.
	Language        string
	Enabled         bool
	Default         bool
	Forced          bool
	DefaultDuration time.Duration
	CodecDelay      time.Duration
	Video           *Video
	Audio           *Audio
	// BlockAdditionMappings carries codec side configuration such as
	// Dolby Vision's dvcC/dvvC record.
	BlockAdditionMappings []BlockAdditionMapping

	// stripped is the header-stripping prefix removed from every frame.
	stripped []byte
	// zlibFrames reports zlib-compressed frame data.
	zlibFrames bool
	// unsupportedEncoding is set for encrypted or exotically compressed tracks.
	unsupportedEncoding string
}

// Supported reports whether frames of this track can be decoded by the demuxer
// (i.e. no unsupported compression or encryption).
func (t *Track) Supported() error {
	if t.unsupportedEncoding != "" {
		return errors.New(t.unsupportedEncoding)
	}
	return nil
}

// BlockAdditionMapping is a BlockAdditionMapping element.
type BlockAdditionMapping struct {
	Type      uint64
	ExtraData []byte
}

// Video holds TrackEntry/Video.
type Video struct {
	PixelWidth, PixelHeight     uint64
	DisplayWidth, DisplayHeight uint64
	DisplayUnit                 uint64
	Colour                      *Colour
}

// Colour holds Video/Colour. Zero means unspecified, matching the defaults.
type Colour struct {
	MatrixCoefficients      uint64
	TransferCharacteristics uint64
	Primaries               uint64
	Range                   uint64
	MaxCLL, MaxFALL         uint64
	Mastering               *MasteringMetadata
}

// MasteringMetadata holds SMPTE 2086 mastering display values in natural units.
type MasteringMetadata struct {
	PrimaryRX, PrimaryRY, PrimaryGX, PrimaryGY, PrimaryBX, PrimaryBY float64
	WhitePointX, WhitePointY                                         float64
	LuminanceMax, LuminanceMin                                       float64
}

// Audio holds TrackEntry/Audio.
type Audio struct {
	SamplingFrequency       float64
	OutputSamplingFrequency float64
	Channels                uint64
	BitDepth                uint64
}

// Position locates a block: the absolute offset of its cluster and its offset
// relative to the cluster's data (as CueRelativePosition encodes it).
type Position struct {
	Cluster int64
	Rel     int64
}

// Less orders positions in file order.
func (p Position) Less(q Position) bool {
	if p.Cluster != q.Cluster {
		return p.Cluster < q.Cluster
	}
	return p.Rel < q.Rel
}

// CuePoint is one cue entry for a track.
type CuePoint struct {
	Time  time.Duration
	Track uint64
	Pos   Position
}

// File is an opened Matroska file.
type File struct {
	r    io.ReaderAt
	size int64

	DocType        string
	Title          string
	TimestampScale int64 // nanoseconds per tick
	Duration       time.Duration
	Tracks         []*Track
	// Cues lists the cue index sorted by position; empty when absent.
	Cues []CuePoint

	// SegmentStart and SegmentEnd bound the Segment's data.
	SegmentStart, SegmentEnd int64
	// FirstCluster is the absolute offset of the first Cluster.
	FirstCluster int64
	// topLevel maps top-level element IDs to their known offsets.
	topLevel map[uint32][]int64
}

// Open parses the header, tracks and cues of a Matroska file.
func Open(r io.ReaderAt, size int64) (*File, error) {
	f := &File{r: r, size: size, TimestampScale: defaultTimestampScale, topLevel: map[uint32][]int64{}}

	ebml, n, err := f.loadAt(0)
	if err != nil {
		return nil, fmt.Errorf("mkv: read EBML header: %w", err)
	}
	if ebml.id != idEBML {
		return nil, errors.New("mkv: not an EBML file")
	}
	f.DocType = "matroska"
	if err := children(ebml.data, func(e element) error {
		if e.id == idDocType {
			f.DocType = readString(e.data)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if f.DocType != "matroska" && f.DocType != "webm" {
		return nil, fmt.Errorf("mkv: unsupported DocType %q", f.DocType)
	}

	// Find the Segment.
	off := n
	for {
		id, esize, hlen, err := readHeaderAt(r, off)
		if err != nil {
			return nil, fmt.Errorf("mkv: find Segment: %w", err)
		}
		if id == idSegment {
			f.SegmentStart = off + int64(hlen)
			f.SegmentEnd = size
			if esize != unknownSize && f.SegmentStart+esize < size {
				f.SegmentEnd = f.SegmentStart + esize
			}
			break
		}
		if esize == unknownSize {
			return nil, errors.New("mkv: unknown-size element before Segment")
		}
		off += int64(hlen) + esize
	}

	if err := f.scanTopLevel(); err != nil {
		return nil, err
	}
	if err := f.loadSeekHeads(); err != nil {
		return nil, err
	}

	info, err := f.loadTopLevel(idInfo)
	if err != nil {
		return nil, err
	}
	if info != nil {
		if err := f.parseInfo(info.data); err != nil {
			return nil, err
		}
	}
	tracks, err := f.loadTopLevel(idTracks)
	if err != nil {
		return nil, err
	}
	if tracks == nil {
		return nil, errors.New("mkv: no Tracks element")
	}
	if err := f.parseTracks(tracks.data); err != nil {
		return nil, err
	}
	cues, err := f.loadTopLevel(idCues)
	if err != nil {
		return nil, err
	}
	if cues != nil {
		if err := f.parseCues(cues.data); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// Track returns the track with the given number.
func (f *File) Track(number uint64) *Track {
	for _, t := range f.Tracks {
		if t.Number == number {
			return t
		}
	}
	return nil
}

// Size is the file size.
func (f *File) Size() int64 { return f.size }

// NextTopLevelAfter returns the offset of the first known non-Cluster
// top-level element after off, or SegmentEnd. It bounds reads of the last
// cluster range so that trailing Cues/Tags are not streamed.
func (f *File) NextTopLevelAfter(off int64) int64 {
	end := f.SegmentEnd
	for id, offs := range f.topLevel {
		if id == idCluster {
			continue
		}
		for _, o := range offs {
			if o > off && o < end {
				end = o
			}
		}
	}
	return end
}

// loadAt reads a complete element at off; it returns the header+data length.
func (f *File) loadAt(off int64) (element, int64, error) {
	id, size, hlen, err := readHeaderAt(f.r, off)
	if err != nil {
		return element{}, 0, err
	}
	if size == unknownSize || size > maxLoadedElement {
		return element{}, 0, fmt.Errorf("mkv: element 0x%X at %d too large to load", id, off)
	}
	data := make([]byte, size)
	if _, err := f.r.ReadAt(data, off+int64(hlen)); err != nil && !(errors.Is(err, io.EOF) && size == 0) {
		return element{}, 0, err
	}
	return element{id: id, data: data}, int64(hlen) + size, nil
}

func (f *File) addTopLevel(id uint32, off int64) bool {
	for _, o := range f.topLevel[id] {
		if o == off {
			return false
		}
	}
	f.topLevel[id] = append(f.topLevel[id], off)
	return true
}

// scanTopLevel walks top-level elements up to the first Cluster.
func (f *File) scanTopLevel() error {
	off := f.SegmentStart
	for off < f.SegmentEnd {
		id, size, hlen, err := readHeaderAt(f.r, off)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("mkv: scan at %d: %w", off, err)
		}
		f.addTopLevel(id, off)
		if id == idCluster {
			f.FirstCluster = off
			return nil
		}
		if size == unknownSize {
			return fmt.Errorf("mkv: unknown-size top-level element 0x%X", id)
		}
		off += int64(hlen) + size
	}
	return errors.New("mkv: no Cluster found")
}

func (f *File) loadSeekHeads() error {
	for done := 0; done < len(f.topLevel[idSeekHead]); done++ {
		off := f.topLevel[idSeekHead][done]
		e, _, err := f.loadAt(off)
		if err != nil {
			return fmt.Errorf("mkv: SeekHead: %w", err)
		}
		if e.id != idSeekHead {
			continue
		}
		err = children(e.data, func(seek element) error {
			if seek.id != idSeek {
				return nil
			}
			var id uint32
			pos := int64(-1)
			if err := children(seek.data, func(c element) error {
				switch c.id {
				case idSeekID:
					id = uint32(readUint(c.data))
				case idSeekPos:
					pos = int64(readUint(c.data))
				}
				return nil
			}); err != nil {
				return err
			}
			if id != 0 && pos >= 0 && f.SegmentStart+pos < f.SegmentEnd {
				f.addTopLevel(id, f.SegmentStart+pos)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("mkv: SeekHead: %w", err)
		}
	}
	return nil
}

// loadTopLevel loads the first valid instance of a top-level element.
func (f *File) loadTopLevel(id uint32) (*element, error) {
	for _, off := range f.topLevel[id] {
		gotID, _, _, err := readHeaderAt(f.r, off)
		if err != nil || gotID != id {
			continue // stale SeekHead entry
		}
		e, _, err := f.loadAt(off)
		if err != nil {
			return nil, err
		}
		return &e, nil
	}
	return nil, nil
}

func (f *File) parseInfo(data []byte) error {
	var duration float64
	err := children(data, func(e element) error {
		switch e.id {
		case idTimestScale:
			if v := int64(readUint(e.data)); v > 0 {
				f.TimestampScale = v
			}
		case idDuration:
			duration = readFloat(e.data)
		case idTitle:
			f.Title = readString(e.data)
		}
		return nil
	})
	f.Duration = time.Duration(duration * float64(f.TimestampScale))
	return err
}

func (f *File) parseTracks(data []byte) error {
	return children(data, func(e element) error {
		if e.id != idTrackEntry {
			return nil
		}
		t, err := parseTrack(e.data)
		if err != nil {
			return err
		}
		f.Tracks = append(f.Tracks, t)
		return nil
	})
}

func parseTrack(data []byte) (*Track, error) {
	t := &Track{Enabled: true, Default: true, Language: "eng"}
	var bcp47 string
	err := children(data, func(e element) error {
		switch e.id {
		case idTrackNumber:
			t.Number = readUint(e.data)
		case idTrackUID:
			t.UID = readUint(e.data)
		case idTrackType:
			t.Type = TrackType(readUint(e.data))
		case idFlagEnabled:
			t.Enabled = readUint(e.data) != 0
		case idFlagDefault:
			t.Default = readUint(e.data) != 0
		case idFlagForced:
			t.Forced = readUint(e.data) != 0
		case idName:
			t.Name = readString(e.data)
		case idLanguage:
			t.Language = readString(e.data)
		case idLanguageBCP47:
			bcp47 = readString(e.data)
		case idCodecID:
			t.CodecID = readString(e.data)
		case idCodecPrivate:
			t.CodecPrivate = append([]byte(nil), e.data...)
		case idDefaultDuration:
			t.DefaultDuration = time.Duration(readUint(e.data))
		case idCodecDelay:
			t.CodecDelay = time.Duration(readUint(e.data))
		case idBlockAddMapping:
			var m BlockAdditionMapping
			if err := children(e.data, func(c element) error {
				switch c.id {
				case idBlockAddIDType:
					m.Type = readUint(c.data)
				case idBlockAddIDExtra:
					m.ExtraData = append([]byte(nil), c.data...)
				}
				return nil
			}); err != nil {
				return err
			}
			t.BlockAdditionMappings = append(t.BlockAdditionMappings, m)
		case idVideo:
			v, err := parseVideo(e.data)
			if err != nil {
				return err
			}
			t.Video = v
		case idAudio:
			a := &Audio{SamplingFrequency: 8000, Channels: 1}
			if err := children(e.data, func(c element) error {
				switch c.id {
				case idSamplingFreq:
					a.SamplingFrequency = readFloat(c.data)
				case idOutputSampling:
					a.OutputSamplingFrequency = readFloat(c.data)
				case idChannels:
					a.Channels = readUint(c.data)
				case idBitDepth:
					a.BitDepth = readUint(c.data)
				}
				return nil
			}); err != nil {
				return err
			}
			t.Audio = a
		case idContentEncs:
			return t.parseContentEncodings(e.data)
		}
		return nil
	})
	if bcp47 != "" {
		t.Language = bcp47
	}
	return t, err
}

func parseVideo(data []byte) (*Video, error) {
	v := &Video{}
	err := children(data, func(e element) error {
		switch e.id {
		case idPixelWidth:
			v.PixelWidth = readUint(e.data)
		case idPixelHeight:
			v.PixelHeight = readUint(e.data)
		case idDisplayWidth:
			v.DisplayWidth = readUint(e.data)
		case idDisplayHeight:
			v.DisplayHeight = readUint(e.data)
		case idDisplayUnit:
			v.DisplayUnit = readUint(e.data)
		case idColour:
			c := &Colour{}
			if err := children(e.data, func(ce element) error {
				switch ce.id {
				case idMatrixCoeffs:
					c.MatrixCoefficients = readUint(ce.data)
				case idRange:
					c.Range = readUint(ce.data)
				case idTransferChar:
					c.TransferCharacteristics = readUint(ce.data)
				case idPrimaries:
					c.Primaries = readUint(ce.data)
				case idMaxCLL:
					c.MaxCLL = readUint(ce.data)
				case idMaxFALL:
					c.MaxFALL = readUint(ce.data)
				case idMasteringMeta:
					m := &MasteringMetadata{}
					fields := map[uint32]*float64{
						idPrimaryRX: &m.PrimaryRX, idPrimaryRY: &m.PrimaryRY,
						idPrimaryGX: &m.PrimaryGX, idPrimaryGY: &m.PrimaryGY,
						idPrimaryBX: &m.PrimaryBX, idPrimaryBY: &m.PrimaryBY,
						idWhitePointX: &m.WhitePointX, idWhitePointY: &m.WhitePointY,
						idLuminanceMax: &m.LuminanceMax, idLuminanceMin: &m.LuminanceMin,
					}
					if err := children(ce.data, func(me element) error {
						if p, ok := fields[me.id]; ok {
							*p = readFloat(me.data)
						}
						return nil
					}); err != nil {
						return err
					}
					c.Mastering = m
				}
				return nil
			}); err != nil {
				return err
			}
			v.Colour = c
		}
		return nil
	})
	return v, err
}

func (t *Track) parseContentEncodings(data []byte) error {
	return children(data, func(enc element) error {
		if enc.id != idContentEnc {
			return nil
		}
		scope, encType := uint64(contentEncScopeData), uint64(0)
		algo, haveComp := uint64(contentCompZlib), false
		var settings []byte
		if err := children(enc.data, func(e element) error {
			switch e.id {
			case idContentEncScope:
				scope = readUint(e.data)
			case idContentEncType:
				encType = readUint(e.data)
			case idContentComp:
				haveComp = true
				return children(e.data, func(c element) error {
					switch c.id {
					case idContentCompAlgo:
						algo = readUint(c.data)
					case idContentCompSet:
						settings = append([]byte(nil), c.data...)
					}
					return nil
				})
			}
			return nil
		}); err != nil {
			return err
		}
		if encType != 0 {
			t.unsupportedEncoding = "encrypted track"
			return nil
		}
		// mkvmerge writes an empty ContentEncoding when header-removal
		// analysis finds nothing to strip; it is a no-op.
		if !haveComp || scope&contentEncScopeData == 0 {
			return nil
		}
		switch algo {
		case contentCompStripped:
			t.stripped = settings
		case contentCompZlib:
			t.zlibFrames = true
		default:
			t.unsupportedEncoding = fmt.Sprintf("unsupported ContentCompAlgo %d", algo)
		}
		return nil
	})
}

// decodeFrame undoes the track's content encoding.
func (t *Track) decodeFrame(data []byte) ([]byte, error) {
	if t.zlibFrames {
		zr, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(zr)
	}
	if len(t.stripped) > 0 {
		out := make([]byte, 0, len(t.stripped)+len(data))
		out = append(out, t.stripped...)
		return append(out, data...), nil
	}
	return data, nil
}

func (f *File) parseCues(data []byte) error {
	err := children(data, func(cp element) error {
		if cp.id != idCuePoint {
			return nil
		}
		var tick int64
		var points []CuePoint
		if err := children(cp.data, func(e element) error {
			switch e.id {
			case idCueTime:
				tick = int64(readUint(e.data))
			case idCueTrackPos:
				p := CuePoint{Pos: Position{Cluster: -1}}
				if err := children(e.data, func(c element) error {
					switch c.id {
					case idCueTrack:
						p.Track = readUint(c.data)
					case idCueClusterPo:
						p.Pos.Cluster = f.SegmentStart + int64(readUint(c.data))
					case idCueRelPos:
						p.Pos.Rel = int64(readUint(c.data))
					}
					return nil
				}); err != nil {
					return err
				}
				if p.Pos.Cluster >= 0 {
					points = append(points, p)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		for _, p := range points {
			p.Time = time.Duration(tick * f.TimestampScale)
			f.Cues = append(f.Cues, p)
		}
		return nil
	})
	sort.SliceStable(f.Cues, func(i, j int) bool { return f.Cues[i].Pos.Less(f.Cues[j].Pos) })
	return err
}
