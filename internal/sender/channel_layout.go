package sender

import (
	"fmt"
	"slices"
	"strings"
)

// ChannelPosition is a speaker position in a multichannel capture, named by
// its short form ("FL", "RR", "LFE", ...).
type ChannelPosition string

// channelPositionInfo names a position for PulseAudio and GStreamer.
// gstBit is the GstAudioChannelPosition value; GStreamer interleaves
// positioned channels in ascending bit order.
type channelPositionInfo struct {
	pulse  string
	gstBit uint
}

var channelPositions = map[ChannelPosition]channelPositionInfo{
	"FL":  {"front-left", 0},
	"FR":  {"front-right", 1},
	"FC":  {"front-center", 2},
	"LFE": {"lfe", 3},
	"RL":  {"rear-left", 4},
	"RR":  {"rear-right", 5},
	"FLC": {"front-left-of-center", 6},
	"FRC": {"front-right-of-center", 7},
	"RC":  {"rear-center", 8},
	"SL":  {"side-left", 10},
	"SR":  {"side-right", 11},
}

var channelLayoutPresets = map[string]string{
	"stereo": "FL,FR",
	"quad":   "FL,FR,RL,RR",
	"5.1":    "FL,FR,FC,LFE,RL,RR",
	"7.1":    "FL,FR,FC,LFE,RL,RR,SL,SR",
}

// ChannelLayout is the ordered set of channels a group capture records, in
// GStreamer's interleaving order. Channel indexes refer to this order.
type ChannelLayout []ChannelPosition

// StereoLayout is the ordinary two-channel layout.
var StereoLayout = ChannelLayout{"FL", "FR"}

// ParseChannelLayout accepts a preset (stereo, quad, 5.1, 7.1) or a comma
// separated list of positions such as "FL,FR,RC".
func ParseChannelLayout(s string) (ChannelLayout, error) {
	s = strings.TrimSpace(s)
	if preset, ok := channelLayoutPresets[strings.ToLower(s)]; ok {
		s = preset
	}
	var layout ChannelLayout
	for _, field := range strings.Split(s, ",") {
		position, err := ParseChannelPosition(field)
		if err != nil {
			return nil, err
		}
		if slices.Contains(layout, position) {
			return nil, fmt.Errorf("channel %s is listed twice", position)
		}
		layout = append(layout, position)
	}
	if len(layout) < 2 {
		return nil, fmt.Errorf("a channel layout needs at least two channels")
	}
	slices.SortFunc(layout, func(a, b ChannelPosition) int {
		return int(channelPositions[a].gstBit) - int(channelPositions[b].gstBit)
	})
	return layout, nil
}

// ParseChannelPosition parses a short position name, ignoring case.
func ParseChannelPosition(s string) (ChannelPosition, error) {
	position := ChannelPosition(strings.ToUpper(strings.TrimSpace(s)))
	if _, ok := channelPositions[position]; !ok {
		return "", fmt.Errorf("unknown channel %q (known: FL FR FC LFE RL RR FLC FRC RC SL SR)", s)
	}
	return position, nil
}

// Index returns the interleaved index of position, or false if the layout
// does not have it.
func (l ChannelLayout) Index(position ChannelPosition) (int, bool) {
	i := slices.Index(l, position)
	return i, i >= 0
}

func (l ChannelLayout) String() string {
	names := make([]string, len(l))
	for i, position := range l {
		names[i] = string(position)
	}
	return strings.Join(names, ",")
}

func (l ChannelLayout) isStereo() bool {
	return slices.Equal(l, StereoLayout)
}

// pulseChannelMap is the channel_map module argument for the layout.
func (l ChannelLayout) pulseChannelMap() string {
	names := make([]string, len(l))
	for i, position := range l {
		names[i] = channelPositions[position].pulse
	}
	return strings.Join(names, ",")
}

// gstChannelMask is the GStreamer channel-mask caps value for the layout.
func (l ChannelLayout) gstChannelMask() uint64 {
	var mask uint64
	for _, position := range l {
		mask |= 1 << channelPositions[position].gstBit
	}
	return mask
}
