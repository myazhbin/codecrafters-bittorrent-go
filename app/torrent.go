package main

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
)

// Torrent describes a single-file torrent, as read from a .torrent (also
// known as "metainfo") file.
type Torrent struct {
	Announce string // URL of the tracker, which keeps track of the torrent's peers
	Length   int64  // Size of the file, in bytes

	// InfoHash is the SHA-1 hash of the bencoded info dictionary. It uniquely
	// identifies the torrent to trackers and peers.
	InfoHash [sha1.Size]byte
}

// loadTorrent reads and parses the torrent file at path.
func loadTorrent(path string) (*Torrent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	torrent, err := parseTorrent(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return torrent, nil
}

// parseTorrent parses the contents of a torrent file, which is a bencoded
// dictionary with (among others) these keys:
//
//	announce: URL of the tracker
//	info:     a dictionary describing the file being shared:
//	    length: size of the file in bytes
//
// The SHA-1 hash of the bencoded info dictionary is the torrent's info hash.
func parseTorrent(data []byte) (*Torrent, error) {
	metainfo, rawValues, err := decodeBencodeDict(string(data))
	if err != nil {
		return nil, fmt.Errorf("invalid torrent: %w", err)
	}

	announce, err := dictString(metainfo, "announce")
	if err != nil {
		return nil, fmt.Errorf("invalid torrent: %w", err)
	}

	info, err := dictDict(metainfo, "info")
	if err != nil {
		return nil, fmt.Errorf("invalid torrent: %w", err)
	}

	// Multi-file torrents list their files under "files" instead of having a
	// top-level "length".
	if _, isMultiFile := info["files"]; isMultiFile {
		return nil, errors.New("multi-file torrents are not supported")
	}
	length, err := dictInt(info, "length")
	if err != nil {
		return nil, fmt.Errorf("invalid torrent: info: %w", err)
	}
	if length < 0 {
		return nil, fmt.Errorf("invalid torrent: info: negative length %d", length)
	}

	return &Torrent{
		Announce: announce,
		Length:   length,

		// The hash covers the info dictionary's bytes exactly as they appear
		// in the file. That's always what other clients hash, whereas
		// re-encoding the decoded dictionary would produce different bytes
		// (and so a different hash) if the file isn't canonically encoded.
		InfoHash: sha1.Sum([]byte(rawValues["info"])),
	}, nil
}
