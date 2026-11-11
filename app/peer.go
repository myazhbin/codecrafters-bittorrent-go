package main

import (
	"fmt"
	"io"
	"net"
	"time"
)

const (
	// protocolName identifies the BitTorrent protocol at the start of a
	// handshake.
	protocolName = "BitTorrent protocol"

	// handshakeSize is the size of a handshake in bytes: the length-prefixed
	// protocol name, 8 reserved bytes, the info hash and the peer ID.
	handshakeSize = 1 + len(protocolName) + 8 + 20 + 20
)

// These are variables rather than constants so that tests can shorten them.
var (
	// peerDialTimeout bounds how long connecting to a peer may take.
	peerDialTimeout = 10 * time.Second

	// peerHandshakeTimeout bounds how long exchanging handshakes with a
	// connected peer may take.
	peerHandshakeTimeout = 10 * time.Second
)

// handshake is the first message sent in each direction on a connection
// between two peers.
type handshake struct {
	Reserved [8]byte  // Flags that announce support for protocol extensions
	InfoHash [20]byte // Identifies the torrent that the connection is for
	PeerID   [20]byte // Identifies the peer that sent the handshake
}

// encode returns the handshake as it's sent over the wire:
//
//	 1 byte   length of the protocol name (19)
//	19 bytes  protocol name ("BitTorrent protocol")
//	 8 bytes  reserved
//	20 bytes  info hash
//	20 bytes  peer ID
func (h handshake) encode() []byte {
	encoded := make([]byte, 0, handshakeSize)
	encoded = append(encoded, byte(len(protocolName)))
	encoded = append(encoded, protocolName...)
	encoded = append(encoded, h.Reserved[:]...)
	encoded = append(encoded, h.InfoHash[:]...)
	encoded = append(encoded, h.PeerID[:]...)
	return encoded
}

// readHandshake reads a handshake from r.
func readHandshake(r io.Reader) (handshake, error) {
	var encoded [handshakeSize]byte
	if _, err := io.ReadFull(r, encoded[:]); err != nil {
		return handshake{}, fmt.Errorf("reading handshake: %w", err)
	}

	protocol, rest := encoded[:1+len(protocolName)], encoded[1+len(protocolName):]
	if protocol[0] != byte(len(protocolName)) || string(protocol[1:]) != protocolName {
		return handshake{}, fmt.Errorf("not a BitTorrent handshake: it starts with %q", protocol)
	}

	var h handshake
	copy(h.Reserved[:], rest[0:8])
	copy(h.InfoHash[:], rest[8:28])
	copy(h.PeerID[:], rest[28:48])
	return h, nil
}

// peerConn is a connection to a peer with which handshakes have been
// exchanged.
type peerConn struct {
	conn net.Conn

	ID [20]byte // The peer's ID, as given in its handshake
}

// dialPeer connects to the peer at address (in "host:port" form) and
// exchanges handshakes with it for the torrent identified by infoHash. peerID
// is the ID that this client identifies itself with.
func dialPeer(address string, infoHash, peerID [20]byte) (*peerConn, error) {
	conn, err := net.DialTimeout("tcp", address, peerDialTimeout)
	if err != nil {
		return nil, err
	}

	theirs, err := exchangeHandshakes(conn, handshake{InfoHash: infoHash, PeerID: peerID})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake with peer %s: %w", address, err)
	}

	return &peerConn{conn: conn, ID: theirs.PeerID}, nil
}

// exchangeHandshakes sends our handshake over a newly opened connection and
// returns the one that the peer answers with.
func exchangeHandshakes(conn net.Conn, ours handshake) (handshake, error) {
	conn.SetDeadline(time.Now().Add(peerHandshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	if _, err := conn.Write(ours.encode()); err != nil {
		return handshake{}, fmt.Errorf("sending handshake: %w", err)
	}

	theirs, err := readHandshake(conn)
	if err != nil {
		return handshake{}, err
	}
	if theirs.InfoHash != ours.InfoHash {
		return handshake{}, fmt.Errorf("peer answered for a different torrent (info hash %x)", theirs.InfoHash)
	}
	return theirs, nil
}

// Close closes the connection to the peer.
func (p *peerConn) Close() error {
	return p.conn.Close()
}
