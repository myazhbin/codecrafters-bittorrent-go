package main

import (
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

	peer, err := dialPeer(args[1], torrent.InfoHash, newPeerID())
	if err != nil {
		return err
	}
	defer peer.Close()

	fmt.Fprintf(stdout, "Peer ID: %x\n", peer.ID)
	return nil
}

// runDownloadPiece implements
// `download_piece -o <output file> <torrent file> <piece index>`, which
// downloads a single piece of a torrent and saves it to a file.
func runDownloadPiece(args []string, stdout io.Writer) error {
	const usage = "usage: download_piece -o <output file> <torrent file> <piece index>"

	flags := flag.NewFlagSet("download_piece", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // Errors are reported by returning them instead
	outputPath := flags.String("o", "", "file to save the piece to")
	if err := flags.Parse(args); err != nil || flags.NArg() != 2 || *outputPath == "" {
		return errors.New(usage)
	}

	torrent, err := loadTorrent(flags.Arg(0))
	if err != nil {
		return err
	}

	index, err := strconv.Atoi(flags.Arg(1))
	if err != nil || index < 0 || index >= len(torrent.PieceHashes) {
		return fmt.Errorf("invalid piece index %q: the torrent has %d pieces, numbered from 0",
			flags.Arg(1), len(torrent.PieceHashes))
	}

	piece, err := downloadPiece(torrent, index)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*outputPath, piece, 0o644); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Piece %d downloaded to %s.\n", index, *outputPath)
	return nil
}

// writeJSON writes value as a single line of compact JSON.
func writeJSON(w io.Writer, value interface{}) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false) // Print "a<b" rather than "a<b"
	return encoder.Encode(value)
}
