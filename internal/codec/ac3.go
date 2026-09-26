package codec

import (
	"errors"
	"fmt"
)

// AC-3 / E-AC-3 constants (ETSI TS 102 366).
const (
	ac3SyncWord       = 0x0B77
	ac3FrameSamples   = 1536
	ac3BlockSamples   = 256
	eac3BsidMin       = 11 // bsid > 10 means E-AC-3
	eac3StreamIndep   = 0
	eac3StreamDep     = 1
	eac3StreamConvert = 2
	eac3FscodReduced  = 3
	eac3MaxDataRate   = 0x1FFF
)

var (
	ac3SampleRates   = [...]uint32{48000, 44100, 32000}
	eac3ReducedRates = [...]uint32{24000, 22050, 16000}
	eac3Blocks       = [...]int{1, 2, 3, 6}
	// ac3Channels is the full-bandwidth channel count per acmod.
	ac3Channels = [...]uint8{2, 1, 2, 3, 3, 4, 4, 5}
	ac3Bitrates = [...]uint32{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 576, 640}
)

var errNotAC3 = errors.New("codec: not an AC-3 syncframe")

// AC3Info describes an AC-3 stream from its first syncframe.
type AC3Info struct {
	SampleRate uint32
	Channels   uint8 // including LFE
	dac3       [3]byte
}

// ParseAC3 parses the first AC-3 syncframe of a frame.
func ParseAC3(frame []byte) (*AC3Info, error) {
	if len(frame) < 7 || uint16(frame[0])<<8|uint16(frame[1]) != ac3SyncWord {
		return nil, errNotAC3
	}
	r := newBitReader(frame[4:])
	fscod := r.u(2)
	frmsizecod := r.u(6)
	bsid := r.u(5)
	bsmod := r.u(3)
	acmod := r.u(3)
	if acmod&1 == 1 && acmod != 1 {
		r.skip(2) // cmixlev
	}
	if acmod&4 != 0 {
		r.skip(2) // surmixlev
	}
	if acmod == 2 {
		r.skip(2) // dsurmod
	}
	lfeon := r.u(1)
	if r.err != nil || fscod >= 3 || bsid >= eac3BsidMin || int(frmsizecod>>1) >= len(ac3Bitrates) {
		return nil, fmt.Errorf("codec: invalid AC-3 header %x", frame[:7])
	}
	var w bitWriter
	w.put(2, fscod)
	w.put(5, bsid)
	w.put(3, bsmod)
	w.put(3, acmod)
	w.put(1, lfeon)
	w.put(5, frmsizecod>>1)
	w.put(5, 0)
	info := &AC3Info{SampleRate: ac3SampleRates[fscod], Channels: ac3Channels[acmod] + uint8(lfeon)}
	copy(info.dac3[:], w.b)
	return info, nil
}

// Dac3 is the AC3SpecificBox payload.
func (i *AC3Info) Dac3() []byte { return i.dac3[:] }

// eac3Frame is one E-AC-3 syncframe header.
type eac3Frame struct {
	strmtyp     uint64
	substreamID uint64
	size        int // bytes
	fscod       uint64
	sampleRate  uint32
	blocks      int
	acmod       uint64
	lfeon       uint64
	bsid        uint64
	chanmap     uint64 // 0 if absent
}

func parseEAC3Frame(b []byte) (*eac3Frame, error) {
	if len(b) < 6 || uint16(b[0])<<8|uint16(b[1]) != ac3SyncWord {
		return nil, errNotAC3
	}
	r := newBitReader(b[2:])
	f := &eac3Frame{}
	f.strmtyp = r.u(2)
	f.substreamID = r.u(3)
	f.size = int(r.u(11)+1) * 2
	f.fscod = r.u(2)
	if f.fscod == eac3FscodReduced {
		fscod2 := r.u(2)
		if fscod2 >= 3 {
			return nil, errors.New("codec: invalid E-AC-3 fscod2")
		}
		f.sampleRate = eac3ReducedRates[fscod2]
		f.blocks = 6
	} else {
		f.sampleRate = ac3SampleRates[f.fscod]
		f.blocks = eac3Blocks[r.u(2)]
	}
	f.acmod = r.u(3)
	f.lfeon = r.u(1)
	f.bsid = r.u(5)
	r.skip(5) // dialnorm
	if r.flag() {
		r.skip(8) // compr
	}
	if f.acmod == 0 {
		r.skip(5)
		if r.flag() {
			r.skip(8)
		}
	}
	if f.strmtyp == eac3StreamDep && r.flag() {
		f.chanmap = r.u(16)
	}
	if r.err != nil {
		return nil, r.err
	}
	if f.bsid < eac3BsidMin {
		return nil, errors.New("codec: not an E-AC-3 syncframe")
	}
	return f, nil
}

// EAC3Info describes an E-AC-3 stream from one Matroska frame (which holds
// an independent substream and any dependent substreams).
type EAC3Info struct {
	SampleRate uint32
	dec3       []byte
}

// ParseEAC3 parses all syncframes of one access unit.
func ParseEAC3(frame []byte) (*EAC3Info, error) {
	var frames []*eac3Frame
	for off := 0; off < len(frame); {
		f, err := parseEAC3Frame(frame[off:])
		if err != nil {
			if len(frames) > 0 {
				break
			}
			return nil, err
		}
		frames = append(frames, f)
		off += f.size
	}
	indep := frames[0]
	if indep.strmtyp == eac3StreamDep {
		return nil, errors.New("codec: E-AC-3 access unit starts with a dependent substream")
	}
	numDep := 0
	var chanLoc uint64
	totalBits := 0
	for _, f := range frames {
		totalBits += f.size * 8
		if f.strmtyp == eac3StreamDep {
			numDep++
			// Same derivation as FFmpeg's movenc, so dec3 matches its output.
			chanLoc |= (f.chanmap >> 5) & 0x1FF
		}
	}
	dataRate := uint64(totalBits) * uint64(indep.sampleRate) / uint64(indep.blocks*ac3BlockSamples) / 1000
	if dataRate > eac3MaxDataRate {
		dataRate = eac3MaxDataRate
	}
	fscod := indep.fscod
	var w bitWriter
	w.put(13, dataRate)
	w.put(3, 0) // num_ind_sub - 1
	w.put(2, fscod)
	w.put(5, indep.bsid)
	w.put(1, 0) // reserved
	w.put(1, 0) // asvc
	w.put(3, 0) // bsmod
	w.put(3, indep.acmod)
	w.put(1, indep.lfeon)
	w.put(3, 0) // reserved
	w.put(4, uint64(numDep))
	if numDep > 0 {
		w.put(9, chanLoc)
	} else {
		w.put(1, 0)
	}
	return &EAC3Info{SampleRate: indep.sampleRate, dec3: w.b}, nil
}

// Dec3 is the EC3SpecificBox payload.
func (i *EAC3Info) Dec3() []byte { return i.dec3 }

// EAC3FrameSamples returns the samples in one E-AC-3 access unit.
func EAC3FrameSamples(frame []byte) (uint32, error) {
	f, err := parseEAC3Frame(frame)
	if err != nil {
		return 0, err
	}
	return uint32(f.blocks * ac3BlockSamples), nil
}

// AC3FrameSamples is the fixed AC-3 frame length.
const AC3FrameSamples = ac3FrameSamples
