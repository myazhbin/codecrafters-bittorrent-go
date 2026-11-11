package main

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// seedTestTorrent sets up everything that's needed to download a file with
// the given contents: a fake seeder, a fake tracker that knows of the seeder,
// and a torrent file, whose path it returns. Any other peer addresses given
// are listed by the tracker ahead of the seeder's.
func seedTestTorrent(t *testing.T, content []byte, pieceLength int, otherPeers ...string) string {
	t.Helper()

	info := testInfoDict(content, pieceLength)
	seeder := startTestPeer(t, &testPeer{
		InfoHash:    sha1.Sum([]byte(info)),
		Content:     content,
		PieceLength: pieceLength,
	})
	tracker := startTestTracker(t, append(otherPeers, seeder.Address)...)

	return writeTempFile(t, "test.torrent", testTorrentFile(tracker.AnnounceURL, info))
}

// zeroingTamper makes a fake peer serve zeros in place of the real data.
func zeroingTamper(index, begin uint32, data []byte) (uint32, uint32, []byte) {
	return index, begin, make([]byte, len(data))
}

// checkFile fails the test if the file at path doesn't have the given
// contents.
func checkFile(t *testing.T, path string, want []byte) {
	t.Helper()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s holds the wrong data (%d bytes, want %d)", filepath.Base(path), len(got), len(want))
	}
}

// checkNoFile fails the test if there's a file at path.
func checkNoFile(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists, though it shouldn't (stat error: %v)", filepath.Base(path), err)
	}
}

func TestRunDownloadPiece(t *testing.T) {
	content := testContent(100_000)
	torrentPath := seedTestTorrent(t, content, 40_000)

	wantPieces := [][]byte{content[:40_000], content[40_000:80_000], content[80_000:]}
	for index, want := range wantPieces {
		outputPath := filepath.Join(t.TempDir(), "piece")

		stdout, err := runCommand(t, "download_piece", "-o", outputPath, torrentPath, fmt.Sprint(index))
		if err != nil {
			t.Fatalf("download_piece %d returned error: %v", index, err)
		}

		checkFile(t, outputPath, want)
		if want := fmt.Sprintf("Piece %d downloaded to %s.\n", index, outputPath); stdout != want {
			t.Errorf("download_piece %d printed %q, want %q", index, stdout, want)
		}
	}
}

// The piece replaces whatever the output file held before.
func TestRunDownloadPieceOverwritesOutputFile(t *testing.T) {
	content := testContent(1000)
	torrentPath := seedTestTorrent(t, content, 400)
	outputPath := writeTempFile(t, "piece", bytes.Repeat([]byte("old contents "), 1000))

	if _, err := runCommand(t, "download_piece", "-o", outputPath, torrentPath, "2"); err != nil {
		t.Fatalf("download_piece returned error: %v", err)
	}
	checkFile(t, outputPath, content[800:])
}

// Peers that can't be reached or don't deliver must be passed over in favor
// of one that does.
func TestRunDownloadPieceTriesOtherPeers(t *testing.T) {
	setDuration(t, &peerMessageTimeout, time.Second)
	setDuration(t, &peerRetryDelay, 10*time.Millisecond)

	content := testContent(100_000)
	infoHash := sha1.Sum([]byte(testInfoDict(content, 40_000)))

	unreachable := stoppedServerAddress(t)
	otherTorrent := startTestPeer(t, &testPeer{InfoHash: [20]byte{0xaa}})
	choker := startTestPeer(t, &testPeer{InfoHash: infoHash, Content: content, PieceLength: 40_000, NeverUnchoke: true})
	corrupter := startTestPeer(t, &testPeer{InfoHash: infoHash, Content: content, PieceLength: 40_000, Tamper: zeroingTamper})
	torrentPath := seedTestTorrent(t, content, 40_000,
		unreachable, otherTorrent.Address, choker.Address, corrupter.Address)

	outputPath := filepath.Join(t.TempDir(), "piece")
	if _, err := runCommand(t, "download_piece", "-o", outputPath, torrentPath, "1"); err != nil {
		t.Fatalf("download_piece returned error: %v", err)
	}
	checkFile(t, outputPath, content[40_000:80_000])
}

