package main

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

// extensionsReserved holds the reserved bytes of a handshake that announces
// support for the extension protocol, and nothing else.
var extensionsReserved = [8]byte{0, 0, 0, 0, 0, 0x10, 0, 0}

func TestNewSwarm(t *testing.T) {
	torrent := newTestTorrent(t, testContent(1000), 400)

	s := newSwarm(torrent)

	if want := []string{"http://tracker.example/announce"}; !reflect.DeepEqual(s.trackers, want) {
		t.Errorf("trackers = %q, want %q", s.trackers, want)
	}
	if s.left != 1000 {
		t.Errorf("left = %d, want 1000 (the size of the file)", s.left)
	}
	if s.greeting.InfoHash != torrent.InfoHash {
		t.Errorf("greeting has info hash %x, want %x", s.greeting.InfoHash, torrent.InfoHash)
	}
	if s.greeting.Reserved != [8]byte{} {
		t.Errorf("greeting has reserved bytes % x, want all zeros", s.greeting.Reserved)
	}
	if s.greeting.PeerID == [20]byte{} {
		t.Error("greeting has no peer ID")
	}
	if other := newSwarm(torrent); other.greeting.PeerID == s.greeting.PeerID {
		t.Errorf("two swarms got the same peer ID, %q", s.greeting.PeerID)
	}
}

func TestNewMagnetSwarm(t *testing.T) {
	link, err := parseMagnetLink(exampleMagnetLink + "&tr=http%3A%2F%2Fbackup.example%2Fannounce")
	if err != nil {
		t.Fatal(err)
	}

	s := newMagnetSwarm(link)

	if want := []string{exampleTrackerURL, "http://backup.example/announce"}; !reflect.DeepEqual(s.trackers, want) {
		t.Errorf("trackers = %q, want %q", s.trackers, want)
	}
	// The size of the download isn't known, but trackers must not be told
	// that there's nothing left of it.
	if s.left <= 0 {
		t.Errorf("left = %d, want a positive number", s.left)
	}
	if s.greeting.InfoHash != link.InfoHash {
		t.Errorf("greeting has info hash %x, want %x", s.greeting.InfoHash, link.InfoHash)
	}
	if s.greeting.Reserved != extensionsReserved {
		t.Errorf("greeting has reserved bytes % x, want % x", s.greeting.Reserved, extensionsReserved)
	}
	if s.greeting.PeerID == [20]byte{} {
		t.Error("greeting has no peer ID")
	}
}

