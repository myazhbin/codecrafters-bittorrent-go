package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
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

func TestRequestMetadataPiece(t *testing.T) {
	infoHash := exampleHandshake.InfoHash

	tests := []struct {
		name       string
		metadataID int
		piece      int
		want       string // The request, as it appears on the wire
	}{
		{"first piece", 16, 0, "\x00\x00\x00\x1b" + "\x14" + "\x10" + "d8:msg_typei0e5:piecei0ee"},
		{"another piece", 16, 3, "\x00\x00\x00\x1b" + "\x14" + "\x10" + "d8:msg_typei0e5:piecei3ee"},
		{"piece with a longer index", 2, 12, "\x00\x00\x00\x1c" + "\x14" + "\x02" + "d8:msg_typei0e5:piecei12ee"},
		{"highest extension message ID", 255, 0, "\x00\x00\x00\x1b" + "\x14" + "\xff" + "d8:msg_typei0e5:piecei0ee"},
		{"same extension message ID as ours", ourMetadataID, 0, "\x00\x00\x00\x1b" + "\x14" + "\x01" + "d8:msg_typei0e5:piecei0ee"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, MetadataID: tt.metadataID})
			peer := dialWithExtensions(t, fake.Address, infoHash)
			if err := peer.handshakeExtensions(); err != nil {
				t.Fatalf("handshakeExtensions returned error: %v", err)
			}

			if err := peer.requestMetadataPiece(tt.piece); err != nil {
				t.Fatalf("requestMetadataPiece returned error: %v", err)
			}
			peer.Close()

			messages := messagesAfterHangup(t, fake)
			if len(messages) != 2 {
				t.Fatalf("the peer received %d messages, want the extension handshake and the request: %+v", len(messages), messages)
			}
			if got := messages[1].encode(); string(got) != tt.want {
				t.Errorf("the peer received the request %q, want %q", got, tt.want)
			}
		})
	}
}

// The request has to go out with the ID that the peer picked for the metadata
// extension, so it can't be sent to a peer that hasn't picked one.
func TestRequestMetadataPieceWithoutSupport(t *testing.T) {
	infoHash := exampleHandshake.InfoHash

	tests := []struct {
		name      string
		peer      *testPeer
		handshake bool // Whether to go through the extension handshake first
	}{
		{"peer doesn't support the metadata extension", &testPeer{OtherExtensions: map[string]interface{}{"ut_pex": 1}}, true},
		{"peer has turned the metadata extension off", &testPeer{ExtensionHandshake: []byte("d1:md11:ut_metadatai0eee")}, true},
		{"no extension handshake yet", &testPeer{MetadataID: 16}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.peer.InfoHash, tt.peer.Reserved = infoHash, extensionsReserved
			fake := startTestPeer(t, tt.peer)
			peer := dialWithExtensions(t, fake.Address, infoHash)
			if tt.handshake {
				if err := peer.handshakeExtensions(); err != nil {
					t.Fatalf("handshakeExtensions returned error: %v", err)
				}
			}

			err := peer.requestMetadataPiece(0)
			if err == nil || !strings.Contains(err.Error(), "peer doesn't support the metadata extension") {
				t.Errorf("requestMetadataPiece error = %v, want one saying that the extension isn't supported", err)
			}
			peer.Close()

			for _, m := range messagesAfterHangup(t, fake) {
				if m.ID == msgExtended && m.Payload[0] != extensionHandshakeID {
					t.Errorf("the peer was sent the extension message %q", m.Payload)
				}
			}
		})
	}
}

// metadataPeer starts a fake peer that serves the given metadata, and returns
// a connection to it on which extension handshakes have been exchanged. The
// peer is set up by adjust first, if that's given.
func metadataPeer(t *testing.T, metadata []byte, adjust func(peer *testPeer)) (*peerConn, *testPeer) {
	t.Helper()

	fake := &testPeer{
		InfoHash:   sha1.Sum(metadata),
		Reserved:   extensionsReserved,
		MetadataID: 42,
		Metadata:   metadata,
	}
	if adjust != nil {
		adjust(fake)
	}
	startTestPeer(t, fake)

	peer := dialWithExtensions(t, fake.Address, fake.InfoHash)
	if err := peer.handshakeExtensions(); err != nil {
		t.Fatalf("handshakeExtensions returned error: %v", err)
	}
	return peer, fake
}

