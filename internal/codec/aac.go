package codec

import (
	"fmt"
	"strings"
)

// MPEG-4 audio object types.
const (
	aotEscape = 31
	aotAACLC  = 2
	aotSBR    = 5
	aotPS     = 29

	aacFrameSamples      = 1024
	aacShortFrameSamples = 960
	aacExplicitFreqIndex = 15
)

var aacSampleRates = [...]uint32{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// AACConfig is the relevant part of an AudioSpecificConfig.
type AACConfig struct {
	ObjectType uint8 // as signalled (5 or 29 for HE-AAC)
	SampleRate uint32
	Channels   uint8
	// FrameSamples is samples per frame at SampleRate (1024 or 960).
	FrameSamples uint32
}

// ParseAudioSpecificConfig parses an MPEG-4 AudioSpecificConfig.
func ParseAudioSpecificConfig(asc []byte) (*AACConfig, error) {
	r := newBitReader(asc)
	readAOT := func() uint8 {
		aot := uint8(r.u(5))
		if aot == aotEscape {
			aot = 32 + uint8(r.u(6))
		}
		return aot
	}
	readRate := func() uint32 {
		idx := r.u(4)
		if idx == aacExplicitFreqIndex {
			return uint32(r.u(24))
		}
		if int(idx) < len(aacSampleRates) {
			return aacSampleRates[idx]
		}
		return 0
	}
	c := &AACConfig{FrameSamples: aacFrameSamples}
	c.ObjectType = readAOT()
	c.SampleRate = readRate()
	c.Channels = uint8(r.u(4))
	core := c.ObjectType
	if core == aotSBR || core == aotPS {
		readRate() // extension sampling frequency
		core = readAOT()
	}
	switch core {
	case 1, 2, 3, 4, 6, 7, 17, 19, 20, 21, 22, 23:
		if r.flag() { // frameLengthFlag
			c.FrameSamples = aacShortFrameSamples
		}
	}
	if r.err != nil || c.SampleRate == 0 {
		return nil, fmt.Errorf("codec: invalid AudioSpecificConfig %x", asc)
	}
	return c, nil
}

// CodecString is the RFC 6381 string, e.g. "mp4a.40.2".
func (c *AACConfig) CodecString() string {
	return fmt.Sprintf("mp4a.40.%d", c.ObjectType)
}

// SynthesizeASC builds an AudioSpecificConfig for legacy Matroska codec IDs
// (A_AAC/MPEG4/LC etc.) that carry no CodecPrivate.
func SynthesizeASC(codecID string, sampleRate uint32, channels uint8) []byte {
	aot := uint64(aotAACLC)
	switch {
	case strings.HasSuffix(codecID, "/MAIN"):
		aot = 1
	case strings.HasSuffix(codecID, "/SSR"):
		aot = 3
	case strings.HasSuffix(codecID, "/LTP"):
		aot = 4
	}
	var w bitWriter
	w.put(5, aot)
	idx := -1
	for i, r := range aacSampleRates {
		if r == sampleRate {
			idx = i
		}
	}
	if idx < 0 {
		w.put(4, aacExplicitFreqIndex)
		w.put(24, uint64(sampleRate))
	} else {
		w.put(4, uint64(idx))
	}
	w.put(4, uint64(channels))
	w.put(3, 0) // GASpecificConfig: frameLength, dependsOnCore, extension
	return w.b
}
