package main

import (
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

const (
	exampleInfoHashHex = "ad42ce8109f54c99613ce38f9b4d87e70f24a165"
	exampleTrackerURL  = "http://bittorrent-test-tracker.codecrafters.io/announce"
	exampleMagnetLink  = "magnet:?xt=urn:btih:ad42ce8109f54c99613ce38f9b4d87e70f24a165&dn=magnet1.gif" +
		"&tr=http%3A%2F%2Fbittorrent-test-tracker.codecrafters.io%2Fannounce"
)

func TestParseMagnetLink(t *testing.T) {
	infoHash, err := hex.DecodeString(exampleInfoHashHex)
	if err != nil {
		t.Fatal(err)
	}
	base32InfoHash := base32.StdEncoding.EncodeToString(infoHash)

	tests := []struct {
		name         string
		link         string
		wantName     string
		wantTrackers []string
	}{
		{"example", exampleMagnetLink, "magnet1.gif", []string{exampleTrackerURL}},
		{"info hash only", "magnet:?xt=urn:btih:" + exampleInfoHashHex, "", nil},
		{
			"parameters in another order",
			"magnet:?tr=http%3A%2F%2Ftracker.example%2Fannounce&dn=file.bin&xt=urn:btih:" + exampleInfoHashHex,
			"file.bin",
			[]string{"http://tracker.example/announce"},
		},
		{
			"several trackers",
			"magnet:?xt=urn:btih:" + exampleInfoHashHex + "&tr=http%3A%2F%2Fone.example%2Fannounce" +
				"&tr=udp%3A%2F%2Ftwo.example%3A6969&tr=https%3A%2F%2Fthree.example%2Fannounce",
			"",
			[]string{"http://one.example/announce", "udp://two.example:6969", "https://three.example/announce"},
		},
		{
			"tracker URL that isn't escaped",
			"magnet:?xt=urn:btih:" + exampleInfoHashHex + "&tr=http://tracker.example/announce",
			"",
			[]string{"http://tracker.example/announce"},
		},
		{
			"tracker URL with a query of its own",
			"magnet:?xt=urn:btih:" + exampleInfoHashHex + "&tr=http%3A%2F%2Ftracker.example%2Fannounce%3Fpasskey%3Dabc%26x%3D1",
			"",
			[]string{"http://tracker.example/announce?passkey=abc&x=1"},
		},
		{"info hash in upper case", "magnet:?xt=urn:btih:" + strings.ToUpper(exampleInfoHashHex), "", nil},
		{"info hash in base32", "magnet:?xt=urn:btih:" + base32InfoHash, "", nil},
		{"info hash in lower-case base32", "magnet:?xt=urn:btih:" + strings.ToLower(base32InfoHash), "", nil},
		{
			"name with escaped characters",
			"magnet:?xt=urn:btih:" + exampleInfoHashHex + "&dn=my+holiday%20photos%20%26%20videos.zip",
			"my holiday photos & videos.zip",
			nil,
		},
		{
			"parameters that aren't used",
			"magnet:?xl=1024&xt=urn:btih:" + exampleInfoHashHex + "&ws=http%3A%2F%2Fmirror.example%2Ffile&x.pe=10.0.0.1%3A6881",
			"",
			nil,
		},
		{
			"another kind of topic besides the info hash",
			"magnet:?xt=urn:btmh:1220caf1e1c30e81cb361b9ee167c4aa64228a7fa4fa9f6105232b28ad099f3a302e" +
				"&xt=urn:btih:" + exampleInfoHashHex,
			"",
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			link, err := parseMagnetLink(tt.link)
			if err != nil {
				t.Fatalf("parseMagnetLink(%q) returned error: %v", tt.link, err)
			}

			if got := hex.EncodeToString(link.InfoHash[:]); got != exampleInfoHashHex {
				t.Errorf("InfoHash = %s, want %s", got, exampleInfoHashHex)
			}
			if link.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", link.Name, tt.wantName)
			}
			if !reflect.DeepEqual(link.Trackers, tt.wantTrackers) {
				t.Errorf("Trackers = %q, want %q", link.Trackers, tt.wantTrackers)
			}
		})
	}
}

