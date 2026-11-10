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
	return nil
}

// writeJSON writes value as a single line of compact JSON.
func writeJSON(w io.Writer, value interface{}) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false) // Print "a<b" rather than "a<b"
	return encoder.Encode(value)
}
