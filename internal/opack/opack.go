// Package opack encodes and decodes OPACK, the binary serialization Apple's
// Companion protocol (CoreUtils) uses for its messages. The format follows
// pyatv's pyatv/support/opack.py (MIT), whose test vectors opack_test.go
// reuses; see docs/10-companion.md.
//
// Values map to Go types as follows:
//
//	OPACK            Go (Unmarshal)   Go (Marshal accepts)
//	true/false       bool             bool
//	null             nil              nil
//	UUID             UUID             UUID
//	integer          int64 / uint64   any int or uint type
//	float32/float64  float64          float32, float64
//	string           string           string
//	data             []byte           []byte
//	array            []any            []any
//	dictionary       map[string]any   map[string]any
//
// Dictionaries with keys other than strings are rejected. Absolute times are
// decoded as their raw integer. Marshal does not emit back-references, which
// only make the encoding shorter; Unmarshal resolves them.
package opack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"
)

// UUID is a 16-byte OPACK UUID.
type UUID [16]byte

// Type bytes.
const (
	tagTrue         = 0x01
	tagFalse        = 0x02
	tagTerminator   = 0x03
	tagNull         = 0x04
	tagUUID         = 0x05
	tagAbsoluteTime = 0x06
	tagSmallInt     = 0x08 // 0x08-0x2f: integers 0-39
	tagSmallIntMax  = 0x2f
	tagInt8         = 0x30 // 0x30-0x33: 1, 2, 4, 8 byte little-endian integers
	tagInt64        = 0x33
	tagFloat32      = 0x35
	tagFloat64      = 0x36
	tagString       = 0x40 // 0x40-0x60: strings of 0-32 bytes
	tagStringMax    = 0x60
	tagString1      = 0x61 // 0x61-0x64: 1, 2, 3, 4 byte length prefix
	tagString4      = 0x64
	tagData         = 0x70 // 0x70-0x90: data of 0-32 bytes
	tagDataMax      = 0x90
	tagData1        = 0x91 // 0x91-0x94: 1, 2, 4, 8 byte length prefix
	tagData8        = 0x94
	tagRef          = 0xa0 // 0xa0-0xc0: back-references 0-32
	tagRefMax       = 0xc0
	tagRef1         = 0xc1 // 0xc1-0xc4: 1, 2, 3, 4 byte reference index
	tagRef4         = 0xc4
	tagArray        = 0xd0 // 0xd0-0xde: arrays of 0-14; 0xdf: terminated
	tagDict         = 0xe0 // 0xe0-0xee: dictionaries of 0-14; 0xef: terminated
	countEndless    = 0x0f

	smallIntLimit = tagSmallIntMax - tagSmallInt + 1 // 40
	shortLimit    = tagStringMax - tagString         // 32
)

// Marshal encodes v. Dictionary keys are written in sorted order so the
// encoding is deterministic.
func Marshal(v any) ([]byte, error) {
	return appendValue(nil, v)
}

