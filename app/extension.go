package main

import (
	"crypto/sha1"
	"errors"
	"fmt"
)

// The extension protocol lets peers exchange messages that aren't part of the
// base BitTorrent protocol. Each extension has a name, and every peer picks
// for itself the numeric IDs that it wants the messages of the extensions it
// supports to be sent to it with. Peers tell each other their choices in an
// extension handshake.

const (
	// extensionHandshakeID is the extension message ID of the extension
	// protocol's own handshake. It's the one ID that's the same for everyone.
	extensionHandshakeID = 0

	// metadataExtension is the name of the extension with which peers send
	// each other a torrent's metadata (its info dictionary). That's what
	// makes it possible to download a torrent given only its magnet link.
	metadataExtension = "ut_metadata"

	// ourMetadataID is the extension message ID that we ask peers to use for
	// the metadata extension's messages when sending them to us. Any ID from
	// 1 to 255 would do.
	ourMetadataID = 1
)

// The types of the metadata extension's messages. Each message is a bencoded
// dictionary that gives its type under the key "msg_type".
const (
	metadataRequest = 0 // Asks for a piece of the metadata
	metadataData    = 1 // Carries a piece of the metadata, which follows the dictionary
	metadataReject  = 2 // Turns down a request for a piece of the metadata
)

const (
	// metadataPieceSize is the size of the pieces that metadata is
	// transferred in. The last piece is smaller if the size of the metadata
	// isn't a multiple of it.
	metadataPieceSize = 16 * 1024

	// maxMetadataSize caps the size of the metadata accepted from a peer, so
	// that a misbehaving peer can't exhaust memory. Real metadata, even of
	// torrents with a huge number of pieces, is a small fraction of this.
	maxMetadataSize = 32 << 20
)

// extensionMessage returns a message of the extension protocol. Such
// messages all share a message ID, and say which extension they belong to
// with the first byte of their payload:
//
//	1 byte  extension message ID
//	        payload, whose format depends on the extension
func extensionMessage(id uint8, payload []byte) message {
	return message{ID: msgExtended, Payload: append([]byte{id}, payload...)}
}

// handshakeExtensions exchanges extension handshakes with the peer, after
// which it's known which extensions the peer supports. It must only be used
// with peers that support the extension protocol.
func (p *peerConn) handshakeExtensions() error {
	// A peer follows up its handshake by announcing which pieces it has.
	// Holding back until that has arrived keeps the conversation in the order
	// that peers expect.
	if _, err := p.receive(); err != nil {
		return err
	}

	if err := p.sendExtensionHandshake(); err != nil {
		return err
	}

	// The peer's own extension handshake may already have arrived, or may
	// still be to come, behind other messages.
	for p.extensionIDs == nil {
		if _, err := p.receive(); err != nil {
			return err
		}
	}
	return nil
}

// sendExtensionHandshake sends the peer our extension handshake. Its payload
// is a bencoded dictionary, in which "m" maps the name of each extension that
// we support to the extension message ID that we've picked for it:
//
//	{"m": {"ut_metadata": 1}}
func (p *peerConn) sendExtensionHandshake() error {
	payload := encodeBencode(map[string]interface{}{
		"m": map[string]interface{}{metadataExtension: ourMetadataID},
	})
	return p.send(extensionMessage(extensionHandshakeID, payload))
}

// receiveExtensionHandshake takes in the payload of an extension handshake
// from the peer, which has the same format as the one that we send. Peers
// tend to include other keys besides "m", none of which are of use here.
//
// A peer may send further handshakes later on to update its extensions. An
// extension that's given an ID of 0 is one that the peer has turned off.
func (p *peerConn) receiveExtensionHandshake(payload []byte) error {
	decoded, err := decodeBencode(string(payload))
	if err != nil {
		return fmt.Errorf("peer sent a malformed extension handshake: %w", err)
	}
	dict, ok := decoded.(map[string]interface{})
	if !ok {
		return errors.New("peer sent a malformed extension handshake: not a dictionary")
	}

	if p.extensionIDs == nil {
		p.extensionIDs = map[string]uint8{}
	}
	extensions, _ := dict["m"].(map[string]interface{}) // Absent if the peer supports no extensions
	for name, value := range extensions {
		id, isInteger := value.(int64)
		switch {
		case !isInteger || id < 0 || id > 255:
			// Not a usable ID, so there's nothing to learn from this entry.
		case id == 0:
			delete(p.extensionIDs, name)
		default:
			p.extensionIDs[name] = uint8(id)
		}
	}
	return nil
}

// metadataID returns the extension message ID that the peer wants the
// metadata extension's messages sent to it with. It reports false if the
// peer doesn't support the extension, or hasn't sent its extension handshake.
func (p *peerConn) metadataID() (id uint8, ok bool) {
	id, ok = p.extensionIDs[metadataExtension]
	return id, ok
}

