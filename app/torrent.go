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

	// InfoHash is the SHA-1 hash of the bencoded info dictionary. It uniquely
	// identifies the torrent to trackers and peers.
	InfoHash [sha1.Size]byte

	Length int64 // Size of the file, in bytes

	// The file is split into pieces of PieceLength bytes each, except for the
	// last piece, which holds whatever remains and so may be shorter.
	// PieceHashes holds the SHA-1 hash of each piece, which is what downloaded
	// data is verified against.
	PieceLength int64
	PieceHashes [][sha1.Size]byte
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
//	    length:       size of the file in bytes
//	    piece length: number of bytes in each piece
//	    pieces:       concatenated SHA-1 hashes of each piece
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

	torrent := &Torrent{
		Announce: announce,

		// The hash covers the info dictionary's bytes exactly as they appear
		// in the file. That's always what other clients hash, whereas
		// re-encoding the decoded dictionary would produce different bytes
		// (and so a different hash) if the file isn't canonically encoded.
		InfoHash: sha1.Sum([]byte(rawValues["info"])),
	}
	if err := torrent.parseInfo(info); err != nil {
		return nil, fmt.Errorf("invalid torrent: info: %w", err)
	}
	return torrent, nil
}

// parseInfo fills in the fields that come from a torrent's info dictionary.
func (t *Torrent) parseInfo(info map[string]interface{}) error {
	// Multi-file torrents list their files under "files" instead of having a
	// top-level "length".
	if _, isMultiFile := info["files"]; isMultiFile {
		return errors.New("multi-file torrents are not supported")
	}

	length, err := dictInt(info, "length")
	if err != nil {
		return err
	}
	if length < 0 {
		return fmt.Errorf("negative length %d", length)
	}

	pieceLength, err := dictInt(info, "piece length")
	if err != nil {
		return err
	}
	if pieceLength <= 0 {
		return fmt.Errorf("piece length %d is not positive", pieceLength)
	}

	pieces, err := dictString(info, "pieces")
	if err != nil {
		return err
	}
	if len(pieces)%sha1.Size != 0 {
		return fmt.Errorf(`"pieces" is %d bytes long, which is not a multiple of %d`, len(pieces), sha1.Size)
	}
	pieceHashes := make([][sha1.Size]byte, len(pieces)/sha1.Size)
	for i := range pieceHashes {
		copy(pieceHashes[i][:], pieces[i*sha1.Size:])
	}

	// There must be exactly one hash for each piece of the file.
	numPieces := length / pieceLength
	if length%pieceLength != 0 {
		numPieces++ // The last piece is a partial one
	}
	if int64(len(pieceHashes)) != numPieces {
		return fmt.Errorf("%d piece hashes for a file of %d bytes, which has %d pieces of %d bytes",
			len(pieceHashes), length, numPieces, pieceLength)
	}

	t.Length = length
	t.PieceLength = pieceLength
	t.PieceHashes = pieceHashes
	return nil
}
