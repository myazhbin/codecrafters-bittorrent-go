package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
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

// stoppedServerAddress returns the address of a server that was listening,
// but no longer is.
func stoppedServerAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	return listener.Addr().String()
}

// shortenTimeout sets a timeout variable to a small value for the duration of
// the test, so that tests of unresponsive peers finish quickly.
func shortenTimeout(t *testing.T, timeout *time.Duration) {
	t.Helper()

	original := *timeout
	*timeout = 200 * time.Millisecond
	t.Cleanup(func() { *timeout = original })
}

// blockRequest is the payload of a request message: a block of a piece that a
// client asked for.
type blockRequest struct {
	Index, Begin, Length uint32
}

// testPeer is a fake peer that shares a single torrent. Unless told otherwise
// by the fields that alter its behavior, it's a seeder that unchokes every
// client that's interested and serves every block that it's asked for.
type testPeer struct {
	// What the peer shares. Content and PieceLength may be left unset by tests
	// that only exchange handshakes.
	InfoHash    [20]byte
	Content     []byte
	PieceLength int

	// Bitfield, if not nil, is sent instead of a bitfield that has every piece.
	Bitfield []byte

	// NoBitfield stops the peer from sending a bitfield at all.
	NoBitfield bool

	// NeverUnchoke makes the peer keep every client choked.
	NeverUnchoke bool

	// Chatty makes the peer intersperse the conversation with messages that
	// clients should take in their stride: keep-alives, a have message for
	// piece 0, and messages of a type that no client knows.
	Chatty bool

	// WriteChunkSize, if positive, makes the peer send everything in chunks of
	// that many bytes, so that the client sees messages arrive in fragments.
	WriteChunkSize int

	// RequireFullPipeline makes the peer hold off on serving a block until
	// the client has as many requests outstanding as it's supposed to keep
	// in flight. A client that doesn't pipeline its requests gets stuck.
	RequireFullPipeline bool

	// ChokeAfterBlocks, if positive, makes the peer choke the client once it
	// has served that many blocks, forgetting the client's other requests as
	// it does so, and then unchoke it again straight away.
	ChokeAfterBlocks int

	// HangUpAfterBlocks, if positive, makes the peer close the connection once
	// it has served that many blocks.
	HangUpAfterBlocks int

	// Tamper, if not nil, is given each block that the peer is about to send,
	// and returns what to send in its place.
	Tamper func(index, begin uint32, data []byte) (uint32, uint32, []byte)

	// The fields below are set up by startTestPeer.

	Address string

	// Handshakes receives the handshake of each client that connects.
	Handshakes chan handshake

	// Requests receives each block request that clients send.
	Requests chan blockRequest

	// PipelineOverflowed records whether a client ever had more requests
	// outstanding than it's supposed to. It's only tracked if
	// RequireFullPipeline is set.
	PipelineOverflowed atomic.Bool
}

// startTestPeer starts the given fake peer, which is shut down when the test
// ends. For convenience, it returns the peer that it was given.
func startTestPeer(t *testing.T, peer *testPeer) *testPeer {
	t.Helper()

	peer.Handshakes = make(chan handshake, 64)
	peer.Requests = make(chan blockRequest, 4096)
	peer.Address = startTestServer(t, peer.serve)
	return peer
}

