package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
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

	// blockSize is the size of the blocks that pieces are downloaded in. A
	// piece's last block is smaller if the piece's size isn't a multiple of it.
	blockSize = 16 * 1024

	// maxPipelinedRequests is how many block requests are kept outstanding
	// at a time. Having several in flight means that the peer always has
	// something to send, instead of sitting idle for a round trip after each
	// block.
	maxPipelinedRequests = 5

	// maxMessageSize caps the size of the messages accepted from a peer, so
	// that a misbehaving peer can't exhaust memory. The largest legitimate
	// messages are blocks of piece data and the bitfields of torrents that
	// have a huge number of pieces.
	maxMessageSize = 1 << 20
)

// These are variables rather than constants so that tests can shorten them.
var (
	// peerDialTimeout bounds how long connecting to a peer may take.
	peerDialTimeout = 10 * time.Second

	// peerHandshakeTimeout bounds how long exchanging handshakes with a
	// connected peer may take.
	peerHandshakeTimeout = 10 * time.Second

	// peerMessageTimeout bounds how long to wait for a peer to send its next
	// message, and how long sending it a message may take.
	peerMessageTimeout = 30 * time.Second
)

// handshake is the first message sent in each direction on a connection
// between two peers.
type handshake struct {
	Reserved [8]byte  // Flags that announce support for protocol extensions
	InfoHash [20]byte // Identifies the torrent that the connection is for
	PeerID   [20]byte // Identifies the peer that sent the handshake
}

// Support for the extension protocol, which lets peers exchange messages that
// aren't part of the base protocol, is announced by setting the 20th bit from
// the right of the handshake's reserved bytes (counting from 0). That bit is
// in the sixth of the eight bytes:
//
//	00 00 00 00 00 10 00 00
const (
	extensionsReservedByte = 5
	extensionsReservedBit  = 0x10
)

// announceExtensions marks the handshake's sender as supporting the extension
// protocol.
func (h *handshake) announceExtensions() {
	h.Reserved[extensionsReservedByte] |= extensionsReservedBit
}

// supportsExtensions reports whether the handshake's sender supports the
// extension protocol.
func (h handshake) supportsExtensions() bool {
	return h.Reserved[extensionsReservedByte]&extensionsReservedBit != 0
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

// messageID identifies the type of a message.
type messageID uint8

const (
	msgChoke      messageID = 0 // The sender won't serve the receiver's requests
	msgUnchoke    messageID = 1 // The sender will serve the receiver's requests
	msgInterested messageID = 2 // The sender wants to download from the receiver
	msgHave       messageID = 4 // The sender has a piece: <index>
	msgBitfield   messageID = 5 // The pieces that the sender has, as one bit per piece
	msgRequest    messageID = 6 // Asks for a block of a piece: <index><begin><length>
	msgPiece      messageID = 7 // A block of a piece: <index><begin><data>
)

// message is one of the messages that peers exchange after their handshakes.
type message struct {
	ID      messageID
	Payload []byte
}

// encode returns the message as it's sent over the wire:
//
//	4 bytes  length of the rest of the message, as a big-endian integer
//	1 byte   message ID
//	         payload, whose format depends on the message ID
func (m message) encode() []byte {
	encoded := make([]byte, 4, 4+1+len(m.Payload))
	binary.BigEndian.PutUint32(encoded, uint32(1+len(m.Payload)))
	encoded = append(encoded, byte(m.ID))
	encoded = append(encoded, m.Payload...)
	return encoded
}

// readMessage reads the next message from r. Keep-alives, which are messages
// with a length of zero (and so without an ID), are skipped.
func readMessage(r io.Reader) (message, error) {
	for {
		var prefix [4]byte
		if _, err := io.ReadFull(r, prefix[:]); err != nil {
			return message{}, err
		}

		length := binary.BigEndian.Uint32(prefix[:])
		if length == 0 {
			continue // A keep-alive
		}
		if length > maxMessageSize {
			return message{}, fmt.Errorf("message of %d bytes is over the limit of %d bytes", length, maxMessageSize)
		}

		body := make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF // The message was cut short, even if on a boundary
			}
			return message{}, err
		}
		return message{ID: messageID(body[0]), Payload: body[1:]}, nil
	}
}

