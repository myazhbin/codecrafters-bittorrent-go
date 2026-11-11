package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
	peer := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, MetadataID: 123})
	tracker := startTestTracker(t, peer.Address)

	got, err := runCommand(t, "magnet_handshake", testMagnetLink(infoHash, tracker.AnnounceURL))
	if err != nil {
		t.Fatalf("magnet_handshake returned error: %v", err)
	}
	want := "Peer ID: 0102030405060708090a0b0c0d0e0f1011121314\n" +
		"Peer Metadata Extension ID: 123\n"
	if got != want {
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

// The ID that a peer uses for the metadata extension is the peer's own
// choice, and is printed whatever it is and however the peer goes about
// sending it.
func TestRunMagnetHandshakePrintsMetadataExtensionID(t *testing.T) {
	infoHash := exampleHandshake.InfoHash

	tests := []struct {
		name string
		peer *testPeer
		want string
	}{
		{"lowest ID", &testPeer{MetadataID: 1}, "Peer Metadata Extension ID: 1\n"},
		{"highest ID", &testPeer{MetadataID: 255}, "Peer Metadata Extension ID: 255\n"},
		{"same ID as ours", &testPeer{MetadataID: ourMetadataID}, "Peer Metadata Extension ID: 1\n"},
		{
			"among other extensions",
			&testPeer{MetadataID: 42, OtherExtensions: map[string]interface{}{"ut_pex": 1, "lt_donthave": 7}},
			"Peer Metadata Extension ID: 42\n",
		},
		{"sent before the bitfield", &testPeer{MetadataID: 77, EagerExtensionHandshake: true}, "Peer Metadata Extension ID: 77\n"},
		{"sent amid other messages", &testPeer{MetadataID: 200, Chatty: true}, "Peer Metadata Extension ID: 200\n"},

		// A peer can support extensions without supporting this one.
		{"metadata extension not supported", &testPeer{OtherExtensions: map[string]interface{}{"ut_pex": 1}}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.peer.InfoHash, tt.peer.Reserved = infoHash, extensionsReserved
			peer := startTestPeer(t, tt.peer)
			tracker := startTestTracker(t, peer.Address)

			got, err := runCommand(t, "magnet_handshake", testMagnetLink(infoHash, tracker.AnnounceURL))
			if err != nil {
				t.Fatalf("magnet_handshake returned error: %v", err)
			}
			if want := "Peer ID: 0102030405060708090a0b0c0d0e0f1011121314\n" + tt.want; got != want {
				t.Errorf("magnet_handshake printed %q, want %q", got, want)
			}
		})
	}
}