func TestRunDownloadPieceWithoutUsablePeers(t *testing.T) {
	setDuration(t, &peerRetryDelay, 10*time.Millisecond)

	content := testContent(1000)
	info := testInfoDict(content, 400)
	infoHash := sha1.Sum([]byte(info))

	t.Run("tracker has no peers", func(t *testing.T) {
		tracker := startTestTracker(t)
		torrentPath := writeTempFile(t, "test.torrent", testTorrentFile(tracker.AnnounceURL, info))
		outputPath := filepath.Join(t.TempDir(), "piece")

		_, err := runCommand(t, "download_piece", "-o", outputPath, torrentPath, "0")
		if err == nil || !strings.Contains(err.Error(), "doesn't know of any peers") {
			t.Errorf("download_piece error = %v, want one saying that there are no peers", err)
		}
		checkNoFile(t, outputPath)
	})

	t.Run("no peer delivers", func(t *testing.T) {
		unreachable := stoppedServerAddress(t)
		corrupter := startTestPeer(t, &testPeer{InfoHash: infoHash, Content: content, PieceLength: 400, Tamper: zeroingTamper})
		tracker := startTestTracker(t, unreachable, corrupter.Address)
		torrentPath := writeTempFile(t, "test.torrent", testTorrentFile(tracker.AnnounceURL, info))
		outputPath := filepath.Join(t.TempDir(), "piece")

		_, err := runCommand(t, "download_piece", "-o", outputPath, torrentPath, "1")
		if err == nil {
			t.Fatal("download_piece succeeded, want an error")
		}
		// The error must say what went wrong with each of the peers.
		for _, want := range []string{
			"ran out of peers with 1 piece(s) left to download",
			"peer " + unreachable + ": ",
			"connection refused",
			"peer " + corrupter.Address + ": piece 1 from peer failed its hash check",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("download_piece error = %q, want it to contain %q", err, want)
			}
		}
		checkNoFile(t, outputPath)

		// A peer gets several chances before it's given up on.
		if got := len(corrupter.Handshakes); got != maxPeerFailures {
			t.Errorf("the corrupting peer was connected to %d times, want %d", got, maxPeerFailures)
		}
	})

	t.Run("tracker is down", func(t *testing.T) {
		torrentPath := writeTempFile(t, "test.torrent",
			testTorrentFile("http://"+stoppedServerAddress(t)+"/announce", info))

		_, err := runCommand(t, "download_piece", "-o", filepath.Join(t.TempDir(), "piece"), torrentPath, "0")
		if err == nil || !strings.Contains(err.Error(), "tracker: ") {
			t.Errorf("download_piece error = %v, want one about the tracker", err)
		}
	})
}

func TestRunDownloadPieceUsageErrors(t *testing.T) {
	content := testContent(1000)
	torrentPath := writeTempFile(t, "test.torrent",
		testTorrentFile("http://tracker.example/announce", testInfoDict(content, 400))) // 3 pieces
	outputPath := filepath.Join(t.TempDir(), "piece")

	const usage = "usage: download_piece -o <output file> <torrent file> <piece index>"
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no arguments", nil, usage},
		{"no output file", []string{torrentPath, "0"}, usage},
		{"empty output file", []string{"-o", "", torrentPath, "0"}, usage},
		{"no piece index", []string{"-o", outputPath, torrentPath}, usage},
		{"no torrent file", []string{"-o", outputPath}, usage},
		{"too many arguments", []string{"-o", outputPath, torrentPath, "0", "1"}, usage},
		{"unknown flag", []string{"-x", "-o", outputPath, torrentPath, "0"}, usage},
		{"flag without a value", []string{torrentPath, "0", "-o"}, usage},
		{"missing torrent file", []string{"-o", outputPath, "does-not-exist.torrent", "0"}, "no such file"},
		{"piece index that isn't a number", []string{"-o", outputPath, torrentPath, "first"}, `invalid piece index "first"`},
		{"fractional piece index", []string{"-o", outputPath, torrentPath, "1.5"}, `invalid piece index "1.5"`},
		{"negative piece index", []string{"-o", outputPath, torrentPath, "-1"}, `invalid piece index "-1"`},
		{"piece index past the last piece", []string{"-o", outputPath, torrentPath, "3"}, `invalid piece index "3": the torrent has 3 pieces`},
		{"huge piece index", []string{"-o", outputPath, torrentPath, "99999999999999999999"}, "invalid piece index"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, err := runCommand(t, append([]string{"download_piece"}, tt.args...)...)
			if err == nil {
				t.Fatalf("download_piece %q succeeded, want an error containing %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("download_piece %q error = %q, want it to contain %q", tt.args, err, tt.wantErr)
			}
			if stdout != "" {
				t.Errorf("download_piece %q printed %q despite failing, want no output", tt.args, stdout)
			}
			checkNoFile(t, outputPath)
		})
	}
}