// requestMessage returns a message that asks for length bytes of the piece
// with the given index, starting at the offset begin within the piece.
func requestMessage(index, begin, length int) message {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], uint32(index))
	binary.BigEndian.PutUint32(payload[4:8], uint32(begin))
	binary.BigEndian.PutUint32(payload[8:12], uint32(length))
	return message{ID: msgRequest, Payload: payload}
}

// peerConn is a connection to a peer with which handshakes have been
// exchanged.
type peerConn struct {
	conn   net.Conn
	reader *bufio.Reader // Buffers reads from conn

	ID [20]byte // The peer's ID, as given in its handshake

	// SupportsExtensions is whether the extension protocol can be used on
	// the connection, which takes both sides announcing support for it in
	// their handshakes.
	SupportsExtensions bool

	// choked is whether the peer is refusing to serve our requests. Peers
	// start out choking, and unchoke those that tell them they're interested
	// as and when they see fit.
	choked bool

	// interested is whether we've told the peer that we want to download
	// from it.
	interested bool

	// bitfield records which pieces the peer has, going by what it has told
	// us. The most significant bit of the first byte stands for piece 0.
	bitfield []byte

	// stopWatchingContext ends the arrangement by which the connection gets
	// closed when the context that it was dialed with is cancelled.
	stopWatchingContext func() bool
}