func TestRunMagnetHandshakeWithoutExtensionHandshake(t *testing.T) {
	shortenTimeout(t, &peerMessageTimeout)

	infoHash := exampleHandshake.InfoHash
	peer := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, NoExtensionHandshake: true})
	tracker := startTestTracker(t, peer.Address)

	got, err := runCommand(t, "magnet_handshake", testMagnetLink(infoHash, tracker.AnnounceURL))
	if err == nil || !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("magnet_handshake error = %v, want a timeout", err)
	}
	if got != "" {
		t.Errorf("magnet_handshake printed %q despite failing, want no output", got)
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

// seedTestMagnet sets up everything that's needed to download a file with
// the given contents starting from a magnet link: a fake seeder that serves
// the torrent's metadata as well as its pieces, and a fake tracker that knows
// of the seeder. It returns the magnet link along with the seeder. Any other
// peer addresses given are listed by the tracker ahead of the seeder's.
func seedTestMagnet(t *testing.T, content []byte, pieceLength int, otherPeers ...string) (string, *testPeer) {
	t.Helper()

	info := testInfoDict(content, pieceLength)
	seeder := startTestPeer(t, &testPeer{
		InfoHash:    sha1.Sum([]byte(info)),
		Content:     content,
		PieceLength: pieceLength,
		Reserved:    extensionsReserved,
		MetadataID:  42,
		Metadata:    []byte(info),
	})
	tracker := startTestTracker(t, append(otherPeers, seeder.Address)...)

	return testMagnetLink(seeder.InfoHash, tracker.AnnounceURL), seeder
}

func TestRunMagnetInfo(t *testing.T) {
	content := testContent(2500)
	info := testInfoDict(content, 1024)
	infoHash := sha1.Sum([]byte(info))
	seeder := startTestPeer(t, &testPeer{
		InfoHash:   infoHash,
		Reserved:   extensionsReserved,
		MetadataID: 42,
		Metadata:   []byte(info),
	})
	tracker := startTestTracker(t, seeder.Address)

	got, err := runCommand(t, "magnet_info", testMagnetLink(infoHash, tracker.AnnounceURL))
	if err != nil {
		t.Fatalf("magnet_info returned error: %v", err)
	}

	want := "Tracker URL: " + tracker.AnnounceURL + "\n" +
		"Length: 2500\n" +
		fmt.Sprintf("Info Hash: %x\n", infoHash) +
		"Piece Length: 1024\n" +
		"Piece Hashes:\n" +
		fmt.Sprintf("%x\n", sha1.Sum(content[:1024])) +
		fmt.Sprintf("%x\n", sha1.Sum(content[1024:2048])) +
		fmt.Sprintf("%x\n", sha1.Sum(content[2048:]))
	if got != want {
		t.Errorf("magnet_info printed:\n%s\nwant:\n%s", got, want)
	}

	// It's the very same as what the info command prints for the torrent's
	// torrent file.
	torrentPath := writeTempFile(t, "test.torrent", testTorrentFile(tracker.AnnounceURL, info))
	if fromFile, err := runCommand(t, "info", torrentPath); err != nil || fromFile != got {
		t.Errorf("info printed:\n%s\n(error: %v), which differs from what magnet_info printed:\n%s", fromFile, err, got)
	}

	// After the extension handshake, the peer must have been asked for the
	// first piece of the metadata, in a message sent with the ID that the
	// peer itself picked for the metadata extension. The metadata fits in one
	// piece, so that's all there should be.
	messages := messagesAfterHangup(t, seeder)
	if len(messages) != 2 {
		t.Fatalf("the peer received %d messages, want the extension handshake and one metadata request: %+v", len(messages), messages)
	}
	if got := messages[0].encode(); string(got) != ourExtensionHandshake {
		t.Errorf("the peer first received the message %q, want %q", got, ourExtensionHandshake)
	}
	wantRequest := "\x00\x00\x00\x1b" + "\x14" + "\x2a" + "d8:msg_typei0e5:piecei0ee"
	if got := messages[1].encode(); string(got) != wantRequest {
		t.Errorf("the peer then received the message %q, want %q", got, wantRequest)
	}

	if ours := <-seeder.Handshakes; ours.Reserved != extensionsReserved || ours.InfoHash != infoHash {
		t.Errorf("the handshake was sent with reserved bytes % x and info hash %x", ours.Reserved, ours.InfoHash)
	}
}

// Metadata that's too big for a single 16 KiB piece has to be put together
// from several.
func TestRunMagnetInfoWithLargeMetadata(t *testing.T) {
	// A thousand piece hashes alone come to 20,000 bytes.
	content := testContent(100_000)
	info := testInfoDict(content, 100)
	if len(info) <= metadataPieceSize {
		t.Fatalf("the metadata is only %d bytes long, which fits in one piece", len(info))
	}
	link, seeder := seedTestMagnet(t, content, 100)

	got, err := runCommand(t, "magnet_info", link)
	if err != nil {
		t.Fatalf("magnet_info returned error: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 5+1000 {
		t.Fatalf("magnet_info printed %d lines, want %d", len(lines), 5+1000)
	}
	if want := fmt.Sprintf("Info Hash: %x", sha1.Sum([]byte(info))); lines[2] != want {
		t.Errorf("magnet_info printed %q, want %q", lines[2], want)
	}
	for _, index := range []int{0, 819, 820, 999} { // Hashes from both pieces of the metadata
		want := fmt.Sprintf("%x", sha1.Sum(content[index*100:(index+1)*100]))
		if lines[5+index] != want {
			t.Errorf("the hash printed for piece %d is %q, want %q", index, lines[5+index], want)
		}
	}

	var requests []string
	for _, m := range messagesAfterHangup(t, seeder)[1:] {
		requests = append(requests, string(m.Payload[1:]))
	}
	if want := []string{"d8:msg_typei0e5:piecei0ee", "d8:msg_typei0e5:piecei1ee"}; !reflect.DeepEqual(requests, want) {
		t.Errorf("the peer was sent the metadata requests %q, want %q", requests, want)
	}
}

// Peers that can't or won't provide the right metadata are passed over.
func TestRunMagnetInfoSkipsPeersWithoutMetadata(t *testing.T) {
	content := testContent(2500)
	info := testInfoDict(content, 1024)
	infoHash := sha1.Sum([]byte(info))

	badPeer := func(peer *testPeer) *testPeer {
		peer.InfoHash = infoHash
		return startTestPeer(t, peer)
	}
	badPeers := map[string]*testPeer{
		"doesn't support extensions":             badPeer(&testPeer{}),
		"doesn't support the metadata extension": badPeer(&testPeer{Reserved: extensionsReserved}),
		"doesn't have the metadata":              badPeer(&testPeer{Reserved: extensionsReserved, MetadataID: 3}),
		"serves the metadata of another torrent": badPeer(&testPeer{Reserved: extensionsReserved, MetadataID: 3, Metadata: []byte(minimalInfo)}),
		"serves corrupt metadata": badPeer(&testPeer{
			Reserved:   extensionsReserved,
			MetadataID: 3,
			Metadata:   []byte(info),
			MetadataTamper: func(header map[string]interface{}, data []byte) (map[string]interface{}, []byte) {
				data = bytes.Clone(data)
				data[len(data)/2] ^= 0x01
				return header, data
			},
		}),
	}
	addresses := []string{stoppedServerAddress(t)}
	for _, peer := range badPeers {
		addresses = append(addresses, peer.Address)
	}
	link, _ := seedTestMagnet(t, content, 1024, addresses...)

	got, err := runCommand(t, "magnet_info", link)
	if err != nil {
		t.Fatalf("magnet_info returned error: %v", err)
	}
	if want := fmt.Sprintf("Info Hash: %x\n", infoHash); !strings.Contains(got, want) {
		t.Errorf("magnet_info printed:\n%s\nwant it to include %q", got, want)
	}
	if want := "Length: 2500\n"; !strings.Contains(got, want) {
		t.Errorf("magnet_info printed:\n%s\nwant it to include %q", got, want)
	}

	for name, peer := range badPeers {
		if len(peer.Handshakes) != 1 {
			t.Errorf("the peer that %s was connected to %d times, want once", name, len(peer.Handshakes))
		}
	}
}

func TestRunMagnetInfoErrors(t *testing.T) {
	infoHash := sha1.Sum([]byte(minimalInfo))
	noExtensions := startTestPeer(t, &testPeer{InfoHash: infoHash})
	noMetadataSupport := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved})
	noMetadata := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, MetadataID: 3})
	wrongMetadata := startTestPeer(t, &testPeer{
		InfoHash:   infoHash,
		Reserved:   extensionsReserved,
		MetadataID: 3,
		Metadata:   []byte(testInfoDict(testContent(10), 5)),
	})

	// A torrent whose metadata is what it should be, but describes something
	// that can't be downloaded.
	const multiFileInfo = "d5:filesld6:lengthi1e4:pathl1:aeee4:name1:a12:piece lengthi1e6:pieces20:" + testPieceHash + "e"
	multiFileHash := sha1.Sum([]byte(multiFileInfo))
	multiFile := startTestPeer(t, &testPeer{
		InfoHash:   multiFileHash,
		Reserved:   extensionsReserved,
		MetadataID: 3,
		Metadata:   []byte(multiFileInfo),
	})

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no link", nil, "usage: magnet_info"},
		{"too many arguments", []string{exampleMagnetLink, "extra"}, "usage: magnet_info"},
		{"not a magnet link", []string{sampleTorrentPath}, "invalid magnet link"},
		{"link without a tracker", []string{testMagnetLink(infoHash)}, "there's no tracker to ask for peers"},
		{"tracker without peers", []string{testMagnetLink(infoHash, startTestTracker(t).AnnounceURL)}, "doesn't know of any peers"},
		{
			"peer without extension support",
			[]string{testMagnetLink(infoHash, startTestTracker(t, noExtensions.Address).AnnounceURL)},
			"peer " + noExtensions.Address + ": peer doesn't support protocol extensions",
		},
		{
			"peer without metadata support",
			[]string{testMagnetLink(infoHash, startTestTracker(t, noMetadataSupport.Address).AnnounceURL)},
			"peer " + noMetadataSupport.Address + ": peer doesn't support the metadata extension",
		},
		{
			"peer without the metadata",
			[]string{testMagnetLink(infoHash, startTestTracker(t, noMetadata.Address).AnnounceURL)},
			"peer " + noMetadata.Address + ": peer turned down the request for piece 0 of the metadata",
		},
		{
			"peer with the wrong metadata",
			[]string{testMagnetLink(infoHash, startTestTracker(t, wrongMetadata.Address).AnnounceURL)},
			"peer " + wrongMetadata.Address + ": metadata from peer doesn't match the torrent's info hash",
		},
		{
			"multi-file torrent",
			[]string{testMagnetLink(multiFileHash, startTestTracker(t, multiFile.Address).AnnounceURL)},
			"invalid metadata: multi-file torrents are not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runCommand(t, append([]string{"magnet_info"}, tt.args...)...)
			if err == nil {
				t.Fatalf("magnet_info %q succeeded, want an error containing %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("magnet_info %q error = %q, want it to contain %q", tt.args, err, tt.wantErr)
			}
			if got != "" {
				t.Errorf("magnet_info %q printed %q despite failing, want no output", tt.args, got)
			}
		})
	}
}