func TestRunDownload(t *testing.T) {
	tests := []struct {
		name        string
		length      int
		pieceLength int
	}{
		{"several pieces with a short last one", 600_000, 65536},
		{"file that's a multiple of the piece length", 4 * 32768, 32768},
		{"single piece", 50_000, 262_144},
		{"single byte", 1, 16384},
		{"many tiny pieces", 20_000, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := testContent(tt.length)
			torrentPath := seedTestTorrent(t, content, tt.pieceLength)
			outputPath := filepath.Join(t.TempDir(), "downloaded.bin")

			stdout, err := runCommand(t, "download", "-o", outputPath, torrentPath)
			if err != nil {
				t.Fatalf("download returned error: %v", err)
			}

			checkFile(t, outputPath, content)
			checkNoFile(t, outputPath+".part")
			if want := fmt.Sprintf("Downloaded %s to %s.\n", torrentPath, outputPath); stdout != want {
				t.Errorf("download printed %q, want %q", stdout, want)
			}
		})
	}
}

// A file with nothing in it has no pieces to download, so no tracker or peer
// is needed to produce it.
func TestRunDownloadEmptyFile(t *testing.T) {
	torrentPath := writeTempFile(t, "empty.torrent",
		testTorrentFile("http://"+stoppedServerAddress(t)+"/announce", testInfoDict(nil, 16384)))
	outputPath := filepath.Join(t.TempDir(), "empty.bin")

	if _, err := runCommand(t, "download", "-o", outputPath, torrentPath); err != nil {
		t.Fatalf("download returned error: %v", err)
	}
	checkFile(t, outputPath, nil)
	checkNoFile(t, outputPath+".part")
}

// The download replaces whatever the output file held before.
func TestRunDownloadOverwritesOutputFile(t *testing.T) {
	content := testContent(1000)
	torrentPath := seedTestTorrent(t, content, 400)
	outputPath := writeTempFile(t, "downloaded.bin", bytes.Repeat([]byte("old contents "), 1000))

	if _, err := runCommand(t, "download", "-o", outputPath, torrentPath); err != nil {
		t.Fatalf("download returned error: %v", err)
	}
	checkFile(t, outputPath, content)
}

// Downloads that start from a torrent file don't need protocol extensions, and
// don't announce support for any.
func TestRunDownloadDoesNotAnnounceExtensions(t *testing.T) {
	content := testContent(1000)
	info := testInfoDict(content, 400)
	seeder := startTestPeer(t, &testPeer{
		InfoHash:    sha1.Sum([]byte(info)),
		Content:     content,
		PieceLength: 400,
		Reserved:    extensionsReserved,
	})
	tracker := startTestTracker(t, seeder.Address)
	torrentPath := writeTempFile(t, "test.torrent", testTorrentFile(tracker.AnnounceURL, info))

	if _, err := runCommand(t, "download", "-o", filepath.Join(t.TempDir(), "downloaded.bin"), torrentPath); err != nil {
		t.Fatalf("download returned error: %v", err)
	}
	if got := (<-seeder.Handshakes).Reserved; got != [8]byte{} {
		t.Errorf("the handshake was sent with reserved bytes % x, want all zeros", got)
	}
}