func appendValue(b []byte, v any) ([]byte, error) {
	switch v := v.(type) {
	case nil:
		return append(b, tagNull), nil
	case bool:
		if v {
			return append(b, tagTrue), nil
		}
		return append(b, tagFalse), nil
	case UUID:
		return append(append(b, tagUUID), v[:]...), nil
	case int:
		return appendInt(b, int64(v))
	case int8:
		return appendInt(b, int64(v))
	case int16:
		return appendInt(b, int64(v))
	case int32:
		return appendInt(b, int64(v))
	case int64:
		return appendInt(b, v)
	case uint:
		return appendUint(b, uint64(v)), nil
	case uint8:
		return appendUint(b, uint64(v)), nil
	case uint16:
		return appendUint(b, uint64(v)), nil
	case uint32:
		return appendUint(b, uint64(v)), nil
	case uint64:
		return appendUint(b, v), nil
	case float32:
		return binary.LittleEndian.AppendUint32(append(b, tagFloat32), math.Float32bits(v)), nil
	case float64:
		return binary.LittleEndian.AppendUint64(append(b, tagFloat64), math.Float64bits(v)), nil
	case string:
		return appendSized(b, []byte(v), tagString, tagString1, []int{1, 2, 3, 4}), nil
	case []byte:
		return appendSized(b, v, tagData, tagData1, []int{1, 2, 4, 8}), nil
	case []any:
		b = append(b, tagArray+byte(min(len(v), countEndless)))
		for _, item := range v {
			var err error
			if b, err = appendValue(b, item); err != nil {
				return nil, err
			}
		}
		if len(v) >= countEndless {
			b = append(b, tagTerminator)
		}
		return b, nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b = append(b, tagDict+byte(min(len(v), countEndless)))
		for _, k := range keys {
			b = appendSized(b, []byte(k), tagString, tagString1, []int{1, 2, 3, 4})
			var err error
			if b, err = appendValue(b, v[k]); err != nil {
				return nil, fmt.Errorf("key %q: %w", k, err)
			}
		}
		if len(v) >= countEndless {
			b = append(b, tagTerminator)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("opack: unsupported type %T", v)
	}
}

func appendInt(b []byte, v int64) ([]byte, error) {
	if v < 0 {
		// OPACK integers are unsigned on the wire; Companion never sends
		// negative numbers.
		return nil, fmt.Errorf("opack: negative integer %d", v)
	}
	return appendUint(b, uint64(v)), nil
}

func appendUint(b []byte, v uint64) []byte {
	switch {
	case v < smallIntLimit:
		return append(b, tagSmallInt+byte(v))
	case v <= math.MaxUint8:
		return append(b, tagInt8, byte(v))
	case v <= math.MaxUint16:
		return binary.LittleEndian.AppendUint16(append(b, tagInt8+1), uint16(v))
	case v <= math.MaxUint32:
		return binary.LittleEndian.AppendUint32(append(b, tagInt8+2), uint32(v))
	default:
		return binary.LittleEndian.AppendUint64(append(b, tagInt64), v)
	}
}

// appendSized writes a string or data value: short values carry their length
// in the tag, longer ones a little-endian length of one of widths.
func appendSized(b, v []byte, short, long byte, widths []int) []byte {
	if len(v) <= shortLimit {
		return append(append(b, short+byte(len(v))), v...)
	}
	for i, width := range widths {
		if width < 8 && uint64(len(v)) >= 1<<(8*width) {
			continue
		}
		b = append(b, long+byte(i))
		b = appendLength(b, uint64(len(v)), width)
		return append(b, v...)
	}
	panic("opack: value too long") // unreachable: the last width is 8 bytes
}

func appendLength(b []byte, n uint64, width int) []byte {
	for i := 0; i < width; i++ {
		b = append(b, byte(n>>(8*i)))
	}
	return b
}

// Unmarshal decodes one value from data, which must hold nothing else.
func Unmarshal(data []byte) (any, error) {
	d := decoder{data: data}
	v, err := d.value()
	if err != nil {
		return nil, err
	}
	if d.pos != len(d.data) {
		return nil, fmt.Errorf("opack: %d trailing bytes", len(d.data)-d.pos)
	}
	return v, nil
}

var errTruncated = errors.New("opack: truncated")

type decoder struct {
	data []byte
	pos  int
	// objects are the values a back-reference can name, in first-seen order.
	objects []any
}

func (d *decoder) take(n int) ([]byte, error) {
	if n < 0 || len(d.data)-d.pos < n {
		return nil, errTruncated
	}
	b := d.data[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

func (d *decoder) uintN(n int) (uint64, error) {
	b, err := d.take(n)
	if err != nil {
		return 0, err
	}
	var v uint64
	for i, octet := range b {
		v |= uint64(octet) << (8 * i)
	}
	return v, nil
}

// remember records a value a later back-reference may name. Like pyatv, a
// value equal to one already recorded is not recorded twice.
func (d *decoder) remember(v any) {
	for _, o := range d.objects {
		if equalScalar(o, v) {
			return
		}
	}
	d.objects = append(d.objects, v)
}

func equalScalar(a, b any) bool {
	switch a := a.(type) {
	case []byte:
		b, ok := b.([]byte)
		return ok && string(a) == string(b)
	case string, int64, uint64, float64, UUID:
		return a == b
	}
	return false
}

func (d *decoder) value() (any, error) {
	tagBytes, err := d.take(1)
	if err != nil {
		return nil, err
	}
	tag := tagBytes[0]
	switch {
	case tag == tagTrue:
		return true, nil
	case tag == tagFalse:
		return false, nil
	case tag == tagNull:
		return nil, nil
	case tag == tagUUID:
		b, err := d.take(16)
		if err != nil {
			return nil, err
		}
		v := UUID(b)
		d.remember(v)
		return v, nil
	case tag == tagAbsoluteTime:
		n, err := d.uintN(8)
		if err != nil {
			return nil, err
		}
		v := intValue(n)
		d.remember(v)
		return v, nil
	case tag >= tagSmallInt && tag <= tagSmallIntMax:
		return int64(tag - tagSmallInt), nil
	case tag >= tagInt8 && tag <= tagInt64:
		n, err := d.uintN(1 << (tag - tagInt8))
		if err != nil {
			return nil, err
		}
		v := intValue(n)
		d.remember(v)
		return v, nil
	case tag == tagFloat32:
		n, err := d.uintN(4)
		if err != nil {
			return nil, err
		}
		v := float64(math.Float32frombits(uint32(n)))
		d.remember(v)
		return v, nil
	case tag == tagFloat64:
		n, err := d.uintN(8)
		if err != nil {
			return nil, err
		}
		v := math.Float64frombits(n)
		d.remember(v)
		return v, nil
	case tag >= tagString && tag <= tagString4:
		b, err := d.sized(tag, tagString, tagString1, []int{1, 2, 3, 4})
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(b) {
			return nil, errors.New("opack: string is not UTF-8")
		}
		v := string(b)
		d.remember(v)
		return v, nil
	case tag >= tagData && tag <= tagData8:
		b, err := d.sized(tag, tagData, tagData1, []int{1, 2, 4, 8})
		if err != nil {
			return nil, err
		}
		v := append([]byte(nil), b...)
		d.remember(v)
		return v, nil
	case tag >= tagRef && tag <= tagRef4:
		index := uint64(tag - tagRef)
		if tag >= tagRef1 {
			if index, err = d.uintN(int(tag - tagRefMax)); err != nil {
				return nil, err
			}
		}
		if index >= uint64(len(d.objects)) {
			return nil, fmt.Errorf("opack: reference %d to unknown object", index)
		}
		return d.objects[index], nil
	case tag&0xf0 == tagArray:
		var items []any
		err := d.container(tag, func() error {
			item, err := d.value()
			items = append(items, item)
			return err
		})
		if items == nil {
			items = []any{}
		}
		return items, err
	case tag&0xf0 == tagDict:
		dict := map[string]any{}
		err := d.container(tag, func() error {
			key, err := d.value()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("opack: dictionary key of type %T", key)
			}
			if dict[name], err = d.value(); err != nil {
				return fmt.Errorf("key %q: %w", name, err)
			}
			return nil
		})
		return dict, err
	default:
		return nil, fmt.Errorf("opack: unknown type 0x%02x", tag)
	}
}

func intValue(n uint64) any {
	if n > math.MaxInt64 {
		return n
	}
	return int64(n)
}

func (d *decoder) sized(tag, short, long byte, widths []int) ([]byte, error) {
	if tag < long {
		return d.take(int(tag - short))
	}
	n, err := d.uintN(widths[tag-long])
	if err != nil {
		return nil, err
	}
	if n > uint64(len(d.data)-d.pos) {
		return nil, errTruncated
	}
	return d.take(int(n))
}

// container reads the elements of an array or dictionary: a count in the
// tag's low nibble, or elements up to a terminator when it is 0xf.
func (d *decoder) container(tag byte, element func() error) error {
	count := int(tag & 0x0f)
	if count != countEndless {
		for i := 0; i < count; i++ {
			if err := element(); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		if d.pos >= len(d.data) {
			return errTruncated
		}
		if d.data[d.pos] == tagTerminator {
			d.pos++
			return nil
		}
		if err := element(); err != nil {
			return err
		}
	}
}
