package main

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

// extensionMessage returns a message of the extension protocol. Such
// messages all share a message ID, and say which extension they belong to
// with the first byte of their payload:
//
//	1 byte  extension message ID
//	        payload, whose format depends on the extension
func extensionMessage(id uint8, payload []byte) message {
	return message{ID: msgExtended, Payload: append([]byte{id}, payload...)}
}

// handshakeExtensions carries out our side of the extension handshake, by
// telling the peer which extensions we support. It must only be used with
// peers that support the extension protocol.
func (p *peerConn) handshakeExtensions() error {
	// A peer follows up its handshake by announcing which pieces it has.
	// Holding back until that has arrived keeps the conversation in the order
	// that peers expect.
	if _, err := p.receive(); err != nil {
		return err
	}

	return p.sendExtensionHandshake()
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
