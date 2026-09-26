// Package fmp4 writes fragmented ISO BMFF (CMAF-style) init and media
// segments, as used by HLS with EXT-X-MAP. Each init segment carries a single
// track with ID 1.
package fmp4

import (
	"encoding/binary"
)

// Buf is a big-endian box builder.
type Buf struct {
	b []byte
}

// Bytes returns the built bytes.
func (w *Buf) Bytes() []byte { return w.b }

// U8 appends a byte.
func (w *Buf) U8(v uint8) { w.b = append(w.b, v) }

// U16 appends a big-endian uint16.
func (w *Buf) U16(v uint16) { w.b = binary.BigEndian.AppendUint16(w.b, v) }

// U24 appends a big-endian 24-bit value.
func (w *Buf) U24(v uint32) { w.b = append(w.b, byte(v>>16), byte(v>>8), byte(v)) }

// U32 appends a big-endian uint32.
func (w *Buf) U32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }

// U64 appends a big-endian uint64.
func (w *Buf) U64(v uint64) { w.b = binary.BigEndian.AppendUint64(w.b, v) }

// Raw appends bytes.
func (w *Buf) Raw(p []byte) { w.b = append(w.b, p...) }

// Zeros appends n zero bytes.
func (w *Buf) Zeros(n int) { w.b = append(w.b, make([]byte, n)...) }

// Box appends a box with the given type whose body is written by fn.
func (w *Buf) Box(typ string, fn func(*Buf)) {
	start := len(w.b)
	w.U32(0)
	w.b = append(w.b, typ[:4]...)
	if fn != nil {
		fn(w)
	}
	binary.BigEndian.PutUint32(w.b[start:], uint32(len(w.b)-start))
}

// FullBox appends a box with a version/flags header.
func (w *Buf) FullBox(typ string, version uint8, flags uint32, fn func(*Buf)) {
	w.Box(typ, func(w *Buf) {
		w.U8(version)
		w.U24(flags)
		if fn != nil {
			fn(w)
		}
	})
}

// Box builds a standalone box.
func Box(typ string, fn func(*Buf)) []byte {
	var w Buf
	w.Box(typ, fn)
	return w.Bytes()
}

// FullBox builds a standalone full box.
func FullBox(typ string, version uint8, flags uint32, fn func(*Buf)) []byte {
	var w Buf
	w.FullBox(typ, version, flags, fn)
	return w.Bytes()
}
