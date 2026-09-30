package transmux

import (
	"fmt"
	"math"
	"strings"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/codec"
	"git.foxden.network/FoxDen/foxCast/internal/fmp4"
	"git.foxden.network/FoxDen/foxCast/internal/mkv"
)

const (
	videoTimescale = 90000

	// Matroska BlockAddIDType values carrying Dolby Vision configuration.
	blockAddDvcC = 0x64766343 // 'dvcC'
	blockAddDvvC = 0x64767643 // 'dvvC'

	// Dolby Vision base-layer signal compatibility IDs.
	dvCompatNone   = 0
	dvCompatHDR10  = 1
	dvCompatSDR    = 2
	dvCompatHLG    = 4
	dvCompatBluRay = 6
	dvProfile5     = 5
	dvProfile8     = 8

	videoRangeSDR = "SDR"
	videoRangePQ  = "PQ"
	videoRangeHLG = "HLG"

	matroskaDisplayUnitPixels = 0
	// matroskaRangeFull is Colour/Range "full range (no clipping)".
	matroskaRangeFull = 2
)

// Matroska codec IDs.
const (
	codecAVC  = "V_MPEG4/ISO/AVC"
	codecHEVC = "V_MPEGH/ISO/HEVC"
	codecAAC  = "A_AAC"
	codecAC3  = "A_AC3"
	codecEAC3 = "A_EAC3"
	codecFLAC = "A_FLAC"
	codecOpus = "A_OPUS"
	codecMP3  = "A_MPEG/L3"
	codecALAC = "A_ALAC"
)

// videoTrack is the mapping of the Matroska video track to fMP4/HLS.
type videoTrack struct {
	src        *mkv.Track
	codecs     string
	videoRange string
	width      uint32
	height     uint32
	frameRate  float64
	init       []byte
	// filter rewrites each access unit (e.g. dropping Dolby Vision layers).
	filter func([]byte) []byte
	// defaultDuration is the frame duration in videoTimescale ticks, or 0.
	defaultDuration uint32
	// note describes any lossy adaptation, for the user.
	note string
}

