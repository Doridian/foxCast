package fmp4

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// boxes splits b into top-level boxes by type.
func boxes(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for len(b) > 0 {
		if len(b) < 8 {
			t.Fatalf("truncated box header: %x", b)
		}
		size := int(binary.BigEndian.Uint32(b))
		if size < 8 || size > len(b) {
			t.Fatalf("bad box size %d (have %d)", size, len(b))
		}
		out[string(b[4:8])] = b[8:size]
		b = b[size:]
	}
	return out
}

func path(t *testing.T, b []byte, types ...string) []byte {
	t.Helper()
	for _, typ := range types {
		var ok bool
		b, ok = boxes(t, b)[typ]
		if !ok {
			t.Fatalf("missing box %s", typ)
		}
	}
	return b
}

func TestInitSegment(t *testing.T) {
	entry := VisualSampleEntry("avc1", 64, 48, ConfigBox("avcC", []byte{1, 2, 3}))
	init := InitSegment(Track{Kind: Video, Timescale: 90000, SampleEntry: entry, Width: 64, Height: 48})
	top := boxes(t, init)
	if !bytes.HasPrefix(top["ftyp"], []byte("iso6")) {
		t.Errorf("ftyp = %q", top["ftyp"])
	}
	mdhd := path(t, top["moov"], "trak", "mdia", "mdhd")
	if ts := binary.BigEndian.Uint32(mdhd[12:]); ts != 90000 {
		t.Errorf("timescale = %d", ts)
	}
	if lang := binary.BigEndian.Uint16(mdhd[20:]); lang != 0x55C4 { // "und"
		t.Errorf("language = %#x", lang)
	}
	stsd := path(t, top["moov"], "trak", "mdia", "minf", "stbl", "stsd")
	if !bytes.Equal(stsd[8:], entry) {
		t.Error("stsd does not contain the sample entry")
	}
	avc1 := boxes(t, stsd[8:])["avc1"]
	if w, h := binary.BigEndian.Uint16(avc1[24:]), binary.BigEndian.Uint16(avc1[26:]); w != 64 || h != 48 {
		t.Errorf("avc1 %dx%d", w, h)
	}
	if got := boxes(t, avc1[78:])["avcC"]; !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Errorf("avcC = %x", got)
	}
	trex := path(t, top["moov"], "mvex", "trex")
	if id := binary.BigEndian.Uint32(trex[4:]); id != 1 {
		t.Errorf("trex track = %d", id)
	}
}

func TestEsds(t *testing.T) {
	// Layout per ISO/IEC 14496-1; matches FFmpeg's output for this ASC apart
	// from bitrate and buffer fields.
	got := Esds(OTIAAC, 0, []byte{0x12, 0x10})
	want := []byte{
		0, 0, 0, 0x27, 'e', 's', 'd', 's', 0, 0, 0, 0,
		0x03, 0x19, 0, 0, 0,
		0x04, 0x11, 0x40, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0x05, 0x02, 0x12, 0x10,
		0x06, 0x01, 0x02,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("esds =\n%x\nwant\n%x", got, want)
	}
}

func TestFragment(t *testing.T) {
	f := &Fragment{
		Sequence:       7,
		BaseDecodeTime: 1 << 33,
		Samples: []Sample{
			{Duration: 3750, CTO: 7500, Flags: SampleFlagsSync, Data: []byte("key")},
			{Duration: 3750, CTO: -3750, Flags: SampleFlagsNonSync, Data: []byte("bframe")},
		},
	}
	var buf bytes.Buffer
	n, err := f.WriteTo(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != f.Size() || int64(buf.Len()) != n {
		t.Fatalf("WriteTo wrote %d, Size %d, buffer %d", n, f.Size(), buf.Len())
	}
	top := boxes(t, buf.Bytes())
	if !bytes.Equal(top["mdat"], []byte("keybframe")) {
		t.Errorf("mdat = %q", top["mdat"])
	}
	if seq := binary.BigEndian.Uint32(path(t, top["moof"], "mfhd")[4:]); seq != 7 {
		t.Errorf("sequence = %d", seq)
	}
	tfdt := path(t, top["moof"], "traf", "tfdt")
	if tfdt[0] != 1 || binary.BigEndian.Uint64(tfdt[4:]) != 1<<33 {
		t.Errorf("tfdt = %x", tfdt)
	}
	trun := path(t, top["moof"], "traf", "trun")
	if trun[0] != 1 {
		t.Errorf("trun version %d, want 1", trun[0])
	}
	// data_offset is relative to the moof start and must point at the payload.
	moofLen := len(top["moof"]) + 8
	if off := binary.BigEndian.Uint32(trun[8:]); int(off) != moofLen+8 {
		t.Errorf("data_offset %d, want %d", off, moofLen+8)
	}
	// Second sample: duration, size, flags, signed CTO.
	s2 := trun[12+16:]
	if cto := int32(binary.BigEndian.Uint32(s2[12:])); cto != -3750 {
		t.Errorf("second CTO = %d", cto)
	}
	if flags := binary.BigEndian.Uint32(s2[8:]); flags != SampleFlagsNonSync {
		t.Errorf("second flags = %#x", flags)
	}
}
