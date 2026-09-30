package opack

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// Vectors from pyatv's tests/support/test_opack.py.

func TestMarshalVectors(t *testing.T) {
	uuid := UUID{0x12, 0x34, 0x56, 0x78, 0x12, 0x34, 0x56, 0x78, 0x12, 0x34, 0x56, 0x78, 0x12, 0x34, 0x56, 0x78}
	fifteen := make([]any, 15)
	for i := range fifteen {
		fifteen[i] = true
	}
	tests := []struct {
		name string
		in   any
		want []byte
	}{
		{"true", true, []byte{0x01}},
		{"false", false, []byte{0x02}},
		{"null", nil, []byte{0x04}},
		{"uuid", uuid, append([]byte{0x05}, uuid[:]...)},
		{"int 0", 0, []byte{0x08}},
		{"int 0xf", 0xf, []byte{0x17}},
		{"int 0x27", 0x27, []byte{0x2f}},
		{"int 0x28", 0x28, []byte{0x30, 0x28}},
		{"int 0x1ff", 0x1ff, []byte{0x31, 0xff, 0x01}},
		{"int 0x1ffffff", 0x1ffffff, []byte{0x32, 0xff, 0xff, 0xff, 0x01}},
		{"int 0x1ffffffffffffff", uint64(0x1ffffffffffffff), []byte{0x33, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}},
		{"float64", 1.0, []byte{0x36, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xf0, 0x3f}},
		{"string a", "a", []byte{0x41, 0x61}},
		{"string abc", "abc", []byte{0x43, 0x61, 0x62, 0x63}},
		{"string 32", strings.Repeat("a", 32), append([]byte{0x60}, bytes.Repeat([]byte("a"), 32)...)},
		{"string 33", strings.Repeat("a", 33), append([]byte{0x61, 0x21}, bytes.Repeat([]byte("a"), 33)...)},
		{"string 256", strings.Repeat("a", 256), append([]byte{0x62, 0x00, 0x01}, bytes.Repeat([]byte("a"), 256)...)},
		{"data 1", []byte{0xac}, []byte{0x71, 0xac}},
		{"data 3", []byte{0x12, 0x34, 0x56}, []byte{0x73, 0x12, 0x34, 0x56}},
		{"data 32", bytes.Repeat([]byte{0xad}, 32), append([]byte{0x90}, bytes.Repeat([]byte{0xad}, 32)...)},
		{"data 33", bytes.Repeat([]byte{0x61}, 33), append([]byte{0x91, 0x21}, bytes.Repeat([]byte{0x61}, 33)...)},
		{"data 256", bytes.Repeat([]byte{0x61}, 256), append([]byte{0x92, 0x00, 0x01}, bytes.Repeat([]byte{0x61}, 256)...)},
		{"data 65536", bytes.Repeat([]byte{0x61}, 65536), append([]byte{0x93, 0x00, 0x00, 0x01, 0x00}, bytes.Repeat([]byte{0x61}, 65536)...)},
		{"empty array", []any{}, []byte{0xd0}},
		{"array", []any{1, "test", false}, []byte{0xd3, 0x09, 0x44, 0x74, 0x65, 0x73, 0x74, 0x02}},
		{"nested array", []any{[]any{true}}, []byte{0xd1, 0xd1, 0x01}},
		{"endless array", fifteen, append(append([]byte{0xdf}, bytes.Repeat([]byte{0x01}, 15)...), 0x03)},
		{"empty dict", map[string]any{}, []byte{0xe0}},
		{"dict", map[string]any{"a": 12}, []byte{0xe1, 0x41, 0x61, 0x14}},
		{"nested dict", map[string]any{"b": map[string]any{"a": 2}}, []byte{0xe1, 0x41, 0x62, 0xe1, 0x41, 0x61, 0x0a}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Marshal(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("Marshal = % x, want % x", got, tt.want)
			}
		})
	}
}

func TestMarshalEndlessDict(t *testing.T) {
	// pyatv: dict(chr(x): chr(x+1) for x in range(97, 127, 2)), 15 entries
	// written in key order, which is also sorted order.
	in := map[string]any{}
	want := []byte{0xef}
	for x := byte(97); x < 127; x += 2 {
		in[string(rune(x))] = string(rune(x + 1))
		want = append(want, 0x41, x, 0x41, x+1)
	}
	want = append(want, 0x03)
	got, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Marshal = % x, want % x", got, want)
	}
}

func TestMarshalRejects(t *testing.T) {
	for _, in := range []any{-1, struct{}{}, map[string]any{"a": []int{1}}} {
		if _, err := Marshal(in); err == nil {
			t.Errorf("Marshal(%#v) succeeded", in)
		}
	}
}

