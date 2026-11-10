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
//	list    -> []interface{}
//	dict    -> map[string]interface{}
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

// maxBencodeDepth caps how deeply lists and dictionaries may be nested.
// Real-world bencoded data is only a handful of levels deep; the cap stops
// hostile input (which can arrive from the network) from exhausting the stack.
const maxBencodeDepth = 512

// bencodeDecoder is a cursor over bencoded data. Each decode method consumes
// exactly one value starting at pos and leaves pos just past it.
type bencodeDecoder struct {
	data  string
	pos   int
	depth int // Number of lists and dictionaries enclosing pos
}

func (d *bencodeDecoder) errorf(format string, args ...interface{}) error {
	return fmt.Errorf("bencode: offset %d: %s", d.pos, fmt.Sprintf(format, args...))
}

// descend records that decoding is entering a list or dictionary, failing if
// that would nest too deeply. Each successful call must be paired with ascend.
func (d *bencodeDecoder) descend() error {
	if d.depth == maxBencodeDepth {
		return d.errorf("values are nested more than %d levels deep", maxBencodeDepth)
	}
	d.depth++
	return nil
}

func (d *bencodeDecoder) ascend() {
	d.depth--
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
	case c == 'l':
		return d.decodeList()
	case c == 'd':
		return d.decodeDict()
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

// decodeList decodes a list, which is encoded as l<bencoded_elements>e, with
// no separators between the elements.
//
// Example: l5:helloi52ee -> ["hello", 52]
func (d *bencodeDecoder) decodeList() ([]interface{}, error) {
	if err := d.descend(); err != nil {
		return nil, err
	}
	defer d.ascend()

	d.pos++ // Skips the leading 'l'

	list := []interface{}{} // Non-nil, so that an empty list is [] rather than null in JSON
	for {
		if d.pos >= len(d.data) {
			return nil, d.errorf("list is not terminated by 'e'")
		}
		if d.data[d.pos] == 'e' {
			d.pos++
			return list, nil
		}

		element, err := d.decodeValue()
		if err != nil {
			return nil, err
		}
		list = append(list, element)
	}
}

// decodeDict decodes a dictionary, which is encoded as
// d<key1><value1>...<keyN><valueN>e. Keys must be strings.
//
// Encoders are required to write the keys in sorted order, but that isn't
// enforced here: out-of-order keys are harmless, and tolerating them lets
// slightly malformed data still be read. Duplicate keys, on the other hand,
// make the value ambiguous and are rejected.
//
// Example: d3:foo3:bar5:helloi52ee -> {"foo": "bar", "hello": 52}
func (d *bencodeDecoder) decodeDict() (map[string]interface{}, error) {
	if err := d.descend(); err != nil {
		return nil, err
	}
	defer d.ascend()

	d.pos++ // Skips the leading 'd'

	dict := map[string]interface{}{} // Non-nil, so that an empty dictionary is {} rather than null in JSON
	for {
		if d.pos >= len(d.data) {
			return nil, d.errorf("dictionary is not terminated by 'e'")
		}
		if d.data[d.pos] == 'e' {
			d.pos++
			return dict, nil
		}

		if !isDigit(d.data[d.pos]) {
			return nil, d.errorf("dictionary keys must be strings")
		}
		key, err := d.decodeString()
		if err != nil {
			return nil, err
		}
		if _, exists := dict[key]; exists {
			return nil, d.errorf("duplicate dictionary key %q", key)
		}

		if d.pos >= len(d.data) || d.data[d.pos] == 'e' {
			return nil, d.errorf("dictionary key %q has no value", key)
		}
		value, err := d.decodeValue()
		if err != nil {
			return nil, err
		}
		dict[key] = value
	}
}

// dictString returns the string stored under key in a decoded dictionary.
func dictString(dict map[string]interface{}, key string) (string, error) {
	value, ok := dict[key]
	if !ok {
		return "", fmt.Errorf("missing %q", key)
	}
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%q is not a string", key)
	}
	return s, nil
}

// dictInt returns the integer stored under key in a decoded dictionary.
func dictInt(dict map[string]interface{}, key string) (int64, error) {
	value, ok := dict[key]
	if !ok {
		return 0, fmt.Errorf("missing %q", key)
	}
	switch n := value.(type) {
	case int64:
		return n, nil
	case *big.Int:
		return 0, fmt.Errorf("%q is out of range: %s", key, n)
	default:
		return 0, fmt.Errorf("%q is not an integer", key)
	}
}

// dictDict returns the dictionary stored under key in a decoded dictionary.
func dictDict(dict map[string]interface{}, key string) (map[string]interface{}, error) {
	value, ok := dict[key]
	if !ok {
		return nil, fmt.Errorf("missing %q", key)
	}
	d, ok := value.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%q is not a dictionary", key)
	}
	return d, nil
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
