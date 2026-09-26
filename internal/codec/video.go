package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// HEVC constants (ITU-T H.265).
const (
	hevcNALSPS            = 33
	hevcNALUnspec62       = 62 // Dolby Vision RPU
	hevcNALUnspec63       = 63 // Dolby Vision enhancement layer
	hvcCMinLen            = 23
	hvcCLengthSizeOffset  = 21
	hvcCArraysOffset      = 22
	hevcScalingSizes      = 4
	hevcScalingMatrices   = 6
	hevcMaxSubLayers      = 8
	hevcExtendedSARIdc    = 255
	hevcMaxShortTermRPS   = 64
	hevcNALHeaderLen      = 2
	avcCMinLen            = 7
	dvConfigMinLen        = 4
	dvProfileBLCompatible = 8 // profiles ≥ 8 use a backward-compatible base layer
)

// Transfer characteristics (ITU-T H.273) that imply HDR.
const (
	TransferPQ  = 16
	TransferHLG = 18
)

// ColorInfo is the colour description from a video bitstream.
type ColorInfo struct {
	Present                                 bool
	Primaries, Transfer, MatrixCoefficients uint8
	FullRange                               bool
}

// HEVCConfig is a parsed HEVCDecoderConfigurationRecord.
type HEVCConfig struct {
	Record     []byte
	LengthSize int
	Color      ColorInfo
	codec      string
}