func TestSwarmDiscover(t *testing.T) {
	infoHash := exampleHandshake.InfoHash
	greeting := handshake{InfoHash: infoHash, PeerID: newPeerID()}
	wantPeers := []netip.AddrPort{
		netip.MustParseAddrPort("10.0.0.1:6881"),
		netip.MustParseAddrPort("10.0.0.2:51413"),
	}

	t.Run("asks the tracker only once", func(t *testing.T) {
		tracker := startTestTracker(t, "10.0.0.1:6881", "10.0.0.2:51413")
		s := &swarm{trackers: []string{tracker.AnnounceURL}, left: 1234, greeting: greeting}

		for range 3 {
			peers, err := s.discover()
			if err != nil {
				t.Fatalf("discover returned error: %v", err)
			}
			if !reflect.DeepEqual(peers, wantPeers) {
				t.Errorf("discover = %v, want %v", peers, wantPeers)
			}
		}

		if got := len(tracker.Requests); got != 1 {
			t.Fatalf("the tracker got %d requests, want 1", got)
		}
		query := (<-tracker.Requests).URL.Query()
		if got := query.Get("info_hash"); got != string(infoHash[:]) {
			t.Errorf("info_hash = %x, want %x", got, infoHash)
		}
		if got := query.Get("peer_id"); got != string(greeting.PeerID[:]) {
			t.Errorf("peer_id = %q, want %q", got, greeting.PeerID)
		}
		if got := query.Get("left"); got != "1234" {
			t.Errorf("left = %q, want %q", got, "1234")
		}
	})

	t.Run("goes through the trackers until one has peers", func(t *testing.T) {
		down := "http://" + stoppedServerAddress(t) + "/announce"
		failing := startTestTrackerFunc(t, 200, "d14:failure reason9:not founde")
		empty := startTestTracker(t)
		good := startTestTracker(t, "10.0.0.1:6881", "10.0.0.2:51413")
		spare := startTestTracker(t, "10.0.0.3:6881")
		s := &swarm{
			trackers: []string{"udp://tracker.example:6969", down, failing, empty.AnnounceURL, good.AnnounceURL, spare.AnnounceURL},
			left:     1,
			greeting: greeting,
		}

		peers, err := s.discover()
		if err != nil {
			t.Fatalf("discover returned error: %v", err)
		}
		if !reflect.DeepEqual(peers, wantPeers) {
			t.Errorf("discover = %v, want %v", peers, wantPeers)
		}
		if got := len(empty.Requests); got != 1 {
			t.Errorf("the tracker without peers got %d requests, want 1", got)
		}
		if got := len(spare.Requests); got != 0 {
			t.Errorf("the tracker after the one with peers got %d requests, want none", got)
		}
	})

	t.Run("no trackers", func(t *testing.T) {
		s := &swarm{left: 1, greeting: greeting}

		peers, err := s.discover()
		if err == nil || !strings.Contains(err.Error(), "there's no tracker to ask for peers") {
			t.Errorf("discover = %v, %v, want an error saying that there's no tracker", peers, err)
		}
	})

	t.Run("no tracker has peers", func(t *testing.T) {
		failing := startTestTrackerFunc(t, 200, "d14:failure reason9:not founde")
		empty := startTestTracker(t)
		s := &swarm{trackers: []string{failing, empty.AnnounceURL}, left: 1, greeting: greeting}

		peers, err := s.discover()
		if err == nil {
			t.Fatalf("discover = %v, want an error", peers)
		}
		// The error must say what went wrong with each of the trackers.
		for _, want := range []string{
			"tracker: request failed: not found",
			"the tracker at " + empty.AnnounceURL + " doesn't know of any peers for this torrent",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("discover error = %q, want it to contain %q", err, want)
			}
		}
	})
}

// anyPeer is for connecting to a swarm's first reachable peer, whichever it
// is.
func anyPeer(*peerConn) error { return nil }

