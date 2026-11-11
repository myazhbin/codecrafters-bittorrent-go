package main

import (
	"context"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ourExtensionHandshake is the extension handshake that this client sends, as
// it appears on the wire: a message with ID 20, whose payload is the
// extension message ID 0 followed by a bencoded dictionary.
const ourExtensionHandshake = "\x00\x00\x00\x1a" + "\x14" + "\x00" + "d1:md11:ut_metadatai1eee"

// messagesAfterHangup waits for a client to hang up on a fake peer, and then
// returns the messages that the peer has received so far.
func messagesAfterHangup(t *testing.T, peer *testPeer) []message {
	t.Helper()

	select {
	case <-peer.Hangups:
	case <-time.After(10 * time.Second):
		t.Fatal("the client didn't hang up on the peer")
	}

	var messages []message
	for {
		select {
		case m := <-peer.Messages:
			messages = append(messages, m)
		default:
			return messages
		}
	}
}

// dialWithExtensions connects to the peer at the given address, announcing
// support for the extension protocol. The connection is closed when the test
// ends.
func dialWithExtensions(t *testing.T, address string, infoHash [20]byte) *peerConn {
	t.Helper()

	peer, err := dialPeer(context.Background(), address,
		handshake{Reserved: extensionsReserved, InfoHash: infoHash, PeerID: newPeerID()})
	if err != nil {
		t.Fatalf("dialPeer returned error: %v", err)
	}
	t.Cleanup(func() { peer.Close() })
	return peer
}

func TestExtensionMessage(t *testing.T) {
	tests := []struct {
		name    string
		message message
		want    string
	}{
		{"handshake", extensionMessage(0, []byte("d1:mdee")), "\x00\x00\x00\x09\x14\x00d1:mdee"},
		{"no payload", extensionMessage(3, nil), "\x00\x00\x00\x02\x14\x03"},
		{"highest ID", extensionMessage(255, []byte("x")), "\x00\x00\x00\x03\x14\xffx"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.message.encode(); string(got) != tt.want {
				t.Errorf("encode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandshakeExtensions(t *testing.T) {
	infoHash := exampleHandshake.InfoHash
	fake := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, MetadataID: 123})

	peer := dialWithExtensions(t, fake.Address, infoHash)
	if _, ok := peer.metadataID(); ok {
		t.Error("the peer's metadata extension ID is known before the extension handshake")
	}

	if err := peer.handshakeExtensions(); err != nil {
		t.Fatalf("handshakeExtensions returned error: %v", err)
	}
	if id, ok := peer.metadataID(); !ok || id != 123 {
		t.Errorf("metadataID() = %d, %v, want 123, true", id, ok)
	}
	peer.Close()

	messages := messagesAfterHangup(t, fake)
	if len(messages) != 1 {
		t.Fatalf("the peer received %d messages, want just the extension handshake: %+v", len(messages), messages)
	}
	if got := messages[0].encode(); string(got) != ourExtensionHandshake {
		t.Errorf("the peer received the message %q, want %q", got, ourExtensionHandshake)
	}

	// The handshake has to hold up when read the way that a peer would.
	payload := messages[0].Payload
	if messages[0].ID != 20 || payload[0] != 0 {
		t.Fatalf("message ID = %d and extension message ID = %d, want 20 and 0", messages[0].ID, payload[0])
	}
	decoded, err := decodeBencode(string(payload[1:]))
	if err != nil {
		t.Fatalf("the extension handshake isn't valid bencode: %v", err)
	}
	extensions, err := dictDict(decoded.(map[string]interface{}), "m")
	if err != nil {
		t.Fatalf("the extension handshake has no dictionary of extensions: %v", err)
	}
	id, err := dictInt(extensions, "ut_metadata")
	if err != nil {
		t.Fatalf("the extension handshake doesn't list the metadata extension: %v", err)
	}
	if id < 1 || id > 255 {
		t.Errorf("the metadata extension's ID is %d, want it to be between 1 and 255", id)
	}
}

// Peers differ in when they send their extension handshake, and in what else
// they send around it.
func TestHandshakeExtensionsWithDifferentPeers(t *testing.T) {
	infoHash := exampleHandshake.InfoHash

	tests := []struct {
		name   string
		peer   *testPeer
		wantID uint8
		wantOK bool
	}{
		{"answers our handshake", &testPeer{MetadataID: 16}, 16, true},
		{"sends its handshake before its bitfield", &testPeer{MetadataID: 16, EagerExtensionHandshake: true}, 16, true},
		{"sends its handshake without a bitfield", &testPeer{MetadataID: 16, EagerExtensionHandshake: true, NoBitfield: true}, 16, true},
		{"sends other messages in between", &testPeer{MetadataID: 16, Chatty: true}, 16, true},
		{"sends everything in fragments", &testPeer{MetadataID: 16, WriteChunkSize: 3}, 16, true},
		{"lowest ID", &testPeer{MetadataID: 1}, 1, true},
		{"highest ID", &testPeer{MetadataID: 255}, 255, true},
		{
			"supports other extensions too",
			&testPeer{MetadataID: 3, OtherExtensions: map[string]interface{}{"ut_pex": 1, "lt_donthave": 7, "ut_holepunch": 4}},
			3, true,
		},
		{"doesn't support the metadata extension", &testPeer{OtherExtensions: map[string]interface{}{"ut_pex": 1}}, 0, false},
		{"supports no extensions at all", &testPeer{}, 0, false},
		{"handshake without a list of extensions", &testPeer{ExtensionHandshake: []byte("d1:v4:Teste")}, 0, false},
		{"empty handshake", &testPeer{ExtensionHandshake: []byte("de")}, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.peer.InfoHash, tt.peer.Reserved = infoHash, extensionsReserved
			fake := startTestPeer(t, tt.peer)
			peer := dialWithExtensions(t, fake.Address, infoHash)

			if err := peer.handshakeExtensions(); err != nil {
				t.Fatalf("handshakeExtensions returned error: %v", err)
			}
			if id, ok := peer.metadataID(); id != tt.wantID || ok != tt.wantOK {
				t.Errorf("metadataID() = %d, %v, want %d, %v", id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}

// The extension handshake must wait its turn: nothing is sent after the base
// handshake until the peer has sent its first message, which is normally its
// bitfield.
func TestHandshakeExtensionsLetsPeerSpeakFirst(t *testing.T) {
	infoHash := exampleHandshake.InfoHash

	type observations struct {
		sentEarly bool    // Whether the client sent something before the bitfield
		received  message // What the client sent after the bitfield
		err       error
	}
	observed := make(chan observations, 1)
	address := startTestServer(t, func(conn net.Conn) {
		var seen observations
		defer func() { observed <- seen }()

		if _, seen.err = readHandshake(conn); seen.err != nil {
			return
		}
		conn.Write(handshake{Reserved: extensionsReserved, InfoHash: infoHash, PeerID: testPeerID}.encode())

		// Hold back the bitfield for a moment, and see whether the client
		// sends anything in the meantime.
		conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		_, err := conn.Read(make([]byte, 1))
		seen.sentEarly = !os.IsTimeout(err)
		conn.SetReadDeadline(time.Time{})
		if seen.sentEarly {
			return
		}

		conn.Write(message{ID: msgBitfield, Payload: []byte{0x80}}.encode())
		if seen.received, seen.err = readMessage(conn); seen.err != nil {
			return
		}
		conn.Write(extensionMessage(0, []byte("d1:md11:ut_metadatai42eee")).encode())
		io.Copy(io.Discard, conn)
	})

	peer := dialWithExtensions(t, address, infoHash)
	if err := peer.handshakeExtensions(); err != nil {
		t.Fatalf("handshakeExtensions returned error: %v", err)
	}
	if !peer.hasPiece(0) {
		t.Error("the bitfield that the peer sent first wasn't taken in")
	}
	if id, ok := peer.metadataID(); !ok || id != 42 {
		t.Errorf("metadataID() = %d, %v, want 42, true", id, ok)
	}
	peer.Close()

	seen := <-observed
	if seen.sentEarly {
		t.Fatal("the client sent something before the peer had sent its bitfield")
	}
	if seen.err != nil {
		t.Fatalf("the peer failed to read the extension handshake: %v", seen.err)
	}
	if got := seen.received.encode(); string(got) != ourExtensionHandshake {
		t.Errorf("the peer received the message %q, want %q", got, ourExtensionHandshake)
	}
}

func TestHandshakeExtensionsErrors(t *testing.T) {
	shortenTimeout(t, &peerMessageTimeout)
	infoHash := exampleHandshake.InfoHash

	// afterHandshake returns the address of a peer that exchanges handshakes
	// and then follows the given script.
	afterHandshake := func(script func(conn net.Conn)) string {
		return startTestServer(t, func(conn net.Conn) {
			if _, err := readHandshake(conn); err != nil {
				return
			}
			conn.Write(handshake{Reserved: extensionsReserved, InfoHash: infoHash, PeerID: testPeerID}.encode())
			script(conn)
		})
	}
	// withExtensionHandshake returns the address of a peer that answers an
	// extension handshake with the given payload.
	withExtensionHandshake := func(payload string) string {
		return startTestPeer(t, &testPeer{
			InfoHash:           infoHash,
			Reserved:           extensionsReserved,
			ExtensionHandshake: []byte(payload),
		}).Address
	}

	tests := []struct {
		name    string
		address string
		wantErr string
	}{
		{"peer hangs up", afterHandshake(func(conn net.Conn) {}), "receiving message from peer"},
		{"peer stays silent", afterHandshake(func(conn net.Conn) { io.Copy(io.Discard, conn) }), "i/o timeout"},
		{
			"peer sends an oversized message",
			afterHandshake(func(conn net.Conn) {
				conn.Write([]byte{0x7f, 0xff, 0xff, 0xff, 5})
				io.Copy(io.Discard, conn)
			}),
			"over the limit",
		},
		{
			"peer never sends its extension handshake",
			startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, NoExtensionHandshake: true}).Address,
			"i/o timeout",
		},
		{
			"peer hangs up instead of sending its extension handshake",
			afterHandshake(func(conn net.Conn) {
				conn.Write(message{ID: msgBitfield, Payload: []byte{0x80}}.encode())
				readMessage(conn)
			}),
			"receiving message from peer",
		},
		{
			"extension message without an ID",
			afterHandshake(func(conn net.Conn) {
				conn.Write(message{ID: msgExtended}.encode())
				io.Copy(io.Discard, conn)
			}),
			"peer sent an extension message without an ID",
		},
		{"extension handshake that isn't bencoded", withExtensionHandshake("hello"), "peer sent a malformed extension handshake"},
		{"extension handshake with nothing in it", withExtensionHandshake(""), "peer sent a malformed extension handshake"},
		{"extension handshake that's cut short", withExtensionHandshake("d1:md11:ut_metadatai16e"), "peer sent a malformed extension handshake"},
		{"extension handshake that isn't a dictionary", withExtensionHandshake("l1:me"), "not a dictionary"},
		{"extension handshake with trailing data", withExtensionHandshake("d1:mdeeextra"), "peer sent a malformed extension handshake"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := dialWithExtensions(t, tt.address, infoHash)

			err := peer.handshakeExtensions()
			if err == nil {
				t.Fatalf("handshakeExtensions succeeded, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("handshakeExtensions error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestReceiveExtensionHandshake(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    map[string]uint8
	}{
		{"metadata extension", "d1:md11:ut_metadatai16eee", map[string]uint8{"ut_metadata": 16}},
		{
			"several extensions",
			"d1:md11:lt_donthavei7e11:ut_metadatai2e6:ut_pexi1eee",
			map[string]uint8{"lt_donthave": 7, "ut_metadata": 2, "ut_pex": 1},
		},
		{
			"other keys besides the extensions",
			"d1:md11:ut_metadatai3ee13:metadata_sizei31235e1:pi6881e4:reqqi512e1:v13:Example 1.2.36:yourip4:\x7f\x00\x00\x01e",
			map[string]uint8{"ut_metadata": 3},
		},
		{"lowest and highest IDs", "d1:md1:ai1e1:bi255eee", map[string]uint8{"a": 1, "b": 255}},
		{"no extensions", "d1:mdee", map[string]uint8{}},
		{"no list of extensions", "de", map[string]uint8{}},
		{"list of extensions of the wrong type", "d1:mli1eee", map[string]uint8{}},

		// Entries that don't hold a usable ID are passed over.
		{
			"unusable IDs",
			"d1:md1:ai256e1:bi-1e1:c1:51:dle1:ei99999999999999999999e11:ut_metadatai9eee",
			map[string]uint8{"ut_metadata": 9},
		},
		{"turned-off extension", "d1:md11:ut_metadatai0e6:ut_pexi1eee", map[string]uint8{"ut_pex": 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := &peerConn{}
			if err := peer.receiveExtensionHandshake([]byte(tt.payload)); err != nil {
				t.Fatalf("receiveExtensionHandshake returned error: %v", err)
			}
			if !reflect.DeepEqual(peer.extensionIDs, tt.want) {
				t.Errorf("extension IDs = %v, want %v", peer.extensionIDs, tt.want)
			}

			id, ok := peer.metadataID()
			if wantID, wantOK := tt.want["ut_metadata"]; id != wantID || ok != wantOK {
				t.Errorf("metadataID() = %d, %v, want %d, %v", id, ok, wantID, wantOK)
			}
		})
	}
}

// A peer can send another extension handshake later on, to add extensions,
// change their IDs or turn them off. What it doesn't mention stays as it was.
func TestReceiveExtensionHandshakeUpdates(t *testing.T) {
	peer := &peerConn{}

	steps := []struct {
		payload string
		want    map[string]uint8
	}{
		{"d1:md11:ut_metadatai3e6:ut_pexi1eee", map[string]uint8{"ut_metadata": 3, "ut_pex": 1}},
		{"d1:md11:lt_donthavei7eee", map[string]uint8{"ut_metadata": 3, "ut_pex": 1, "lt_donthave": 7}},
		{"d1:md11:ut_metadatai5eee", map[string]uint8{"ut_metadata": 5, "ut_pex": 1, "lt_donthave": 7}},
		{"d1:md6:ut_pexi0eee", map[string]uint8{"ut_metadata": 5, "lt_donthave": 7}},
		{"de", map[string]uint8{"ut_metadata": 5, "lt_donthave": 7}},
	}

	for i, step := range steps {
		if err := peer.receiveExtensionHandshake([]byte(step.payload)); err != nil {
			t.Fatalf("handshake %d returned error: %v", i, err)
		}
		if !reflect.DeepEqual(peer.extensionIDs, step.want) {
			t.Errorf("extension IDs after handshake %d = %v, want %v", i, peer.extensionIDs, step.want)
		}
	}
}