func TestRunMagnetDownloadPiece(t *testing.T) {
	content := testContent(100_000)
	wantPieces := [][]byte{content[:40_000], content[40_000:80_000], content[80_000:]}

	for index, want := range wantPieces {
		link, seeder := seedTestMagnet(t, content, 40_000)
		outputPath := filepath.Join(t.TempDir(), "piece")

		stdout, err := runCommand(t, "magnet_download_piece", "-o", outputPath, link, fmt.Sprint(index))
		if err != nil {
			t.Fatalf("magnet_download_piece %d returned error: %v", index, err)
		}

		checkFile(t, outputPath, want)
		if want := fmt.Sprintf("Piece %d downloaded to %s.\n", index, outputPath); stdout != want {
			t.Errorf("magnet_download_piece %d printed %q, want %q", index, stdout, want)
		}

		// Everything happens over a single connection, in this order: the
		// extension handshake, the request for the metadata, and then the
		// download of the piece's blocks.
		if got := len(seeder.Handshakes); got != 1 {
			t.Errorf("magnet_download_piece %d connected to the peer %d times, want once", index, got)
		}
		var got []string
		for _, m := range messagesAfterHangup(t, seeder) {
			switch {
			case m.ID == msgExtended && m.Payload[0] == extensionHandshakeID:
				got = append(got, "extension handshake")
			case m.ID == msgExtended:
				got = append(got, "metadata request")
			case m.ID == msgInterested:
				got = append(got, "interested")
			case m.ID == msgRequest:
				got = append(got, "request")
			default:
				got = append(got, fmt.Sprintf("message %d", m.ID))
			}
		}
		wantMessages := []string{"extension handshake", "metadata request", "interested", "request", "request", "request"}
		if index == 2 {
			wantMessages = wantMessages[:5] // The last piece only has two blocks
		}
		if !reflect.DeepEqual(got, wantMessages) {
			t.Errorf("magnet_download_piece %d sent the peer %q, want %q", index, got, wantMessages)
		}
	}
}