func TestSwarmConnect(t *testing.T) {
	infoHash := exampleHandshake.InfoHash
	greeting := handshake{Reserved: extensionsReserved, InfoHash: infoHash, PeerID: newPeerID()}

	t.Run("connects to the first peer that answers", func(t *testing.T) {
		unreachable := stoppedServerAddress(t)
		otherTorrent := startTestPeer(t, &testPeer{InfoHash: [20]byte{0xaa}})
		first := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved})
		second := startTestPeer(t, &testPeer{InfoHash: infoHash})
		tracker := startTestTracker(t, unreachable, otherTorrent.Address, first.Address, second.Address)
		s := &swarm{trackers: []string{tracker.AnnounceURL}, left: 1, greeting: greeting}

		peer, err := s.connect(context.Background(), anyPeer)
		if err != nil {
			t.Fatalf("connect returned error: %v", err)
		}
		defer peer.Close()

		if peer.ID != testPeerID {
			t.Errorf("peer ID = %x, want %x", peer.ID, testPeerID)
		}
		if !peer.SupportsExtensions {
			t.Error("SupportsExtensions = false, want true")
		}
		if got := <-first.Handshakes; got != greeting {
			t.Errorf("the peer was sent the handshake %+v, want %+v", got, greeting)
		}
		if got := len(otherTorrent.Handshakes); got != 1 {
			t.Errorf("the peer with another torrent was connected to %d times, want once", got)
		}
		if got := len(second.Handshakes); got != 0 {
			t.Errorf("the peer after the one that answered was connected to %d times, want never", got)
		}
	})

	t.Run("no peer answers", func(t *testing.T) {
		unreachable := stoppedServerAddress(t)
		otherTorrent := startTestPeer(t, &testPeer{InfoHash: [20]byte{0xaa}})
		tracker := startTestTracker(t, unreachable, otherTorrent.Address)
		s := &swarm{trackers: []string{tracker.AnnounceURL}, left: 1, greeting: greeting}

		peer, err := s.connect(context.Background(), anyPeer)
		if err == nil {
			peer.Close()
			t.Fatal("connect succeeded, want an error")
		}
		// The error must say what went wrong with each of the peers.
		for _, want := range []string{
			"no peer worked out",
			"peer " + unreachable + ": ",
			"connection refused",
			"peer " + otherTorrent.Address + ": peer answered for a different torrent",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("connect error = %q, want it to contain %q", err, want)
			}
		}
	})

	t.Run("passes over peers that don't work out", func(t *testing.T) {
		plain := startTestPeer(t, &testPeer{InfoHash: infoHash})
		extended := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved})
		tracker := startTestTracker(t, plain.Address, extended.Address)
		s := &swarm{trackers: []string{tracker.AnnounceURL}, left: 1, greeting: greeting}

		var tried []*peerConn
		peer, err := s.connect(context.Background(), func(peer *peerConn) error {
			tried = append(tried, peer)
			if !peer.SupportsExtensions {
				return errors.New("extensions are needed")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("connect returned error: %v", err)
		}
		defer peer.Close()

		if len(tried) != 2 || peer != tried[1] {
			t.Fatalf("connect tried %d peers and returned %p, want it to return the second of 2", len(tried), peer)
		}
		if !peer.SupportsExtensions {
			t.Error("connect returned the peer that didn't work out")
		}

		// The peer that didn't work out must have been hung up on.
		select {
		case <-plain.Hangups:
		case <-time.After(10 * time.Second):
			t.Error("the connection to the peer that didn't work out was left open")
		}
	})

	t.Run("no peer works out", func(t *testing.T) {
		first := startTestPeer(t, &testPeer{InfoHash: infoHash})
		second := startTestPeer(t, &testPeer{InfoHash: infoHash})
		tracker := startTestTracker(t, first.Address, second.Address)
		s := &swarm{trackers: []string{tracker.AnnounceURL}, left: 1, greeting: greeting}

		attempts := 0
		peer, err := s.connect(context.Background(), func(peer *peerConn) error {
			attempts++
			return fmt.Errorf("failure %d", attempts)
		})
		if err == nil {
			peer.Close()
			t.Fatal("connect succeeded, want an error")
		}
		for _, want := range []string{
			"no peer worked out",
			"peer " + first.Address + ": failure 1",
			"peer " + second.Address + ": failure 2",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("connect error = %q, want it to contain %q", err, want)
			}
		}
	})

	t.Run("no peers to be found", func(t *testing.T) {
		s := &swarm{trackers: []string{startTestTracker(t).AnnounceURL}, left: 1, greeting: greeting}

		peer, err := s.connect(context.Background(), anyPeer)
		if err == nil {
			peer.Close()
			t.Fatal("connect succeeded, want an error")
		}
		if !strings.Contains(err.Error(), "doesn't know of any peers") {
			t.Errorf("connect error = %q, want it to say that the tracker has no peers", err)
		}
	})
}

