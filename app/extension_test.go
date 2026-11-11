package main

import (
	"context"
	"io"
	"net"
	"os"
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
	fake := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved})

	peer, err := dialPeer(context.Background(), fake.Address,
		handshake{Reserved: extensionsReserved, InfoHash: infoHash, PeerID: newPeerID()})
	if err != nil {
		t.Fatalf("dialPeer returned error: %v", err)
	}
	if err := peer.handshakeExtensions(); err != nil {
		t.Fatalf("handshakeExtensions returned error: %v", err)
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
		seen.received, seen.err = readMessage(conn)
		io.Copy(io.Discard, conn)
	})

	peer, err := dialPeer(context.Background(), address,
		handshake{Reserved: extensionsReserved, InfoHash: infoHash, PeerID: newPeerID()})
	if err != nil {
		t.Fatalf("dialPeer returned error: %v", err)
	}
	if err := peer.handshakeExtensions(); err != nil {
		t.Fatalf("handshakeExtensions returned error: %v", err)
	}
	if !peer.hasPiece(0) {
		t.Error("the bitfield that the peer sent first wasn't taken in")
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer, err := dialPeer(context.Background(), tt.address,
				handshake{Reserved: extensionsReserved, InfoHash: infoHash, PeerID: newPeerID()})
			if err != nil {
				t.Fatalf("dialPeer returned error: %v", err)
			}
			defer peer.Close()

			err = peer.handshakeExtensions()
			if err == nil {
				t.Fatalf("handshakeExtensions succeeded, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("handshakeExtensions error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
