package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// testPeerID is the peer ID of the fake peers that tests talk to.
var testPeerID = [20]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}

// startTestServer starts a TCP server on the loopback interface that calls
// handle for each incoming connection, and returns the server's address. Each
// connection is closed when handle returns, and the server is shut down when
// the test ends.
func startTestServer(t *testing.T, handle func(conn net.Conn)) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // The listener was closed
			}
			go func() {
				defer conn.Close()
				handle(conn)
			}()
		}
	}()

	return listener.Addr().String()
}

// testPeer is a fake peer that shares a single torrent.
type testPeer struct {
	Address string

	// Handshakes receives the handshake of each client that connects.
	Handshakes chan handshake
}

// startTestPeer starts a fake peer for the torrent with the given info hash.
// The peer is shut down when the test ends.
func startTestPeer(t *testing.T, infoHash [20]byte) *testPeer {
	t.Helper()

	peer := &testPeer{Handshakes: make(chan handshake, 16)}
	peer.Address = startTestServer(t, func(conn net.Conn) {
		theirs, err := readHandshake(conn)
		if err != nil {
			return
		}
		select {
		case peer.Handshakes <- theirs:
		default: // Nobody is checking the handshakes
		}

		conn.Write(handshake{InfoHash: infoHash, PeerID: testPeerID}.encode())
		io.Copy(io.Discard, conn) // Stays connected until the client hangs up
	})
	return peer
}

// shortenTimeout sets a timeout variable to a small value for the duration of
// the test, so that tests of unresponsive peers finish quickly.
func shortenTimeout(t *testing.T, timeout *time.Duration) {
	t.Helper()

	original := *timeout
	*timeout = 200 * time.Millisecond
	t.Cleanup(func() { *timeout = original })
}

// A handshake with distinctive values in every field, and its wire encoding.
var (
	exampleHandshake = handshake{
		Reserved: [8]byte{0, 0, 0, 0, 0, 0x10, 0, 0x01},
		InfoHash: [20]byte([]byte("\xd6\x9f\x91\xe6\xb2\xae\x4c\x54\x24\x68\xd1\x07\x3a\x71\xd4\xea\x13\x87\x9a\x7f")),
		PeerID:   [20]byte([]byte("-TS0001-abcdefghijkl")),
	}

	exampleHandshakeEncoded = "\x13BitTorrent protocol" +
		"\x00\x00\x00\x00\x00\x10\x00\x01" +
		"\xd6\x9f\x91\xe6\xb2\xae\x4c\x54\x24\x68\xd1\x07\x3a\x71\xd4\xea\x13\x87\x9a\x7f" +
		"-TS0001-abcdefghijkl"
)

func TestHandshakeEncode(t *testing.T) {
	got := exampleHandshake.encode()

	if len(got) != 68 {
		t.Errorf("encoded handshake is %d bytes long, want 68", len(got))
	}
	if string(got) != exampleHandshakeEncoded {
		t.Errorf("encode() = %q, want %q", got, exampleHandshakeEncoded)
	}
}

func TestReadHandshake(t *testing.T) {
	// Whatever follows the handshake belongs to the next message, and must
	// be left unread.
	reader := strings.NewReader(exampleHandshakeEncoded + "next message")

	got, err := readHandshake(reader)
	if err != nil {
		t.Fatalf("readHandshake returned error: %v", err)
	}
	if got != exampleHandshake {
		t.Errorf("readHandshake = %+v, want %+v", got, exampleHandshake)
	}

	if rest, _ := io.ReadAll(reader); string(rest) != "next message" {
		t.Errorf("readHandshake left %q unread, want %q", rest, "next message")
	}
}