func newVideoTrack(t *mkv.Track, opts Options) (*videoTrack, error) {
	if err := t.Supported(); err != nil {
		return nil, err
	}
	if t.Video == nil || t.Video.PixelWidth == 0 {
		return nil, fmt.Errorf("video track %d has no dimensions", t.Number)
	}
	v := &videoTrack{src: t, width: uint32(t.Video.PixelWidth), height: uint32(t.Video.PixelHeight), videoRange: videoRangeSDR}
	if t.DefaultDuration > 0 {
		v.frameRate = math.Round(float64(time.Second)/float64(t.DefaultDuration)*1000) / 1000
		v.defaultDuration = uint32(toTimescale(t.DefaultDuration, videoTimescale))
	}

	var children [][]byte
	var bitstreamColor codec.ColorInfo
	fourcc := ""
	switch t.CodecID {
	case codecAVC:
		cfg, err := codec.ParseAVCConfig(t.CodecPrivate)
		if err != nil {
			return nil, err
		}
		fourcc, v.codecs = "avc1", cfg.CodecString()
		children = append(children, fmp4.ConfigBox("avcC", cfg.Record))
	case codecHEVC:
		cfg, err := codec.ParseHEVCConfig(t.CodecPrivate)
		if err != nil {
			return nil, err
		}
		bitstreamColor = cfg.Color
		fourcc, v.codecs = "hvc1", cfg.CodecString()
		children = append(children, fmp4.ConfigBox("hvcC", cfg.Record))

		dv := dolbyVisionConfig(t)
		switch {
		case dv == nil:
		case opts.DolbyVision && (dv.Profile == dvProfile5 || dv.Profile == dvProfile8):
			// Single-layer profiles Apple TV decodes natively.
			fourcc, v.codecs = "dvh1", dv.CodecString("dvh1")
			children = append(children, fmp4.ConfigBox(dv.BoxType(), dv.Record))
			if dv.Profile == dvProfile5 {
				v.videoRange = videoRangePQ
			}
		default:
			// Dual-layer profile 7 (or DV disabled): play the base layer. It
			// is HDR10/SDR/HLG on its own; drop RPU and enhancement-layer NALs.
			if dv.Profile == dvProfile5 {
				return nil, fmt.Errorf("video uses Dolby Vision profile 5, which has no backward-compatible base layer; enable Dolby Vision")
			}
			lengthSize := cfg.LengthSize
			v.filter = func(b []byte) []byte { return codec.FilterNALs(b, lengthSize, codec.IsDolbyVisionNAL) }
			v.note = fmt.Sprintf("Dolby Vision profile %d is not supported by Apple TV; playing the %s base layer", dv.Profile, blCompatName(dv.BLCompatibility))
		}
	default:
		return nil, fmt.Errorf("video codec %s cannot be transmuxed for Apple TV (needs H.264 or HEVC; would require transcoding)", t.CodecID)
	}

	// Colour: prefer the container, fall back to the bitstream VUI.
	var primaries, transfer, matrix uint64
	fullRange := false
	if c := t.Video.Colour; c != nil && c.TransferCharacteristics != 0 {
		primaries, transfer, matrix = c.Primaries, c.TransferCharacteristics, c.MatrixCoefficients
		fullRange = c.Range == matroskaRangeFull
	} else if bitstreamColor.Present {
		primaries, transfer, matrix = uint64(bitstreamColor.Primaries), uint64(bitstreamColor.Transfer), uint64(bitstreamColor.MatrixCoefficients)
		fullRange = bitstreamColor.FullRange
	}
	switch transfer {
	case codec.TransferPQ:
		v.videoRange = videoRangePQ
	case codec.TransferHLG:
		v.videoRange = videoRangeHLG
	}
	if transfer != 0 {
		children = append(children, fmp4.Colr(uint16(primaries), uint16(transfer), uint16(matrix), fullRange))
	}
	if c := t.Video.Colour; c != nil {
		if m := c.Mastering; m != nil && m.LuminanceMax > 0 {
			children = append(children, fmp4.Mdcv(fmp4.MasteringDisplay{
				GX: m.PrimaryGX, GY: m.PrimaryGY, BX: m.PrimaryBX, BY: m.PrimaryBY, RX: m.PrimaryRX, RY: m.PrimaryRY,
				WhiteX: m.WhitePointX, WhiteY: m.WhitePointY, MaxLuminance: m.LuminanceMax, MinLuminance: m.LuminanceMin,
			}))
		}
		if c.MaxCLL > 0 || c.MaxFALL > 0 {
			children = append(children, fmp4.Clli(uint16(c.MaxCLL), uint16(c.MaxFALL)))
		}
	}

	displayW, displayH := v.width, v.height
	if vi := t.Video; vi.DisplayUnit == matroskaDisplayUnitPixels && vi.DisplayWidth > 0 && vi.DisplayHeight > 0 &&
		(vi.DisplayWidth != vi.PixelWidth || vi.DisplayHeight != vi.PixelHeight) {
		h := vi.DisplayWidth * vi.PixelHeight
		vs := vi.DisplayHeight * vi.PixelWidth
		g := gcd(h, vs)
		children = append(children, fmp4.Pasp(uint32(h/g), uint32(vs/g)))
		displayW, displayH = uint32(vi.DisplayWidth), uint32(vi.DisplayHeight)
	}

	entry := fmp4.VisualSampleEntry(fourcc, uint16(v.width), uint16(v.height), children...)
	v.init = fmp4.InitSegment(fmp4.Track{
		Kind: fmp4.Video, Timescale: videoTimescale, SampleEntry: entry,
		Width: displayW, Height: displayH, Language: "und",
	})
	return v, nil
}

func dolbyVisionConfig(t *mkv.Track) *codec.DolbyVisionConfig {
	for _, m := range t.BlockAdditionMappings {
		if m.Type == blockAddDvcC || m.Type == blockAddDvvC {
			if dv, err := codec.ParseDolbyVisionConfig(m.ExtraData); err == nil {
				return dv
			}
		}
	}
	return nil
}

