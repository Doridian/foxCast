package sender

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"
)

type hevcSPSInfo struct {
	width, height        int
	profileByte          byte
	compatibility        [4]byte
	constraints          [6]byte
	levelIDC             byte
	maxSubLayersMinus1   byte
	temporalIDNested     byte
	chromaFormatIDC      byte
	bitDepthLumaMinus8   byte
	bitDepthChromaMinus8 byte
}

func hevcNALType(raw []byte) byte {
	if len(raw) < 2 {
		return 63
	}
	return (raw[0] >> 1) & 0x3f
}

func parseHEVCSPS(sps []byte) (hevcSPSInfo, bool) {
	var info hevcSPSInfo
	if len(sps) < 15 || hevcNALType(sps) != 33 {
		return info, false
	}
	rbsp := stripEmulationPrevention(sps[2:])
	if len(rbsp) < 13 {
		return info, false
	}
	info.maxSubLayersMinus1 = (rbsp[0] >> 1) & 7
	info.temporalIDNested = rbsp[0] & 1
	info.profileByte = rbsp[1]
	copy(info.compatibility[:], rbsp[2:6])
	copy(info.constraints[:], rbsp[6:12])
	info.levelIDC = rbsp[12]

	r := &h264BitReader{data: rbsp}
	r.readBits(8)  // sps_video_parameter_set_id, max_sub_layers, nesting
	r.readBits(96) // general_profile_tier_level
	profilePresent := make([]uint, info.maxSubLayersMinus1)
	levelPresent := make([]uint, info.maxSubLayersMinus1)
	for n := range profilePresent {
		profilePresent[n] = r.readBit()
		levelPresent[n] = r.readBit()
	}
	if info.maxSubLayersMinus1 > 0 {
		for n := int(info.maxSubLayersMinus1); n < 8; n++ {
			r.readBits(2)
		}
	}
	for n := range profilePresent {
		if profilePresent[n] != 0 {
			r.readBits(88)
		}
		if levelPresent[n] != 0 {
			r.readBits(8)
		}
	}
	r.readUE() // sps_seq_parameter_set_id
	chroma := r.readUE()
	if chroma > 3 {
		return info, false
	}
	info.chromaFormatIDC = byte(chroma)
	separateColourPlane := uint(0)
	if chroma == 3 {
		separateColourPlane = r.readBit()
	}
	width := r.readUE()
	height := r.readUE()
	var cropLeft, cropRight, cropTop, cropBottom uint
	if r.readBit() != 0 {
		cropLeft, cropRight = r.readUE(), r.readUE()
		cropTop, cropBottom = r.readUE(), r.readUE()
	}
	lumaDepth, chromaDepth := r.readUE(), r.readUE()
	if r.err || width == 0 || height == 0 || lumaDepth > 7 || chromaDepth > 7 {
		return info, false
	}
	subWidthC, subHeightC := uint(1), uint(1)
	if separateColourPlane == 0 {
		switch chroma {
		case 1:
			subWidthC, subHeightC = 2, 2
		case 2:
			subWidthC, subHeightC = 2, 1
		}
	}
	if (cropLeft+cropRight)*subWidthC >= width || (cropTop+cropBottom)*subHeightC >= height {
		return info, false
	}
	info.width = int(width - (cropLeft+cropRight)*subWidthC)
	info.height = int(height - (cropTop+cropBottom)*subHeightC)
	info.bitDepthLumaMinus8 = byte(lumaDepth)
	info.bitDepthChromaMinus8 = byte(chromaDepth)
	return info, true
}

// buildHEVCSampleDescription serializes the same hvc1 visual sample entry and
// hvcC decoder configuration that Apple's generic screen codec path copies
// from its CMVideoFormatDescription.
func buildHEVCSampleDescription(vps, sps, pps []byte) ([]byte, error) {
	info, ok := parseHEVCSPS(sps)
	if !ok || len(vps) < 2 || hevcNALType(vps) != 32 || len(pps) < 2 || hevcNALType(pps) != 34 {
		return nil, fmt.Errorf("invalid HEVC VPS/SPS/PPS")
	}
	hvcc := make([]byte, 23)
	hvcc[0] = 1
	hvcc[1] = info.profileByte
	copy(hvcc[2:6], info.compatibility[:])
	copy(hvcc[6:12], info.constraints[:])
	hvcc[12] = info.levelIDC
	binary.BigEndian.PutUint16(hvcc[13:15], 0xf000) // reserved + min_spatial_segmentation_idc
	hvcc[15] = 0xfc                                 // reserved + parallelismType unknown
	hvcc[16] = 0xfc | info.chromaFormatIDC
	hvcc[17] = 0xf8 | info.bitDepthLumaMinus8
	hvcc[18] = 0xf8 | info.bitDepthChromaMinus8
	// avgFrameRate remains zero. lengthSizeMinusOne=3 selects four-byte NAL lengths.
	hvcc[21] = ((info.maxSubLayersMinus1 + 1) << 3) | (info.temporalIDNested << 2) | 3
	hvcc[22] = 3
	for _, parameterSet := range []struct {
		typ byte
		nal []byte
	}{{32, vps}, {33, sps}, {34, pps}} {
		hvcc = append(hvcc, 0x80|parameterSet.typ, 0, 1)
		if len(parameterSet.nal) > 0xffff {
			return nil, fmt.Errorf("HEVC parameter set is too large")
		}
		hvcc = binary.BigEndian.AppendUint16(hvcc, uint16(len(parameterSet.nal)))
		hvcc = append(hvcc, parameterSet.nal...)
	}

	const visualSampleEntrySize = 86
	payload := make([]byte, visualSampleEntrySize+8+len(hvcc))
	binary.BigEndian.PutUint32(payload[0:4], uint32(len(payload)))
	copy(payload[4:8], "hvc1")
	binary.BigEndian.PutUint16(payload[14:16], 1) // data_reference_index
	binary.BigEndian.PutUint16(payload[32:34], uint16(info.width))
	binary.BigEndian.PutUint16(payload[34:36], uint16(info.height))
	binary.BigEndian.PutUint32(payload[36:40], 0x00480000) // 72 dpi
	binary.BigEndian.PutUint32(payload[40:44], 0x00480000)
	binary.BigEndian.PutUint16(payload[48:50], 1)      // frame_count
	binary.BigEndian.PutUint16(payload[82:84], 0x0018) // depth
	binary.BigEndian.PutUint16(payload[84:86], 0xffff)
	binary.BigEndian.PutUint32(payload[86:90], uint32(8+len(hvcc)))
	copy(payload[90:94], "hvcC")
	copy(payload[94:], hvcc)
	return payload, nil
}

