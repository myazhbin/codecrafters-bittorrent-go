package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
)

// unknownLeft is the number of bytes that trackers are told is left to
// download when a torrent's size isn't known yet, as is the case when starting
// from a magnet link. All that matters is that it isn't zero, since that
// would mean that the download is complete, and trackers needn't tell clients
// that are done downloading about peers to download from.
const unknownLeft = 1

// swarm stands for the peers that share a torrent. It finds them by asking
// the torrent's trackers, which it puts off doing until peers are actually
// needed.
type swarm struct {
	trackers []string // The URLs of the trackers to ask for peers, in the order to try them
	left     int64    // The number of bytes to tell the trackers is left to download

	// greeting is the handshake to send to the swarm's peers. It identifies
	// both the torrent and this client.
	greeting handshake

	peers []netip.AddrPort // The peers, once they have been asked for

	// idle holds the connections that are open but not in use, by the address
	// of their peer. A connection is kept when there's likely to be more to
	// ask of its peer soon, which saves connecting to the peer all over again.
	idle     map[string]*peerConn
	idleLock sync.Mutex
}

// newSwarm returns the swarm of peers that share the given torrent.
func newSwarm(torrent *Torrent) *swarm {
	return &swarm{
		trackers: []string{torrent.Announce},
		left:     torrent.Length, // Nothing has been downloaded yet, so the whole file is left
		greeting: handshake{InfoHash: torrent.InfoHash, PeerID: newPeerID()},
	}
}

// newMagnetSwarm returns the swarm of peers that share the torrent that a
// magnet link identifies.
func newMagnetSwarm(link *MagnetLink) *swarm {
	greeting := handshake{InfoHash: link.InfoHash, PeerID: newPeerID()}

	// All that a magnet link gives of a torrent is its info hash. The rest
	// has to be requested from peers, which takes the extension protocol.
	greeting.announceExtensions()

	return &swarm{
		trackers: link.Trackers,
		left:     unknownLeft,
		greeting: greeting,
	}
}

// Close closes the connections to peers that the swarm has kept open.
func (s *swarm) Close() {
	s.idleLock.Lock()
	defer s.idleLock.Unlock()

	for address, peer := range s.idle {
		peer.Close()
		delete(s.idle, address)
	}
}

// discover returns the addresses of the swarm's peers. The first call asks
// the trackers for them, trying each tracker in turn until one comes up with
// some peers.
func (s *swarm) discover() ([]netip.AddrPort, error) {
	if s.peers != nil {
		return s.peers, nil
	}
	if len(s.trackers) == 0 {
		return nil, errors.New("there's no tracker to ask for peers")
	}

	var failures []error
	for _, tracker := range s.trackers {
		peers, err := discoverPeers(tracker, s.greeting.InfoHash, s.greeting.PeerID, s.left)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if len(peers) == 0 {
			failures = append(failures, fmt.Errorf("the tracker at %s doesn't know of any peers for this torrent", tracker))
			continue
		}

		s.peers = peers
		return peers, nil
	}
	return nil, errors.Join(failures...)
}

// dial returns a connection to the peer at the given address (in "host:port"
// form) on which handshakes have been exchanged. That includes extension
// handshakes, if both sides support the extension protocol. A connection to
// the peer that the swarm has kept open is used in preference to a new one.
//
// Cancelling the context closes the connection.
func (s *swarm) dial(ctx context.Context, address string) (*peerConn, error) {
	s.idleLock.Lock()
	peer := s.idle[address]
	delete(s.idle, address)
	s.idleLock.Unlock()
	if peer != nil {
		peer.bindContext(ctx)
		return peer, nil
	}

	peer, err := dialPeer(ctx, address, s.greeting)
	if err != nil {
		return nil, err
	}
	if peer.SupportsExtensions {
		if err := peer.handshakeExtensions(); err != nil {
			peer.Close()
			return nil, err
		}
	}
	return peer, nil
}

// keep takes charge of a connection that's not needed for now, but whose peer
// there's likely to be more to ask of. The next call to dial for the peer
// returns the connection.
func (s *swarm) keep(peer *peerConn) {
	s.idleLock.Lock()
	defer s.idleLock.Unlock()

	if s.idle == nil {
		s.idle = map[string]*peerConn{}
	}
	if previous := s.idle[peer.Address]; previous != nil {
		previous.Close()
	}
	s.idle[peer.Address] = peer
}

// connect connects to one of the swarm's peers. The peers are tried in turn,
// in the order that the tracker listed them, until one of them works out:
// that is, it answers, completes the handshakes, and then gets through
// whatever the given function does with the connection. A peer that doesn't
// work out is dropped in favor of the next.
func (s *swarm) connect(ctx context.Context, prepare func(peer *peerConn) error) (*peerConn, error) {
	peers, err := s.discover()
	if err != nil {
		return nil, err
	}

	var failures []error
	for _, address := range peers {
		peer, err := s.dial(ctx, address.String())
		if err == nil {
			if err = prepare(peer); err == nil {
				return peer, nil
			}
			peer.Close()
		}
		failures = append(failures, fmt.Errorf("peer %s: %w", address, err))
	}
	return nil, fmt.Errorf("no peer worked out:\n%w", errors.Join(failures...))
}

// fetchTorrent gets the torrent's metadata from one of the swarm's peers, and
// returns the torrent that the metadata describes. It's how the details of a
// torrent are come by when all there is to go on is a magnet link.
//
// The connection to the peer that provided the metadata is kept open, ready
// for the torrent's data to be downloaded from the peer next.
func (s *swarm) fetchTorrent(ctx context.Context) (*Torrent, error) {
	var torrent *Torrent
	peer, err := s.connect(ctx, func(peer *peerConn) error {
		if !peer.SupportsExtensions {
			return errors.New("peer doesn't support protocol extensions")
		}

		metadata, err := peer.downloadMetadata(s.greeting.InfoHash)
		if err != nil {
			return err
		}
		torrent, err = torrentFromMetadata(metadata, s.trackers[0])
		return err
	})
	if err != nil {
		return nil, err
	}

	s.keep(peer)
	return torrent, nil
}
