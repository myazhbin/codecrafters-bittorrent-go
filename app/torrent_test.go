package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleTorrentPath is the torrent file that ships with this repository.
const sampleTorrentPath = "../sample.torrent"

const (
	// testPieceHash stands in for the 20-byte SHA-1 hash of a piece in tests
	// that never check downloaded data against it.
	testPieceHash = "0123456789abcdefghij"

	// minimalInfo is the bencoded info dictionary of a one-byte, one-piece
	// torrent.
	minimalInfo = "d6:lengthi1e4:name1:a12:piece lengthi1e6:pieces20:" + testPieceHash + "e"
)

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

// testContent returns n bytes of arbitrary data to share in a test torrent.
// The data is the same on every run.
func testContent(n int) []byte {
	content := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(content)
	return content
}

// testInfoDict returns the bencoded info dictionary of a torrent for a file
// with the given contents.
func testInfoDict(content []byte, pieceLength int) string {
	var pieces strings.Builder
	for start := 0; start < len(content); start += pieceLength {
		hash := sha1.Sum(content[start:min(start+pieceLength, len(content))])
		pieces.Write(hash[:])
	}

	return fmt.Sprintf("d6:lengthi%de4:name8:test.bin12:piece lengthi%de6:pieces%d:%se",
		len(content), pieceLength, pieces.Len(), pieces.String())
}

// testTorrentFile returns the contents of a torrent file with the given
// tracker URL and bencoded info dictionary.
func testTorrentFile(announce, info string) []byte {
	return []byte(fmt.Sprintf("d8:announce%d:%s4:info%se", len(announce), announce, info))
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
	if want := int64(32768); torrent.PieceLength != want {
		t.Errorf("PieceLength = %d, want %d", torrent.PieceLength, want)
	}

	wantHashes := []string{
		"e876f67a2a8886e8f36b136726c30fa29703022d",
		"6e2275e604a0766656736e81ff10b55204ad8d35",
		"f00d937a0213df1982bc8d097227ad9e909acc17",
	}
	if len(torrent.PieceHashes) != len(wantHashes) {
		t.Fatalf("got %d piece hashes, want %d", len(torrent.PieceHashes), len(wantHashes))
	}
	for i, want := range wantHashes {
		if got := hex.EncodeToString(torrent.PieceHashes[i][:]); got != want {
			t.Errorf("PieceHashes[%d] = %s, want %s", i, got, want)
		}
	}
}

func TestParseTorrent(t *testing.T) {
	content := testContent(100)
	info := testInfoDict(content, 32)
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
	if want := int64(100); torrent.Length != want {
		t.Errorf("Length = %d, want %d", torrent.Length, want)
	}
	if want := sha1.Sum([]byte(info)); torrent.InfoHash != want {
		t.Errorf("InfoHash = %x, want %x", torrent.InfoHash, want)
	}
	if want := int64(32); torrent.PieceLength != want {
		t.Errorf("PieceLength = %d, want %d", torrent.PieceLength, want)
	}

	// 100 bytes in pieces of 32 bytes: three whole pieces, and one of 4 bytes.
	wantHashes := [][sha1.Size]byte{
		sha1.Sum(content[0:32]),
		sha1.Sum(content[32:64]),
		sha1.Sum(content[64:96]),
		sha1.Sum(content[96:100]),
	}
	if len(torrent.PieceHashes) != len(wantHashes) {
		t.Fatalf("got %d piece hashes, want %d", len(torrent.PieceHashes), len(wantHashes))
	}
	for i, want := range wantHashes {
		if torrent.PieceHashes[i] != want {
			t.Errorf("PieceHashes[%d] = %x, want %x", i, torrent.PieceHashes[i], want)
		}
	}
}

func TestParseTorrentPieceCounts(t *testing.T) {
	tests := []struct {
		name        string
		length      int
		pieceLength int
		wantPieces  int
	}{
		{"empty file", 0, 16, 0},
		{"one byte", 1, 16, 1},
		{"one byte short of a piece", 15, 16, 1},
		{"exactly one piece", 16, 16, 1},
		{"one byte over a piece", 17, 16, 2},
		{"exact multiple of the piece length", 64, 16, 4},
		{"partial last piece", 70, 16, 5},
		{"one-byte pieces", 5, 1, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := testTorrentFile("http://tracker.example/announce",
				testInfoDict(testContent(tt.length), tt.pieceLength))

			torrent, err := parseTorrent(data)
			if err != nil {
				t.Fatalf("parseTorrent returned error: %v", err)
			}
			if len(torrent.PieceHashes) != tt.wantPieces {
				t.Errorf("got %d piece hashes, want %d", len(torrent.PieceHashes), tt.wantPieces)
			}
		})
	}
}

