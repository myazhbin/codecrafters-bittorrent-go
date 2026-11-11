package main

import (
	"bytes"
	"crypto/sha1"
	"fmt"
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

		// Dictionaries
		{"d3:foo3:bar5:helloi52ee", `{"foo":"bar","hello":52}`},
		{"de", "{}"},
		{"d5:helloi52e3:foo3:bare", `{"foo":"bar","hello":52}`}, // Printed with sorted keys
		{
			"d10:inner_dictd4:key16:value14:key2i42e8:list_keyl5:item15:item2i3eeee",
			`{"inner_dict":{"key1":"value1","key2":42,"list_key":["item1","item2",3]}}`,
		},
		{"ld1:ai1eelee", `[{"a":1},[]]`},
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

func TestRunInfo(t *testing.T) {
	t.Run("sample torrent", func(t *testing.T) {
		got, err := runCommand(t, "info", sampleTorrentPath)
		if err != nil {
			t.Fatalf("info returned error: %v", err)
		}

		want := "Tracker URL: http://bittorrent-test-tracker.codecrafters.io/announce\n" +
			"Length: 92063\n" +
			"Info Hash: d69f91e6b2ae4c542468d1073a71d4ea13879a7f\n" +
			"Piece Length: 32768\n" +
			"Piece Hashes:\n" +
			"e876f67a2a8886e8f36b136726c30fa29703022d\n" +
			"6e2275e604a0766656736e81ff10b55204ad8d35\n" +
			"f00d937a0213df1982bc8d097227ad9e909acc17\n"
		if got != want {
			t.Errorf("info printed:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("generated torrent", func(t *testing.T) {
		content := testContent(2500)
		info := testInfoDict(content, 1024)
		path := writeTempFile(t, "test.torrent", testTorrentFile("http://tracker.example/announce", info))

		got, err := runCommand(t, "info", path)
		if err != nil {
			t.Fatalf("info returned error: %v", err)
		}

		want := "Tracker URL: http://tracker.example/announce\n" +
			"Length: 2500\n" +
			fmt.Sprintf("Info Hash: %x\n", sha1.Sum([]byte(info))) +
			"Piece Length: 1024\n" +
			"Piece Hashes:\n" +
			fmt.Sprintf("%x\n", sha1.Sum(content[:1024])) +
			fmt.Sprintf("%x\n", sha1.Sum(content[1024:2048])) +
			fmt.Sprintf("%x\n", sha1.Sum(content[2048:]))
		if got != want {
			t.Errorf("info printed:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		info := testInfoDict(nil, 1024)
		path := writeTempFile(t, "empty.torrent", testTorrentFile("http://tracker.example/announce", info))

		got, err := runCommand(t, "info", path)
		if err != nil {
			t.Fatalf("info returned error: %v", err)
		}

		want := "Tracker URL: http://tracker.example/announce\n" +
			"Length: 0\n" +
			fmt.Sprintf("Info Hash: %x\n", sha1.Sum([]byte(info))) +
			"Piece Length: 1024\n" +
			"Piece Hashes:\n"
		if got != want {
			t.Errorf("info printed:\n%s\nwant:\n%s", got, want)
		}
	})
}

func TestRunPeers(t *testing.T) {
	content := testContent(2500)
	info := testInfoDict(content, 1024)
	tracker := startTestTracker(t, "165.232.41.73:51556", "165.232.38.164:51493", "165.232.35.114:51476")
	path := writeTempFile(t, "test.torrent", testTorrentFile(tracker.AnnounceURL, info))

	got, err := runCommand(t, "peers", path)
	if err != nil {
		t.Fatalf("peers returned error: %v", err)
	}

	want := "165.232.41.73:51556\n" +
		"165.232.38.164:51493\n" +
		"165.232.35.114:51476\n"
	if got != want {
		t.Errorf("peers printed:\n%s\nwant:\n%s", got, want)
	}

	// The tracker must have been asked about this torrent in particular.
	query := (<-tracker.Requests).URL.Query()
	if got, want := query.Get("info_hash"), sha1.Sum([]byte(info)); got != string(want[:]) {
		t.Errorf("info_hash = %x, want %x", got, want)
	}
	if got := query.Get("left"); got != "2500" {
		t.Errorf("left = %q, want %q", got, "2500")
	}
	if got := query.Get("peer_id"); len(got) != 20 {
		t.Errorf("peer_id = %q, want 20 bytes", got)
	}
}

func TestRunPeersTrackerFailure(t *testing.T) {
	announceURL := startTestTrackerFunc(t, 200, "d14:failure reason9:not founde")
	path := writeTempFile(t, "test.torrent", testTorrentFile(announceURL, minimalInfo))

	got, err := runCommand(t, "peers", path)
	if err == nil || !strings.Contains(err.Error(), "request failed: not found") {
		t.Errorf("peers error = %v, want the tracker's failure reason", err)
	}
	if got != "" {
		t.Errorf("peers printed %q despite failing, want no output", got)
	}
}

func TestRunHandshake(t *testing.T) {
	info := testInfoDict(testContent(100), 32)
	infoHash := sha1.Sum([]byte(info))
	peer := startTestPeer(t, infoHash)
	path := writeTempFile(t, "test.torrent", testTorrentFile("http://tracker.example/announce", info))

	got, err := runCommand(t, "handshake", path, peer.Address)
	if err != nil {
		t.Fatalf("handshake returned error: %v", err)
	}

	if want := "Peer ID: 0102030405060708090a0b0c0d0e0f1011121314\n"; got != want {
		t.Errorf("handshake printed %q, want %q", got, want)
	}

	// The peer must have been greeted for this torrent in particular.
	theirs := <-peer.Handshakes
	if theirs.InfoHash != infoHash {
		t.Errorf("handshake was sent with info hash %x, want %x", theirs.InfoHash, infoHash)
	}
	if theirs.Reserved != [8]byte{} {
		t.Errorf("handshake was sent with reserved bytes %x, want all zeros", theirs.Reserved)
	}
}

func TestRunHandshakeWithWrongTorrent(t *testing.T) {
	peer := startTestPeer(t, [20]byte{0xaa})
	path := writeTempFile(t, "test.torrent", testTorrentFile("http://tracker.example/announce", minimalInfo))

	got, err := runCommand(t, "handshake", path, peer.Address)
	if err == nil || !strings.Contains(err.Error(), "different torrent") {
		t.Errorf("handshake error = %v, want one about the peer having a different torrent", err)
	}
	if got != "" {
		t.Errorf("handshake printed %q despite failing, want no output", got)
	}
}

func TestRunErrors(t *testing.T) {
	notATorrent := writeTempFile(t, "not-a.torrent", []byte("i52e"))

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
		{"decode with an unterminated dictionary", []string{"decode", "d3:foo3:bar"}, "dictionary is not terminated"},
		{"decode with a non-string dictionary key", []string{"decode", "di1ei2ee"}, "keys must be strings"},
		{"decode with a duplicate dictionary key", []string{"decode", "d1:ai1e1:ai2ee"}, `duplicate dictionary key "a"`},
		{"decode with a valueless dictionary key", []string{"decode", "d1:ae"}, `key "a" has no value`},
		{"info without a file", []string{"info"}, "usage: info"},
		{"info with too many files", []string{"info", "a.torrent", "b.torrent"}, "usage: info"},
		{"info with a missing file", []string{"info", "does-not-exist.torrent"}, "no such file"},
		{"info with a file that isn't a torrent", []string{"info", notATorrent}, "not a dictionary"},
		{"peers without a file", []string{"peers"}, "usage: peers"},
		{"peers with a missing file", []string{"peers", "does-not-exist.torrent"}, "no such file"},
		{"handshake without a peer", []string{"handshake", sampleTorrentPath}, "usage: handshake"},
		{"handshake without arguments", []string{"handshake"}, "usage: handshake"},
		{"handshake with a missing file", []string{"handshake", "does-not-exist.torrent", "127.0.0.1:6881"}, "no such file"},
		{"handshake with a malformed peer address", []string{"handshake", sampleTorrentPath, "nowhere"}, "missing port"},
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
