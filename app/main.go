package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

// writeJSON writes value as a single line of compact JSON.
func writeJSON(w io.Writer, value interface{}) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false) // Print "a<b" rather than "a<b"
	return encoder.Encode(value)
}
