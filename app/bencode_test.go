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

// Hostile input must produce an error rather than recursing until the stack
// overflows.
func TestDecodeBencodeNestingLimit(t *testing.T) {
	nested := func(depth int) string {
		return strings.Repeat("l", depth) + strings.Repeat("e", depth)
	}

	if _, err := decodeBencode(nested(maxBencodeDepth)); err != nil {
		t.Errorf("decoding %d nested lists returned error: %v", maxBencodeDepth, err)
	}

	if _, err := decodeBencode(nested(maxBencodeDepth + 1)); err == nil {
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
