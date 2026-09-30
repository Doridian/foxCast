package transmux

import (
	"encoding/binary"
	"testing"

	"git.foxden.network/FoxDen/foxCast/internal/mkv"
)

func TestALACTrackSnapsToFrameGrid(t *testing.T) {
	cookie := make([]byte, 24)
	binary.BigEndian.PutUint32(cookie[0:], 4096) // frameLength
	cookie[5] = 16                               // bitDepth
	cookie[9] = 2                                // numChannels
	binary.BigEndian.PutUint32(cookie[20:], 44100)
	track := &mkv.Track{
		Number:       1,
		Type:         mkv.TrackAudio,
		CodecID:      codecALAC,
		CodecPrivate: cookie,
		Audio:        &mkv.Audio{SamplingFrequency: 44100, Channels: 2},
	}
	a, err := newAudioTrack(track, &mkv.Frame{})
	if err != nil {
		t.Fatalf("newAudioTrack: %v", err)
	}
	if a.fixedSamples != 4096 {
		t.Fatalf("fixedSamples = %d, want 4096", a.fixedSamples)
	}
}
