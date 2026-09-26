package mkv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
)

// unknownSize marks an element whose size field has all value bits set.
const unknownSize = -1

// maxHeaderLen is the longest possible element header: 4-byte ID + 8-byte size.
const maxHeaderLen = 12

var errInvalidVint = errors.New("mkv: invalid EBML variable-length integer")

// vintLen returns the encoded length of a vint from its first byte.
func vintLen(first byte) int {
	return bits.LeadingZeros8(first) + 1
}

// parseID decodes an element ID (marker bit kept, as IDs are written in specs).
func parseID(b []byte) (id uint32, n int, err error) {
	if len(b) == 0 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	n = vintLen(b[0])
	if n > 4 {
		return 0, 0, errInvalidVint
	}
	if len(b) < n {
		return 0, 0, io.ErrUnexpectedEOF
	}
	for i := 0; i < n; i++ {
		id = id<<8 | uint32(b[i])
	}
	return id, n, nil
}

// parseSize decodes an element data size; unknownSize if all value bits are set.
func parseSize(b []byte) (size int64, n int, err error) {
	if len(b) == 0 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	n = vintLen(b[0])
	if n > 8 {
		return 0, 0, errInvalidVint
	}
	if len(b) < n {
		return 0, 0, io.ErrUnexpectedEOF
	}
	v := uint64(b[0]) & (0xFF >> n)
	allOnes := v == uint64(0xFF>>n)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
		allOnes = allOnes && b[i] == 0xFF
	}
	if allOnes {
		return unknownSize, n, nil
	}
	if v > math.MaxInt64 {
		return 0, 0, errInvalidVint
	}
	return int64(v), n, nil
}

// parseHeader decodes an element header.
func parseHeader(b []byte) (id uint32, size int64, n int, err error) {
	id, idLen, err := parseID(b)
	if err != nil {
		return 0, 0, 0, err
	}
	size, sizeLen, err := parseSize(b[idLen:])
	if err != nil {
		return 0, 0, 0, err
	}
	return id, size, idLen + sizeLen, nil
}

// readHeaderAt reads an element header at absolute offset off.
func readHeaderAt(r io.ReaderAt, off int64) (id uint32, size int64, n int, err error) {
	var buf [maxHeaderLen]byte
	got, err := r.ReadAt(buf[:], off)
	if got == 0 && err != nil {
		return 0, 0, 0, err
	}
	return parseHeader(buf[:got])
}

// element is a fully loaded element body.
type element struct {
	id   uint32
	data []byte
}

// children iterates the child elements of a loaded body. Children with an
// unknown size are rejected; they only occur for Segment and Cluster.
func children(data []byte, fn func(e element) error) error {
	for len(data) > 0 {
		id, size, n, err := parseHeader(data)
		if err != nil {
			return err
		}
		if size == unknownSize || int64(len(data)-n) < size {
			return fmt.Errorf("mkv: element 0x%X overruns its parent", id)
		}
		if err := fn(element{id: id, data: data[n : n+int(size)]}); err != nil {
			return err
		}
		data = data[n+int(size):]
	}
	return nil
}

func readUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func readInt(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	v := int64(int8(b[0]))
	for _, c := range b[1:] {
		v = v<<8 | int64(c)
	}
	return v
}

func readFloat(b []byte) float64 {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b))
	}
	return 0
}

func readString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
