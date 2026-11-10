package main

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// decodeBencode decodes a single bencoded value. The value must span the
// entire input: anything left over after it is reported as an error.
//
// Bencoded types map to Go types as follows:
//
//	string  -> string (may hold arbitrary bytes, not just valid UTF-8)
//	integer -> int64, or *big.Int when the value doesn't fit in 64 bits
func decodeBencode(bencoded string) (interface{}, error) {
	d := bencodeDecoder{data: bencoded}

	value, err := d.decodeValue()
	if err != nil {
		return nil, err
	}
	if d.pos != len(d.data) {
		return nil, d.errorf("unexpected trailing data")
	}

	return value, nil
}

// bencodeDecoder is a cursor over bencoded data. Each decode method consumes
// exactly one value starting at pos and leaves pos just past it.
type bencodeDecoder struct {
	data string
	pos  int
}

func (d *bencodeDecoder) errorf(format string, args ...interface{}) error {
	return fmt.Errorf("bencode: offset %d: %s", d.pos, fmt.Sprintf(format, args...))
}

// decodeValue decodes whichever kind of value starts at the current position.
func (d *bencodeDecoder) decodeValue() (interface{}, error) {
	if d.pos >= len(d.data) {
		return nil, d.errorf("unexpected end of input")
	}

	switch c := d.data[d.pos]; {
	case isDigit(c):
		return d.decodeString()
	case c == 'i':
		return d.decodeInteger()
	default:
		return nil, d.errorf("unexpected character %q", c)
	}
}

// decodeString decodes a string, which is encoded as <length>:<contents>.
//
// Example: 5:hello -> hello
func (d *bencodeDecoder) decodeString() (string, error) {
	colon := strings.IndexByte(d.data[d.pos:], ':')
	if colon < 0 {
		return "", d.errorf("string length is not followed by ':'")
	}

	lengthStr := d.data[d.pos : d.pos+colon]
	if !isAllDigits(lengthStr) {
		return "", d.errorf("invalid string length %q", lengthStr)
	}
	length, err := strconv.Atoi(lengthStr)
	if err != nil {
		return "", d.errorf("invalid string length %q", lengthStr)
	}

	start := d.pos + colon + 1
	if length > len(d.data)-start {
		return "", d.errorf("string of length %d runs past the end of input", length)
	}

	d.pos = start + length
	return d.data[start:d.pos], nil
}

// decodeInteger decodes an integer, which is encoded as i<number>e.
//
// Example: i52e -> 52, i-52e -> -52
func (d *bencodeDecoder) decodeInteger() (interface{}, error) {
	end := strings.IndexByte(d.data[d.pos:], 'e')
	if end < 0 {
		return nil, d.errorf("integer is not terminated by 'e'")
	}

	number := d.data[d.pos+1 : d.pos+end] // Skips the leading 'i'
	digits := strings.TrimPrefix(number, "-")
	switch {
	case !isAllDigits(digits):
		return nil, d.errorf("invalid integer %q", number)
	case len(digits) > 1 && digits[0] == '0':
		return nil, d.errorf("invalid integer %q: leading zeros are not allowed", number)
	case number == "-0":
		return nil, d.errorf("invalid integer %q: negative zero is not allowed", number)
	}

	d.pos += end + 1

	// Bencoded integers have no size limit, so fall back to arbitrary
	// precision for the (rare) values that overflow an int64.
	if n, err := strconv.ParseInt(number, 10, 64); err == nil {
		return n, nil
	}
	n, _ := new(big.Int).SetString(number, 10) // Can't fail: number was validated above
	return n, nil
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// isAllDigits reports whether s is a non-empty run of ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}