// With several peers to hand, pieces are downloaded from all of them.
func TestRunDownloadUsesEveryPeer(t *testing.T) {
	content := testContent(40 * 16384)
	info := testInfoDict(content, 16384)
	infoHash := sha1.Sum([]byte(info))

	var seeders []*testPeer
	var addresses []string
	for range 4 {
		seeder := startTestPeer(t, &testPeer{InfoHash: infoHash, Content: content, PieceLength: 16384})
		seeders = append(seeders, seeder)
		addresses = append(addresses, seeder.Address)
	}
	tracker := startTestTracker(t, addresses...)
	torrentPath := writeTempFile(t, "test.torrent", testTorrentFile(tracker.AnnounceURL, info))
	outputPath := filepath.Join(t.TempDir(), "downloaded.bin")

	if _, err := runCommand(t, "download", "-o", outputPath, torrentPath); err != nil {
		t.Fatalf("download returned error: %v", err)
	}
	checkFile(t, outputPath, content)

	// Between them, the peers must have been asked for each piece exactly
	// once, and each of them must have had a share of the work.
	var requested []int
	for i, seeder := range seeders {
		requests := drainRequests(seeder)
		if len(requests) == 0 {
			t.Errorf("peer %d wasn't asked for any pieces", i)
		}
		if got := len(seeder.Handshakes); got != 1 {
			t.Errorf("peer %d was connected to %d times, want once", i, got)
		}
		for _, request := range requests {
			requested = append(requested, int(request.Index))
		}
	}
	sort.Ints(requested)
	for index := range 40 {
		if index >= len(requested) || requested[index] != index {
			t.Fatalf("the pieces asked for were %v, want each of 0 to 39 once", requested)
		}
	}

	// The tracker is only asked for peers once.
	if got := len(tracker.Requests); got != 1 {
		t.Errorf("the tracker got %d requests, want 1", got)
	}
}

// Whatever a peer does wrong, the pieces that it was meant to deliver must
// end up being downloaded from another peer.
func TestRunDownloadSurvivesBadPeers(t *testing.T) {
	setDuration(t, &peerMessageTimeout, time.Second)
	setDuration(t, &peerRetryDelay, 10*time.Millisecond)

	content := testContent(30 * 32768)
	info := testInfoDict(content, 32768)
	infoHash := sha1.Sum([]byte(info))

	badPeer := func(peer *testPeer) *testPeer {
		peer.InfoHash, peer.Content, peer.PieceLength = infoHash, content, 32768
		return startTestPeer(t, peer)
	}
	badPeers := map[string]*testPeer{
		"serves another torrent":    startTestPeer(t, &testPeer{InfoHash: [20]byte{0xaa}}),
		"never unchokes":            badPeer(&testPeer{NeverUnchoke: true}),
		"serves corrupt data":       badPeer(&testPeer{Tamper: zeroingTamper}),
		"hangs up mid-piece":        badPeer(&testPeer{HangUpAfterBlocks: 1}),
		"has no pieces":             badPeer(&testPeer{Bitfield: make([]byte, 4)}),
		"sends blocks nobody asked": badPeer(&testPeer{Tamper: func(index, begin uint32, data []byte) (uint32, uint32, []byte) { return index + 1, begin, data }}),
	}

	addresses := []string{stoppedServerAddress(t)}
	for _, peer := range badPeers {
		addresses = append(addresses, peer.Address)
	}
	torrentPath := seedTestTorrent(t, content, 32768, addresses...)
	outputPath := filepath.Join(t.TempDir(), "downloaded.bin")

	if _, err := runCommand(t, "download", "-o", outputPath, torrentPath); err != nil {
		t.Fatalf("download returned error: %v", err)
	}
	checkFile(t, outputPath, content)
	checkNoFile(t, outputPath+".part")

	// There were pieces enough for every peer to be given one to start with,
	// so each of the bad peers must have had its chance to cause trouble.
	for name, peer := range badPeers {
		if len(peer.Handshakes) == 0 {
			t.Errorf("the peer that %s was never connected to", name)
		}
	}
}