// dialPeer connects to the peer at address (in "host:port" form) and
// exchanges handshakes with it. ours is the handshake to send: it says which
// torrent the connection is for, and who this client is.
//
// Cancelling the context closes the connection, even after dialPeer has
// returned. This makes anything that's blocked on the connection fail
// promptly, rather than when its timeout runs out.
func dialPeer(ctx context.Context, address string, ours handshake) (*peerConn, error) {
	dialer := net.Dialer{Timeout: peerDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	stopWatchingContext := context.AfterFunc(ctx, func() { conn.Close() })

	theirs, err := exchangeHandshakes(conn, ours)
	if err != nil {
		stopWatchingContext()
		conn.Close()
		return nil, err
	}

	return &peerConn{
		conn:                conn,
		reader:              bufio.NewReader(conn),
		ID:                  theirs.PeerID,
		SupportsExtensions:  ours.supportsExtensions() && theirs.supportsExtensions(),
		choked:              true,
		stopWatchingContext: stopWatchingContext,
	}, nil
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
	p.stopWatchingContext()
	return p.conn.Close()
}

// send sends a message to the peer.
func (p *peerConn) send(m message) error {
	p.conn.SetWriteDeadline(time.Now().Add(peerMessageTimeout))
	if _, err := p.conn.Write(m.encode()); err != nil {
		return fmt.Errorf("sending message to peer: %w", err)
	}
	return nil
}

// receive waits for the next message from the peer. Before returning the
// message, it updates what's known about the peer accordingly: whether it's
// choking us, and which pieces it has.
func (p *peerConn) receive() (message, error) {
	p.conn.SetReadDeadline(time.Now().Add(peerMessageTimeout))
	m, err := readMessage(p.reader)
	if err != nil {
		return message{}, fmt.Errorf("receiving message from peer: %w", err)
	}

	switch m.ID {
	case msgChoke:
		p.choked = true
	case msgUnchoke:
		p.choked = false
	case msgBitfield:
		p.bitfield = m.Payload
	case msgHave:
		if len(m.Payload) != 4 {
			return message{}, fmt.Errorf("peer sent a malformed have message (payload of %d bytes)", len(m.Payload))
		}
		p.setHasPiece(int(binary.BigEndian.Uint32(m.Payload)))
	}
	return m, nil
}

// hasPiece reports whether the peer has said that it has the piece with the
// given index.
func (p *peerConn) hasPiece(index int) bool {
	byteIndex, bit := index/8, byte(0x80)>>(index%8)
	return byteIndex < len(p.bitfield) && p.bitfield[byteIndex]&bit != 0
}

// setHasPiece records that the peer has the piece with the given index.
func (p *peerConn) setHasPiece(index int) {
	byteIndex, bit := index/8, byte(0x80)>>(index%8)
	if byteIndex >= len(p.bitfield) {
		if byteIndex >= maxMessageSize {
			return // No torrent has this many pieces: its bitfield couldn't even be sent
		}
		p.bitfield = append(p.bitfield, make([]byte, byteIndex+1-len(p.bitfield))...)
	}
	p.bitfield[byteIndex] |= bit
}

// downloadPiece downloads the piece of the torrent with the given index from
// the peer. The returned data has been verified against the piece's hash.
//
// The piece is downloaded in blocks, with several block requests kept in
// flight at once. Blocks can only be requested while the peer isn't choking
// us, so this starts by telling the peer that we're interested and waiting
// for it to unchoke us.
func (p *peerConn) downloadPiece(torrent *Torrent, index int) ([]byte, error) {
	if !p.interested {
		if err := p.send(message{ID: msgInterested}); err != nil {
			return nil, err
		}
		p.interested = true
	}

	// The state of each block of the piece.
	const (
		blockPending   = iota // Has yet to be requested
		blockRequested        // Requested, but not received yet
		blockReceived
	)

	piece := make([]byte, torrent.pieceSize(index))
	blocks := make([]uint8, (len(piece)+blockSize-1)/blockSize)
	blockLength := func(block int) int {
		return min(blockSize, len(piece)-block*blockSize)
	}

	next := 0                // No block before this one is pending
	inFlight := 0            // Number of blocks that are in the requested state
	remaining := len(blocks) // Number of blocks that are yet to be received
	for remaining > 0 {
		// Top up the pipeline of requests, if the peer is taking requests.
		for ; !p.choked && inFlight < maxPipelinedRequests && next < len(blocks); next++ {
			if blocks[next] != blockPending {
				continue
			}
			// A peer only unchokes us after it has announced its pieces, so
			// by now it's certain whether it has this one.
			if !p.hasPiece(index) {
				return nil, fmt.Errorf("peer doesn't have piece %d", index)
			}
			if err := p.send(requestMessage(index, next*blockSize, blockLength(next))); err != nil {
				return nil, err
			}
			blocks[next] = blockRequested
			inFlight++
		}

		m, err := p.receive()
		if err != nil {
			return nil, err
		}

		switch m.ID {
		case msgChoke:
			// A peer forgets our outstanding requests when it chokes us, so
			// they will all have to be sent again once it unchokes us.
			for block, state := range blocks {
				if state == blockRequested {
					blocks[block] = blockPending
				}
			}
			next, inFlight = 0, 0

		case msgPiece:
			block, data, err := parseBlock(m.Payload, index, len(piece))
			if err != nil {
				return nil, err
			}
			if len(data) != blockLength(block) {
				return nil, fmt.Errorf("peer sent %d bytes for a block of %d bytes", len(data), blockLength(block))
			}

			switch blocks[block] {
			case blockReceived:
				continue // A duplicate, which can happen if a choke crossed with our requests
			case blockRequested:
				inFlight--
			}
			copy(piece[block*blockSize:], data)
			blocks[block] = blockReceived
			remaining--
		}
	}

	if sha1.Sum(piece) != torrent.PieceHashes[index] {
		return nil, fmt.Errorf("piece %d from peer failed its hash check", index)
	}
	return piece, nil
}

// parseBlock parses the payload of a piece message, which carries a block of
// data for the piece with index wantIndex, a piece of pieceSize bytes. It
// returns the block's number within the piece along with the block's data.
//
// The payload's format is:
//
//	4 bytes  index of the piece
//	4 bytes  offset of the block within the piece
//	         the block's data
func parseBlock(payload []byte, wantIndex, pieceSize int) (block int, data []byte, err error) {
	if len(payload) < 8 {
		return 0, nil, errors.New("peer sent a truncated piece message")
	}
	index := binary.BigEndian.Uint32(payload[0:4])
	begin := binary.BigEndian.Uint32(payload[4:8])

	if int64(index) != int64(wantIndex) {
		return 0, nil, fmt.Errorf("peer sent a block of piece %d, which wasn't requested", index)
	}
	if begin%blockSize != 0 || int64(begin) >= int64(pieceSize) {
		return 0, nil, fmt.Errorf("peer sent a block at offset %d, which wasn't requested", begin)
	}
	return int(begin / blockSize), payload[8:], nil
}
