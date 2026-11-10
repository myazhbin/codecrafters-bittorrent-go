package main

import (
	"crypto/sha1"
	"encoding/hex"
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
	if got, want := hex.EncodeToString(torrent.InfoHash[:]), "d69f91e6b2ae4c542468d1073a71d4ea13879a7f"; got != want {
		t.Errorf("InfoHash = %s, want %s", got, want)
	}
}

func TestParseTorrent(t *testing.T) {
	info := "d" +
		"6:lengthi12345e" +
		"4:name8:file.txt" +
		"12:piece lengthi16384e" +
		"6:pieces20:" + strings.Repeat("\xab", 20) +
		"e"
	data := "d" +
		"8:announce31:http://tracker.example/announce" +
		"10:created by4:test" +
		"4:info" + info +
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
	if want := sha1.Sum([]byte(info)); torrent.InfoHash != want {
		t.Errorf("InfoHash = %x, want %x", torrent.InfoHash, want)
	}
}

// The info hash depends on nothing but the info dictionary.
func TestParseTorrentInfoHashCoversOnlyInfo(t *testing.T) {
	const info = "d6:lengthi1e4:name1:ae"
	want := sha1.Sum([]byte(info))

	tests := []struct {
		name string
		data string
	}{
		{"minimal", "d8:announce3:url4:info" + info + "e"},
		{"different tracker", "d8:announce9:other-url4:info" + info + "e"},
		{"keys after info", "d8:announce3:url4:info" + info + "8:url-listl1:aee"},
		{"keys out of order", "d4:info" + info + "8:announce3:urle"},

		// A nested dictionary's "info" key isn't the torrent's info dictionary.
		{"nested info before", "d8:announce3:url5:extrad4:infod6:lengthi2eee4:info" + info + "e"},
		{"nested info after", "d8:announce3:url4:info" + info + "5:otherd4:infod6:lengthi2eeee"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			torrent, err := parseTorrent([]byte(tt.data))
			if err != nil {
				t.Fatalf("parseTorrent returned error: %v", err)
			}
			if torrent.InfoHash != want {
				t.Errorf("InfoHash = %x, want %x", torrent.InfoHash, want)
			}
		})
	}
}

// A torrent whose info dictionary isn't canonically encoded must still get the
// hash of the bytes in the file, since that's what every other client uses.
func TestParseTorrentInfoHashOfNonCanonicalInfo(t *testing.T) {
	const (
		unsorted = "d4:name1:a6:lengthi1ee" // "name" should come after "length"
		sorted   = "d6:lengthi1e4:name1:ae"
	)

	torrent, err := parseTorrent([]byte("d8:announce3:url4:info" + unsorted + "e"))
	if err != nil {
		t.Fatalf("parseTorrent returned error: %v", err)
	}

	if want := sha1.Sum([]byte(unsorted)); torrent.InfoHash != want {
		t.Errorf("InfoHash = %x, want %x (the hash of the info dictionary as written)", torrent.InfoHash, want)
	}
	if reencoded := sha1.Sum([]byte(sorted)); torrent.InfoHash == reencoded {
		t.Errorf("InfoHash = %x, which is the hash of the re-encoded info dictionary", torrent.InfoHash)
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
		{"not a dictionary", "l8:announcee", "not a dictionary"},
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