func TestParseMagnetLinkErrors(t *testing.T) {
	tests := []struct {
		name    string
		link    string
		wantErr string
	}{
		{"empty", "", `doesn't start with "magnet:"`},
		{"web address", "http://example.com/?xt=urn:btih:" + exampleInfoHashHex, `doesn't start with "magnet:"`},
		{"torrent file", "sample.torrent", `doesn't start with "magnet:"`},
		{"unparseable", "magnet:?xt=urn:btih:" + exampleInfoHashHex + "\x7f", "invalid magnet link"},
		{"no parameters", "magnet:", "it has no info hash"},
		{"empty query", "magnet:?", "it has no info hash"},
		{"no topic", "magnet:?dn=magnet1.gif&tr=http%3A%2F%2Ftracker.example%2Fannounce", "it has no info hash"},
		{"topic that isn't an info hash", "magnet:?xt=urn:sha1:YNCKHTQCWBTRNJIV4WNAE52SJUQCZO5C", "it has no info hash"},
		{"empty info hash", "magnet:?xt=urn:btih:", "it's 0 characters long, rather than 40"},
		{"info hash that's too short", "magnet:?xt=urn:btih:" + exampleInfoHashHex[:39], "it's 39 characters long, rather than 40"},
		{"info hash that's too long", "magnet:?xt=urn:btih:" + exampleInfoHashHex + "0", "it's 41 characters long, rather than 40"},
		{"info hash that isn't hexadecimal", "magnet:?xt=urn:btih:" + exampleInfoHashHex[:39] + "g", "malformed info hash"},
		{"info hash that isn't base32", "magnet:?xt=urn:btih:" + strings.Repeat("1", 32), "malformed info hash"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			link, err := parseMagnetLink(tt.link)
			if err == nil {
				t.Fatalf("parseMagnetLink(%q) = %+v, want an error containing %q", tt.link, link, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("parseMagnetLink(%q) error = %q, want it to contain %q", tt.link, err, tt.wantErr)
			}
		})
	}
}

func TestRunMagnetParse(t *testing.T) {
	tests := []struct {
		name string
		link string
		want string
	}{
		{
			"example",
			exampleMagnetLink,
			"Tracker URL: http://bittorrent-test-tracker.codecrafters.io/announce\n" +
				"Info Hash: ad42ce8109f54c99613ce38f9b4d87e70f24a165\n",
		},
		{
			"another torrent",
			"magnet:?xt=urn:btih:d69f91e6b2ae4c542468d1073a71d4ea13879a7f&tr=http%3A%2F%2F127.0.0.1%3A8080%2Fannounce&dn=sample.txt",
			"Tracker URL: http://127.0.0.1:8080/announce\n" +
				"Info Hash: d69f91e6b2ae4c542468d1073a71d4ea13879a7f\n",
		},
		{
			"several trackers",
			"magnet:?xt=urn:btih:" + exampleInfoHashHex + "&tr=http%3A%2F%2Fone.example%2Fannounce&tr=http%3A%2F%2Ftwo.example%2Fannounce",
			"Tracker URL: http://one.example/announce\n" +
				"Info Hash: ad42ce8109f54c99613ce38f9b4d87e70f24a165\n",
		},
		{
			"no tracker",
			"magnet:?xt=urn:btih:" + exampleInfoHashHex,
			"Tracker URL: \n" +
				"Info Hash: ad42ce8109f54c99613ce38f9b4d87e70f24a165\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runCommand(t, "magnet_parse", tt.link)
			if err != nil {
				t.Fatalf("magnet_parse returned error: %v", err)
			}
			if got != tt.want {
				t.Errorf("magnet_parse printed:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestRunMagnetParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no link", nil, "usage: magnet_parse"},
		{"too many arguments", []string{exampleMagnetLink, exampleMagnetLink}, "usage: magnet_parse"},
		{"not a magnet link", []string{sampleTorrentPath}, "invalid magnet link"},
		{"no info hash", []string{"magnet:?dn=magnet1.gif"}, "it has no info hash"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runCommand(t, append([]string{"magnet_parse"}, tt.args...)...)
			if err == nil {
				t.Fatalf("magnet_parse %q succeeded, want an error containing %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("magnet_parse %q error = %q, want it to contain %q", tt.args, err, tt.wantErr)
			}
			if got != "" {
				t.Errorf("magnet_parse %q printed %q despite failing, want no output", tt.args, got)
			}
		})
	}
}

// testMagnetLink returns a magnet link for the torrent with the given info
// hash and trackers.
func testMagnetLink(infoHash [20]byte, trackers ...string) string {
	link := fmt.Sprintf("magnet:?xt=urn:btih:%x&dn=test.bin", infoHash)
	for _, tracker := range trackers {
		link += "&tr=" + url.QueryEscape(tracker)
	}
	return link
}

func TestRunMagnetHandshake(t *testing.T) {
	infoHash := exampleHandshake.InfoHash
	peer := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved})
	tracker := startTestTracker(t, peer.Address)

	got, err := runCommand(t, "magnet_handshake", testMagnetLink(infoHash, tracker.AnnounceURL))
	if err != nil {
		t.Fatalf("magnet_handshake returned error: %v", err)
	}
	if want := "Peer ID: 0102030405060708090a0b0c0d0e0f1011121314\n"; got != want {
		t.Errorf("magnet_handshake printed %q, want %q", got, want)
	}

	// The handshake must have been for the torrent that the link identifies,
	// and must have announced support for extensions by setting the 20th
	// reserved bit from the right, and no other.
	ours := <-peer.Handshakes
	if ours.InfoHash != infoHash {
		t.Errorf("the handshake was sent with info hash %x, want %x", ours.InfoHash, infoHash)
	}
	if want := [8]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x00}; ours.Reserved != want {
		t.Errorf("the handshake was sent with reserved bytes % x, want % x", ours.Reserved, want)
	}

	// The peer must have been found by asking the link's tracker about the
	// torrent, as a client that still has data left to download.
	query := (<-tracker.Requests).URL.Query()
	if got := query.Get("info_hash"); got != string(infoHash[:]) {
		t.Errorf("the tracker was asked about info hash %x, want %x", got, infoHash)
	}
	if got := query.Get("peer_id"); got != string(ours.PeerID[:]) {
		t.Errorf("the tracker was given the peer ID %q, but the peer was given %q", got, ours.PeerID)
	}
	if got := query.Get("left"); got == "" || got == "0" {
		t.Errorf("the tracker was told that %q bytes are left, want a positive number", got)
	}
	if got := query.Get("compact"); got != "1" {
		t.Errorf("compact = %q, want %q", got, "1")
	}

	// As the peer supports extensions, it must have been sent an extension
	// handshake that lists the metadata extension, and nothing else.
	messages := messagesAfterHangup(t, peer)
	if len(messages) != 1 {
		t.Fatalf("the peer received %d messages, want just the extension handshake: %+v", len(messages), messages)
	}
	if got := messages[0].encode(); string(got) != ourExtensionHandshake {
		t.Errorf("the peer received the message %q, want %q", got, ourExtensionHandshake)
	}
}