// requestMetadataPiece asks the peer for a piece of the torrent's metadata.
// The metadata is the torrent's bencoded info dictionary, and is split into
// pieces of 16 KiB for transfer.
//
// The request is a message of the metadata extension, whose payload is a
// bencoded dictionary:
//
//	{"msg_type": 0, "piece": <index of the piece>}
func (p *peerConn) requestMetadataPiece(piece int) error {
	id, ok := p.metadataID()
	if !ok {
		return errors.New("peer doesn't support the metadata extension")
	}

	payload := encodeBencode(map[string]interface{}{
		"msg_type": metadataRequest,
		"piece":    piece,
	})
	return p.send(extensionMessage(id, payload))
}

// downloadMetadata downloads the torrent's metadata from the peer, one piece
// after another. The returned metadata has been verified against the
// torrent's info hash, which is the hash of the metadata.
func (p *peerConn) downloadMetadata(infoHash [20]byte) ([]byte, error) {
	var metadata []byte
	totalSize := 0 // The size of the whole of the metadata
	for piece := 0; ; piece++ {
		if err := p.requestMetadataPiece(piece); err != nil {
			return nil, err
		}
		reply, err := p.receiveMetadataPiece(piece)
		if err != nil {
			return nil, err
		}

		// Every piece comes with the size of the metadata as a whole, which
		// isn't known before the first piece arrives.
		if piece == 0 {
			if reply.TotalSize <= 0 || reply.TotalSize > maxMetadataSize {
				return nil, fmt.Errorf("peer claims that the metadata is %d bytes long", reply.TotalSize)
			}
			totalSize = int(reply.TotalSize)
			metadata = make([]byte, 0, totalSize)
		} else if reply.TotalSize != int64(totalSize) {
			return nil, fmt.Errorf("peer changed its mind about the size of the metadata, from %d bytes to %d",
				totalSize, reply.TotalSize)
		}

		// A piece is the standard size, unless it's the last one.
		if want := min(metadataPieceSize, totalSize-len(metadata)); len(reply.Data) != want {
			return nil, fmt.Errorf("peer sent %d bytes for a piece of metadata of %d bytes", len(reply.Data), want)
		}
		metadata = append(metadata, reply.Data...)
		if len(metadata) == totalSize {
			break
		}
	}

	if sha1.Sum(metadata) != infoHash {
		return nil, errors.New("metadata from peer doesn't match the torrent's info hash")
	}
	return metadata, nil
}

// receiveMetadataPiece waits for the peer's answer to a request for the
// piece of metadata with the given index, and returns it if it's the piece.
func (p *peerConn) receiveMetadataPiece(piece int) (metadataMessage, error) {
	for {
		m, err := p.receive()
		if err != nil {
			return metadataMessage{}, err
		}
		// The peer sends the metadata extension's messages with the ID that
		// we picked for the extension.
		if m.ID != msgExtended || m.Payload[0] != ourMetadataID {
			continue
		}

		reply, err := parseMetadataMessage(m.Payload[1:])
		if err != nil {
			return metadataMessage{}, err
		}
		switch reply.Type {
		case metadataData:
			if reply.Piece != int64(piece) {
				return metadataMessage{}, fmt.Errorf("peer sent piece %d of the metadata, which wasn't requested", reply.Piece)
			}
			return reply, nil
		case metadataReject:
			return metadataMessage{}, fmt.Errorf("peer turned down the request for piece %d of the metadata", piece)
		}
		// Anything else isn't an answer to the request: requests of the
		// peer's own go unanswered, as there's no metadata here to give it,
		// and message types that aren't known are to be ignored.
	}
}

// metadataMessage is a message of the metadata extension.
type metadataMessage struct {
	Type      int64  // One of the metadata message types
	Piece     int64  // The index of the piece of metadata that the message is about
	TotalSize int64  // The size of the whole of the metadata (data messages only)
	Data      []byte // The piece of metadata (data messages only)
}

// parseMetadataMessage parses the payload of a metadata extension message.
// It's a bencoded dictionary which, in the case of a data message, is
// followed by the piece of metadata that the message carries:
//
//	{"msg_type": 1, "piece": 0, "total_size": 3425}<piece of metadata>
func parseMetadataMessage(payload []byte) (metadataMessage, error) {
	decoded, rest, err := decodeBencodePrefix(string(payload))
	if err != nil {
		return metadataMessage{}, fmt.Errorf("peer sent a malformed metadata message: %w", err)
	}
	dict, ok := decoded.(map[string]interface{})
	if !ok {
		return metadataMessage{}, errors.New("peer sent a malformed metadata message: not a dictionary")
	}

	var m metadataMessage
	if m.Type, err = dictInt(dict, "msg_type"); err != nil {
		return metadataMessage{}, fmt.Errorf("peer sent a malformed metadata message: %w", err)
	}
	if m.Piece, err = dictInt(dict, "piece"); err != nil {
		return metadataMessage{}, fmt.Errorf("peer sent a malformed metadata message: %w", err)
	}
	if m.Type == metadataData {
		if m.TotalSize, err = dictInt(dict, "total_size"); err != nil {
			return metadataMessage{}, fmt.Errorf("peer sent a malformed metadata message: %w", err)
		}
		m.Data = []byte(rest)
	}
	return m, nil
}
