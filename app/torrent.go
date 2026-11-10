package main

import (
	"errors"
	"fmt"
	"os"
)

// Torrent describes a single-file torrent, as read from a .torrent (also
// known as "metainfo") file.
type Torrent struct {
	Announce string // URL of the tracker, which keeps track of the torrent's peers
	Length   int64  // Size of the file, in bytes
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
func parseTorrent(data []byte) (*Torrent, error) {
	decoded, err := decodeBencode(string(data))
	if err != nil {
		return nil, fmt.Errorf("invalid torrent: %w", err)
	}
	metainfo, ok := decoded.(map[string]interface{})
	if !ok {
		return nil, errors.New("invalid torrent: not a bencoded dictionary")
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
	}, nil
}
