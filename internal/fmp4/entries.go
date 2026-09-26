package fmp4

import "math"

const (
	fixed16_16One       = 0x00010000
	screenResolution72  = 0x00480000
	visualDepth         = 0x0018
	audioSampleSize     = 16
	maxAudioSampleRate  = 0xFFFF
	esdsTag             = 0x03
	decoderConfigTag    = 0x04
	decSpecificInfoTag  = 0x05
	slConfigTag         = 0x06
	slConfigPredefined  = 0x02
	streamTypeAudio     = 0x05
	decoderConfigFlags  = streamTypeAudio<<2 | 1 // upStream=0, reserved=1
	mdcvChromaScale     = 50000
	mdcvLuminanceScale  = 10000
	colrNCLXFullRange   = 0x80
	compressorNameBytes = 32
)

// Object type indications for esds.
const (
	OTIAAC = 0x40
	OTIMP3 = 0x6B
)

// VisualSampleEntry builds a video sample entry (avc1, hvc1, dvh1, …) with
// the given child boxes (codec config, colr, pasp, …).
func VisualSampleEntry(fourcc string, width, height uint16, children ...[]byte) []byte {
	return Box(fourcc, func(w *Buf) {
		w.Zeros(6)
		w.U16(1) // data_reference_index
		w.U16(0) // pre_defined
		w.U16(0) // reserved
		w.Zeros(12)
		w.U16(width)
		w.U16(height)
		w.U32(screenResolution72)
		w.U32(screenResolution72)
		w.U32(0)
		w.U16(1) // frame_count
		w.Zeros(compressorNameBytes)
		w.U16(visualDepth)
		w.U16(0xFFFF) // pre_defined = -1
		for _, c := range children {
			w.Raw(c)
		}
	})
}

// AudioSampleEntry builds an audio sample entry. Sample rates above 65535
// are written as 0, as the 16.16 field cannot hold them.
func AudioSampleEntry(fourcc string, channels uint16, sampleRate uint32, children ...[]byte) []byte {
	if sampleRate > maxAudioSampleRate {
		sampleRate = 0
	}
	return Box(fourcc, func(w *Buf) {
		w.Zeros(6)
		w.U16(1) // data_reference_index
		w.Zeros(8)
		w.U16(channels)
		w.U16(audioSampleSize)
		w.U16(0) // pre_defined
		w.U16(0) // reserved
		w.U32(sampleRate << 16)
		for _, c := range children {
			w.Raw(c)
		}
	})
}

// ConfigBox wraps a codec configuration record (avcC, hvcC, dvcC, …).
func ConfigBox(typ string, record []byte) []byte {
	return Box(typ, func(w *Buf) { w.Raw(record) })
}

func descriptor(w *Buf, tag byte, body []byte) {
	w.U8(tag)
	n := len(body)
	// Size is a 7-bit-per-byte big-endian varint.
	var sz []byte
	for {
		sz = append([]byte{byte(n & 0x7F)}, sz...)
		n >>= 7
		if n == 0 {
			break
		}
	}
	for i := 0; i < len(sz)-1; i++ {
		sz[i] |= 0x80
	}
	w.Raw(sz)
	w.Raw(body)
}

// Esds builds an MPEG-4 elementary stream descriptor box.
func Esds(objectType byte, avgBitrate uint32, decoderSpecific []byte) []byte {
	var dcd Buf
	dcd.U8(objectType)
	dcd.U8(decoderConfigFlags)
	dcd.U24(0) // bufferSizeDB
	dcd.U32(avgBitrate)
	dcd.U32(avgBitrate)
	if len(decoderSpecific) > 0 {
		descriptor(&dcd, decSpecificInfoTag, decoderSpecific)
	}
	var es Buf
	es.U16(0) // ES_ID
	es.U8(0)  // flags
	descriptor(&es, decoderConfigTag, dcd.Bytes())
	descriptor(&es, slConfigTag, []byte{slConfigPredefined})
	return FullBox("esds", 0, 0, func(w *Buf) {
		descriptor(w, esdsTag, es.Bytes())
	})
}

// Colr builds an nclx colour box.
func Colr(primaries, transfer, matrix uint16, fullRange bool) []byte {
	return Box("colr", func(w *Buf) {
		w.Raw([]byte("nclx"))
		w.U16(primaries)
		w.U16(transfer)
		w.U16(matrix)
		if fullRange {
			w.U8(colrNCLXFullRange)
		} else {
			w.U8(0)
		}
	})
}

// Pasp builds a pixel aspect ratio box.
func Pasp(hSpacing, vSpacing uint32) []byte {
	return Box("pasp", func(w *Buf) {
		w.U32(hSpacing)
		w.U32(vSpacing)
	})
}

// Clli builds a content light level box.
func Clli(maxCLL, maxFALL uint16) []byte {
	return Box("clli", func(w *Buf) {
		w.U16(maxCLL)
		w.U16(maxFALL)
	})
}

// MasteringDisplay holds SMPTE 2086 values in natural units (CIE xy, cd/m²).
type MasteringDisplay struct {
	// Primaries in G, B, R order, as mdcv and HEVC SEI store them.
	GX, GY, BX, BY, RX, RY float64
	WhiteX, WhiteY         float64
	MaxLuminance           float64
	MinLuminance           float64
}

// Mdcv builds a mastering display colour volume box.
func Mdcv(m MasteringDisplay) []byte {
	chroma := func(v float64) uint16 { return uint16(math.Round(v * mdcvChromaScale)) }
	lum := func(v float64) uint32 { return uint32(math.Round(v * mdcvLuminanceScale)) }
	return Box("mdcv", func(w *Buf) {
		for _, v := range []float64{m.GX, m.GY, m.BX, m.BY, m.RX, m.RY, m.WhiteX, m.WhiteY} {
			w.U16(chroma(v))
		}
		w.U32(lum(m.MaxLuminance))
		w.U32(lum(m.MinLuminance))
	})
}

// Btrt builds a bit rate box.
func Btrt(maxBitrate, avgBitrate uint32) []byte {
	return Box("btrt", func(w *Buf) {
		w.U32(0) // bufferSizeDB
		w.U32(maxBitrate)
		w.U32(avgBitrate)
	})
}