// A peer that doesn't support extensions can still be shaken hands with, but
// it mustn't be sent an extension handshake: it wouldn't know what to make of
// one.
func TestRunMagnetHandshakeWithPeerWithoutExtensions(t *testing.T) {
	infoHash := exampleHandshake.InfoHash

	for name, reserved := range map[string][8]byte{
		"no reserved bits set":    {},
		"other reserved bits set": {0xff, 0xff, 0xff, 0xff, 0xff, 0xef, 0xff, 0xff},
	} {
		t.Run(name, func(t *testing.T) {
			peer := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: reserved})
			tracker := startTestTracker(t, peer.Address)

			got, err := runCommand(t, "magnet_handshake", testMagnetLink(infoHash, tracker.AnnounceURL))
			if err != nil {
				t.Fatalf("magnet_handshake returned error: %v", err)
			}
			if want := "Peer ID: 0102030405060708090a0b0c0d0e0f1011121314\n"; got != want {
				t.Errorf("magnet_handshake printed %q, want %q", got, want)
			}

			if messages := messagesAfterHangup(t, peer); len(messages) != 0 {
				t.Errorf("the peer received %d messages after the handshake, want none: %+v", len(messages), messages)
			}
		})
	}
}

func TestRunMagnetHandshakeSkipsUselessPeersAndTrackers(t *testing.T) {
	infoHash := exampleHandshake.InfoHash
	otherTorrent := startTestPeer(t, &testPeer{InfoHash: [20]byte{0xaa}})
	peer := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved})
	tracker := startTestTracker(t, stoppedServerAddress(t), otherTorrent.Address, peer.Address)
	deadTracker := "http://" + stoppedServerAddress(t) + "/announce"

	got, err := runCommand(t, "magnet_handshake", testMagnetLink(infoHash, deadTracker, tracker.AnnounceURL))
	if err != nil {
		t.Fatalf("magnet_handshake returned error: %v", err)
	}
	if want := "Peer ID: 0102030405060708090a0b0c0d0e0f1011121314\n"; got != want {
		t.Errorf("magnet_handshake printed %q, want %q", got, want)
	}
	if got := len(peer.Handshakes); got != 1 {
		t.Errorf("the peer was connected to %d times, want once", got)
	}
}

func TestRunMagnetHandshakeErrors(t *testing.T) {
	infoHash := exampleHandshake.InfoHash
	emptyTracker := startTestTracker(t)
	otherTorrent := startTestPeer(t, &testPeer{InfoHash: [20]byte{0xaa}})
	uselessTracker := startTestTracker(t, otherTorrent.Address)

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no link", nil, "usage: magnet_handshake"},
		{"too many arguments", []string{exampleMagnetLink, "extra"}, "usage: magnet_handshake"},
		{"not a magnet link", []string{sampleTorrentPath}, "invalid magnet link"},
		{"link without a tracker", []string{testMagnetLink(infoHash)}, "there's no tracker to ask for peers"},
		{"tracker that can't be reached", []string{testMagnetLink(infoHash, "http://"+stoppedServerAddress(t)+"/announce")}, "tracker: "},
		{"tracker without peers", []string{testMagnetLink(infoHash, emptyTracker.AnnounceURL)}, "doesn't know of any peers"},
		{"no peer with the torrent", []string{testMagnetLink(infoHash, uselessTracker.AnnounceURL)}, "peer answered for a different torrent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runCommand(t, append([]string{"magnet_handshake"}, tt.args...)...)
			if err == nil {
				t.Fatalf("magnet_handshake %q succeeded, want an error containing %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("magnet_handshake %q error = %q, want it to contain %q", tt.args, err, tt.wantErr)
			}
			if got != "" {
				t.Errorf("magnet_handshake %q printed %q despite failing, want no output", tt.args, got)
			}
		})
	}
}
