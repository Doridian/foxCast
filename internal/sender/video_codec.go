package sender

import (
	"fmt"
	"math"
)

// VideoCodec selects the compressed screen codec carried by AirPlay mirroring.
// The empty value remains H.264 for backwards-compatible programmatic use;
// command-line callers may request Auto and provide the local encoder gate in
// StreamConfig after capture preflight.
type VideoCodec string

const (
	VideoCodecAuto VideoCodec = "auto"
	VideoCodecH264 VideoCodec = "h264"
	VideoCodecHEVC VideoCodec = "hevc"
)

const featureScreenMultiCodec uint = 42

func normalizeVideoCodec(codec VideoCodec) VideoCodec {
	if codec == "" {
		return VideoCodecH264
	}
	return codec
}

// ValidateVideoCodec checks a command/config codec value.
func ValidateVideoCodec(codec string) error {
	switch VideoCodec(codec) {
	case "", VideoCodecAuto, VideoCodecH264, VideoCodecHEVC:
		return nil
	default:
		return fmt.Errorf("unknown video codec %q (want auto, h264, or hevc)", codec)
	}
}

func (i *ReceiverInfo) supportsVideoCodec(codec VideoCodec) bool {
	switch normalizeVideoCodec(codec) {
	case VideoCodecH264:
		return true
	case VideoCodecHEVC:
		// Apple's SupportsScreenMultiCodec property is feature index 42.
		return i != nil && i.HasFeature(featureScreenMultiCodec)
	default:
		return false
	}
}

type videoSelection struct {
	codec         VideoCodec
	width, height int
	reason        string
}

// selectVideo chooses one concrete codec and canvas from the final receiver
// snapshot. Apple's sender treats SupportsScreenMultiCodec (feature 42), a
// maximum above 1080p, and local HEVC-4K support as independent gates. Auto
// follows that shape; explicit codec requests remain deterministic.
func (i *ReceiverInfo) selectVideo(requested VideoCodec, automaticHEVCAvailable bool) (videoSelection, error) {
	if requested == "" {
		requested = VideoCodecH264
	}
	if err := ValidateVideoCodec(string(requested)); err != nil {
		return videoSelection{}, err
	}

	switch requested {
	case VideoCodecH264:
		width, height := i.MirrorSize()
		return videoSelection{codec: VideoCodecH264, width: width, height: height, reason: "explicit H.264"}, nil
	case VideoCodecHEVC:
		width, height, err := i.videoCanvas(VideoCodecHEVC)
		if err != nil {
			return videoSelection{}, err
		}
		return videoSelection{codec: VideoCodecHEVC, width: width, height: height, reason: "explicit HEVC"}, nil
	case VideoCodecAuto:
		if !automaticHEVCAvailable {
			width, height := i.MirrorSize()
			return videoSelection{codec: VideoCodecH264, width: width, height: height, reason: "local hardware HEVC path unavailable"}, nil
		}
		if !i.supportsVideoCodec(VideoCodecHEVC) {
			width, height := i.MirrorSize()
			return videoSelection{codec: VideoCodecH264, width: width, height: height, reason: "receiver lacks feature 42"}, nil
		}
		width, height, ok := i.highResolutionVideoCanvas()
		if !ok {
			width, height = i.MirrorSize()
			return videoSelection{codec: VideoCodecH264, width: width, height: height, reason: "receiver maximum does not exceed 1080p"}, nil
		}
		return videoSelection{codec: VideoCodecHEVC, width: width, height: height, reason: "feature 42, high-resolution maximum, and local hardware HEVC"}, nil
	default:
		panic("validated video codec has no selection policy")
	}
}

// highResolutionVideoCanvas implements the high-resolution half of Apple's
// display-size gate. A missing maximum falls back to nominal, a malformed
// maximum cannot shrink nominal, and the result is capped aspect-preserving to
// 3840x2160. A maximum of exactly 1920x1080 remains on the ordinary path.
func (i *ReceiverInfo) highResolutionVideoCanvas() (int, int, bool) {
	maximumW, maximumH := i.maximumVideoCanvas()
	if maximumW <= 1920 && maximumH <= 1080 {
		return 0, 0, false
	}
	if maximumW <= 0 || maximumH <= 0 {
		return 0, 0, false
	}
	return maximumW, maximumH, true
}

func (i *ReceiverInfo) maximumVideoCanvas() (int, int) {
	nominalW, nominalH := i.MirrorSize()
	maximumW, maximumH := i.MaxVideoSize()
	if maximumW < nominalW {
		maximumW = nominalW
	}
	if maximumH < nominalH {
		maximumH = nominalH
	}
	if maximumW <= 0 || maximumH <= 0 {
		return 0, 0
	}
	return fitVideoSize(maximumW, maximumH, 3840, 2160)
}

func fitVideoSize(width, height, maximumW, maximumH int) (int, int) {
	if width <= maximumW && height <= maximumH {
		return width, height
	}
	scale := math.Min(float64(maximumW)/float64(width), float64(maximumH)/float64(height))
	return int(math.Round(float64(width) * scale)), int(math.Round(float64(height) * scale))
}

func (i *ReceiverInfo) videoCanvas(codec VideoCodec) (int, int, error) {
	codec = normalizeVideoCodec(codec)
	if !i.supportsVideoCodec(codec) {
		return 0, 0, fmt.Errorf("receiver does not advertise AirPlay feature 42 (SupportsScreenMultiCodec) required for HEVC")
	}
	if codec == VideoCodecHEVC {
		// Apple's high-resolution screen path uses PixelSizeMax only after HEVC/
		// HDR sender support has been established. The ordinary H.264 path uses
		// the nominal PixelSize instead.
		width, height := i.maximumVideoCanvas()
		return width, height, nil
	}
	width, height := i.MirrorSize()
	return width, height, nil
}
