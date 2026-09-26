// Package codec parses just enough of each codec's bitstream to build ISO
// BMFF sample entries, RFC 6381 codec strings and per-frame durations for
// transmuxing. It never decodes media.
package codec

import "errors"

var errShortBitstream = errors.New("codec: bitstream too short")

// bitReader reads MSB-first bits.
type bitReader struct {
	b   []byte
	pos int // bit position
	err error
}

func newBitReader(b []byte) *bitReader { return &bitReader{b: b} }

func (r *bitReader) u(n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		if r.pos>>3 >= len(r.b) {
			r.err = errShortBitstream
			return 0
		}
		bit := (r.b[r.pos>>3] >> (7 - uint(r.pos&7))) & 1
		v = v<<1 | uint64(bit)
		r.pos++
	}
	return v
}

func (r *bitReader) flag() bool { return r.u(1) == 1 }

func (r *bitReader) skip(n int) { r.pos += n }

// ue reads an unsigned Exp-Golomb code.
func (r *bitReader) ue() uint64 {
	zeros := 0
	for r.u(1) == 0 {
		if r.err != nil || zeros > 32 {
			r.err = errShortBitstream
			return 0
		}
		zeros++
	}
	return (1<<zeros - 1) + r.u(zeros)
}

// se reads a signed Exp-Golomb code.
func (r *bitReader) se() int64 {
	v := r.ue()
	if v&1 == 1 {
		return int64(v+1) / 2
	}
	return -int64(v / 2)
}

// bitWriter writes MSB-first bits.
type bitWriter struct {
	b    []byte
	nbit int
}

func (w *bitWriter) put(n int, v uint64) {
	for i := n - 1; i >= 0; i-- {
		if w.nbit&7 == 0 {
			w.b = append(w.b, 0)
		}
		if (v>>uint(i))&1 == 1 {
			w.b[len(w.b)-1] |= 1 << (7 - uint(w.nbit&7))
		}
		w.nbit++
	}
}

// unescapeRBSP removes emulation-prevention bytes (00 00 03).
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}