func TestDownloadMetadata(t *testing.T) {
	// Metadata is hashed rather than parsed while it's downloaded, so any
	// bytes will do for it here. Pieces of metadata are 16,384 bytes long.
	for _, size := range []int{1, 100, 16383, 16384, 16385, 32768, 40_000, 100_000} {
		metadata := testContent(size)
		peer, fake := metadataPeer(t, metadata, nil)

		got, err := peer.downloadMetadata(sha1.Sum(metadata))
		if err != nil {
			t.Fatalf("downloading %d bytes of metadata returned error: %v", size, err)
		}
		if !bytes.Equal(got, metadata) {
			t.Errorf("downloading %d bytes of metadata returned the wrong data (%d bytes)", size, len(got))
		}
		peer.Close()

		// One request per piece, in order, and no more than that.
		var want []string
		for piece := 0; piece*16384 < size; piece++ {
			want = append(want, fmt.Sprintf("\x2ad8:msg_typei0e5:piecei%dee", piece))
		}
		var requests []string
		for _, m := range messagesAfterHangup(t, fake)[1:] {
			requests = append(requests, string(m.Payload))
		}
		if !reflect.DeepEqual(requests, want) {
			t.Errorf("downloading %d bytes of metadata sent the requests %q, want %q", size, requests, want)
		}
	}
}

func TestDownloadMetadataFromAwkwardPeers(t *testing.T) {
	metadata := testContent(40_000)

	tests := []struct {
		name   string
		adjust func(peer *testPeer)
	}{
		{"sends other messages in between", func(peer *testPeer) { peer.Chatty = true }},
		{"sends everything in fragments", func(peer *testPeer) { peer.WriteChunkSize = 97 }},
		{"sent its extension handshake early", func(peer *testPeer) { peer.EagerExtensionHandshake = true }},
		{"uses the same ID for the extension as we do", func(peer *testPeer) { peer.MetadataID = ourMetadataID }},
		{"has no pieces of the file to announce", func(peer *testPeer) { peer.NoBitfield, peer.EagerExtensionHandshake = true, true }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer, _ := metadataPeer(t, metadata, tt.adjust)

			got, err := peer.downloadMetadata(sha1.Sum(metadata))
			if err != nil {
				t.Fatalf("downloadMetadata returned error: %v", err)
			}
			if !bytes.Equal(got, metadata) {
				t.Errorf("downloadMetadata returned the wrong data (%d bytes, want %d)", len(got), len(metadata))
			}
		})
	}
}