func TestSwarmFetchTorrent(t *testing.T) {
	content := testContent(2500)
	info := testInfoDict(content, 1024)
	infoHash := sha1.Sum([]byte(info))

	t.Run("gets the metadata from the first peer that has it", func(t *testing.T) {
		noExtensions := startTestPeer(t, &testPeer{InfoHash: infoHash})
		noMetadata := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, MetadataID: 5})
		good := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, MetadataID: 5, Metadata: []byte(info)})
		spare := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, MetadataID: 5, Metadata: []byte(info)})
		tracker := startTestTracker(t, noExtensions.Address, noMetadata.Address, good.Address, spare.Address)

		link, err := parseMagnetLink(testMagnetLink(infoHash, tracker.AnnounceURL))
		if err != nil {
			t.Fatal(err)
		}
		peers := newMagnetSwarm(link)
		torrent, err := peers.fetchTorrent(context.Background())
		if err != nil {
			t.Fatalf("fetchTorrent returned error: %v", err)
		}

		want, err := parseTorrent(testTorrentFile(tracker.AnnounceURL, info))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(torrent, want) {
			t.Errorf("fetchTorrent = %+v, want %+v", torrent, want)
		}

		if got := len(spare.Handshakes); got != 0 {
			t.Errorf("the peer after the one with the metadata was connected to %d times, want never", got)
		}

		// The connection to the peer that provided the metadata is kept open,
		// and is what dialing that peer gets from then on.
		if len(good.Hangups) != 0 {
			t.Fatal("the connection to the peer with the metadata was closed")
		}
		peer, err := peers.dial(context.Background(), good.Address)
		if err != nil {
			t.Fatalf("dial returned error: %v", err)
		}
		if id, ok := peer.metadataID(); !ok || id != 5 {
			t.Errorf("the kept connection's metadataID() = %d, %v, want 5, true", id, ok)
		}
		if got := len(good.Handshakes); got != 1 {
			t.Errorf("the peer with the metadata was connected to %d times, want once", got)
		}

		// Whatever the swarm is keeping open is closed along with it.
		peers.keep(peer)
		peers.Close()
		select {
		case <-good.Hangups:
		case <-time.After(10 * time.Second):
			t.Error("closing the swarm left the connection that it had kept open")
		}
	})

	t.Run("no peer has the metadata", func(t *testing.T) {
		noExtensions := startTestPeer(t, &testPeer{InfoHash: infoHash})
		noMetadata := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, MetadataID: 5})
		tracker := startTestTracker(t, noExtensions.Address, noMetadata.Address)

		link, err := parseMagnetLink(testMagnetLink(infoHash, tracker.AnnounceURL))
		if err != nil {
			t.Fatal(err)
		}
		torrent, err := newMagnetSwarm(link).fetchTorrent(context.Background())
		if err == nil {
			t.Fatalf("fetchTorrent = %+v, want an error", torrent)
		}
		if torrent != nil {
			t.Errorf("fetchTorrent returned %+v alongside an error", torrent)
		}
		for _, want := range []string{
			"peer " + noExtensions.Address + ": peer doesn't support protocol extensions",
			"peer " + noMetadata.Address + ": peer turned down the request for piece 0 of the metadata",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("fetchTorrent error = %q, want it to contain %q", err, want)
			}
		}
	})
}

func TestSwarmDial(t *testing.T) {
	infoHash := exampleHandshake.InfoHash
	link, err := parseMagnetLink(testMagnetLink(infoHash, "http://tracker.example/announce"))
	if err != nil {
		t.Fatal(err)
	}
	torrent := &Torrent{Announce: "http://tracker.example/announce", InfoHash: infoHash, Length: 1}

	// extensionHandshakes counts the extension handshakes among the messages
	// that a fake peer received, once the client has hung up on it.
	extensionHandshakes := func(t *testing.T, fake *testPeer) int {
		count := 0
		for _, m := range messagesAfterHangup(t, fake) {
			if m.ID == msgExtended && m.Payload[0] == extensionHandshakeID {
				count++
			}
		}
		return count
	}

	t.Run("exchanges extension handshakes when both sides support extensions", func(t *testing.T) {
		fake := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, MetadataID: 9})

		peer, err := newMagnetSwarm(link).dial(context.Background(), fake.Address)
		if err != nil {
			t.Fatalf("dial returned error: %v", err)
		}
		if peer.Address != fake.Address {
			t.Errorf("Address = %q, want %q", peer.Address, fake.Address)
		}
		if id, ok := peer.metadataID(); !ok || id != 9 {
			t.Errorf("metadataID() = %d, %v, want 9, true", id, ok)
		}
		peer.Close()

		if got := extensionHandshakes(t, fake); got != 1 {
			t.Errorf("the peer was sent %d extension handshakes, want 1", got)
		}
	})

	t.Run("leaves extensions alone when the peer doesn't support them", func(t *testing.T) {
		fake := startTestPeer(t, &testPeer{InfoHash: infoHash})

		peer, err := newMagnetSwarm(link).dial(context.Background(), fake.Address)
		if err != nil {
			t.Fatalf("dial returned error: %v", err)
		}
		peer.Close()

		if got := extensionHandshakes(t, fake); got != 0 {
			t.Errorf("the peer was sent %d extension handshakes, want none", got)
		}
	})

	t.Run("leaves extensions alone when the download doesn't need them", func(t *testing.T) {
		fake := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, MetadataID: 9})

		peer, err := newSwarm(torrent).dial(context.Background(), fake.Address)
		if err != nil {
			t.Fatalf("dial returned error: %v", err)
		}
		peer.Close()

		if got := (<-fake.Handshakes).Reserved; got != [8]byte{} {
			t.Errorf("the handshake was sent with reserved bytes % x, want all zeros", got)
		}
		if got := extensionHandshakes(t, fake); got != 0 {
			t.Errorf("the peer was sent %d extension handshakes, want none", got)
		}
	})

	t.Run("fails if the extension handshake does", func(t *testing.T) {
		fake := startTestPeer(t, &testPeer{InfoHash: infoHash, Reserved: extensionsReserved, ExtensionHandshake: []byte("nonsense")})

		peer, err := newMagnetSwarm(link).dial(context.Background(), fake.Address)
		if err == nil {
			peer.Close()
			t.Fatal("dial succeeded, want an error")
		}
		if !strings.Contains(err.Error(), "malformed extension handshake") {
			t.Errorf("dial error = %q, want it to be about the extension handshake", err)
		}

		select {
		case <-fake.Hangups:
		case <-time.After(10 * time.Second):
			t.Error("the connection was left open after the extension handshake failed")
		}
	})

	t.Run("fails if the peer can't be reached", func(t *testing.T) {
		peer, err := newMagnetSwarm(link).dial(context.Background(), stoppedServerAddress(t))
		if err == nil {
			peer.Close()
			t.Fatal("dial succeeded, want an error")
		}
	})
}

