package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// FLAC constants.
const (
	flacMagic            = "fLaC"
	flacBlockStreamInfo  = 0
	flacLastBlockFlag    = 0x80
	flacStreamInfoLen    = 34
	flacBlockHeaderLen   = 4
	flacFrameSyncMask    = 0xFFFE
	flacFrameSync        = 0xFFF8
	flacBlockSize8Bit    = 6
	flacBlockSize16Bit   = 7
	flacBlockSize192     = 1
	flacUTF8MaxBytes     = 7
	flacMinFrameHeaderSz = 5
)

// FLACConfig is the STREAMINFO of a FLAC stream.
type FLACConfig struct {
	SampleRate uint32
	Channels   uint8
	BitDepth   uint8
	// FixedBlockSize is the block size of a fixed-blocksize stream (every
	// frame but the last has it), or 0.
	FixedBlockSize uint32
	streamInfo     []byte
}

// ParseFLACPrivate parses Matroska's A_FLAC CodecPrivate ("fLaC" +
// metadata blocks) and returns its STREAMINFO.
func ParseFLACPrivate(priv []byte) (*FLACConfig, error) {
	if len(priv) >= 4 && string(priv[:4]) == flacMagic {
		priv = priv[4:]
	}
	for len(priv) >= flacBlockHeaderLen {
		typ := priv[0] &^ flacLastBlockFlag
		n := int(priv[1])<<16 | int(priv[2])<<8 | int(priv[3])
		if len(priv) < flacBlockHeaderLen+n {
			break
		}
		if typ == flacBlockStreamInfo && n == flacStreamInfoLen {
			si := priv[flacBlockHeaderLen : flacBlockHeaderLen+n]
			r := newBitReader(si[10:])
			c := &FLACConfig{streamInfo: append([]byte(nil), si...)}
			c.SampleRate = uint32(r.u(20))
			c.Channels = uint8(r.u(3)) + 1
			c.BitDepth = uint8(r.u(5)) + 1
			if minBlock, maxBlock := binary.BigEndian.Uint16(si), binary.BigEndian.Uint16(si[2:]); minBlock == maxBlock {
				c.FixedBlockSize = uint32(minBlock)
			}
			return c, nil
		}
		if priv[0]&flacLastBlockFlag != 0 {
			break
		}
		priv = priv[flacBlockHeaderLen+n:]
	}
	return nil, errors.New("codec: FLAC STREAMINFO not found")
}

// DfLa is the FLACSpecificBox payload (after the full-box header): the
// STREAMINFO block marked as the last metadata block.
func (c *FLACConfig) DfLa() []byte {
	out := []byte{flacLastBlockFlag | flacBlockStreamInfo, 0, 0, flacStreamInfoLen}
	return append(out, c.streamInfo...)
}

// FLACFrameSamples returns the block size from a FLAC frame header.
func FLACFrameSamples(frame []byte) (uint32, error) {
	if len(frame) < flacMinFrameHeaderSz || binary.BigEndian.Uint16(frame)&flacFrameSyncMask != flacFrameSync {
		return 0, errors.New("codec: not a FLAC frame")
	}
	code := frame[2] >> 4
	// Skip the UTF-8-style coded frame/sample number after byte 3.
	p := 4
	lead := frame[p]
	n := 1
	for lead&0x80 != 0 && n <= flacUTF8MaxBytes {
		lead <<= 1
		n++
	}
	if n > 1 {
		n--
	}
	p += n
	switch {
	case code == flacBlockSize192:
		return 192, nil
	case code >= 2 && code <= 5:
		return 576 << (code - 2), nil
	case code == flacBlockSize8Bit:
		if len(frame) <= p {
			return 0, errShortBitstream
		}
		return uint32(frame[p]) + 1, nil
	case code == flacBlockSize16Bit:
		if len(frame) <= p+1 {
			return 0, errShortBitstream
		}
		return uint32(binary.BigEndian.Uint16(frame[p:])) + 1, nil
	case code >= 8:
		return 256 << (code - 8), nil
	}
	return 0, fmt.Errorf("codec: reserved FLAC block size code %d", code)
}

// Opus constants.
const (
	opusHeadMagic    = "OpusHead"
	opusHeadMinLen   = 19
	OpusSampleRate   = 48000
	opusMaxFrames    = 48
	opusSamplesPerMs = 48
)

// OpusConfig is the parsed OpusHead.
type OpusConfig struct {
	Channels uint8
	dops     []byte
}

