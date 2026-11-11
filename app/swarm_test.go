package main

import (
	"context"
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
