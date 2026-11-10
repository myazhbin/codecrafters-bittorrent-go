package main

import (
	"math/big"
	"reflect"
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