// The info hash depends on nothing but the info dictionary.
func TestParseTorrentInfoHashCoversOnlyInfo(t *testing.T) {
	const info = minimalInfo
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
		// "name" should come after "length".
		unsorted = "d4:name1:a6:lengthi1e12:piece lengthi1e6:pieces20:" + testPieceHash + "e"
		sorted   = minimalInfo
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
	// withInfo returns a torrent file with the given bencoded info dictionary.
	withInfo := func(info string) string {
		return "d8:announce3:url4:info" + info + "e"
	}
	const hash = testPieceHash

	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{"empty file", "", "invalid torrent"},
		{"not bencode", "hello", "invalid torrent"},
		{"not a dictionary", "l8:announcee", "not a dictionary"},
		{"trailing data", withInfo(minimalInfo) + "junk", "trailing data"},
		{"missing announce", "d4:info" + minimalInfo + "e", `missing "announce"`},
		{"announce is not a string", "d8:announcei1e4:info" + minimalInfo + "e", `"announce" is not a string`},
		{"missing info", "d8:announce3:urle", `missing "info"`},
		{"info is not a dictionary", "d8:announce3:url4:info3:fooe", `"info" is not a dictionary`},

		{"missing length", withInfo("d4:name1:a12:piece lengthi1e6:pieces20:" + hash + "e"), `info: missing "length"`},
		{"length is not an integer", withInfo("d6:length1:112:piece lengthi1e6:pieces20:" + hash + "e"), `"length" is not an integer`},
		{"negative length", withInfo("d6:lengthi-1e12:piece lengthi1e6:pieces20:" + hash + "e"), "negative length"},
		{"length overflows", withInfo("d6:lengthi99999999999999999999e12:piece lengthi1e6:pieces20:" + hash + "e"), `"length" is out of range`},
		{
			"multi-file torrent",
			withInfo("d5:filesld6:lengthi1e4:pathl1:aeee4:name1:a12:piece lengthi1e6:pieces20:" + hash + "e"),
			"multi-file torrents are not supported",
		},

		{"missing piece length", withInfo("d6:lengthi1e6:pieces20:" + hash + "e"), `missing "piece length"`},
		{"piece length is not an integer", withInfo("d6:lengthi1e12:piece length1:16:pieces20:" + hash + "e"), `"piece length" is not an integer`},
		{"zero piece length", withInfo("d6:lengthi1e12:piece lengthi0e6:pieces20:" + hash + "e"), "piece length 0 is not positive"},
		{"negative piece length", withInfo("d6:lengthi1e12:piece lengthi-1e6:pieces20:" + hash + "e"), "piece length -1 is not positive"},

		{"missing pieces", withInfo("d6:lengthi1e12:piece lengthi1ee"), `missing "pieces"`},
		{"pieces is not a string", withInfo("d6:lengthi1e12:piece lengthi1e6:piecesi5ee"), `"pieces" is not a string`},
		{"truncated piece hash", withInfo("d6:lengthi1e12:piece lengthi1e6:pieces19:" + hash[:19] + "e"), "not a multiple of 20"},
		{"too few piece hashes", withInfo("d6:lengthi2e12:piece lengthi1e6:pieces20:" + hash + "e"), "1 piece hashes for a file of 2 bytes"},
		{"too many piece hashes", withInfo("d6:lengthi1e12:piece lengthi1e6:pieces40:" + hash + hash + "e"), "2 piece hashes for a file of 1 bytes"},
		{"no piece hashes", withInfo("d6:lengthi1e12:piece lengthi1e6:pieces0:e"), "0 piece hashes for a file of 1 bytes"},
		{"piece hash for an empty file", withInfo("d6:lengthi0e12:piece lengthi1e6:pieces20:" + hash + "e"), "1 piece hashes for a file of 0 bytes"},
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
