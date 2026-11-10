package main

import (
	"bytes"
	"strings"
	"testing"
)

// runCommand runs the CLI with the given arguments and returns what it wrote
// to stdout.
func runCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var stdout bytes.Buffer
	err := run(args, &stdout)
	return stdout.String(), err
}

func TestRunDecode(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Strings
		{"5:hello", `"hello"`},
		{"0:", `""`},
		{"55:http://bittorrent-test-tracker.codecrafters.io/announce", `"http://bittorrent-test-tracker.codecrafters.io/announce"`},
		{`7:a<b>&"c`, `"a<b>&\"c"`},

		// Integers
		{"i52e", "52"},
		{"i-52e", "-52"},
		{"i0e", "0"},
		{"i4294967300e", "4294967300"},
		{"i123456789012345678901234567890e", "123456789012345678901234567890"},

		// Lists
		{"l5:helloi52ee", `["hello",52]`},
		{"le", "[]"},
		{"lli4eei5ee", "[[4],5]"},
		{"lli724e9:pineappleee", `[[724,"pineapple"]]`},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := runCommand(t, "decode", tt.input)
			if err != nil {
				t.Fatalf("decode %q returned error: %v", tt.input, err)
			}
			if want := tt.want + "\n"; got != want {
				t.Errorf("decode %q printed %q, want %q", tt.input, got, want)
			}
		})
	}
}

func TestRunErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no command", nil, "usage"},
		{"unknown command", []string{"frobnicate"}, "unknown command: frobnicate"},
		{"decode without a value", []string{"decode"}, "usage: decode"},
		{"decode with too many values", []string{"decode", "i1e", "i2e"}, "usage: decode"},
		{"decode with invalid bencode", []string{"decode", "i52"}, "bencode"},
		{"decode with an unterminated list", []string{"decode", "l5:hello"}, "list is not terminated"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runCommand(t, tt.args...)
			if err == nil {
				t.Fatalf("run(%q) succeeded, want an error containing %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("run(%q) error = %q, want it to contain %q", tt.args, err, tt.wantErr)
			}
			if got != "" {
				t.Errorf("run(%q) printed %q despite failing, want no output", tt.args, got)
			}
		})
	}
}