// ParseHEVCConfig parses an hvcC record (Matroska V_MPEGH/ISO/HEVC
// CodecPrivate) and the colour description of its first SPS.
func ParseHEVCConfig(rec []byte) (*HEVCConfig, error) {
	if len(rec) < hvcCMinLen || rec[0] != 1 {
		return nil, errors.New("codec: invalid hvcC record")
	}
	c := &HEVCConfig{Record: rec, LengthSize: int(rec[hvcCLengthSizeOffset]&3) + 1}

	space := rec[1] >> 6
	tier := "L"
	if rec[1]&0x20 != 0 {
		tier = "H"
	}
	var compat uint32
	flags := binary.BigEndian.Uint32(rec[2:])
	for i := 0; i < 32; i++ {
		if flags&(1<<i) != 0 {
			compat |= 1 << (31 - i)
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "hvc1.%s%d.%X.%s%d", []string{"", "A", "B", "C"}[space], rec[1]&0x1F, compat, tier, rec[12])
	constraints := rec[6:12]
	last := len(constraints)
	for last > 0 && constraints[last-1] == 0 {
		last--
	}
	for _, b := range constraints[:last] {
		fmt.Fprintf(&sb, ".%X", b)
	}
	c.codec = sb.String()

	// Find the SPS for the colour description.
	p := rec[hvcCArraysOffset+1:]
	for arrays := int(rec[hvcCArraysOffset]); arrays > 0 && len(p) >= 3; arrays-- {
		typ := p[0] & 0x3F
		n := int(binary.BigEndian.Uint16(p[1:]))
		p = p[3:]
		for i := 0; i < n && len(p) >= 2; i++ {
			l := int(binary.BigEndian.Uint16(p))
			if len(p) < 2+l {
				return c, nil
			}
			if typ == hevcNALSPS && !c.Color.Present {
				if ci, err := parseHEVCSPSColor(p[2 : 2+l]); err == nil {
					c.Color = ci
				}
			}
			p = p[2+l:]
		}
	}
	return c, nil
}

// CodecString is the RFC 6381 string, e.g. "hvc1.2.4.L153.B0".
func (c *HEVCConfig) CodecString() string { return c.codec }

// parseHEVCSPSColor walks an HEVC SPS up to the VUI colour description.
func parseHEVCSPSColor(nal []byte) (ColorInfo, error) {
	if len(nal) < hevcNALHeaderLen {
		return ColorInfo{}, errShortBitstream
	}
	r := newBitReader(unescapeRBSP(nal[hevcNALHeaderLen:]))
	r.skip(4) // sps_video_parameter_set_id
	maxSubLayersMinus1 := int(r.u(3))
	r.skip(1)
	// profile_tier_level
	r.skip(88) // general profile
	r.skip(8)  // general_level_idc
	subProfile := make([]bool, maxSubLayersMinus1)
	subLevel := make([]bool, maxSubLayersMinus1)
	for i := 0; i < maxSubLayersMinus1; i++ {
		subProfile[i] = r.flag()
		subLevel[i] = r.flag()
	}
	if maxSubLayersMinus1 > 0 {
		r.skip(2 * (hevcMaxSubLayers - maxSubLayersMinus1))
	}
	for i := 0; i < maxSubLayersMinus1; i++ {
		if subProfile[i] {
			r.skip(88)
		}
		if subLevel[i] {
			r.skip(8)
		}
	}
	r.ue() // sps_seq_parameter_set_id
	if r.ue() == 3 {
		r.skip(1) // separate_colour_plane_flag
	}
	r.ue()        // width
	r.ue()        // height
	if r.flag() { // conformance_window
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	r.ue() // bit_depth_luma_minus8
	r.ue() // bit_depth_chroma_minus8
	log2MaxPOCLsb := int(r.ue()) + 4
	orderingAll := r.flag()
	start := maxSubLayersMinus1
	if orderingAll {
		start = 0
	}
	for i := start; i <= maxSubLayersMinus1; i++ {
		r.ue()
		r.ue()
		r.ue()
	}
	for i := 0; i < 6; i++ {
		r.ue() // block sizes and transform hierarchy depths
	}
	if r.flag() && r.flag() { // scaling_list_enabled && sps_scaling_list_data_present
		skipHEVCScalingList(r)
	}
	r.skip(2)     // amp, sao
	if r.flag() { // pcm_enabled
		r.skip(8)
		r.ue()
		r.ue()
		r.skip(1)
	}
	numRPS := int(r.ue())
	if numRPS > hevcMaxShortTermRPS {
		return ColorInfo{}, errors.New("codec: invalid num_short_term_ref_pic_sets")
	}
	numDeltaPocs := make([]int, numRPS)
	for i := 0; i < numRPS && r.err == nil; i++ {
		interPred := i != 0 && r.flag()
		if interPred {
			r.skip(1) // delta_rps_sign
			r.ue()    // abs_delta_rps_minus1
			n := 0
			for j := 0; j <= numDeltaPocs[i-1]; j++ {
				used := r.flag()
				useDelta := used || r.flag()
				if useDelta {
					n++
				}
			}
			numDeltaPocs[i] = n
		} else {
			neg := int(r.ue())
			pos := int(r.ue())
			if neg+pos > 32 {
				return ColorInfo{}, errors.New("codec: invalid short-term RPS")
			}
			for j := 0; j < neg+pos; j++ {
				r.ue()
				r.skip(1)
			}
			numDeltaPocs[i] = neg + pos
		}
	}
	if r.flag() { // long_term_ref_pics_present
		n := int(r.ue())
		for i := 0; i < n && r.err == nil; i++ {
			r.skip(log2MaxPOCLsb + 1)
		}
	}
	r.skip(2) // temporal_mvp, strong_intra_smoothing
	if !r.flag() {
		return ColorInfo{}, r.err // no VUI
	}
	return parseVUIColor(r)
}

func skipHEVCScalingList(r *bitReader) {
	for size := 0; size < hevcScalingSizes; size++ {
		step := 1
		if size == 3 {
			step = 3
		}
		for m := 0; m < hevcScalingMatrices; m += step {
			if !r.flag() { // scaling_list_pred_mode_flag
				r.ue()
				continue
			}
			coefs := 1 << (4 + size<<1)
			if coefs > 64 {
				coefs = 64
			}
			if size > 1 {
				r.se()
			}
			for i := 0; i < coefs && r.err == nil; i++ {
				r.se()
			}
		}
	}
}

// parseVUIColor reads the start of vui_parameters (shared by H.264 and H.265).
func parseVUIColor(r *bitReader) (ColorInfo, error) {
	if r.flag() { // aspect_ratio_info_present
		if r.u(8) == hevcExtendedSARIdc {
			r.skip(32)
		}
	}
	if r.flag() { // overscan_info_present
		r.skip(1)
	}
	var ci ColorInfo
	if r.flag() { // video_signal_type_present
		r.skip(3) // video_format
		ci.FullRange = r.flag()
		if r.flag() {
			ci.Present = true
			ci.Primaries = uint8(r.u(8))
			ci.Transfer = uint8(r.u(8))
			ci.MatrixCoefficients = uint8(r.u(8))
		}
	}
	return ci, r.err
}

// FilterNALs rewrites a length-prefixed access unit without the NAL unit
// types for which drop returns true. It returns data unchanged when nothing
// is dropped.
func FilterNALs(data []byte, lengthSize int, drop func(nalType byte) bool) []byte {
	var out []byte
	for p := 0; p+lengthSize < len(data); {
		var n int
		for i := 0; i < lengthSize; i++ {
			n = n<<8 | int(data[p+i])
		}
		end := p + lengthSize + n
		if end > len(data) || n == 0 {
			break
		}
		nalType := (data[p+lengthSize] >> 1) & 0x3F
		if drop(nalType) {
			if out == nil {
				out = append(make([]byte, 0, len(data)), data[:p]...)
			}
		} else if out != nil {
			out = append(out, data[p:end]...)
		}
		p = end
	}
	if out == nil {
		return data
	}
	return out
}

// IsDolbyVisionNAL reports the HEVC NAL types that carry Dolby Vision RPU
// and enhancement-layer data.
func IsDolbyVisionNAL(nalType byte) bool {
	return nalType == hevcNALUnspec62 || nalType == hevcNALUnspec63
}

// DolbyVisionConfig is a parsed DOVIDecoderConfigurationRecord.
type DolbyVisionConfig struct {
	Record          []byte
	Profile         uint8
	Level           uint8
	RPU, EL, BL     bool
	BLCompatibility uint8
}

// ParseDolbyVisionConfig parses a dvcC/dvvC record.
func ParseDolbyVisionConfig(rec []byte) (*DolbyVisionConfig, error) {
	if len(rec) < dvConfigMinLen {
		return nil, errors.New("codec: invalid Dolby Vision config")
	}
	return &DolbyVisionConfig{
		Record:          rec,
		Profile:         rec[2] >> 1,
		Level:           (rec[2]&1)<<5 | rec[3]>>3,
		RPU:             rec[3]&4 != 0,
		EL:              rec[3]&2 != 0,
		BL:              rec[3]&1 != 0,
		BLCompatibility: rec[4] >> 4,
	}, nil
}

// BoxType is the ISO BMFF box type for this record.
func (c *DolbyVisionConfig) BoxType() string {
	if c.Profile >= dvProfileBLCompatible {
		return "dvvC"
	}
	return "dvcC"
}

// CodecString is the RFC 6381 string with the given sample entry, e.g. "dvh1.05.06".
func (c *DolbyVisionConfig) CodecString(entry string) string {
	return fmt.Sprintf("%s.%02d.%02d", entry, c.Profile, c.Level)
}

// AVCConfig is a parsed AVCDecoderConfigurationRecord.
type AVCConfig struct {
	Record     []byte
	LengthSize int
}

// ParseAVCConfig parses an avcC record.
func ParseAVCConfig(rec []byte) (*AVCConfig, error) {
	if len(rec) < avcCMinLen || rec[0] != 1 {
		return nil, errors.New("codec: invalid avcC record")
	}
	return &AVCConfig{Record: rec, LengthSize: int(rec[4]&3) + 1}, nil
}

// CodecString is the RFC 6381 string, e.g. "avc1.640028".
func (c *AVCConfig) CodecString() string {
	return fmt.Sprintf("avc1.%02X%02X%02X", c.Record[1], c.Record[2], c.Record[3])
}