// The peer that the metadata comes from needn't be the one that the piece
// ends up being downloaded from.
func TestRunMagnetDownloadPieceTriesOtherPeers(t *testing.T) {
	setDuration(t, &peerMessageTimeout, time.Second)
	setDuration(t, &peerRetryDelay, 10*time.Millisecond)

	content := testContent(100_000)
	info := testInfoDict(content, 40_000)
	infoHash := sha1.Sum([]byte(info))

	// These peers are all good for the metadata, but not for the piece.
	badPeer := func(peer *testPeer) string {
		peer.InfoHash, peer.Content, peer.PieceLength = infoHash, content, 40_000
		peer.Reserved, peer.MetadataID, peer.Metadata = extensionsReserved, 7, []byte(info)
		return startTestPeer(t, peer).Address
	}
	link, _ := seedTestMagnet(t, content, 40_000,
		stoppedServerAddress(t),
		startTestPeer(t, &testPeer{InfoHash: infoHash}).Address, // No extensions, so no metadata
		badPeer(&testPeer{Tamper: zeroingTamper}),
		badPeer(&testPeer{NeverUnchoke: true}),
		badPeer(&testPeer{HangUpAfterBlocks: 1}),
	)
	outputPath := filepath.Join(t.TempDir(), "piece")

	if _, err := runCommand(t, "magnet_download_piece", "-o", outputPath, link, "1"); err != nil {
		t.Fatalf("magnet_download_piece returned error: %v", err)
	}
	checkFile(t, outputPath, content[40_000:80_000])
}

