package fmp4

import (
	"encoding/binary"
	"io"
)

const (
	trackID          = 1
	movieTimescale   = 1000
	unityVolume      = 0x0100
	tkhdEnabledInMov = 0x3

	tfhdDefaultBaseIsMoof = 0x020000
	trunDataOffset        = 0x000001
	trunSampleDuration    = 0x000100
	trunSampleSize        = 0x000200
	trunSampleFlags       = 0x000400
	trunSampleCTO         = 0x000800

	// SampleFlagsSync marks a sample that depends on no other sample.
	SampleFlagsSync = 0x02000000
	// SampleFlagsNonSync marks a dependent, non-sync sample.
	SampleFlagsNonSync = 0x01010000

	boxHeaderLen = 8
)

// Kind is a track's media kind.
type Kind int

// Media kinds.
const (
	Video Kind = iota
	Audio
)

// Track describes the single track of an init segment.
type Track struct {
	Kind      Kind
	Timescale uint32
	// SampleEntry is the complete stsd entry (e.g. from VisualSampleEntry).
	SampleEntry []byte
	// Width and Height are the display dimensions for tkhd (video only).
	Width, Height uint32
	// Language is an ISO 639-2/T code; empty means "und".
	Language string
}

var unityMatrix = [9]uint32{fixed16_16One, 0, 0, 0, fixed16_16One, 0, 0, 0, 0x40000000}

// InitSegment builds ftyp+moov for t.
func InitSegment(t Track) []byte {
	var w Buf
	w.Box("ftyp", func(w *Buf) {
		w.Raw([]byte("iso6"))
		w.U32(0)
		for _, b := range []string{"iso6", "cmfc", "mp41"} {
			w.Raw([]byte(b))
		}
	})
	w.Box("moov", func(w *Buf) {
		w.FullBox("mvhd", 0, 0, func(w *Buf) {
			w.U32(0) // creation
			w.U32(0) // modification
			w.U32(movieTimescale)
			w.U32(0) // duration
			w.U32(fixed16_16One)
			w.U16(unityVolume)
			w.Zeros(10)
			for _, m := range unityMatrix {
				w.U32(m)
			}
			w.Zeros(24)
			w.U32(trackID + 1)
		})
		w.Box("trak", func(w *Buf) {
			w.FullBox("tkhd", 0, tkhdEnabledInMov, func(w *Buf) {
				w.U32(0)
				w.U32(0)
				w.U32(trackID)
				w.U32(0)
				w.U32(0) // duration
				w.Zeros(8)
				w.U16(0) // layer
				w.U16(0) // alternate_group
				if t.Kind == Audio {
					w.U16(unityVolume)
				} else {
					w.U16(0)
				}
				w.U16(0)
				for _, m := range unityMatrix {
					w.U32(m)
				}
				w.U32(t.Width << 16)
				w.U32(t.Height << 16)
			})
			w.Box("mdia", func(w *Buf) {
				w.FullBox("mdhd", 0, 0, func(w *Buf) {
					w.U32(0)
					w.U32(0)
					w.U32(t.Timescale)
					w.U32(0)
					w.U16(packLanguage(t.Language))
					w.U16(0)
				})
				w.FullBox("hdlr", 0, 0, func(w *Buf) {
					w.U32(0)
					if t.Kind == Audio {
						w.Raw([]byte("soun"))
						w.Zeros(12)
						w.Raw([]byte("SoundHandler\x00"))
					} else {
						w.Raw([]byte("vide"))
						w.Zeros(12)
						w.Raw([]byte("VideoHandler\x00"))
					}
				})
				w.Box("minf", func(w *Buf) {
					if t.Kind == Audio {
						w.FullBox("smhd", 0, 0, func(w *Buf) { w.U32(0) })
					} else {
						w.FullBox("vmhd", 0, 1, func(w *Buf) { w.Zeros(8) })
					}
					w.Box("dinf", func(w *Buf) {
						w.FullBox("dref", 0, 0, func(w *Buf) {
							w.U32(1)
							w.FullBox("url ", 0, 1, nil)
						})
					})
					w.Box("stbl", func(w *Buf) {
						w.FullBox("stsd", 0, 0, func(w *Buf) {
							w.U32(1)
							w.Raw(t.SampleEntry)
						})
						w.FullBox("stts", 0, 0, func(w *Buf) { w.U32(0) })
						w.FullBox("stsc", 0, 0, func(w *Buf) { w.U32(0) })
						w.FullBox("stsz", 0, 0, func(w *Buf) { w.U32(0); w.U32(0) })
						w.FullBox("stco", 0, 0, func(w *Buf) { w.U32(0) })
					})
				})
			})
		})
		w.Box("mvex", func(w *Buf) {
			w.FullBox("trex", 0, 0, func(w *Buf) {
				w.U32(trackID)
				w.U32(1) // default_sample_description_index
				w.U32(0)
				w.U32(0)
				w.U32(0)
			})
		})
	})
	return w.Bytes()
}

