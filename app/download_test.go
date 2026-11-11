package main

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

		got, err := os.ReadFile(outputPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("download_piece %d saved the wrong data (%d bytes, want %d)", index, len(got), len(want))
		}
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

	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content[800:]) {
		t.Errorf("download_piece saved the wrong data (%d bytes, want %d)", len(got), 200)
	}
}

// Peers that can't be reached or don't deliver must be passed over in favor
// of one that does.
func TestRunDownloadPieceTriesOtherPeers(t *testing.T) {
	shortenTimeout(t, &peerMessageTimeout)

	content := testContent(100_000)
	infoHash := sha1.Sum([]byte(testInfoDict(content, 40_000)))

	unreachable := stoppedServerAddress(t)
	otherTorrent := startTestPeer(t, &testPeer{InfoHash: [20]byte{0xaa}})
	choker := startTestPeer(t, &testPeer{InfoHash: infoHash, Content: content, PieceLength: 40_000, NeverUnchoke: true})
	corrupter := startTestPeer(t, &testPeer{
		InfoHash:    infoHash,
		Content:     content,
		PieceLength: 40_000,
		Tamper: func(index, begin uint32, data []byte) (uint32, uint32, []byte) {
			return index, begin, make([]byte, len(data))
		},
	})
	torrentPath := seedTestTorrent(t, content, 40_000,
		unreachable, otherTorrent.Address, choker.Address, corrupter.Address)

	outputPath := filepath.Join(t.TempDir(), "piece")
	if _, err := runCommand(t, "download_piece", "-o", outputPath, torrentPath, "1"); err != nil {
		t.Fatalf("download_piece returned error: %v", err)
	}

	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content[40_000:80_000]) {
		t.Errorf("download_piece saved the wrong data (%d bytes, want %d)", len(got), 40_000)
	}

	// Each of the bad peers must have been given its chance first.
	for name, peer := range map[string]*testPeer{"other torrent": otherTorrent, "choker": choker, "corrupter": corrupter} {
		if len(peer.Handshakes) != 1 {
			t.Errorf("the %q peer was connected to %d times, want once", name, len(peer.Handshakes))
		}
	}
}

func TestRunDownloadPieceWithoutUsablePeers(t *testing.T) {
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
		if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
			t.Errorf("download_piece created the output file despite failing (stat error: %v)", err)
		}
	})

	t.Run("no peer delivers", func(t *testing.T) {
		unreachable := stoppedServerAddress(t)
		corrupter := startTestPeer(t, &testPeer{
			InfoHash:    infoHash,
			Content:     content,
			PieceLength: 400,
			Tamper: func(index, begin uint32, data []byte) (uint32, uint32, []byte) {
				return index, begin, make([]byte, len(data))
			},
		})
		tracker := startTestTracker(t, unreachable, corrupter.Address)
		torrentPath := writeTempFile(t, "test.torrent", testTorrentFile(tracker.AnnounceURL, info))
		outputPath := filepath.Join(t.TempDir(), "piece")

		_, err := runCommand(t, "download_piece", "-o", outputPath, torrentPath, "1")
		if err == nil {
			t.Fatal("download_piece succeeded, want an error")
		}
		// The error must say what went wrong with each of the peers.
		for _, want := range []string{
			"no peer provided piece 1",
			"peer " + unreachable + ": ",
			"connection refused",
			"peer " + corrupter.Address + ": piece 1 from peer failed its hash check",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("download_piece error = %q, want it to contain %q", err, want)
			}
		}
		if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
			t.Errorf("download_piece created the output file despite failing (stat error: %v)", err)
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
			if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
				t.Errorf("download_piece %q created the output file despite failing (stat error: %v)", tt.args, err)
			}
		})
	}
}