// serve talks to a client that connected to the peer.
func (p *testPeer) serve(conn net.Conn) {
	theirs, err := readHandshake(conn)
	if err != nil {
		return
	}
	select {
	case p.Handshakes <- theirs:
	default: // The buffer is full, and evidently nobody is checking
	}

	var out io.Writer = conn
	if p.WriteChunkSize > 0 {
		out = chunkWriter{conn, p.WriteChunkSize}
	}
	send := func(m message) { out.Write(m.encode()) }
	chatter := func() {
		if p.Chatty {
			out.Write([]byte{0, 0, 0, 0}) // A keep-alive
			send(message{ID: msgHave, Payload: []byte{0, 0, 0, 0}})
			send(message{ID: 99, Payload: []byte("a message from the future")})
		}
	}

	out.Write(handshake{InfoHash: p.InfoHash, PeerID: testPeerID}.encode())
	if !p.NoBitfield {
		send(message{ID: msgBitfield, Payload: p.bitfield()})
	}
	chatter()

	reader := bufio.NewReader(conn)
	choking := true
	var pending []blockRequest // Requests that are yet to be answered
	served := map[uint32]int{} // Number of blocks served, by piece
	totalServed := 0
	for {
		m, err := readMessage(reader)
		if err != nil {
			return // The client hung up
		}

		switch m.ID {
		case msgInterested:
			if !p.NeverUnchoke {
				chatter()
				send(message{ID: msgUnchoke})
				choking = false
			}

		case msgRequest:
			if len(m.Payload) != 12 {
				return
			}
			request := blockRequest{
				Index:  binary.BigEndian.Uint32(m.Payload[0:4]),
				Begin:  binary.BigEndian.Uint32(m.Payload[4:8]),
				Length: binary.BigEndian.Uint32(m.Payload[8:12]),
			}
			select {
			case p.Requests <- request:
			default:
			}
			if !choking { // Requests from choked clients are ignored
				pending = append(pending, request)
			}
		}

		for len(pending) > 0 && p.readyToServe(conn, reader, pending, served) {
			request := pending[0]
			pending = pending[1:]

			data, ok := p.block(request)
			if !ok {
				return // A request for data that doesn't exist
			}
			index, begin := request.Index, request.Begin
			if p.Tamper != nil {
				index, begin, data = p.Tamper(index, begin, data)
			}

			chatter()
			payload := binary.BigEndian.AppendUint32(nil, index)
			payload = binary.BigEndian.AppendUint32(payload, begin)
			send(message{ID: msgPiece, Payload: append(payload, data...)})
			served[request.Index]++
			totalServed++

			if totalServed == p.HangUpAfterBlocks {
				return
			}
			if totalServed == p.ChokeAfterBlocks {
				send(message{ID: msgChoke})
				pending = nil
				send(message{ID: msgUnchoke})
			}
		}
	}
}

// bitfield returns the payload of the peer's bitfield message.
func (p *testPeer) bitfield() []byte {
	if p.Bitfield != nil {
		return p.Bitfield
	}

	numPieces := 0
	if p.PieceLength > 0 {
		numPieces = (len(p.Content) + p.PieceLength - 1) / p.PieceLength
	}
	bitfield := make([]byte, (numPieces+7)/8)
	for piece := 0; piece < numPieces; piece++ {
		bitfield[piece/8] |= 0x80 >> (piece % 8)
	}
	return bitfield
}

// block returns the data that a request asks for, if it's a valid request.
func (p *testPeer) block(request blockRequest) ([]byte, bool) {
	start := int64(request.Index)*int64(p.PieceLength) + int64(request.Begin)
	end := start + int64(request.Length)
	pieceEnd := min((int64(request.Index)+1)*int64(p.PieceLength), int64(len(p.Content)))
	if request.Length == 0 || request.Length > blockSize || end > pieceEnd {
		return nil, false
	}
	return p.Content[start:end], true
}

// readyToServe reports whether the peer should answer the oldest of the
// pending requests now, rather than wait for more requests to arrive first.
func (p *testPeer) readyToServe(conn net.Conn, reader *bufio.Reader, pending []blockRequest, served map[uint32]int) bool {
	if !p.RequireFullPipeline {
		return true
	}

	// The client is expected to keep maxPipelinedRequests requests in flight
	// for as long as the piece has that many blocks left.
	index := pending[0].Index
	pieceSize := min(p.PieceLength, len(p.Content)-int(index)*p.PieceLength)
	blocksLeft := (pieceSize+blockSize-1)/blockSize - served[index]
	if len(pending) < min(maxPipelinedRequests, blocksLeft) {
		return false
	}

	// With the pipeline full, the client mustn't be sending any more
	// requests. Give it a moment to prove otherwise.
	if len(pending) >= maxPipelinedRequests {
		conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
		if _, err := reader.Peek(1); err == nil || len(pending) > maxPipelinedRequests {
			p.PipelineOverflowed.Store(true)
		}
		conn.SetReadDeadline(time.Time{})
	}
	return true
}

// chunkWriter writes data a few bytes at a time, pausing after each chunk,
// so that it reaches the other end of a connection in fragments.
type chunkWriter struct {
	w    io.Writer
	size int
}

func (c chunkWriter) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		n, err := c.w.Write(data[:min(c.size, len(data))])
		written += n
		if err != nil {
			return written, err
		}
		data = data[n:]
		time.Sleep(50 * time.Microsecond)
	}
	return written, nil
}

// drainRequests returns the block requests that a fake peer has received so
// far.
func drainRequests(peer *testPeer) []blockRequest {
	var requests []blockRequest
	for {
		select {
		case request := <-peer.Requests:
			requests = append(requests, request)
		default:
			return requests
		}
	}
}

