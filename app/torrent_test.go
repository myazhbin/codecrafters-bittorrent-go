package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleTorrentPath is the torrent file that ships with this repository.
const sampleTorrentPath = "../sample.torrent"

// writeTempFile writes data to a new file in a temporary directory and
// returns the file's path.
func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadTorrentSample(t *testing.T) {
	torrent, err := loadTorrent(sampleTorrentPath)
	if err != nil {
		t.Fatalf("loadTorrent(%q) returned error: %v", sampleTorrentPath, err)
	}

	if want := "http://bittorrent-test-tracker.codecrafters.io/announce"; torrent.Announce != want {
		t.Errorf("Announce = %q, want %q", torrent.Announce, want)
	}
	if want := int64(92063); torrent.Length != want {
		t.Errorf("Length = %d, want %d", torrent.Length, want)
	}
}

func TestParseTorrent(t *testing.T) {
	data := "d" +
		"8:announce31:http://tracker.example/announce" +
		"10:created by4:test" +
		"4:infod" +
		"6:lengthi12345e" +
		"4:name8:file.txt" +
		"12:piece lengthi16384e" +
		"6:pieces20:" + strings.Repeat("\xab", 20) +
		"e" +
		"e"

	torrent, err := parseTorrent([]byte(data))
	if err != nil {
		t.Fatalf("parseTorrent returned error: %v", err)
	}

	if want := "http://tracker.example/announce"; torrent.Announce != want {
		t.Errorf("Announce = %q, want %q", torrent.Announce, want)
	}
	if want := int64(12345); torrent.Length != want {
		t.Errorf("Length = %d, want %d", torrent.Length, want)
	}
}

func TestParseTorrentErrors(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{"empty file", "", "invalid torrent"},
		{"not bencode", "hello", "invalid torrent"},
		{"not a dictionary", "l8:announcee", "not a bencoded dictionary"},
		{"trailing data", "d8:announce3:url4:infod6:lengthi1eeejunk", "trailing data"},
		{"missing announce", "d4:infod6:lengthi1eee", `missing "announce"`},
		{"announce is not a string", "d8:announcei1e4:infod6:lengthi1eee", `"announce" is not a string`},
		{"missing info", "d8:announce3:urle", `missing "info"`},
		{"info is not a dictionary", "d8:announce3:url4:info3:fooe", `"info" is not a dictionary`},
		{"missing length", "d8:announce3:url4:infod4:name1:aee", `missing "length"`},
		{"length is not an integer", "d8:announce3:url4:infod6:length1:5ee", `"length" is not an integer`},
		{"negative length", "d8:announce3:url4:infod6:lengthi-1eee", "negative length"},
		{"length overflows", "d8:announce3:url4:infod6:lengthi99999999999999999999eee", `"length" is out of range`},
		{
			"multi-file torrent",
			"d8:announce3:url4:infod5:filesld6:lengthi1e4:pathl1:aeeeee",
			"multi-file torrents are not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			torrent, err := parseTorrent([]byte(tt.data))
			if err == nil {
				t.Fatalf("parseTorrent succeeded with %+v, want an error containing %q", torrent, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("parseTorrent error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadTorrentErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.torrent")
		if _, err := loadTorrent(path); !os.IsNotExist(err) {
			t.Errorf("loadTorrent error = %v, want a file-not-found error", err)
		}
	})

	t.Run("invalid file", func(t *testing.T) {
		path := writeTempFile(t, "broken.torrent", []byte("not a torrent"))

		_, err := loadTorrent(path)
		if err == nil {
			t.Fatal("loadTorrent succeeded, want an error")
		}
		// The error should say which file is broken.
		if !strings.Contains(err.Error(), path) {
			t.Errorf("loadTorrent error = %q, want it to mention %q", err, path)
		}
	})
}
