package main

import (
	"errors"
	"fmt"
)

// downloadPiece downloads the piece of the torrent with the given index from
// the torrent's peers, which it asks the tracker for. The returned data has
// been verified against the piece's hash.
func downloadPiece(torrent *Torrent, index int) ([]byte, error) {
	peerID := newPeerID()

	// Nothing has been downloaded yet, so the whole file is left.
	peers, err := discoverPeers(torrent.Announce, torrent.InfoHash, peerID, torrent.Length)
	if err != nil {
		return nil, err
	}
	if len(peers) == 0 {
		return nil, errors.New("the tracker doesn't know of any peers for this torrent")
	}

	// Peers come and go, and not all of them cooperate, so each one is tried
	// in turn until one of them delivers the piece.
	var failures []error
	for _, address := range peers {
		piece, err := downloadPieceFrom(address.String(), torrent, peerID, index)
		if err == nil {
			return piece, nil
		}
		failures = append(failures, fmt.Errorf("peer %s: %w", address, err))
	}
	return nil, fmt.Errorf("no peer provided piece %d:\n%w", index, errors.Join(failures...))
}

// downloadPieceFrom downloads a piece of the torrent from the peer at the
// given address.
func downloadPieceFrom(address string, torrent *Torrent, peerID [20]byte, index int) ([]byte, error) {
	peer, err := dialPeer(address, torrent.InfoHash, peerID)
	if err != nil {
		return nil, err
	}
	defer peer.Close()

	return peer.downloadPiece(torrent, index)
}