func hevcCodecConfigNeedsSend(primed bool, current, sent [3][]byte) bool {
	return !primed || !bytes.Equal(current[0], sent[0]) || !bytes.Equal(current[1], sent[1]) || !bytes.Equal(current[2], sent[2])
}

func (s *MirrorSession) streamHEVCFrames(ctx context.Context, capture *ScreenCapture, startDelay time.Duration) error {
	if capture == nil || capture.frames == nil {
		return fmt.Errorf("HEVC requires timestamped access-unit capture")
	}
	if startDelay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(startDelay):
		}
	}
	var current, sent [3][]byte // VPS, SPS, PPS
	primed := false
	frameCount := 0
	var lastProgress time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		unit, err := capture.ReadVideoAccessUnit()
		if err != nil {
			if err == io.EOF && ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read timestamped HEVC capture: %w", err)
		}
		var frame []byte
		keyframe, hasVCL := false, false
		var types strings.Builder
		for _, wrapped := range splitAnnexBAccessUnit(unit.AnnexB) {
			raw := stripStartCode(wrapped)
			typ := hevcNALType(raw)
			if frameCount < 12 {
				fmt.Fprintf(&types, "%d|", typ)
			}
			switch typ {
			case 32:
				current[0] = append(current[0][:0], raw...)
			case 33:
				current[1] = append(current[1][:0], raw...)
			case 34:
				current[2] = append(current[2][:0], raw...)
			case 35: // access-unit delimiter is not part of the codec sample
			default:
				// Preserve prefix/suffix SEI: HDR mastering/content-light metadata is
				// carried there when the source and encoder actually provide it.
				frame = append(frame, avccWrap(raw)...)
				if typ <= 31 {
					hasVCL = true
					if typ >= 16 && typ <= 21 {
						keyframe = true
					}
				}
			}
		}
		if !hasVCL {
			continue
		}
		if !primed && (!keyframe || len(current[0]) == 0 || len(current[1]) == 0 || len(current[2]) == 0) {
			continue
		}
		timestamp, timeline, ok := s.frameTimeAt(unit.PTS)
		if !ok {
			return fmt.Errorf("map HEVC capture PTS %v to receiver clock", unit.PTS)
		}
		if keyframe && hevcCodecConfigNeedsSend(primed, current, sent) {
			config, configErr := buildHEVCSampleDescription(current[0], current[1], current[2])
			if configErr != nil {
				return configErr
			}
			if info, parsed := parseHEVCSPS(current[1]); parsed {
				s.videoWidth, s.videoHeight = info.width, info.height
				dbg("[STREAM] encoded HEVC content size from SPS: %dx%d", info.width, info.height)
			}
			if err := s.sendCodecFrame(config, timestamp, VideoCodecHEVC); err != nil {
				return fmt.Errorf("send HEVC codec: %w", err)
			}
			for n := range current {
				sent[n] = append(sent[n][:0], current[n]...)
			}
			primed = true
		}
		if s.streamCipher != nil {
			frame = s.streamCipher(frame)
		}
		if err := s.sendFrame(frame, keyframe, timestamp, timeline); err != nil {
			return fmt.Errorf("send HEVC frame: %w", err)
		}
		if frameCount == 0 {
			select {
			case <-s.firstFrameSent:
			default:
				close(s.firstFrameSent)
			}
		}
		frameCount++
		if frameCount <= 12 {
			dbg("[STREAM] HEVC frame %d key=%v bytes=%d NALs=%s", frameCount, keyframe, len(frame), types.String())
		}
		if time.Since(lastProgress) >= 5*time.Second {
			if unit.PTS.IsZero() {
				dbg("[STREAM] HEVC video progress: sent frame %d, source PTS unavailable", frameCount)
			} else {
				age := time.Since(unit.PTS)
				dbg("[STREAM] HEVC video progress: sent frame %d, source age=%v local presentation slack=%v",
					frameCount, age, s.timestampBias-age)
			}
			lastProgress = time.Now()
		}
	}
}