// ParseOpusHead converts an OpusHead (Matroska A_OPUS CodecPrivate) into a
// dOps payload.
func ParseOpusHead(head []byte) (*OpusConfig, error) {
	if len(head) < opusHeadMinLen || string(head[:8]) != opusHeadMagic {
		return nil, errors.New("codec: invalid OpusHead")
	}
	channels := head[9]
	family := head[18]
	var w []byte
	w = append(w, 0, channels)
	w = binary.BigEndian.AppendUint16(w, binary.LittleEndian.Uint16(head[10:]))
	w = binary.BigEndian.AppendUint32(w, binary.LittleEndian.Uint32(head[12:]))
	w = binary.BigEndian.AppendUint16(w, binary.LittleEndian.Uint16(head[16:]))
	w = append(w, family)
	if family != 0 {
		if len(head) < 21+int(channels) {
			return nil, errors.New("codec: truncated OpusHead mapping table")
		}
		w = append(w, head[19:21+int(channels)]...)
	}
	return &OpusConfig{Channels: channels, dops: w}, nil
}

// DOps is the OpusSpecificBox payload.
func (c *OpusConfig) DOps() []byte { return c.dops }

// OpusPacketSamples returns the 48 kHz sample count of an Opus packet (RFC 6716 §3.1).
func OpusPacketSamples(pkt []byte) (uint32, error) {
	if len(pkt) < 1 {
		return 0, errShortBitstream
	}
	cfg := pkt[0] >> 3
	var tenthsMs uint32
	switch {
	case cfg < 12: // SILK
		tenthsMs = [...]uint32{100, 200, 400, 600}[cfg&3]
	case cfg < 16: // Hybrid
		tenthsMs = [...]uint32{100, 200}[cfg&1]
	default: // CELT
		tenthsMs = [...]uint32{25, 50, 100, 200}[cfg&3]
	}
	frames := uint32(1)
	switch pkt[0] & 3 {
	case 1, 2:
		frames = 2
	case 3:
		if len(pkt) < 2 {
			return 0, errShortBitstream
		}
		frames = uint32(pkt[1] & 0x3F)
		if frames == 0 || frames > opusMaxFrames {
			return 0, errors.New("codec: invalid Opus frame count")
		}
	}
	return frames * tenthsMs * opusSamplesPerMs / 10, nil
}

// MPEG audio (MP3) constants.
const (
	mpaSyncMask   = 0xFFE0
	mpaVersion1   = 3
	mpaVersion2   = 2
	mpaVersion25  = 0
	mpaLayer1     = 3
	mpaLayer2     = 2
	mpaLayer3     = 1
	mpaChannMono  = 3
	mpaHeaderSize = 4
)

// MPAInfo is an MPEG-1/2 audio frame header.
type MPAInfo struct {
	SampleRate   uint32
	Channels     uint8
	FrameSamples uint32
}

// ParseMPAHeader parses an MPEG audio frame header.
func ParseMPAHeader(frame []byte) (*MPAInfo, error) {
	if len(frame) < mpaHeaderSize || binary.BigEndian.Uint16(frame)&mpaSyncMask != mpaSyncMask {
		return nil, errors.New("codec: not an MPEG audio frame")
	}
	version := (frame[1] >> 3) & 3
	layer := (frame[1] >> 1) & 3
	rateIdx := (frame[2] >> 2) & 3
	if version == 1 || layer == 0 || rateIdx == 3 {
		return nil, errors.New("codec: invalid MPEG audio header")
	}
	rate := [...]uint32{44100, 48000, 32000}[rateIdx]
	switch version {
	case mpaVersion2:
		rate /= 2
	case mpaVersion25:
		rate /= 4
	}
	samples := uint32(1152)
	switch {
	case layer == mpaLayer1:
		samples = 384
	case layer == mpaLayer3 && version != mpaVersion1:
		samples = 576
	}
	channels := uint8(2)
	if frame[3]>>6 == mpaChannMono {
		channels = 1
	}
	return &MPAInfo{SampleRate: rate, Channels: channels, FrameSamples: samples}, nil
}

// ALAC constants.
const alacConfigLen = 24

// ALACConfig is the ALACSpecificConfig ("magic cookie").
type ALACConfig struct {
	FrameLength uint32
	Channels    uint8
	SampleRate  uint32
	cookie      []byte
}

// ParseALACConfig parses Matroska's A_ALAC CodecPrivate.
func ParseALACConfig(priv []byte) (*ALACConfig, error) {
	// Some muxers include the enclosing 'alac' atom header and version.
	if len(priv) >= alacConfigLen+12 && string(priv[4:8]) == "alac" {
		priv = priv[12:]
	}
	if len(priv) < alacConfigLen {
		return nil, errors.New("codec: invalid ALAC config")
	}
	c := &ALACConfig{
		FrameLength: binary.BigEndian.Uint32(priv[0:]),
		Channels:    priv[9],
		SampleRate:  binary.BigEndian.Uint32(priv[20:]),
		cookie:      append([]byte(nil), priv[:alacConfigLen]...),
	}
	return c, nil
}

// Cookie is the ALACSpecificConfig for the 'alac' child box.
func (c *ALACConfig) Cookie() []byte { return c.cookie }