func TestRunMagnetDownloadPieceWithLargeMetadata(t *testing.T) {
	content := testContent(100_000)
	link, _ := seedTestMagnet(t, content, 100) // A thousand pieces, whose hashes take up 20,000 bytes
	outputPath := filepath.Join(t.TempDir(), "piece")

	if _, err := runCommand(t, "magnet_download_piece", "-o", outputPath, link, "999"); err != nil {
		t.Fatalf("magnet_download_piece returned error: %v", err)
	}
	checkFile(t, outputPath, content[99_900:])
}

func TestRunMagnetDownloadPieceErrors(t *testing.T) {
	setDuration(t, &peerRetryDelay, 10*time.Millisecond)

	content := testContent(1000)
	info := testInfoDict(content, 400) // 3 pieces
	infoHash := sha1.Sum([]byte(info))
	link, seeder := seedTestMagnet(t, content, 400)
	outputPath := filepath.Join(t.TempDir(), "piece")

	corrupter := startTestPeer(t, &testPeer{
		InfoHash:    infoHash,
		Content:     content,
		PieceLength: 400,
		Reserved:    extensionsReserved,
		MetadataID:  7,
		Metadata:    []byte(info),
		Tamper:      zeroingTamper,
	})
	noMetadata := startTestPeer(t, &testPeer{InfoHash: infoHash, Content: content, PieceLength: 400})

	const usage = "usage: magnet_download_piece -o <output file> <magnet link> <piece index>"
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no arguments", nil, usage},
		{"no output file", []string{link, "0"}, usage},
		{"no piece index", []string{"-o", outputPath, link}, usage},
		{"too many arguments", []string{"-o", outputPath, link, "0", "1"}, usage},
		{"not a magnet link", []string{"-o", outputPath, sampleTorrentPath, "0"}, "invalid magnet link"},
		{"piece index that isn't a number", []string{"-o", outputPath, link, "first"}, `invalid piece index "first"`},
		{"negative piece index", []string{"-o", outputPath, link, "-1"}, `invalid piece index "-1"`},
		{"piece index past the last piece", []string{"-o", outputPath, link, "3"}, "invalid piece index 3: the torrent has 3 pieces"},
		{"link without a tracker", []string{"-o", outputPath, testMagnetLink(infoHash), "0"}, "there's no tracker to ask for peers"},
		{
			"no peer with the metadata",
			[]string{"-o", outputPath, testMagnetLink(infoHash, startTestTracker(t, noMetadata.Address).AnnounceURL), "0"},
			"peer doesn't support protocol extensions",
		},
		{
			"no peer that delivers the piece",
			[]string{"-o", outputPath, testMagnetLink(infoHash, startTestTracker(t, corrupter.Address).AnnounceURL), "0"},
			"peer " + corrupter.Address + ": piece 0 from peer failed its hash check",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, err := runCommand(t, append([]string{"magnet_download_piece"}, tt.args...)...)
			if err == nil {
				t.Fatalf("magnet_download_piece %q succeeded, want an error containing %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("magnet_download_piece %q error = %q, want it to contain %q", tt.args, err, tt.wantErr)
			}
			if stdout != "" {
				t.Errorf("magnet_download_piece %q printed %q despite failing, want no output", tt.args, stdout)
			}
			checkNoFile(t, outputPath)
		})
	}

	// A piece index that can't be right for any torrent is turned down
	// before any peer is bothered with it. The one that's merely out of range
	// for this torrent is the only one that takes asking a peer to tell.
	if got := len(seeder.Handshakes); got != 1 {
		t.Errorf("the peer was connected to %d times over all of the failed commands, want once", got)
	}
}