// A peer that keeps dropping the connection is still worth going back to for
// as long as it delivers something each time.
func TestRunDownloadReconnectsToFlakyPeer(t *testing.T) {
	setDuration(t, &peerRetryDelay, 5*time.Millisecond)

	// Pieces are two blocks each, and the peer hangs up after every third
	// block: each connection yields one piece, and fails on the next.
	content := testContent(8 * 32768)
	info := testInfoDict(content, 32768)
	flaky := startTestPeer(t, &testPeer{
		InfoHash:          sha1.Sum([]byte(info)),
		Content:           content,
		PieceLength:       32768,
		HangUpAfterBlocks: 3,
	})
	tracker := startTestTracker(t, flaky.Address)
	torrentPath := writeTempFile(t, "test.torrent", testTorrentFile(tracker.AnnounceURL, info))
	outputPath := filepath.Join(t.TempDir(), "downloaded.bin")

	if _, err := runCommand(t, "download", "-o", outputPath, torrentPath); err != nil {
		t.Fatalf("download returned error: %v", err)
	}
	checkFile(t, outputPath, content)

	if got := len(flaky.Handshakes); got != 8 {
		t.Errorf("the peer was connected to %d times, want 8 (once per piece)", got)
	}
}

func TestRunDownloadFailure(t *testing.T) {
	setDuration(t, &peerRetryDelay, 5*time.Millisecond)

	content := testContent(10 * 16384)
	info := testInfoDict(content, 16384)
	infoHash := sha1.Sum([]byte(info))

	// The first peer delivers three pieces before it starts failing for
	// good. The second one never delivers anything.
	var delivered atomic.Int32
	unreliable := startTestPeer(t, &testPeer{
		InfoHash:    infoHash,
		Content:     content,
		PieceLength: 16384,
		Tamper: func(index, begin uint32, data []byte) (uint32, uint32, []byte) {
			if delivered.Add(1) > 3 {
				data = make([]byte, len(data))
			}
			return index, begin, data
		},
	})
	corrupter := startTestPeer(t, &testPeer{InfoHash: infoHash, Content: content, PieceLength: 16384, Tamper: zeroingTamper})
	tracker := startTestTracker(t, unreliable.Address, corrupter.Address)
	torrentPath := writeTempFile(t, "test.torrent", testTorrentFile(tracker.AnnounceURL, info))

	// The file that's already at the output path must survive the failure.
	oldContents := []byte("a previous download")
	outputPath := writeTempFile(t, "downloaded.bin", oldContents)

	stdout, err := runCommand(t, "download", "-o", outputPath, torrentPath)
	if err == nil {
		t.Fatal("download succeeded, want an error")
	}
	for _, want := range []string{
		"ran out of peers with 7 piece(s) left to download",
		"peer " + unreliable.Address + ": piece ",
		"peer " + corrupter.Address + ": piece ",
		"failed its hash check",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("download error = %q, want it to contain %q", err, want)
		}
	}
	if stdout != "" {
		t.Errorf("download printed %q despite failing, want no output", stdout)
	}

	checkFile(t, outputPath, oldContents)
	checkNoFile(t, outputPath+".part")
}

func TestRunDownloadUsageErrors(t *testing.T) {
	torrentPath := writeTempFile(t, "test.torrent",
		testTorrentFile("http://tracker.example/announce", testInfoDict(testContent(1000), 400)))
	outputPath := filepath.Join(t.TempDir(), "downloaded.bin")

	const usage = "usage: download -o <output file> <torrent file>"
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no arguments", nil, usage},
		{"no output file", []string{torrentPath}, usage},
		{"empty output file", []string{"-o", "", torrentPath}, usage},
		{"no torrent file", []string{"-o", outputPath}, usage},
		{"too many arguments", []string{"-o", outputPath, torrentPath, "0"}, usage},
		{"unknown flag", []string{"-o", outputPath, "-v", torrentPath}, usage},
		{"missing torrent file", []string{"-o", outputPath, "does-not-exist.torrent"}, "no such file"},
		{
			"output file in a directory that doesn't exist",
			[]string{"-o", filepath.Join(t.TempDir(), "missing", "downloaded.bin"), torrentPath},
			"no such file or directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, err := runCommand(t, append([]string{"download"}, tt.args...)...)
			if err == nil {
				t.Fatalf("download %q succeeded, want an error containing %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("download %q error = %q, want it to contain %q", tt.args, err, tt.wantErr)
			}
			if stdout != "" {
				t.Errorf("download %q printed %q despite failing, want no output", tt.args, stdout)
			}
			checkNoFile(t, outputPath)
			checkNoFile(t, outputPath+".part")
		})
	}
}

