// Package tlv8 encodes and decodes TLV8 (Type-Length-Value, 8-bit) byte streams
// as used in Apple's HomeKit Accessory Protocol (HAP) pairing messages.
//
// Format: each record is [tag:1][length:1][value:length].
// Values longer than 255 bytes are fragmented into consecutive records with the
// same tag, each at most 255 bytes. Decoding concatenates consecutive fragments.
package tlv8

import "fmt"

// Encode serialises records into a TLV8 byte slice.
// The map iteration order is not guaranteed; for deterministic output use
// EncodeOrdered.
func Encode(records map[uint8][]byte) []byte {
	var out []byte
	for tag, val := range records {
		out = append(out, encodeOne(tag, val)...)
	}
	return out
}

// EncodeOrdered serialises an ordered list of [tag, value] pairs into TLV8.
// Use this when the protocol requires a specific field order.
func EncodeOrdered(records []Record) []byte {
	var out []byte
	for _, r := range records {
		out = append(out, encodeOne(r.Tag, r.Value)...)
	}
	return out
}

// Record is a single TLV8 tag/value pair.
type Record struct {
	Tag   uint8
	Value []byte
}

// Decode parses a TLV8 byte slice into a map of tag → value.
// Consecutive records with the same tag are concatenated (de-fragmented).
func Decode(data []byte) (map[uint8][]byte, error) {
	out := make(map[uint8][]byte)
	var lastTag *uint8

	for len(data) > 0 {
		if len(data) < 2 {
			return nil, fmt.Errorf("tlv8: truncated record (only %d bytes remain)", len(data))
		}
		tag := data[0]
		length := int(data[1])
		data = data[2:]

		if len(data) < length {
			return nil, fmt.Errorf("tlv8: tag 0x%02x claims length %d but only %d bytes remain", tag, length, len(data))
		}
		value := data[:length]
		data = data[length:]

		// Concatenate consecutive fragments of the same tag.
		if lastTag != nil && *lastTag == tag {
			out[tag] = append(out[tag], value...)
		} else {
			out[tag] = append([]byte(nil), value...)
			t := tag
			lastTag = &t
		}
	}
	return out, nil
}

// encodeOne encodes a single tag/value pair, fragmenting if len(value) > 255.
func encodeOne(tag uint8, value []byte) []byte {
	if len(value) == 0 {
		return []byte{tag, 0}
	}
	var out []byte
	for len(value) > 0 {
		chunk := value
		if len(chunk) > 255 {
			chunk = chunk[:255]
		}
		out = append(out, tag, byte(len(chunk)))
		out = append(out, chunk...)
		value = value[len(chunk):]
	}
	return out
}
