package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run executes a single CLI command, writing its result to stdout.
func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: your_program.sh <command> [arguments]")
	}

	switch command, args := args[0], args[1:]; command {
	case "decode":
		return runDecode(args, stdout)
	case "info":
		return runInfo(args, stdout)
	case "peers":
		return runPeers(args, stdout)
	case "handshake":
		return runHandshake(args, stdout)
	case "download_piece":
		return runDownloadPiece(args, stdout)
	case "download":
		return runDownload(args, stdout)
	case "magnet_parse":
		return runMagnetParse(args, stdout)
	case "magnet_handshake":
		return runMagnetHandshake(args, stdout)
	default:
		return fmt.Errorf("unknown command: %s", command)
	}
}

// runDecode implements `decode <bencoded value>`, which prints the bencoded
// value as JSON.
func runDecode(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: decode <bencoded value>")
	}

	decoded, err := decodeBencode(args[0])
	if err != nil {
		return err
	}

	return writeJSON(stdout, decoded)
}

// runInfo implements `info <torrent file>`, which prints details about a
// torrent.
func runInfo(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: info <torrent file>")
	}

	torrent, err := loadTorrent(args[0])
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Tracker URL: %s\n", torrent.Announce)
	fmt.Fprintf(stdout, "Length: %d\n", torrent.Length)
	fmt.Fprintf(stdout, "Info Hash: %x\n", torrent.InfoHash)
	fmt.Fprintf(stdout, "Piece Length: %d\n", torrent.PieceLength)
	fmt.Fprintln(stdout, "Piece Hashes:")
	for _, hash := range torrent.PieceHashes {
		fmt.Fprintf(stdout, "%x\n", hash)
	}
	return nil
}

// runPeers implements `peers <torrent file>`, which asks the torrent's
// tracker for peers and prints the address of each one.
func runPeers(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: peers <torrent file>")
	}

	torrent, err := loadTorrent(args[0])
	if err != nil {
		return err
	}

	// Nothing has been downloaded yet, so the whole file is left.
	peers, err := discoverPeers(torrent.Announce, torrent.InfoHash, newPeerID(), torrent.Length)
	if err != nil {
		return err
	}

	for _, peer := range peers {
		fmt.Fprintln(stdout, peer)
	}
	return nil
}

// runHandshake implements `handshake <torrent file> <peer ip>:<peer port>`,
// which exchanges handshakes with a peer and prints the peer's ID.
func runHandshake(args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return errors.New("usage: handshake <torrent file> <peer ip>:<peer port>")
	}

	torrent, err := loadTorrent(args[0])
	if err != nil {
		return err
	}

	peer, err := dialPeer(context.Background(), args[1], handshake{InfoHash: torrent.InfoHash, PeerID: newPeerID()})
	if err != nil {
		return fmt.Errorf("peer %s: %w", args[1], err)
	}
	defer peer.Close()

	fmt.Fprintf(stdout, "Peer ID: %x\n", peer.ID)
	return nil
}

// runDownloadPiece implements
// `download_piece -o <output file> <torrent file> <piece index>`, which
// downloads a single piece of a torrent and saves it to a file.
func runDownloadPiece(args []string, stdout io.Writer) error {
	outputPath, args, ok := parseOutputFlag(args)
	if !ok || len(args) != 2 {
		return errors.New("usage: download_piece -o <output file> <torrent file> <piece index>")
	}

	torrent, err := loadTorrent(args[0])
	if err != nil {
		return err
	}

	index, err := strconv.Atoi(args[1])
	if err != nil || index < 0 || index >= len(torrent.PieceHashes) {
		return fmt.Errorf("invalid piece index %q: the torrent has %d pieces, numbered from 0",
			args[1], len(torrent.PieceHashes))
	}

	var piece []byte
	err = newSwarm(torrent).downloadPieces(torrent, []int{index}, func(_ int, data []byte) error {
		piece = data
		return nil
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(outputPath, piece, 0o644); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Piece %d downloaded to %s.\n", index, outputPath)
	return nil
}

// runDownload implements `download -o <output file> <torrent file>`, which
// downloads a torrent's file.
func runDownload(args []string, stdout io.Writer) error {
	outputPath, args, ok := parseOutputFlag(args)
	if !ok || len(args) != 1 {
		return errors.New("usage: download -o <output file> <torrent file>")
	}

	torrent, err := loadTorrent(args[0])
	if err != nil {
		return err
	}

	if err := newSwarm(torrent).downloadFile(torrent, outputPath); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Downloaded %s to %s.\n", args[0], outputPath)
	return nil
}

// runMagnetParse implements `magnet_parse <magnet link>`, which prints what a
// magnet link says about a torrent.
func runMagnetParse(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: magnet_parse <magnet link>")
	}

	link, err := parseMagnetLink(args[0])
	if err != nil {
		return err
	}

	tracker := ""
	if len(link.Trackers) > 0 {
		tracker = link.Trackers[0]
	}
	fmt.Fprintf(stdout, "Tracker URL: %s\n", tracker)
	fmt.Fprintf(stdout, "Info Hash: %x\n", link.InfoHash)
	return nil
}

// runMagnetHandshake implements `magnet_handshake <magnet link>`, which
// exchanges handshakes with a peer that has the torrent that a magnet link
// identifies, and prints the peer's ID.
func runMagnetHandshake(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: magnet_handshake <magnet link>")
	}

	link, err := parseMagnetLink(args[0])
	if err != nil {
		return err
	}

	peer, err := newMagnetSwarm(link).connect(context.Background())
	if err != nil {
		return err
	}
	defer peer.Close()

	if peer.SupportsExtensions {
		if err := peer.handshakeExtensions(); err != nil {
			return err
		}
	}

	fmt.Fprintf(stdout, "Peer ID: %x\n", peer.ID)
	return nil
}

// parseOutputFlag parses the arguments of a command that takes a mandatory
// "-o <output file>" flag ahead of its other arguments. It returns the output
// file's path and the other arguments, or reports that the arguments aren't
// valid.
func parseOutputFlag(args []string) (outputPath string, rest []string, ok bool) {
	flags := flag.NewFlagSet("", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // The caller reports the problem, by printing how the command is used
	flags.StringVar(&outputPath, "o", "", "file to save the download to")
	if err := flags.Parse(args); err != nil || outputPath == "" {
		return "", nil, false
	}
	return outputPath, flags.Args(), true
}

// writeJSON writes value as a single line of compact JSON.
func writeJSON(w io.Writer, value interface{}) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false) // Print characters like "<" as they are, rather than as Unicode escapes
	return encoder.Encode(value)
}