// Each piece is handed over exactly once, and never while another is still
// being dealt with.
func TestDownloadPiecesSavesPiecesOneAtATime(t *testing.T) {
	content := testContent(50 * 1000)
	info := testInfoDict(content, 1000)
	infoHash := sha1.Sum([]byte(info))

	var addresses []string
	for range 5 {
		addresses = append(addresses, startTestPeer(t, &testPeer{InfoHash: infoHash, Content: content, PieceLength: 1000}).Address)
	}
	tracker := startTestTracker(t, addresses...)
	torrent, err := parseTorrent(testTorrentFile(tracker.AnnounceURL, info))
	if err != nil {
		t.Fatal(err)
	}

	// Only the odd-numbered pieces are asked for.
	var indexes []int
	for index := 1; index < 50; index += 2 {
		indexes = append(indexes, index)
	}

	var saving atomic.Int32
	saved := map[int]int{}
	err = newSwarm(torrent).downloadPieces(torrent, indexes, func(index int, piece []byte) error {
		if saving.Add(1) != 1 {
			t.Error("save was called while another call was in progress")
		}
		defer saving.Add(-1)
		time.Sleep(time.Millisecond) // Gives overlapping calls a chance to show up

		if !bytes.Equal(piece, content[index*1000:(index+1)*1000]) {
			t.Errorf("piece %d was saved with the wrong data", index)
		}
		saved[index]++
		return nil
	})
	if err != nil {
		t.Fatalf("downloadPieces returned error: %v", err)
	}

	if len(saved) != len(indexes) {
		t.Errorf("%d different pieces were saved, want %d", len(saved), len(indexes))
	}
	for _, index := range indexes {
		if saved[index] != 1 {
			t.Errorf("piece %d was saved %d times, want once", index, saved[index])
		}
	}
}

// A failure to save a piece ends the download there and then.
func TestDownloadPiecesStopsWhenSavingFails(t *testing.T) {
	content := testContent(50 * 1000)
	info := testInfoDict(content, 1000)
	seeder := startTestPeer(t, &testPeer{InfoHash: sha1.Sum([]byte(info)), Content: content, PieceLength: 1000})
	tracker := startTestTracker(t, seeder.Address)
	torrent, err := parseTorrent(testTorrentFile(tracker.AnnounceURL, info))
	if err != nil {
		t.Fatal(err)
	}

	indexes := make([]int, 50)
	for index := range indexes {
		indexes[index] = index
	}

	diskFull := errors.New("disk full")
	calls := 0
	err = newSwarm(torrent).downloadPieces(torrent, indexes, func(index int, piece []byte) error {
		calls++
		if calls == 3 {
			return diskFull
		}
		return nil
	})

	if !errors.Is(err, diskFull) {
		t.Errorf("downloadPieces error = %v, want the error from saving", err)
	}
	if calls != 3 {
		t.Errorf("save was called %d times, want it not to be called again after failing on call 3", calls)
	}
}

func TestDownloadPiecesWithNothingToDownload(t *testing.T) {
	// The tracker can't be reached, which doesn't matter as it's not needed.
	torrent, err := parseTorrent(testTorrentFile("http://"+stoppedServerAddress(t)+"/announce", minimalInfo))
	if err != nil {
		t.Fatal(err)
	}

	err = newSwarm(torrent).downloadPieces(torrent, nil, func(index int, piece []byte) error {
		t.Errorf("save was called with piece %d", index)
		return nil
	})
	if err != nil {
		t.Errorf("downloadPieces returned error: %v", err)
	}
}