// A connection that the swarm has been given to keep is what dialing its peer
// returns, in place of a new connection.
func TestSwarmKeepsConnections(t *testing.T) {
	infoHash := exampleHandshake.InfoHash
	torrent := &Torrent{Announce: "http://tracker.example/announce", InfoHash: infoHash, Length: 1}
	fake := startTestPeer(t, &testPeer{InfoHash: infoHash})
	other := startTestPeer(t, &testPeer{InfoHash: infoHash})
	peers := newSwarm(torrent)
	defer peers.Close()

	first, err := peers.dial(context.Background(), fake.Address)
	if err != nil {
		t.Fatalf("dial returned error: %v", err)
	}
	peers.keep(first)

	// Dialing another peer has nothing to do with the kept connection.
	elsewhere, err := peers.dial(context.Background(), other.Address)
	if err != nil {
		t.Fatalf("dial returned error: %v", err)
	}
	defer elsewhere.Close()
	if elsewhere == first {
		t.Fatal("dialing another peer returned the kept connection")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	again, err := peers.dial(ctx, fake.Address)
	if err != nil {
		t.Fatalf("dial returned error: %v", err)
	}
	if again != first {
		t.Error("dialing the peer again made a new connection, rather than returning the kept one")
	}
	if got := len(fake.Handshakes); got != 1 {
		t.Errorf("the peer was connected to %d times, want once", got)
	}

	// Having been handed out, the connection is no longer the swarm's to
	// give: the next dial makes a new one.
	fresh, err := peers.dial(context.Background(), fake.Address)
	if err != nil {
		t.Fatalf("dial returned error: %v", err)
	}
	defer fresh.Close()
	if fresh == first {
		t.Error("the kept connection was handed out twice")
	}

	// The kept connection now answers to the context that it was last dialed
	// with: cancelling that closes it.
	if len(fake.Hangups) != 0 {
		t.Fatal("a connection to the peer was closed early")
	}
	cancel()
	select {
	case <-fake.Hangups:
	case <-time.After(10 * time.Second):
		t.Error("cancelling the context didn't close the connection that was dialed with it")
	}
}

// Keeping a second connection to a peer closes the one that was kept before.
func TestSwarmKeepReplacesConnection(t *testing.T) {
	infoHash := exampleHandshake.InfoHash
	torrent := &Torrent{Announce: "http://tracker.example/announce", InfoHash: infoHash, Length: 1}
	fake := startTestPeer(t, &testPeer{InfoHash: infoHash})
	peers := newSwarm(torrent)
	defer peers.Close()

	first, err := dialPeer(context.Background(), fake.Address, peers.greeting)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dialPeer(context.Background(), fake.Address, peers.greeting)
	if err != nil {
		t.Fatal(err)
	}

	peers.keep(first)
	peers.keep(second)
	select {
	case <-fake.Hangups:
	case <-time.After(10 * time.Second):
		t.Fatal("the connection that was kept first wasn't closed when it was replaced")
	}

	got, err := peers.dial(context.Background(), fake.Address)
	if err != nil {
		t.Fatalf("dial returned error: %v", err)
	}
	defer got.Close()
	if got != second {
		t.Error("dial didn't return the connection that was kept last")
	}
}