// packLanguage packs an ISO 639-2/T code into mdhd's 15-bit form.
func packLanguage(lang string) uint16 {
	if len(lang) != 3 {
		lang = "und"
	}
	var v uint16
	for i := 0; i < 3; i++ {
		c := lang[i]
		if c < 'a' || c > 'z' {
			return packLanguage("und")
		}
		v = v<<5 | uint16(c-0x60)
	}
	return v
}

// Sample is one media sample of a fragment.
type Sample struct {
	Duration uint32
	// CTO is the composition time offset (PTS − DTS); may be negative.
	CTO   int32
	Flags uint32
	Data  []byte
}

// Fragment is one moof+mdat pair for the single track.
type Fragment struct {
	Sequence       uint32
	BaseDecodeTime uint64
	Samples        []Sample
}

func (f *Fragment) hasCTO() bool {
	for _, s := range f.Samples {
		if s.CTO != 0 {
			return true
		}
	}
	return false
}

func (f *Fragment) moof() []byte {
	cto := f.hasCTO()
	flags := uint32(trunDataOffset | trunSampleDuration | trunSampleSize | trunSampleFlags)
	if cto {
		flags |= trunSampleCTO
	}
	var w Buf
	var dataOffsetAt int
	w.Box("moof", func(w *Buf) {
		w.FullBox("mfhd", 0, 0, func(w *Buf) { w.U32(f.Sequence) })
		w.Box("traf", func(w *Buf) {
			w.FullBox("tfhd", 0, tfhdDefaultBaseIsMoof, func(w *Buf) { w.U32(trackID) })
			w.FullBox("tfdt", 1, 0, func(w *Buf) { w.U64(f.BaseDecodeTime) })
			// Version 1 allows signed composition offsets.
			w.FullBox("trun", 1, flags, func(w *Buf) {
				w.U32(uint32(len(f.Samples)))
				dataOffsetAt = len(w.b)
				w.U32(0)
				for _, s := range f.Samples {
					w.U32(s.Duration)
					w.U32(uint32(len(s.Data)))
					w.U32(s.Flags)
					if cto {
						w.U32(uint32(s.CTO))
					}
				}
			})
		})
	})
	b := w.Bytes()
	binary.BigEndian.PutUint32(b[dataOffsetAt:], uint32(len(b)+f.mdatHeaderLen()))
	return b
}

func (f *Fragment) payloadLen() int64 {
	var n int64
	for _, s := range f.Samples {
		n += int64(len(s.Data))
	}
	return n
}

func (f *Fragment) mdatHeaderLen() int {
	if f.payloadLen()+boxHeaderLen > 0xFFFFFFFF {
		return 16
	}
	return boxHeaderLen
}

// Size is the encoded size of the fragment.
func (f *Fragment) Size() int64 {
	return int64(len(f.moof())) + int64(f.mdatHeaderLen()) + f.payloadLen()
}

// WriteTo streams the fragment without concatenating sample data.
func (f *Fragment) WriteTo(w io.Writer) (int64, error) {
	var total int64
	write := func(p []byte) error {
		n, err := w.Write(p)
		total += int64(n)
		return err
	}
	if err := write(f.moof()); err != nil {
		return total, err
	}
	payload := f.payloadLen()
	var hdr []byte
	if f.mdatHeaderLen() == boxHeaderLen {
		hdr = binary.BigEndian.AppendUint32(nil, uint32(payload+boxHeaderLen))
		hdr = append(hdr, "mdat"...)
	} else {
		hdr = binary.BigEndian.AppendUint32(nil, 1)
		hdr = append(hdr, "mdat"...)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(payload+16))
	}
	if err := write(hdr); err != nil {
		return total, err
	}
	for _, s := range f.Samples {
		if err := write(s.Data); err != nil {
			return total, err
		}
	}
	return total, nil
}