// newTestTorrent returns a torrent for a file with the given contents.
func newTestTorrent(t *testing.T, content []byte, pieceLength int) *Torrent {
	t.Helper()

	torrent, err := parseTorrent(testTorrentFile("http://tracker.example/announce", testInfoDict(content, pieceLength)))
	if err != nil {
		t.Fatal(err)
	}
	return torrent
}

// dialTestPeer connects to a fake peer. The connection is closed when the
// test ends.
func dialTestPeer(t *testing.T, peer *testPeer) *peerConn {
	t.Helper()

	conn, err := dialPeer(peer.Address, peer.InfoHash, newPeerID())
	if err != nil {
		t.Fatalf("dialPeer returned error: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
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
		{"nothing is listening", stoppedServerAddress(t), "connection refused"},
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
	fake := startTestPeer(t, &testPeer{InfoHash: infoHash})

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

func TestMessageEncode(t *testing.T) {
	tests := []struct {
		name    string
		message message
		want    string
	}{
		{"interested", message{ID: msgInterested}, "\x00\x00\x00\x01\x02"},
		{"unchoke", message{ID: msgUnchoke}, "\x00\x00\x00\x01\x01"},
		{"have", message{ID: msgHave, Payload: []byte{0, 0, 1, 2}}, "\x00\x00\x00\x05\x04\x00\x00\x01\x02"},
		{
			"request for a whole block",
			requestMessage(1, 16384, 16384),
			"\x00\x00\x00\x0d\x06" + "\x00\x00\x00\x01" + "\x00\x00\x40\x00" + "\x00\x00\x40\x00",
		},
		{
			"request for a final, shorter block",
			requestMessage(0x01020304, 0x00058000, 0x1234),
			"\x00\x00\x00\x0d\x06" + "\x01\x02\x03\x04" + "\x00\x05\x80\x00" + "\x00\x00\x12\x34",
		},
		{
			"payload longer than 255 bytes",
			message{ID: msgPiece, Payload: bytes.Repeat([]byte{0xee}, 0x0123)},
			"\x00\x00\x01\x24\x07" + strings.Repeat("\xee", 0x0123),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.message.encode(); string(got) != tt.want {
				t.Errorf("encode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadMessage(t *testing.T) {
	input := "\x00\x00\x00\x01\x01" + // Unchoke
		"\x00\x00\x00\x00" + // A keep-alive, which is skipped
		"\x00\x00\x00\x00" + // And another
		"\x00\x00\x00\x0c\x07\x00\x00\x00\x02\x00\x00\x40\x00abc" + // A block of a piece
		"\x00\x00\x00\x03\x05\xff\xe0" + // Bitfield
		"\x00\x00\x00\x00" // A final keep-alive

	want := []message{
		{ID: msgUnchoke, Payload: []byte{}},
		{ID: msgPiece, Payload: []byte("\x00\x00\x00\x02\x00\x00\x40\x00abc")},
		{ID: msgBitfield, Payload: []byte{0xff, 0xe0}},
	}

	reader := strings.NewReader(input)
	for i, want := range want {
		got, err := readMessage(reader)
		if err != nil {
			t.Fatalf("reading message %d returned error: %v", i, err)
		}
		if got.ID != want.ID || !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("message %d = %+v, want %+v", i, got, want)
		}
	}

	// Reaching the end of the stream between messages is a clean EOF.
	if _, err := readMessage(reader); err != io.EOF {
		t.Errorf("reading past the last message returned error %v, want io.EOF", err)
	}
}

func TestReadMessageErrors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"truncated length", "\x00\x00\x00", "unexpected EOF"},
		{"missing body", "\x00\x00\x00\x05", "unexpected EOF"},
		{"truncated body", "\x00\x00\x00\x05\x04\x00\x00", "unexpected EOF"},
		{"oversized message", "\x7f\xff\xff\xff\x07", "over the limit"},
		{"message just over the limit", "\x00\x10\x00\x01\x07", "over the limit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readMessage(strings.NewReader(tt.input))
			if err == nil {
				t.Fatalf("readMessage = %+v, want an error containing %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("readMessage error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// A message of exactly the maximum size is still accepted.
func TestReadMessageAtSizeLimit(t *testing.T) {
	body := bytes.Repeat([]byte{msgPieceByte}, maxMessageSize)
	input := append([]byte{0x00, 0x10, 0x00, 0x00}, body...)

	got, err := readMessage(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("readMessage returned error: %v", err)
	}
	if got.ID != msgPiece || len(got.Payload) != maxMessageSize-1 {
		t.Errorf("readMessage = ID %d with a payload of %d bytes, want ID %d with %d bytes",
			got.ID, len(got.Payload), msgPiece, maxMessageSize-1)
	}
}

const msgPieceByte = byte(msgPiece)

func TestPeerPieceTracking(t *testing.T) {
	peer := &peerConn{bitfield: []byte{0b10100000, 0b00000001}}

	for index, want := range map[int]bool{
		0: true, 1: false, 2: true, 3: false, 7: false, 8: false, 14: false, 15: true,
		16: false, 1000: false, // Beyond the end of the bitfield
	} {
		if got := peer.hasPiece(index); got != want {
			t.Errorf("hasPiece(%d) = %v, want %v", index, got, want)
		}
	}

	peer.setHasPiece(1)
	peer.setHasPiece(8)
	peer.setHasPiece(37) // Beyond the end of the bitfield, which has to grow
	if want := []byte{0b11100000, 0b10000001, 0, 0, 0b00000100}; !bytes.Equal(peer.bitfield, want) {
		t.Errorf("bitfield = %08b, want %08b", peer.bitfield, want)
	}
	for _, index := range []int{0, 1, 2, 8, 15, 37} {
		if !peer.hasPiece(index) {
			t.Errorf("hasPiece(%d) = false, want true", index)
		}
	}
	for _, index := range []int{3, 9, 36, 38, 39, 40} {
		if peer.hasPiece(index) {
			t.Errorf("hasPiece(%d) = true, want false", index)
		}
	}

	// An absurd piece index must not make the bitfield balloon.
	peer.setHasPiece(1 << 31)
	if len(peer.bitfield) != 5 {
		t.Errorf("bitfield grew to %d bytes, want it to stay at 5", len(peer.bitfield))
	}
}

func TestDownloadPiece(t *testing.T) {
	// Pieces of 40,000 bytes are two whole blocks and one of 7,232 bytes. The
	// file's last piece is 20,000 bytes: one whole block and one of 3,616.
	content := testContent(100_000)
	torrent := newTestTorrent(t, content, 40_000)

	tests := []struct {
		index        int
		want         []byte
		wantRequests []blockRequest
	}{
		{0, content[:40_000], []blockRequest{{0, 0, 16384}, {0, 16384, 16384}, {0, 32768, 7232}}},
		{1, content[40_000:80_000], []blockRequest{{1, 0, 16384}, {1, 16384, 16384}, {1, 32768, 7232}}},
		{2, content[80_000:], []blockRequest{{2, 0, 16384}, {2, 16384, 3616}}},
	}

	for _, tt := range tests {
		fake := startTestPeer(t, &testPeer{InfoHash: torrent.InfoHash, Content: content, PieceLength: 40_000})
		peer := dialTestPeer(t, fake)

		got, err := peer.downloadPiece(torrent, tt.index)
		if err != nil {
			t.Fatalf("downloadPiece(%d) returned error: %v", tt.index, err)
		}
		if !bytes.Equal(got, tt.want) {
			t.Errorf("downloadPiece(%d) returned the wrong data (%d bytes, want %d)", tt.index, len(got), len(tt.want))
		}
		if requests := drainRequests(fake); !reflect.DeepEqual(requests, tt.wantRequests) {
			t.Errorf("downloadPiece(%d) requested %v, want %v", tt.index, requests, tt.wantRequests)
		}
	}
}

// Several pieces can be downloaded over one connection, in any order.
func TestDownloadPieceReusesConnection(t *testing.T) {
	content := testContent(100_000)
	torrent := newTestTorrent(t, content, 40_000)
	fake := startTestPeer(t, &testPeer{InfoHash: torrent.InfoHash, Content: content, PieceLength: 40_000})
	peer := dialTestPeer(t, fake)

	for _, index := range []int{2, 0, 1, 0} {
		got, err := peer.downloadPiece(torrent, index)
		if err != nil {
			t.Fatalf("downloadPiece(%d) returned error: %v", index, err)
		}
		if want := content[index*40_000 : min((index+1)*40_000, len(content))]; !bytes.Equal(got, want) {
			t.Errorf("downloadPiece(%d) returned the wrong data", index)
		}
	}

	if got := len(fake.Handshakes); got != 1 {
		t.Errorf("the peer was connected to %d times, want once", got)
	}
}

func TestDownloadPieceSizes(t *testing.T) {
	tests := []struct {
		name        string
		length      int
		pieceLength int
		index       int
		wantBlocks  []uint32 // The length of each block that should be requested
	}{
		{"one byte", 1, 16384, 0, []uint32{1}},
		{"one byte short of a block", 16383, 16384, 0, []uint32{16383}},
		{"exactly one block", 16384, 16384, 0, []uint32{16384}},
		{"one byte over a block", 16385, 32768, 0, []uint32{16384, 1}},
		{"exactly two blocks", 32768, 32768, 0, []uint32{16384, 16384}},
		{"piece smaller than a block", 300, 100, 1, []uint32{100}},
		{"short last piece", 250, 100, 2, []uint32{50}},
		{"last piece of a file that's a multiple of the piece length", 65536, 32768, 1, []uint32{16384, 16384}},
		{"last piece with a short last block", 50_000, 32768, 1, []uint32{16384, 848}},
		{"whole file in one piece", 70_000, 262_144, 0, []uint32{16384, 16384, 16384, 16384, 4464}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := testContent(tt.length)
			torrent := newTestTorrent(t, content, tt.pieceLength)
			fake := startTestPeer(t, &testPeer{InfoHash: torrent.InfoHash, Content: content, PieceLength: tt.pieceLength})
			peer := dialTestPeer(t, fake)

			got, err := peer.downloadPiece(torrent, tt.index)
			if err != nil {
				t.Fatalf("downloadPiece returned error: %v", err)
			}

			start := tt.index * tt.pieceLength
			if want := content[start:min(start+tt.pieceLength, len(content))]; !bytes.Equal(got, want) {
				t.Errorf("downloadPiece returned the wrong data (%d bytes, want %d)", len(got), len(want))
			}

			var wantRequests []blockRequest
			for block, length := range tt.wantBlocks {
				wantRequests = append(wantRequests, blockRequest{uint32(tt.index), uint32(block * blockSize), length})
			}
			if requests := drainRequests(fake); !reflect.DeepEqual(requests, wantRequests) {
				t.Errorf("downloadPiece requested %v, want %v", requests, wantRequests)
			}
		})
	}
}

// Requests must be pipelined: five are kept in flight for as long as there
// are that many blocks left to ask for, and never more than that.
func TestDownloadPiecePipelinesRequests(t *testing.T) {
	shortenTimeout(t, &peerMessageTimeout) // So that a client that doesn't pipeline fails quickly

	for _, numBlocks := range []int{1, 4, 5, 6, 12} {
		content := testContent(numBlocks*blockSize - 100)
		torrent := newTestTorrent(t, content, len(content))
		fake := startTestPeer(t, &testPeer{
			InfoHash:            torrent.InfoHash,
			Content:             content,
			PieceLength:         len(content),
			RequireFullPipeline: true,
		})
		peer := dialTestPeer(t, fake)

		got, err := peer.downloadPiece(torrent, 0)
		if err != nil {
			t.Fatalf("downloading a piece of %d blocks returned error: %v", numBlocks, err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("downloading a piece of %d blocks returned the wrong data", numBlocks)
		}
		if fake.PipelineOverflowed.Load() {
			t.Errorf("downloading a piece of %d blocks had more than %d requests in flight", numBlocks, maxPipelinedRequests)
		}
		if got := len(drainRequests(fake)); got != numBlocks {
			t.Errorf("downloading a piece of %d blocks took %d requests", numBlocks, got)
		}
	}
}

// Messages don't arrive whole: the bytes of one can be spread over many reads.
func TestDownloadPieceFromFragmentedStream(t *testing.T) {
	for _, chunkSize := range []int{1, 3, 501, 16384 + 12} {
		content := testContent(40_000)
		if chunkSize < 100 {
			content = testContent(700) // Keeps the byte-at-a-time cases quick
		}
		torrent := newTestTorrent(t, content, len(content))
		fake := startTestPeer(t, &testPeer{
			InfoHash:       torrent.InfoHash,
			Content:        content,
			PieceLength:    len(content),
			WriteChunkSize: chunkSize,
			Chatty:         true,
		})
		peer := dialTestPeer(t, fake)

		got, err := peer.downloadPiece(torrent, 0)
		if err != nil {
			t.Fatalf("downloadPiece with %d-byte chunks returned error: %v", chunkSize, err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("downloadPiece with %d-byte chunks returned the wrong data", chunkSize)
		}
	}
}

// Keep-alives, have messages and messages of unknown types can turn up at any
// point, and must not get in the way.
func TestDownloadPieceIgnoresUnrelatedMessages(t *testing.T) {
	content := testContent(100_000)
	torrent := newTestTorrent(t, content, 40_000)
	fake := startTestPeer(t, &testPeer{InfoHash: torrent.InfoHash, Content: content, PieceLength: 40_000, Chatty: true})
	peer := dialTestPeer(t, fake)

	for index := range 3 {
		got, err := peer.downloadPiece(torrent, index)
		if err != nil {
			t.Fatalf("downloadPiece(%d) returned error: %v", index, err)
		}
		if want := content[index*40_000 : min((index+1)*40_000, len(content))]; !bytes.Equal(got, want) {
			t.Errorf("downloadPiece(%d) returned the wrong data", index)
		}
	}
}

// A peer that chokes us part-way through a piece drops the requests that it
// hasn't served yet. They must be sent again once it unchokes us.
func TestDownloadPieceSurvivesChoke(t *testing.T) {
	shortenTimeout(t, &peerMessageTimeout) // So that a client that doesn't re-request fails quickly

	content := testContent(8 * blockSize)
	torrent := newTestTorrent(t, content, len(content))
	fake := startTestPeer(t, &testPeer{
		InfoHash:            torrent.InfoHash,
		Content:             content,
		PieceLength:         len(content),
		RequireFullPipeline: true, // Guarantees that requests are outstanding when the choke comes
		ChokeAfterBlocks:    2,
	})
	peer := dialTestPeer(t, fake)

	got, err := peer.downloadPiece(torrent, 0)
	if err != nil {
		t.Fatalf("downloadPiece returned error: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("downloadPiece returned the wrong data")
	}

	// Blocks 2 to 5 were requested before the choke, and then dropped by it.
	timesRequested := map[uint32]int{}
	for _, request := range drainRequests(fake) {
		timesRequested[request.Begin/blockSize]++
	}
	for block := uint32(0); block < 8; block++ {
		want := 1
		if block >= 2 && block <= 5 {
			want = 2
		}
		if got := timesRequested[block]; got < want {
			t.Errorf("block %d was requested %d times, want at least %d", block, got, want)
		}
	}
}

func TestDownloadPieceRejectsBadData(t *testing.T) {
	content := testContent(40_000)
	torrent := newTestTorrent(t, content, len(content))

	tests := []struct {
		name    string
		tamper  func(index, begin uint32, data []byte) (uint32, uint32, []byte)
		wantErr string
	}{
		{
			"corrupted first block",
			func(index, begin uint32, data []byte) (uint32, uint32, []byte) {
				if begin == 0 {
					data = bytes.Clone(data)
					data[100] ^= 0x01
				}
				return index, begin, data
			},
			"piece 0 from peer failed its hash check",
		},
		{
			"corrupted last byte",
			func(index, begin uint32, data []byte) (uint32, uint32, []byte) {
				if begin == 32768 {
					data = bytes.Clone(data)
					data[len(data)-1] ^= 0x80
				}
				return index, begin, data
			},
			"piece 0 from peer failed its hash check",
		},
		{
			"block of another piece",
			func(index, begin uint32, data []byte) (uint32, uint32, []byte) { return 7, begin, data },
			"peer sent a block of piece 7, which wasn't requested",
		},
		{
			"block at an offset that isn't on a block boundary",
			func(index, begin uint32, data []byte) (uint32, uint32, []byte) { return index, begin + 1, data },
			"peer sent a block at offset 1, which wasn't requested",
		},
		{
			"block beyond the end of the piece",
			func(index, begin uint32, data []byte) (uint32, uint32, []byte) { return index, 49152, data },
			"peer sent a block at offset 49152, which wasn't requested",
		},
		{
			"block at a huge offset",
			func(index, begin uint32, data []byte) (uint32, uint32, []byte) { return index, 0xffffc000, data },
			"which wasn't requested",
		},
		{
			"block that's too short",
			func(index, begin uint32, data []byte) (uint32, uint32, []byte) {
				return index, begin, data[:len(data)-1]
			},
			"peer sent 16383 bytes for a block of 16384 bytes",
		},
		{
			"block that's too long",
			func(index, begin uint32, data []byte) (uint32, uint32, []byte) {
				return index, begin, append(bytes.Clone(data), 0)
			},
			"peer sent 16385 bytes for a block of 16384 bytes",
		},
		{
			"empty block",
			func(index, begin uint32, data []byte) (uint32, uint32, []byte) { return index, begin, nil },
			"peer sent 0 bytes for a block of 16384 bytes",
		},
		{
			"whole block in place of the short last one",
			func(index, begin uint32, data []byte) (uint32, uint32, []byte) {
				if begin == 32768 {
					data = make([]byte, blockSize)
				}
				return index, begin, data
			},
			"peer sent 16384 bytes for a block of 7232 bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := startTestPeer(t, &testPeer{
				InfoHash:    torrent.InfoHash,
				Content:     content,
				PieceLength: len(content),
				Tamper:      tt.tamper,
			})
			peer := dialTestPeer(t, fake)

			got, err := peer.downloadPiece(torrent, 0)
			if err == nil {
				t.Fatalf("downloadPiece returned %d bytes, want an error containing %q", len(got), tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("downloadPiece error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestDownloadPieceFromPeerWithoutThePiece(t *testing.T) {
	content := testContent(300)
	torrent := newTestTorrent(t, content, 100)

	t.Run("bitfield lacks the piece", func(t *testing.T) {
		fake := startTestPeer(t, &testPeer{
			InfoHash:    torrent.InfoHash,
			Content:     content,
			PieceLength: 100,
			Bitfield:    []byte{0b10100000}, // Pieces 0 and 2, but not 1
		})

		for index, wantErr := range map[int]string{0: "", 1: "peer doesn't have piece 1", 2: ""} {
			got, err := dialTestPeer(t, fake).downloadPiece(torrent, index)
			switch {
			case wantErr == "" && err != nil:
				t.Errorf("downloadPiece(%d) returned error: %v", index, err)
			case wantErr == "" && !bytes.Equal(got, content[index*100:(index+1)*100]):
				t.Errorf("downloadPiece(%d) returned the wrong data", index)
			case wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)):
				t.Errorf("downloadPiece(%d) error = %v, want one containing %q", index, err, wantErr)
			}
		}

		// The piece that the peer lacks must not have been asked for.
		for _, request := range drainRequests(fake) {
			if request.Index == 1 {
				t.Errorf("the peer was asked for %+v, though it doesn't have piece 1", request)
			}
		}
	})

	t.Run("no bitfield", func(t *testing.T) {
		fake := startTestPeer(t, &testPeer{InfoHash: torrent.InfoHash, Content: content, PieceLength: 100, NoBitfield: true})

		_, err := dialTestPeer(t, fake).downloadPiece(torrent, 0)
		if err == nil || !strings.Contains(err.Error(), "peer doesn't have piece 0") {
			t.Errorf("downloadPiece error = %v, want one saying that the peer doesn't have the piece", err)
		}
	})

	// A peer can also announce its pieces one at a time with have messages.
	// A chatty fake peer sends one for piece 0.
	t.Run("have message instead of bitfield", func(t *testing.T) {
		fake := startTestPeer(t, &testPeer{
			InfoHash:    torrent.InfoHash,
			Content:     content,
			PieceLength: 100,
			NoBitfield:  true,
			Chatty:      true,
		})

		got, err := dialTestPeer(t, fake).downloadPiece(torrent, 0)
		if err != nil {
			t.Fatalf("downloadPiece(0) returned error: %v", err)
		}
		if !bytes.Equal(got, content[:100]) {
			t.Error("downloadPiece(0) returned the wrong data")
		}

		_, err = dialTestPeer(t, fake).downloadPiece(torrent, 1)
		if err == nil || !strings.Contains(err.Error(), "peer doesn't have piece 1") {
			t.Errorf("downloadPiece(1) error = %v, want one saying that the peer doesn't have the piece", err)
		}
	})
}

func TestDownloadPieceFromUncooperativePeer(t *testing.T) {
	shortenTimeout(t, &peerMessageTimeout)

	content := testContent(40_000)
	torrent := newTestTorrent(t, content, len(content))

	// scripted returns the address of a peer that exchanges handshakes, sends
	// a bitfield, waits to be told that the client is interested, and then
	// follows the given script.
	scripted := func(script func(conn net.Conn)) string {
		return startTestServer(t, func(conn net.Conn) {
			if _, err := readHandshake(conn); err != nil {
				return
			}
			conn.Write(handshake{InfoHash: torrent.InfoHash, PeerID: testPeerID}.encode())
			conn.Write(message{ID: msgBitfield, Payload: []byte{0x80}}.encode())
			if m, err := readMessage(conn); err != nil || m.ID != msgInterested {
				return
			}
			script(conn)
			io.Copy(io.Discard, conn)
		})
	}
	seeder := func(peer *testPeer) string {
		peer.InfoHash, peer.Content, peer.PieceLength = torrent.InfoHash, content, len(content)
		return startTestPeer(t, peer).Address
	}

	tests := []struct {
		name    string
		address string
		wantErr string
	}{
		{"never unchokes", seeder(&testPeer{NeverUnchoke: true}), "i/o timeout"},
		{"hangs up part-way through the piece", seeder(&testPeer{HangUpAfterBlocks: 1}), "receiving message from peer"},
		{
			"goes quiet part-way through the piece",
			scripted(func(conn net.Conn) {
				conn.Write(message{ID: msgUnchoke}.encode())
				if _, err := readMessage(conn); err != nil {
					return
				}
				conn.Write(message{ID: msgPiece, Payload: append(make([]byte, 8), content[:blockSize]...)}.encode())
			}),
			"i/o timeout",
		},
		{
			"stalls in the middle of a block",
			scripted(func(conn net.Conn) {
				conn.Write(message{ID: msgUnchoke}.encode())
				block := message{ID: msgPiece, Payload: append(make([]byte, 8), content[:blockSize]...)}.encode()
				conn.Write(block[:len(block)/2])
			}),
			"i/o timeout",
		},
		{
			"chokes without unchoking again",
			scripted(func(conn net.Conn) {
				conn.Write(message{ID: msgUnchoke}.encode())
				conn.Write(message{ID: msgChoke}.encode())
			}),
			"i/o timeout",
		},
		{
			"sends an oversized message",
			scripted(func(conn net.Conn) { conn.Write([]byte{0x7f, 0xff, 0xff, 0xff, byte(msgPiece)}) }),
			"over the limit",
		},
		{
			"sends a truncated piece message",
			scripted(func(conn net.Conn) {
				conn.Write(message{ID: msgUnchoke}.encode())
				conn.Write(message{ID: msgPiece, Payload: []byte{0, 0, 0, 0, 0, 0, 0}}.encode())
			}),
			"truncated piece message",
		},
		{
			"sends a malformed have message",
			scripted(func(conn net.Conn) { conn.Write(message{ID: msgHave, Payload: []byte{0, 0, 0}}.encode()) }),
			"malformed have message",
		},
		{
			"hangs up straight after the handshake",
			startTestServer(t, func(conn net.Conn) {
				if _, err := readHandshake(conn); err == nil {
					conn.Write(handshake{InfoHash: torrent.InfoHash, PeerID: testPeerID}.encode())
				}
			}),
			"receiving message from peer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer, err := dialPeer(tt.address, torrent.InfoHash, newPeerID())
			if err != nil {
				t.Fatalf("dialPeer returned error: %v", err)
			}
			defer peer.Close()

			got, err := peer.downloadPiece(torrent, 0)
			if err == nil {
				t.Fatalf("downloadPiece returned %d bytes, want an error containing %q", len(got), tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("downloadPiece error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// A block that arrives twice, as can happen around a choke, is harmless.
func TestDownloadPieceToleratesDuplicateBlocks(t *testing.T) {
	content := testContent(3 * blockSize)
	torrent := newTestTorrent(t, content, len(content))

	address := startTestServer(t, func(conn net.Conn) {
		if _, err := readHandshake(conn); err != nil {
			return
		}
		conn.Write(handshake{InfoHash: torrent.InfoHash, PeerID: testPeerID}.encode())
		conn.Write(message{ID: msgBitfield, Payload: []byte{0x80}}.encode())
		conn.Write(message{ID: msgUnchoke}.encode())

		// Every block is sent twice in a row, starting with the last block.
		for block := 2; block >= 0; block-- {
			payload := binary.BigEndian.AppendUint32(make([]byte, 4), uint32(block*blockSize))
			payload = append(payload, content[block*blockSize:(block+1)*blockSize]...)
			for range 2 {
				conn.Write(message{ID: msgPiece, Payload: payload}.encode())
			}
		}
		io.Copy(io.Discard, conn)
	})

	peer, err := dialPeer(address, torrent.InfoHash, newPeerID())
	if err != nil {
		t.Fatalf("dialPeer returned error: %v", err)
	}
	defer peer.Close()

	got, err := peer.downloadPiece(torrent, 0)
	if err != nil {
		t.Fatalf("downloadPiece returned error: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("downloadPiece returned the wrong data")
	}
}

func TestTorrentPieceSize(t *testing.T) {
	tests := []struct {
		length      int
		pieceLength int
		want        []int64
	}{
		{100, 32, []int64{32, 32, 32, 4}},
		{96, 32, []int64{32, 32, 32}},
		{1, 32, []int64{1}},
		{32, 32, []int64{32}},
		{33, 32, []int64{32, 1}},
	}

	for _, tt := range tests {
		torrent := newTestTorrent(t, testContent(tt.length), tt.pieceLength)

		var got []int64
		for index := range torrent.PieceHashes {
			got = append(got, torrent.pieceSize(index))
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("piece sizes of a %d-byte file with %d-byte pieces = %v, want %v",
				tt.length, tt.pieceLength, got, tt.want)
		}
	}
}
