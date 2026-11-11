package main

import (
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