func TestReadHandshakeErrors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"nothing", "", "EOF"},
		{"truncated", exampleHandshakeEncoded[:67], "unexpected EOF"},
		{"wrong protocol name length", "\x12" + exampleHandshakeEncoded[1:], "not a BitTorrent handshake"},
		{"wrong protocol name", "\x13BitTorrent protocoL" + exampleHandshakeEncoded[20:], "not a BitTorrent handshake"},
		{"another protocol", "HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n", "not a BitTorrent handshake"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readHandshake(strings.NewReader(tt.input))
			if err == nil {
				t.Fatalf("readHandshake = %+v, want an error containing %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("readHandshake error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestDialPeer(t *testing.T) {
	infoHash := exampleHandshake.InfoHash
	ourID := [20]byte([]byte("-CC0001-0123456789ab"))

	// This peer is scripted byte by byte, rather than built from the code
	// under test, to pin down the exact wire format.
	received := make(chan []byte, 1)
	address := startTestServer(t, func(conn net.Conn) {
		request := make([]byte, 68)
		if _, err := io.ReadFull(conn, request); err != nil {
			return
		}
		received <- request

		io.WriteString(conn, "\x13BitTorrent protocol"+
			"\x00\x00\x00\x00\x00\x00\x00\x00"+
			string(infoHash[:])+
			string(testPeerID[:]))
		io.Copy(io.Discard, conn)
	})

	peer, err := dialPeer(address, infoHash, ourID)
	if err != nil {
		t.Fatalf("dialPeer returned error: %v", err)
	}
	defer peer.Close()

	if peer.ID != testPeerID {
		t.Errorf("peer ID = %x, want %x", peer.ID, testPeerID)
	}

	want := "\x13BitTorrent protocol" +
		"\x00\x00\x00\x00\x00\x00\x00\x00" + // The reserved bytes are all zero
		string(infoHash[:]) +
		"-CC0001-0123456789ab"
	if got := <-received; string(got) != want {
		t.Errorf("peer received the handshake %q, want %q", got, want)
	}
}

func TestDialPeerErrors(t *testing.T) {
	shortenTimeout(t, &peerHandshakeTimeout)

	infoHash := exampleHandshake.InfoHash
	otherInfoHash := [20]byte{0xff}

	// respondWith returns a connection handler that reads the client's
	// handshake, sends the given response and then waits for the client to
	// hang up.
	respondWith := func(response string) func(net.Conn) {
		return func(conn net.Conn) {
			if _, err := io.ReadFull(conn, make([]byte, 68)); err != nil {
				return
			}
			io.WriteString(conn, response)
			io.Copy(io.Discard, conn)
		}
	}
	validResponse := string(handshake{InfoHash: infoHash, PeerID: testPeerID}.encode())

	// A peer that was listening, but no longer is.
	stopped, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped.Close()

	tests := []struct {
		name    string
		address string
		wantErr string
	}{
		{
			"peer has a different torrent",
			startTestServer(t, respondWith(string(handshake{InfoHash: otherInfoHash, PeerID: testPeerID}.encode()))),
			"peer answered for a different torrent (info hash ff000000",
		},
		{
			"peer hangs up without answering",
			startTestServer(t, func(conn net.Conn) { io.ReadFull(conn, make([]byte, 68)) }),
			"reading handshake: EOF",
		},
		{
			"peer hangs up as soon as it's connected to",
			startTestServer(t, func(conn net.Conn) {}),
			"handshake with peer",
		},
		{
			"peer hangs up in the middle of its handshake",
			startTestServer(t, func(conn net.Conn) {
				io.ReadFull(conn, make([]byte, 68))
				io.WriteString(conn, validResponse[:40])
			}),
			"reading handshake: unexpected EOF",
		},
		{
			"peer speaks another protocol",
			startTestServer(t, respondWith("HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n")),
			"not a BitTorrent handshake",
		},
		{
			"peer never answers",
			startTestServer(t, respondWith("")),
			"i/o timeout",
		},
		{
			"peer stalls in the middle of its handshake",
			startTestServer(t, respondWith(validResponse[:67])),
			"i/o timeout",
		},
		{"nothing is listening", stopped.Addr().String(), "connection refused"},
		{"address without a port", "127.0.0.1", "missing port"},
		{"empty address", "", "missing address"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer, err := dialPeer(tt.address, infoHash, newPeerID())
			if err == nil {
				peer.Close()
				t.Fatalf("dialPeer succeeded, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("dialPeer error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// A peer that doesn't answer must be given up on after the handshake timeout,
// rather than waited for indefinitely.
func TestDialPeerHandshakeTimeout(t *testing.T) {
	shortenTimeout(t, &peerHandshakeTimeout)

	address := startTestServer(t, func(conn net.Conn) { io.Copy(io.Discard, conn) })

	start := time.Now()
	_, err := dialPeer(address, [20]byte{}, newPeerID())
	elapsed := time.Since(start)

	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("dialPeer error = %v, want a timeout", err)
	}
	if elapsed < peerHandshakeTimeout || elapsed > 20*peerHandshakeTimeout {
		t.Errorf("dialPeer gave up after %v, want it to take about %v", elapsed, peerHandshakeTimeout)
	}
}

// Once the handshake is done, its timeout must no longer apply to the
// connection.
func TestDialPeerClearsHandshakeDeadline(t *testing.T) {
	shortenTimeout(t, &peerHandshakeTimeout)

	infoHash := exampleHandshake.InfoHash
	address := startTestServer(t, func(conn net.Conn) {
		if _, err := readHandshake(conn); err != nil {
			return
		}
		conn.Write(handshake{InfoHash: infoHash, PeerID: testPeerID}.encode())

		time.Sleep(2 * peerHandshakeTimeout)
		io.WriteString(conn, "late")
		io.Copy(io.Discard, conn)
	})

	peer, err := dialPeer(address, infoHash, newPeerID())
	if err != nil {
		t.Fatalf("dialPeer returned error: %v", err)
	}
	defer peer.Close()

	got := make([]byte, 4)
	if _, err := io.ReadFull(peer.conn, got); err != nil {
		t.Fatalf("reading after the handshake timeout had passed returned error: %v", err)
	}
	if !bytes.Equal(got, []byte("late")) {
		t.Errorf("read %q, want %q", got, "late")
	}
}

func TestTestPeerRecordsHandshakes(t *testing.T) {
	infoHash := exampleHandshake.InfoHash
	ourID := newPeerID()
	fake := startTestPeer(t, infoHash)

	peer, err := dialPeer(fake.Address, infoHash, ourID)
	if err != nil {
		t.Fatalf("dialPeer returned error: %v", err)
	}
	defer peer.Close()

	if peer.ID != testPeerID {
		t.Errorf("peer ID = %x, want %x", peer.ID, testPeerID)
	}
	if got, want := <-fake.Handshakes, (handshake{InfoHash: infoHash, PeerID: ourID}); got != want {
		t.Errorf("peer received the handshake %+v, want %+v", got, want)
	}
}
