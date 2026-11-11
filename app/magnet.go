package main

import (
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// MagnetLink holds what a magnet link says about a torrent. That's just
// enough to find peers for the torrent: the rest of what a torrent file would
// provide has to be requested from those peers.
type MagnetLink struct {
	InfoHash [20]byte
	Name     string   // The suggested name for the download, which may be empty
	Trackers []string // The URLs of the torrent's trackers, of which there may be none
}

// parseMagnetLink parses a magnet link, which looks like this:
//
//	magnet:?xt=urn:btih:<info hash>&dn=<name>&tr=<tracker URL>
//
// Its query parameters are:
//
//	xt: "urn:btih:" followed by the info hash, as 40 hexadecimal characters
//	dn: the name of the download (optional)
//	tr: the URL of a tracker (optional, and may be given more than once)
func parseMagnetLink(link string) (*MagnetLink, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("invalid magnet link: %w", err)
	}
	if u.Scheme != "magnet" {
		return nil, errors.New(`invalid magnet link: it doesn't start with "magnet:"`)
	}
	query := u.Query()

	// A link can have several "exact topics", each identifying the content
	// in a different way. The one that's needed is the BitTorrent info hash.
	const infoHashPrefix = "urn:btih:"
	var infoHash *[20]byte
	for _, topic := range query["xt"] {
		if encoded, ok := strings.CutPrefix(topic, infoHashPrefix); ok {
			hash, err := decodeInfoHash(encoded)
			if err != nil {
				return nil, fmt.Errorf("invalid magnet link: %w", err)
			}
			infoHash = &hash
			break
		}
	}
	if infoHash == nil {
		return nil, fmt.Errorf("invalid magnet link: it has no info hash (an xt parameter starting with %q)", infoHashPrefix)
	}

	return &MagnetLink{
		InfoHash: *infoHash,
		Name:     query.Get("dn"),
		Trackers: query["tr"],
	}, nil
}

// decodeInfoHash decodes an info hash as written in a magnet link: usually
// as 40 hexadecimal characters, though some links have it as 32 characters of
// base32 instead.
func decodeInfoHash(encoded string) ([20]byte, error) {
	var decoded []byte
	var err error
	switch len(encoded) {
	case 40:
		decoded, err = hex.DecodeString(encoded)
	case 32:
		decoded, err = base32.StdEncoding.DecodeString(strings.ToUpper(encoded))
	default:
		err = fmt.Errorf("it's %d characters long, rather than 40", len(encoded))
	}
	if err != nil {
		return [20]byte{}, fmt.Errorf("malformed info hash %q: %w", encoded, err)
	}
	return [20]byte(decoded), nil
}