func TestUnmarshalVectors(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want any
	}{
		{"true", []byte{0x01}, true},
		{"false", []byte{0x02}, false},
		{"null", []byte{0x04}, nil},
		{"absolute time", []byte{0x06, 0x01, 0, 0, 0, 0, 0, 0, 0}, int64(1)},
		{"int 0", []byte{0x08}, int64(0)},
		{"int 0x27", []byte{0x2f}, int64(0x27)},
		{"int 0x28", []byte{0x30, 0x28}, int64(0x28)},
		{"int 0x1ff", []byte{0x31, 0xff, 0x01}, int64(0x1ff)},
		{"int 0x1ffffff", []byte{0x32, 0xff, 0xff, 0xff, 0x01}, int64(0x1ffffff)},
		{"int 0x1ffffffffffffff", []byte{0x33, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, int64(0x1ffffffffffffff)},
		{"uint64 max", []byte{0x33, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, uint64(0xffffffffffffffff)},
		{"float32", []byte{0x35, 0x00, 0x00, 0x80, 0x3f}, 1.0},
		{"float64", []byte{0x36, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xf0, 0x3f}, 1.0},
		{"string", []byte{0x43, 0x61, 0x62, 0x63}, "abc"},
		{"string 33", append([]byte{0x61, 0x21}, bytes.Repeat([]byte("a"), 33)...), strings.Repeat("a", 33)},
		{"string 256", append([]byte{0x62, 0x00, 0x01}, bytes.Repeat([]byte("a"), 256)...), strings.Repeat("a", 256)},
		{"string 3-byte length", append([]byte{0x63, 0x00, 0x00, 0x01}, bytes.Repeat([]byte("a"), 65536)...), strings.Repeat("a", 65536)},
		{"data", []byte{0x73, 0x12, 0x34, 0x56}, []byte{0x12, 0x34, 0x56}},
		{"data 33", append([]byte{0x91, 0x21}, bytes.Repeat([]byte{0x61}, 33)...), bytes.Repeat([]byte{0x61}, 33)},
		{"array", []byte{0xd3, 0x09, 0x44, 0x74, 0x65, 0x73, 0x74, 0x02}, []any{int64(1), "test", false}},
		{"empty array", []byte{0xd0}, []any{}},
		{"endless array", []byte{0xdf, 0x41, 0x61, 0x02, 0x03}, []any{"a", false}},
		{"dict", []byte{0xe2, 0x41, 0x61, 0x14, 0x41, 0x62, 0x04}, map[string]any{"a": int64(12), "b": nil}},
		{"nested dict", []byte{0xe1, 0x41, 0x62, 0xe1, 0x41, 0x61, 0x0a}, map[string]any{"b": map[string]any{"a": int64(2)}}},
		{"endless dict", []byte{0xef, 0x41, 0x61, 0x41, 0x62, 0x41, 0x63, 0x41, 0x64, 0x03}, map[string]any{"a": "b", "c": "d"}},
		{"references", []byte{0xd2, 0x41, 0x61, 0xa0}, []any{"a", "a"}},
		{"more references", []byte{0xd4, 0x43, 0x66, 0x6f, 0x6f, 0x43, 0x62, 0x61, 0x72, 0xa0, 0xa1}, []any{"foo", "bar", "foo", "bar"}},
		{
			"references in dict",
			[]byte{0xe3, 0x41, 0x61, 0x41, 0x62, 0x41, 0x63, 0xe1, 0x41, 0x64, 0xa0, 0xa3, 0x01},
			map[string]any{"a": "b", "c": map[string]any{"d": "a"}, "d": true},
		},
		{"long reference", []byte{0xd2, 0x41, 0x61, 0xc1, 0x00}, []any{"a", "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Unmarshal(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Unmarshal = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestUnmarshalUUID(t *testing.T) {
	in := []byte{0x05, 0x12, 0x34, 0x56, 0x78, 0x12, 0x34, 0x56, 0x78, 0x12, 0x34, 0x56, 0x78, 0x12, 0x34, 0x56, 0x78}
	got, err := Unmarshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := UUID(in[1:]); got != want {
		t.Fatalf("Unmarshal = %v, want %v", got, want)
	}
}

func TestUnmarshalManyReferences(t *testing.T) {
	// pyatv's test_pack_more_ptr, in its shape: 257 distinct one-byte data
	// values, then references to all of them (0xa0-0xc0, then 0xc1 xx, then
	// 0xc2 xx xx).
	in := []byte{0xdf}
	var want []any
	for i := 0; i < 257; i++ {
		v := []byte(string(rune(i)))
		in = append(in, 0x70+byte(len(v)))
		in = append(in, v...)
		want = append(want, v)
	}
	for i := 0; i < 257; i++ {
		switch {
		case i <= 0x20:
			in = append(in, 0xa0+byte(i))
		case i <= 0xff:
			in = append(in, 0xc1, byte(i))
		default:
			in = append(in, 0xc2, byte(i), byte(i>>8))
		}
		want = append(want, want[i])
	}
	in = append(in, 0x03)
	got, err := Unmarshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Unmarshal mismatch")
	}
}

func TestUnmarshalErrors(t *testing.T) {
	for _, in := range [][]byte{
		{},
		{0x00},             // unknown type
		{0x43, 0x61},       // truncated string
		{0x91},             // missing length
		{0x91, 0x05, 0x1},  // truncated data
		{0xe1, 0x41},       // truncated dict
		{0xe1, 0x08, 0x08}, // non-string key
		{0xdf, 0x01},       // unterminated array
		{0xa0},             // reference to nothing
		{0x01, 0x01},       // trailing bytes
	} {
		if v, err := Unmarshal(in); err == nil {
			t.Errorf("Unmarshal(% x) = %#v, want error", in, v)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	in := map[string]any{
		"_i": "_launchApp",
		"_t": int64(2),
		"_x": int64(12345),
		"_c": map[string]any{"_urlS": "youtube://www.youtube.com/watch?v=dQw4w9WgXcQ"},
		"_l": []any{true, nil, 1.5, []byte{1, 2, 3}, uint64(1) << 63},
	}
	data, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip = %#v, want %#v", got, in)
	}
}
