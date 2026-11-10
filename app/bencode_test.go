package main

import (
	"math/big"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeBencodeString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"5:hello", "hello"},
		{"10:hello12345", "hello12345"},
		{"0:", ""},
		{"1::", ":"},
		{"4:i52e", "i52e"},
		{"3:\x00\xff\n", "\x00\xff\n"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := decodeBencode(tt.input)
			if err != nil {
				t.Fatalf("decodeBencode(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("decodeBencode(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDecodeBencodeInteger(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"i52e", 52},
		{"i-52e", -52},
		{"i0e", 0},
		{"i7e", 7},
		{"i-1e", -1},
		{"i1000000e", 1000000},
		{"i4294967300e", 4294967300}, // Doesn't fit in 32 bits
		{"i9223372036854775807e", 9223372036854775807},
		{"i-9223372036854775808e", -9223372036854775808},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := decodeBencode(tt.input)
			if err != nil {
				t.Fatalf("decodeBencode(%q) returned error: %v", tt.input, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("decodeBencode(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

// Bencoded integers have no size limit, so values that overflow an int64 must
// still decode exactly.
func TestDecodeBencodeBigInteger(t *testing.T) {
	tests := []string{
		"9223372036854775808",
		"-9223372036854775809",
		"123456789012345678901234567890",
	}

	for _, number := range tests {
		t.Run(number, func(t *testing.T) {
			input := "i" + number + "e"

			got, err := decodeBencode(input)
			if err != nil {
				t.Fatalf("decodeBencode(%q) returned error: %v", input, err)
			}
			n, ok := got.(*big.Int)
			if !ok {
				t.Fatalf("decodeBencode(%q) = %#v, want a *big.Int", input, got)
			}
			if n.String() != number {
				t.Errorf("decodeBencode(%q) = %s, want %s", input, n, number)
			}
		})
	}
}

func TestDecodeBencodeList(t *testing.T) {
	tests := []struct {
		input string
		want  []interface{}
	}{
		{"l5:helloi52ee", []interface{}{"hello", int64(52)}},
		{"le", []interface{}{}},
		{"li1ei2ei3ee", []interface{}{int64(1), int64(2), int64(3)}},
		{"l0:e", []interface{}{""}},
		{"l1:ee", []interface{}{"e"}}, // The first "e" is string contents, not the terminator
		{"l1:l1:ie", []interface{}{"l", "i"}},
		{"llee", []interface{}{[]interface{}{}}},
		{"lli4eei5ee", []interface{}{[]interface{}{int64(4)}, int64(5)}},
		{"lli724e9:pineappleee", []interface{}{[]interface{}{int64(724), "pineapple"}}},
		{
			"l4:spamli-1el3:egg3:hameee",
			[]interface{}{"spam", []interface{}{int64(-1), []interface{}{"egg", "ham"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := decodeBencode(tt.input)
			if err != nil {
				t.Fatalf("decodeBencode(%q) returned error: %v", tt.input, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("decodeBencode(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDecodeBencodeDict(t *testing.T) {
	type dict = map[string]interface{}
	type list = []interface{}

	tests := []struct {
		input string
		want  dict
	}{
		{"d3:foo3:bar5:helloi52ee", dict{"foo": "bar", "hello": int64(52)}},
		{"de", dict{}},
		{"d3:foo9:blueberry5:helloi52ee", dict{"foo": "blueberry", "hello": int64(52)}},
		{"d0:i1ee", dict{"": int64(1)}},
		{"d1:e1:ee", dict{"e": "e"}}, // Neither "e" is the terminator
		{"d4:spaml1:a1:bee", dict{"spam": list{"a", "b"}}},
		{"d1:ad1:bd1:cdeeee", dict{"a": dict{"b": dict{"c": dict{}}}}},
		{
			"d10:inner_dictd4:key16:value14:key2i42e8:list_keyl5:item15:item2i3eeee",
			dict{"inner_dict": dict{
				"key1":     "value1",
				"key2":     int64(42),
				"list_key": list{"item1", "item2", int64(3)},
			}},
		},
		{
			"d1:ai1e1:b1:x1:cli1ee1:dd1:ei2eee",
			dict{"a": int64(1), "b": "x", "c": list{int64(1)}, "d": dict{"e": int64(2)}},
		},

		// Keys that aren't in sorted order are tolerated.
		{"d5:helloi52e3:foo3:bare", dict{"foo": "bar", "hello": int64(52)}},

		// Keys are arbitrary byte strings.
		{"d2:\xff\x00i1ee", dict{"\xff\x00": int64(1)}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := decodeBencode(tt.input)
			if err != nil {
				t.Fatalf("decodeBencode(%q) returned error: %v", tt.input, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("decodeBencode(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDecodeBencodeDictInsideList(t *testing.T) {
	got, err := decodeBencode("ld1:ai1eed1:bi2eee")
	if err != nil {
		t.Fatalf("decodeBencode returned error: %v", err)
	}

	want := []interface{}{
		map[string]interface{}{"a": int64(1)},
		map[string]interface{}{"b": int64(2)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decodeBencode = %#v, want %#v", got, want)
	}
}

func TestDecodeBencodeDictRawValues(t *testing.T) {
	type dict = map[string]interface{}

	// The nested dictionary reuses the key "a": only the outermost
	// dictionary's values must be reported.
	input := "d1:ai1e1:b3:foo1:cl1:xi2ee1:dd1:ai9e1:zdeee"

	got, raw, err := decodeBencodeDict(input)
	if err != nil {
		t.Fatalf("decodeBencodeDict(%q) returned error: %v", input, err)
	}

	want := dict{
		"a": int64(1),
		"b": "foo",
		"c": []interface{}{"x", int64(2)},
		"d": dict{"a": int64(9), "z": dict{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decodeBencodeDict(%q) = %#v, want %#v", input, got, want)
	}

	wantRaw := map[string]string{
		"a": "i1e",
		"b": "3:foo",
		"c": "l1:xi2ee",
		"d": "d1:ai9e1:zdee",
	}
	if !reflect.DeepEqual(raw, wantRaw) {
		t.Errorf("decodeBencodeDict(%q) raw values = %#v, want %#v", input, raw, wantRaw)
	}
}

// The raw values must be the input's bytes verbatim, even where they aren't
// what an encoder would produce.
func TestDecodeBencodeDictRawValuesAreVerbatim(t *testing.T) {
	input := "d4:infod1:bi2e1:ai1ee5:alphai0ee" // Keys are out of order at both levels

	_, raw, err := decodeBencodeDict(input)
	if err != nil {
		t.Fatalf("decodeBencodeDict(%q) returned error: %v", input, err)
	}
	if want := "d1:bi2e1:ai1ee"; raw["info"] != want {
		t.Errorf(`raw["info"] = %q, want %q`, raw["info"], want)
	}
	if want := "i0e"; raw["alpha"] != want {
		t.Errorf(`raw["alpha"] = %q, want %q`, raw["alpha"], want)
	}
}

func TestDecodeBencodeDictEmpty(t *testing.T) {
	got, raw, err := decodeBencodeDict("de")
	if err != nil {
		t.Fatalf(`decodeBencodeDict("de") returned error: %v`, err)
	}
	if len(got) != 0 || len(raw) != 0 {
		t.Errorf(`decodeBencodeDict("de") = %#v, %#v, want both empty`, got, raw)
	}
}

func TestDecodeBencodeDictErrors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"empty input", "", "unexpected end of input"},
		{"string", "5:hello", "not a dictionary"},
		{"integer", "i52e", "not a dictionary"},
		{"list of dictionaries", "ld1:ai1eee", "not a dictionary"},
		{"unterminated dictionary", "d1:ai1e", "dictionary is not terminated"},
		{"trailing data", "d1:ai1eejunk", "trailing data"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, raw, err := decodeBencodeDict(tt.input)
			if err == nil {
				t.Fatalf("decodeBencodeDict(%q) = %#v, %#v, want an error", tt.input, got, raw)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("decodeBencodeDict(%q) error = %q, want it to contain %q", tt.input, err, tt.wantErr)
			}
			if got != nil || raw != nil {
				t.Errorf("decodeBencodeDict(%q) returned %#v, %#v alongside an error, want nil", tt.input, got, raw)
			}
		})
	}
}

// Hostile input must produce an error rather than recursing until the stack
// overflows.
func TestDecodeBencodeNestingLimit(t *testing.T) {
	nestedLists := func(depth int) string {
		return strings.Repeat("l", depth) + strings.Repeat("e", depth)
	}
	nestedDicts := func(depth int) string {
		return strings.Repeat("d1:a", depth-1) + "de" + strings.Repeat("e", depth-1)
	}
	nestedMixed := func(depth int) string { // Alternates dictionaries and lists
		return strings.Repeat("d1:al", depth/2) + strings.Repeat("ee", depth/2)
	}

	for name, nested := range map[string]func(int) string{
		"lists":        nestedLists,
		"dictionaries": nestedDicts,
		"mixed":        nestedMixed,
	} {
		if _, err := decodeBencode(nested(maxBencodeDepth)); err != nil {
			t.Errorf("decoding %d nested %s returned error: %v", maxBencodeDepth, name, err)
		}
		if _, err := decodeBencode(nested(maxBencodeDepth + 2)); err == nil {
			t.Errorf("decoding %d nested %s succeeded, want an error", maxBencodeDepth+2, name)
		}
	}

	if _, err := decodeBencode(nestedLists(maxBencodeDepth + 1)); err == nil {
		t.Errorf("decoding %d nested lists succeeded, want an error", maxBencodeDepth+1)
	}

	// The depth must be tracked per branch rather than accumulated: many
	// shallow siblings are fine.
	siblings := "l" + strings.Repeat("le", 4*maxBencodeDepth) + "e"
	if _, err := decodeBencode(siblings); err != nil {
		t.Errorf("decoding %d sibling lists returned error: %v", 4*maxBencodeDepth, err)
	}

	if _, err := decodeBencode(strings.Repeat("l", 1<<20)); err == nil {
		t.Error("decoding a million unterminated lists succeeded, want an error")
	}
	if _, err := decodeBencode(strings.Repeat("d1:a", 1<<20)); err == nil {
		t.Error("decoding a million unterminated dictionaries succeeded, want an error")
	}
}

func TestDecodeBencodeErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty input", ""},
		{"unknown type", "x"},
		{"string shorter than its length", "5:hell"},
		{"string without colon", "5hello"},
		{"string with non-numeric length", "5x:hello"},
		{"string with overflowing length", "99999999999999999999999:a"},
		{"string followed by trailing data", "5:helloworld"},
		{"negative string length", "-1:a"},
		{"integer without digits", "ie"},
		{"integer with only a sign", "i-e"},
		{"unterminated integer", "i52"},
		{"integer without terminator before end", "i"},
		{"negative zero", "i-0e"},
		{"integer with leading zero", "i03e"},
		{"negative integer with leading zero", "i-03e"},
		{"integer with plus sign", "i+5e"},
		{"integer with double sign", "i--5e"},
		{"integer with a space", "i 5e"},
		{"integer with letters", "i5x2e"},
		{"fractional integer", "i5.2e"},
		{"integer followed by trailing data", "i52ei53e"},
		{"unterminated empty list", "l"},
		{"unterminated list", "l5:helloi52e"},
		{"unterminated nested list", "lli4eei5e"},
		{"list with an invalid element", "lxe"},
		{"list with a truncated string", "l5:helle"},
		{"list with a truncated integer", "li52"},
		{"list followed by trailing data", "lee"},
		{"list terminator without a list", "e"},
		{"unterminated empty dictionary", "d"},
		{"unterminated dictionary", "d3:foo3:bar"},
		{"dictionary key without a value at end of input", "d3:foo"},
		{"dictionary key without a value", "d3:fooe"},
		{"dictionary with a trailing key", "d3:foo3:bar5:helloe"},
		{"dictionary with an integer key", "di1ei2ee"},
		{"dictionary with a list key", "dli1eei2ee"},
		{"dictionary with a dictionary key", "ddei2ee"},
		{"dictionary with a duplicate key", "d3:fooi1e3:fooi2ee"},
		{"dictionary with a truncated key", "d5:fooi1ee"},
		{"dictionary with an invalid value", "d3:fooxe"},
		{"dictionary with a truncated value", "d3:fooi52"},
		{"dictionary followed by trailing data", "dee"},
		{"unterminated dictionary inside a list", "ld3:foo3:bare"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeBencode(tt.input)
			if err == nil {
				t.Fatalf("decodeBencode(%q) = %#v, want an error", tt.input, got)
			}
			if got != nil {
				t.Errorf("decodeBencode(%q) returned %#v alongside an error, want nil", tt.input, got)
			}
		})
	}
}
