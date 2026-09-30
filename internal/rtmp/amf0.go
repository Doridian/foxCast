package rtmp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// AMF0 type markers (AMF0 specification, section 2.1).
const (
	amf0Number      = 0x00
	amf0Boolean     = 0x01
	amf0String      = 0x02
	amf0Object      = 0x03
	amf0Null        = 0x05
	amf0Undefined   = 0x06
	amf0ECMAArray   = 0x08
	amf0ObjectEnd   = 0x09
	amf0StrictArray = 0x0a
	amf0Date        = 0x0b
	amf0LongString  = 0x0c
)

// property is one key of an AMF0 object, kept in order for encoding.
type property struct {
	key   string
	value any
}

// object is an ordered AMF0 object for encoding.
type object []property

// undefined encodes as the AMF0 undefined marker.
type undefined struct{}

var errAMFShort = errors.New("amf0: truncated value")

// encodeAMF0 appends the values to b. Supported: float64, int, bool, string,
// nil (null), undefined, and object.
func encodeAMF0(b []byte, values ...any) []byte {
	for _, v := range values {
		switch v := v.(type) {
		case float64:
			b = append(b, amf0Number)
			b = binary.BigEndian.AppendUint64(b, math.Float64bits(v))
		case int:
			b = append(b, amf0Number)
			b = binary.BigEndian.AppendUint64(b, math.Float64bits(float64(v)))
		case bool:
			b = append(b, amf0Boolean)
			if v {
				b = append(b, 1)
			} else {
				b = append(b, 0)
			}
		case string:
			if len(v) > math.MaxUint16 {
				b = append(b, amf0LongString)
				b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
			} else {
				b = append(b, amf0String)
				b = binary.BigEndian.AppendUint16(b, uint16(len(v)))
			}
			b = append(b, v...)
		case nil:
			b = append(b, amf0Null)
		case undefined:
			b = append(b, amf0Undefined)
		case object:
			b = append(b, amf0Object)
			for _, p := range v {
				b = appendAMF0Key(b, p.key)
				b = encodeAMF0(b, p.value)
			}
			b = append(b, 0, 0, amf0ObjectEnd)
		default:
			panic(fmt.Sprintf("amf0: cannot encode %T", v))
		}
	}
	return b
}

func appendAMF0Key(b []byte, key string) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(key)))
	return append(b, key...)
}

// decodeAMF0 decodes every value in b. Numbers decode as float64, objects
// and ECMA arrays as map[string]any, strict arrays as []any, null and
// undefined as nil, and dates as float64 milliseconds.
func decodeAMF0(b []byte) ([]any, error) {
	var values []any
	for len(b) > 0 {
		v, rest, err := decodeAMF0Value(b)
		if err != nil {
			return values, err
		}
		values = append(values, v)
		b = rest
	}
	return values, nil
}

func decodeAMF0Value(b []byte) (any, []byte, error) {
	if len(b) == 0 {
		return nil, nil, errAMFShort
	}
	marker, b := b[0], b[1:]
	switch marker {
	case amf0Number:
		if len(b) < 8 {
			return nil, nil, errAMFShort
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), b[8:], nil
	case amf0Boolean:
		if len(b) < 1 {
			return nil, nil, errAMFShort
		}
		return b[0] != 0, b[1:], nil
	case amf0String:
		return decodeAMF0String(b, 2)
	case amf0LongString:
		return decodeAMF0String(b, 4)
	case amf0Null, amf0Undefined:
		return nil, b, nil
	case amf0Object:
		return decodeAMF0Object(b)
	case amf0ECMAArray:
		if len(b) < 4 {
			return nil, nil, errAMFShort
		}
		return decodeAMF0Object(b[4:]) // the count is advisory
	case amf0StrictArray:
		if len(b) < 4 {
			return nil, nil, errAMFShort
		}
		n := binary.BigEndian.Uint32(b)
		b = b[4:]
		var items []any
		for range n {
			v, rest, err := decodeAMF0Value(b)
			if err != nil {
				return nil, nil, err
			}
			items = append(items, v)
			b = rest
		}
		return items, b, nil
	case amf0Date:
		if len(b) < 10 {
			return nil, nil, errAMFShort
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), b[10:], nil
	default:
		return nil, nil, fmt.Errorf("amf0: unsupported type 0x%02x", marker)
	}
}

func decodeAMF0String(b []byte, lengthSize int) (any, []byte, error) {
	if len(b) < lengthSize {
		return nil, nil, errAMFShort
	}
	var n int
	if lengthSize == 2 {
		n = int(binary.BigEndian.Uint16(b))
	} else {
		n = int(binary.BigEndian.Uint32(b))
	}
	b = b[lengthSize:]
	if len(b) < n {
		return nil, nil, errAMFShort
	}
	return string(b[:n]), b[n:], nil
}

func decodeAMF0Object(b []byte) (any, []byte, error) {
	obj := map[string]any{}
	for {
		if len(b) < 3 {
			return nil, nil, errAMFShort
		}
		n := int(binary.BigEndian.Uint16(b))
		if n == 0 && b[2] == amf0ObjectEnd {
			return obj, b[3:], nil
		}
		b = b[2:]
		if len(b) < n {
			return nil, nil, errAMFShort
		}
		key := string(b[:n])
		v, rest, err := decodeAMF0Value(b[n:])
		if err != nil {
			return nil, nil, err
		}
		obj[key] = v
		b = rest
	}
}