func blCompatName(id uint8) string {
	switch id {
	case dvCompatHDR10, dvCompatBluRay:
		return "HDR10"
	case dvCompatSDR:
		return "SDR"
	case dvCompatHLG:
		return "HLG"
	case dvCompatNone:
		return "non-compatible"
	}
	return fmt.Sprintf("compatibility-%d", id)
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

// toTimescale converts a duration to ticks of the given timescale.
func toTimescale(d time.Duration, timescale uint32) int64 {
	return int64(math.Round(float64(d) * float64(timescale) / float64(time.Second)))
}

// audioTrack is the mapping of one Matroska audio track to an HLS rendition.
type audioTrack struct {
	src       *mkv.Track
	codecs    string
	timescale uint32
	channels  int
	init      []byte
	// frameSamples returns a frame's length in timescale ticks.
	frameSamples func(frame []byte) (uint32, error)
	// fixedSamples is the constant frame length if the codec has one; it
	// lets fragment start times snap to the frame grid (see fragments.go).
	fixedSamples uint32
	// firstPTS anchors that grid.
	firstPTS time.Duration
	name     string
}

// audioCodecSupported reports whether Apple TV can decode codecID from fMP4.
func audioCodecSupported(codecID string) bool {
	switch {
	case strings.HasPrefix(codecID, codecAAC), codecID == codecAC3, codecID == codecEAC3,
		codecID == codecFLAC, codecID == codecOpus, codecID == codecMP3, codecID == codecALAC:
		return true
	}
	return false
}

// newAudioTrack maps t; firstFrame is its first frame (needed for AC-3/E-AC-3
// configuration and constant frame sizes).
func newAudioTrack(t *mkv.Track, firstFrame *mkv.Frame) (*audioTrack, error) {
	if err := t.Supported(); err != nil {
		return nil, err
	}
	if !audioCodecSupported(t.CodecID) {
		return nil, fmt.Errorf("audio codec %s is not supported by Apple TV", t.CodecID)
	}
	if t.Audio == nil {
		return nil, fmt.Errorf("audio track %d has no audio settings", t.Number)
	}
	if firstFrame == nil {
		return nil, fmt.Errorf("audio track %d has no frames", t.Number)
	}
	a := &audioTrack{src: t, channels: int(t.Audio.Channels), firstPTS: firstFrame.PTS}
	rate := uint32(t.Audio.SamplingFrequency)
	var entry []byte
	constant := func(n uint32) func([]byte) (uint32, error) {
		return func([]byte) (uint32, error) { return n, nil }
	}

	switch {
	case strings.HasPrefix(t.CodecID, codecAAC):
		asc := t.CodecPrivate
		if len(asc) == 0 {
			asc = codec.SynthesizeASC(t.CodecID, rate, uint8(t.Audio.Channels))
		}
		cfg, err := codec.ParseAudioSpecificConfig(asc)
		if err != nil {
			return nil, err
		}
		a.codecs, a.timescale = cfg.CodecString(), cfg.SampleRate
		a.fixedSamples = cfg.FrameSamples
		a.frameSamples = constant(cfg.FrameSamples)
		entry = fmp4.AudioSampleEntry("mp4a", uint16(a.channels), cfg.SampleRate, fmp4.Esds(fmp4.OTIAAC, 0, asc))
	case t.CodecID == codecAC3:
		info, err := codec.ParseAC3(firstFrame.Data)
		if err != nil {
			return nil, err
		}
		a.codecs, a.timescale = "ac-3", info.SampleRate
		a.fixedSamples = codec.AC3FrameSamples
		a.frameSamples = constant(codec.AC3FrameSamples)
		entry = fmp4.AudioSampleEntry("ac-3", uint16(a.channels), info.SampleRate, fmp4.ConfigBox("dac3", info.Dac3()))
	case t.CodecID == codecEAC3:
		info, err := codec.ParseEAC3(firstFrame.Data)
		if err != nil {
			return nil, err
		}
		n, err := codec.EAC3FrameSamples(firstFrame.Data)
		if err != nil {
			return nil, err
		}
		a.codecs, a.timescale = "ec-3", info.SampleRate
		a.fixedSamples = n
		a.frameSamples = codec.EAC3FrameSamples
		entry = fmp4.AudioSampleEntry("ec-3", uint16(a.channels), info.SampleRate, fmp4.ConfigBox("dec3", info.Dec3()))
	case t.CodecID == codecFLAC:
		cfg, err := codec.ParseFLACPrivate(t.CodecPrivate)
		if err != nil {
			return nil, err
		}
		a.codecs, a.timescale = "fLaC", cfg.SampleRate
		a.fixedSamples = cfg.FixedBlockSize
		a.frameSamples = codec.FLACFrameSamples
		entry = fmp4.AudioSampleEntry("fLaC", uint16(cfg.Channels), cfg.SampleRate,
			fmp4.FullBox("dfLa", 0, 0, func(w *fmp4.Buf) { w.Raw(cfg.DfLa()) }))
	case t.CodecID == codecOpus:
		cfg, err := codec.ParseOpusHead(t.CodecPrivate)
		if err != nil {
			return nil, err
		}
		a.codecs, a.timescale = "opus", codec.OpusSampleRate
		if n, err := codec.OpusPacketSamples(firstFrame.Data); err == nil {
			a.fixedSamples = n
		}
		a.frameSamples = codec.OpusPacketSamples
		entry = fmp4.AudioSampleEntry("Opus", uint16(cfg.Channels), codec.OpusSampleRate, fmp4.ConfigBox("dOps", cfg.DOps()))
	case t.CodecID == codecMP3:
		info, err := codec.ParseMPAHeader(firstFrame.Data)
		if err != nil {
			return nil, err
		}
		a.codecs, a.timescale = "mp4a.6B", info.SampleRate
		a.fixedSamples = info.FrameSamples
		a.frameSamples = constant(info.FrameSamples)
		entry = fmp4.AudioSampleEntry("mp4a", uint16(info.Channels), info.SampleRate, fmp4.Esds(fmp4.OTIMP3, 0, nil))
	case t.CodecID == codecALAC:
		cfg, err := codec.ParseALACConfig(t.CodecPrivate)
		if err != nil {
			return nil, err
		}
		a.codecs, a.timescale = "alac", cfg.SampleRate
		a.frameSamples = constant(cfg.FrameLength)
		a.fixedSamples = cfg.FrameLength
		entry = fmp4.AudioSampleEntry("alac", uint16(cfg.Channels), cfg.SampleRate,
			fmp4.FullBox("alac", 0, 0, func(w *fmp4.Buf) { w.Raw(cfg.Cookie()) }))
	}
	if a.timescale == 0 {
		return nil, fmt.Errorf("audio track %d: unknown sample rate", t.Number)
	}
	a.init = fmp4.InitSegment(fmp4.Track{
		Kind: fmp4.Audio, Timescale: a.timescale, SampleEntry: entry, Language: mdhdLanguage(t.Language),
	})
	return a, nil
}

// channelLayoutName describes a channel count the way players label tracks.
func channelLayoutName(n int) string {
	switch n {
	case 1:
		return "Mono"
	case 2:
		return "Stereo"
	case 6:
		return "5.1"
	case 8:
		return "7.1"
	}
	return fmt.Sprintf("%dch", n)
}

// codecDisplayName names a Matroska codec ID for track labels.
func codecDisplayName(codecID string) string {
	switch {
	case strings.HasPrefix(codecID, codecAAC):
		return "AAC"
	case codecID == codecAC3:
		return "Dolby Digital"
	case codecID == codecEAC3:
		return "Dolby Digital Plus"
	case codecID == codecFLAC:
		return "FLAC"
	case codecID == codecOpus:
		return "Opus"
	case codecID == codecMP3:
		return "MP3"
	case codecID == codecALAC:
		return "ALAC"
	case codecID == "A_TRUEHD":
		return "Dolby TrueHD"
	case strings.HasPrefix(codecID, "A_DTS"):
		return "DTS"
	case strings.HasPrefix(codecID, "A_PCM"):
		return "PCM"
	case codecID == "A_VORBIS":
		return "Vorbis"
	}
	return codecID
}
