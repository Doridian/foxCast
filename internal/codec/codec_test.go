package codec

import (
	"bytes"
	"encoding/hex"
	"os"
	"testing"

	"git.foxden.network/FoxDen/foxCast/internal/mkv"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureTrack(t *testing.T, name string, number uint64) *mkv.Track {
	t.Helper()
	fh, err := os.Open("../mkv/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	st, _ := fh.Stat()
	f, err := mkv.Open(fh, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	return f.Track(number)
}

// Reference boxes below were produced by FFmpeg's MP4 muxer from the
// fixtures in ../mkv/testdata (see its README).

func TestAC3Dac3(t *testing.T) {
	// 5.1, 48 kHz, 192 kbit/s syncframe header.
	info, err := ParseAC3(unhex(t, "0b7702021440ebf8"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(info.Dac3()); got != "103d40" {
		t.Errorf("dac3 = %s, want 103d40", got)
	}
	if info.SampleRate != 48000 || info.Channels != 6 {
		t.Errorf("info = %+v", info)
	}
	if _, err := ParseAC3([]byte{0, 1, 2, 3, 4, 5, 6}); err == nil {
		t.Error("non-AC-3 data should fail")
	}
}

func TestEAC3Dec3(t *testing.T) {
	// 5.1, 48 kHz, 192 kbit/s independent substream (frame size 768 bytes).
	frame := append(unhex(t, "0b77017f3f87c000"), make([]byte, 760)...)
	info, err := ParseEAC3(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(info.Dec3()); got != "0600200f00" {
		t.Errorf("dec3 = %s, want 0600200f00", got)
	}
	n, err := EAC3FrameSamples(frame)
	if err != nil || n != 1536 {
		t.Errorf("EAC3FrameSamples = %d, %v", n, err)
	}
}

func TestAudioSpecificConfig(t *testing.T) {
	for _, tc := range []struct {
		asc     string
		aot     uint8
		rate    uint32
		ch      uint8
		samples uint32
		codec   string
	}{
		{"1210", 2, 44100, 2, 1024, "mp4a.40.2"},
		{"1208", 2, 44100, 1, 1024, "mp4a.40.2"},
		{"1214", 2, 44100, 2, 960, "mp4a.40.2"},      // frameLengthFlag
		{"2b920800", 5, 22050, 2, 1024, "mp4a.40.5"}, // HE-AAC, explicit SBR
	} {
		c, err := ParseAudioSpecificConfig(unhex(t, tc.asc))
		if err != nil {
			t.Errorf("%s: %v", tc.asc, err)
			continue
		}
		if c.ObjectType != tc.aot || c.SampleRate != tc.rate || c.Channels != tc.ch || c.FrameSamples != tc.samples || c.CodecString() != tc.codec {
			t.Errorf("%s: got %+v %s", tc.asc, c, c.CodecString())
		}
	}
	if got := hex.EncodeToString(SynthesizeASC("A_AAC/MPEG4/LC", 44100, 2)); got != "1210" {
		t.Errorf("SynthesizeASC = %s, want 1210", got)
	}
}

func TestFLAC(t *testing.T) {
	dfla := unhex(t, "800000220400040000011a00015e0bb800f000017700a18df3a88fe45648b111ac899b294db5")
	cfg, err := ParseFLACPrivate(append([]byte("fLaC"), dfla...))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cfg.DfLa(), dfla) {
		t.Errorf("dfLa = %x", cfg.DfLa())
	}
	if cfg.SampleRate != 48000 || cfg.Channels != 1 || cfg.BitDepth != 16 || cfg.FixedBlockSize != 1024 {
		t.Errorf("cfg = %+v", cfg)
	}
	for _, tc := range []struct {
		hdr  string
		want uint32
	}{
		{"fff8c9180000", 4096},      // block size code 12
		{"fff86918000b", 12},        // 8-bit block size (11+1) after 1-byte frame number
		{"fff97918c3a4ffff", 65536}, // 16-bit block size after 2-byte sample number
	} {
		n, err := FLACFrameSamples(unhex(t, tc.hdr))
		if err != nil || n != tc.want {
			t.Errorf("FLACFrameSamples(%s) = %d, %v; want %d", tc.hdr, n, err, tc.want)
		}
	}
}

func TestOpus(t *testing.T) {
	head := append([]byte("OpusHead"), unhex(t, "0102380180bb0000000000")...)
	cfg, err := ParseOpusHead(head)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(cfg.DOps()); got != "000201380000bb80000000" {
		t.Errorf("dOps = %s", got)
	}
	for _, tc := range []struct {
		pkt  string
		want uint32
	}{
		{"fc", 960},    // CELT 20 ms, one frame
		{"f8", 960},    // CELT 20 ms
		{"0303", 1440}, // SILK 10 ms × 3 (code 3)
		{"f1", 960},    // CELT 10 ms, two frames (code 1)
		{"e1", 240},    // CELT 2.5 ms, two frames
	} {
		n, err := OpusPacketSamples(unhex(t, tc.pkt))
		if err != nil || n != tc.want {
			t.Errorf("OpusPacketSamples(%s) = %d, %v; want %d", tc.pkt, n, err, tc.want)
		}
	}
}

func TestMPAHeader(t *testing.T) {
	info, err := ParseMPAHeader(unhex(t, "fffb9064"))
	if err != nil {
		t.Fatal(err)
	}
	if info.SampleRate != 44100 || info.Channels != 2 || info.FrameSamples != 1152 {
		t.Errorf("info = %+v", info)
	}
	info, err = ParseMPAHeader(unhex(t, "fff3c0c4")) // MPEG-2 layer 3, 22.05 kHz, mono
	if err != nil {
		t.Fatal(err)
	}
	if info.SampleRate != 22050 || info.Channels != 1 || info.FrameSamples != 576 {
		t.Errorf("info = %+v", info)
	}
}

func TestHEVCConfig(t *testing.T) {
	tr := fixtureTrack(t, "hevc_multi.mkv", 1)
	cfg, err := ParseHEVCConfig(tr.CodecPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CodecString(); got != "hvc1.2.4.L30.90" {
		t.Errorf("codec = %s, want hvc1.2.4.L30.90", got)
	}
	if cfg.LengthSize != 4 {
		t.Errorf("LengthSize = %d", cfg.LengthSize)
	}
	// x265 was run with BT.2020 / SMPTE 2084 / BT.2020nc in the VUI.
	want := ColorInfo{Present: true, Primaries: 9, Transfer: TransferPQ, MatrixCoefficients: 9}
	if cfg.Color != want {
		t.Errorf("colour = %+v, want %+v", cfg.Color, want)
	}
}

func TestAVCConfig(t *testing.T) {
	tr := fixtureTrack(t, "h264_aac.mkv", 1)
	cfg, err := ParseAVCConfig(tr.CodecPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CodecString(); got != "avc1.64000A" {
		t.Errorf("codec = %s", got)
	}
}

func TestDolbyVisionConfig(t *testing.T) {
	// From a UHD Blu-ray remux: profile 7, level 6, RPU+EL+BL, compatibility 6.
	dv, err := ParseDolbyVisionConfig(unhex(t, "01000e376000000000000000000000000000000000000000"))
	if err != nil {
		t.Fatal(err)
	}
	if dv.Profile != 7 || dv.Level != 6 || !dv.RPU || !dv.EL || !dv.BL || dv.BLCompatibility != 6 {
		t.Errorf("dv = %+v", dv)
	}
	if dv.BoxType() != "dvcC" || dv.CodecString("dvh1") != "dvh1.07.06" {
		t.Errorf("box %s codec %s", dv.BoxType(), dv.CodecString("dvh1"))
	}
	// Profile 8.1, level 6.
	dv8, err := ParseDolbyVisionConfig(unhex(t, "0100103510"))
	if err != nil {
		t.Fatal(err)
	}
	if dv8.Profile != 8 || dv8.BLCompatibility != 1 || dv8.BoxType() != "dvvC" {
		t.Errorf("dv8 = %+v", dv8)
	}
}

func TestFilterNALs(t *testing.T) {
	nal := func(typ byte, payload ...byte) []byte {
		n := append([]byte{typ << 1, 1}, payload...)
		return append([]byte{0, 0, 0, byte(len(n))}, n...)
	}
	au := bytes.Join([][]byte{nal(35), nal(1, 0xAA), nal(62, 0xBB), nal(63, 0xCC, 0xDD)}, nil)
	got := FilterNALs(au, 4, IsDolbyVisionNAL)
	want := bytes.Join([][]byte{nal(35), nal(1, 0xAA)}, nil)
	if !bytes.Equal(got, want) {
		t.Errorf("FilterNALs = %x, want %x", got, want)
	}
	plain := bytes.Join([][]byte{nal(35), nal(1, 0xAA)}, nil)
	if out := FilterNALs(plain, 4, IsDolbyVisionNAL); &out[0] != &plain[0] {
		t.Error("FilterNALs should not copy when nothing is dropped")
	}
}