func TestDownloadMetadataRejectsBadReplies(t *testing.T) {
	metadata := testContent(40_000) // Three pieces: two whole ones, and one of 7,232 bytes

	type header = map[string]interface{}
	tests := []struct {
		name    string
		tamper  func(h header, data []byte) (header, []byte)
		wantErr string
	}{
		{
			"corrupted data",
			func(h header, data []byte) (header, []byte) {
				if h["piece"] == int64(1) {
					data = bytes.Clone(data)
					data[5] ^= 0x01
				}
				return h, data
			},
			"metadata from peer doesn't match the torrent's info hash",
		},
		{
			"another piece than the one asked for",
			func(h header, data []byte) (header, []byte) { h["piece"] = h["piece"].(int64) + 1; return h, data },
			"peer sent piece 1 of the metadata, which wasn't requested",
		},
		{
			"size of zero",
			func(h header, data []byte) (header, []byte) { h["total_size"] = 0; return h, data },
			"peer claims that the metadata is 0 bytes long",
		},
		{
			"negative size",
			func(h header, data []byte) (header, []byte) { h["total_size"] = -40_000; return h, data },
			"peer claims that the metadata is -40000 bytes long",
		},
		{
			"enormous size",
			func(h header, data []byte) (header, []byte) { h["total_size"] = int64(1) << 40; return h, data },
			"peer claims that the metadata is 1099511627776 bytes long",
		},
		{
			"size that changes from one piece to the next",
			func(h header, data []byte) (header, []byte) {
				if h["piece"] == int64(1) {
					h["total_size"] = 50_000
				}
				return h, data
			},
			"peer changed its mind about the size of the metadata, from 40000 bytes to 50000",
		},
		{
			"piece that's too short",
			func(h header, data []byte) (header, []byte) { return h, data[:len(data)-1] },
			"peer sent 16383 bytes for a piece of metadata of 16384 bytes",
		},
		{
			"piece that's too long",
			func(h header, data []byte) (header, []byte) { return h, append(bytes.Clone(data), 0) },
			"peer sent 16385 bytes for a piece of metadata of 16384 bytes",
		},
		{
			"piece without any data",
			func(h header, data []byte) (header, []byte) { return h, nil },
			"peer sent 0 bytes for a piece of metadata of 16384 bytes",
		},
		{
			"last piece that's padded to the full size",
			func(h header, data []byte) (header, []byte) {
				if h["piece"] == int64(2) {
					data = append(bytes.Clone(data), make([]byte, 16384-len(data))...)
				}
				return h, data
			},
			"peer sent 16384 bytes for a piece of metadata of 7232 bytes",
		},
		{
			"size smaller than the first piece",
			func(h header, data []byte) (header, []byte) { h["total_size"] = 100; return h, data },
			"peer sent 16384 bytes for a piece of metadata of 100 bytes",
		},
		{
			"no size",
			func(h header, data []byte) (header, []byte) { delete(h, "total_size"); return h, data },
			`peer sent a malformed metadata message: missing "total_size"`,
		},
		{
			"no piece index",
			func(h header, data []byte) (header, []byte) { delete(h, "piece"); return h, data },
			`peer sent a malformed metadata message: missing "piece"`,
		},
		{
			"no message type",
			func(h header, data []byte) (header, []byte) { delete(h, "msg_type"); return h, data },
			`peer sent a malformed metadata message: missing "msg_type"`,
		},
		{
			"message type that isn't a number",
			func(h header, data []byte) (header, []byte) { h["msg_type"] = "data"; return h, data },
			`peer sent a malformed metadata message: "msg_type" is not an integer`,
		},
		{
			"rejection",
			func(h header, data []byte) (header, []byte) { return header{"msg_type": 2, "piece": h["piece"]}, nil },
			"peer turned down the request for piece 0 of the metadata",
		},
		{
			"rejection part-way through",
			func(h header, data []byte) (header, []byte) {
				if h["piece"] == int64(2) {
					return header{"msg_type": 2, "piece": 2}, nil
				}
				return h, data
			},
			"peer turned down the request for piece 2 of the metadata",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer, _ := metadataPeer(t, metadata, func(peer *testPeer) { peer.MetadataTamper = tt.tamper })

			got, err := peer.downloadMetadata(sha1.Sum(metadata))
			if err == nil {
				t.Fatalf("downloadMetadata returned %d bytes, want an error containing %q", len(got), tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("downloadMetadata error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// The metadata must be the one that the info hash stands for, however
// convincing what a peer sends may otherwise be.
func TestDownloadMetadataChecksInfoHash(t *testing.T) {
	metadata := []byte(minimalInfo)
	peer, _ := metadataPeer(t, metadata, nil)

	got, err := peer.downloadMetadata([20]byte{0xaa})
	if err == nil || !strings.Contains(err.Error(), "metadata from peer doesn't match the torrent's info hash") {
		t.Errorf("downloadMetadata = %q, %v, want an error about the info hash", got, err)
	}
}

func TestDownloadMetadataFromPeerWithoutIt(t *testing.T) {
	peer, _ := metadataPeer(t, nil, nil)

	_, err := peer.downloadMetadata([20]byte{})
	if err == nil || !strings.Contains(err.Error(), "peer turned down the request for piece 0 of the metadata") {
		t.Errorf("downloadMetadata error = %v, want one saying that the request was turned down", err)
	}
}

// scriptedMetadataPeer returns a connection to a peer that goes through the
// handshakes, waits for a metadata request, and then follows the given
// script. The script's send function sends a metadata extension message with
// the given payload.
func scriptedMetadataPeer(t *testing.T, infoHash [20]byte, script func(conn net.Conn, send func(payload string))) *peerConn {
	t.Helper()

	address := startTestServer(t, func(conn net.Conn) {
		if _, err := readHandshake(conn); err != nil {
			return
		}
		conn.Write(handshake{Reserved: extensionsReserved, InfoHash: infoHash, PeerID: testPeerID}.encode())
		conn.Write(message{ID: msgBitfield, Payload: []byte{0x80}}.encode())
		if _, err := readMessage(conn); err != nil { // The client's extension handshake
			return
		}
		conn.Write(extensionMessage(0, []byte("d1:md11:ut_metadatai42eee")).encode())
		if _, err := readMessage(conn); err != nil { // The metadata request
			return
		}

		script(conn, func(payload string) {
			conn.Write(extensionMessage(ourMetadataID, []byte(payload)).encode())
		})
		io.Copy(io.Discard, conn)
	})

	peer := dialWithExtensions(t, address, infoHash)
	if err := peer.handshakeExtensions(); err != nil {
		t.Fatalf("handshakeExtensions returned error: %v", err)
	}
	return peer
}

// While waiting for a piece of metadata, anything else that the peer sends is
// passed over, including metadata messages that aren't an answer.
func TestDownloadMetadataIgnoresUnrelatedMessages(t *testing.T) {
	metadata := []byte(minimalInfo)
	infoHash := sha1.Sum(metadata)

	peer := scriptedMetadataPeer(t, infoHash, func(conn net.Conn, send func(payload string)) {
		conn.Write([]byte{0, 0, 0, 0})                                         // A keep-alive
		conn.Write(message{ID: msgHave, Payload: []byte{0, 0, 0, 0}}.encode()) // A message of the base protocol
		conn.Write(extensionMessage(9, []byte("for another extension")).encode())
		conn.Write(extensionMessage(0, []byte("d1:md6:ut_pexi2eee")).encode()) // A further extension handshake
		send("d8:msg_typei0e5:piecei0ee")                                      // A request of the peer's own
		send("d8:msg_typei3e5:piecei0ee")                                      // A type of message that doesn't exist yet
		send(fmt.Sprintf("d8:msg_typei1e5:piecei0e10:total_sizei%dee%s", len(metadata), metadata))
	})

	got, err := peer.downloadMetadata(infoHash)
	if err != nil {
		t.Fatalf("downloadMetadata returned error: %v", err)
	}
	if !bytes.Equal(got, metadata) {
		t.Errorf("downloadMetadata = %q, want %q", got, metadata)
	}

	// What the unrelated messages had to say was still taken in.
	if !peer.hasPiece(0) {
		t.Error("the have message that arrived while waiting wasn't taken in")
	}
	if id := peer.extensionIDs["ut_pex"]; id != 2 {
		t.Errorf("the ID of the extension added while waiting is %d, want 2", id)
	}
}

func TestDownloadMetadataFromUncooperativePeer(t *testing.T) {
	shortenTimeout(t, &peerMessageTimeout)

	metadata := []byte(minimalInfo)
	infoHash := sha1.Sum(metadata)

	tests := []struct {
		name    string
		script  func(conn net.Conn, send func(payload string))
		wantErr string
	}{
		{"never answers", func(conn net.Conn, send func(payload string)) {}, "i/o timeout"},
		{"hangs up", func(conn net.Conn, send func(payload string)) { conn.Close() }, "receiving message from peer"},
		{
			"only sends unrelated messages",
			func(conn net.Conn, send func(payload string)) {
				send("d8:msg_typei3e5:piecei0ee")
				conn.Write(message{ID: msgUnchoke}.encode())
			},
			"i/o timeout",
		},
		{
			"sends something that isn't bencoded",
			func(conn net.Conn, send func(payload string)) { send("metadata, as requested") },
			"peer sent a malformed metadata message",
		},
		{
			"sends an empty message",
			func(conn net.Conn, send func(payload string)) { send("") },
			"peer sent a malformed metadata message",
		},
		{
			"sends a list",
			func(conn net.Conn, send func(payload string)) { send("li1ei0ee") },
			"peer sent a malformed metadata message: not a dictionary",
		},
		{
			"sends a dictionary that's cut short",
			func(conn net.Conn, send func(payload string)) { send("d8:msg_typei1e5:piecei0e") },
			"peer sent a malformed metadata message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := scriptedMetadataPeer(t, infoHash, tt.script)

			got, err := peer.downloadMetadata(infoHash)
			if err == nil {
				t.Fatalf("downloadMetadata = %q, want an error containing %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("downloadMetadata error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseMetadataMessage(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    metadataMessage
	}{
		{
			"data",
			"d8:msg_typei1e5:piecei0e10:total_sizei3425eeABCDEF",
			metadataMessage{Type: 1, Piece: 0, TotalSize: 3425, Data: []byte("ABCDEF")},
		},
		{
			"data that looks like bencode",
			"d8:msg_typei1e5:piecei2e10:total_sizei40000eed4:name1:ae",
			metadataMessage{Type: 1, Piece: 2, TotalSize: 40000, Data: []byte("d4:name1:ae")},
		},
		{
			"data message without data",
			"d8:msg_typei1e5:piecei0e10:total_sizei10ee",
			metadataMessage{Type: 1, Piece: 0, TotalSize: 10, Data: []byte{}},
		},
		{
			"keys in another order, with extras",
			"d10:total_sizei7e5:piecei1e5:extra3:abc8:msg_typei1eexyz",
			metadataMessage{Type: 1, Piece: 1, TotalSize: 7, Data: []byte("xyz")},
		},
		{"request", "d8:msg_typei0e5:piecei4ee", metadataMessage{Type: 0, Piece: 4}},
		{"reject", "d8:msg_typei2e5:piecei4ee", metadataMessage{Type: 2, Piece: 4}},
		{"unknown type", "d8:msg_typei7e5:piecei0ee", metadataMessage{Type: 7, Piece: 0}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMetadataMessage([]byte(tt.payload))
			if err != nil {
				t.Fatalf("parseMetadataMessage(%q) returned error: %v", tt.payload, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseMetadataMessage(%q) = %+v, want %+v", tt.payload, got, tt.want)
			}
		})
	}
}
