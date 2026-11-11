package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// maxPeerFailures is how many times in a row a peer may fail to deliver a
// piece before it's given up on.
const maxPeerFailures = 3

// peerRetryDelay is how long to leave a peer alone after it has failed to
// deliver a piece. It's a variable rather than a constant so that tests can
// shorten it.
var peerRetryDelay = time.Second

// downloadFile downloads the torrent's file from the swarm's peers, and saves
// it at path.
//
// The file only appears at path once it's complete. Until then, its data is
// kept in a temporary file alongside, which is removed if the download fails.
func (s *swarm) downloadFile(torrent *Torrent, path string) error {
	partPath := path + ".part"
	file, err := os.Create(partPath)
	if err != nil {
		return err
	}

	indexes := make([]int, len(torrent.PieceHashes))
	for index := range indexes {
		indexes[index] = index
	}
	// Pieces arrive in no particular order, so each one is written at its
	// own offset in the file as it comes in.
	err = s.downloadPieces(torrent, indexes, func(index int, piece []byte) error {
		_, err := file.WriteAt(piece, int64(index)*torrent.PieceLength)
		return err
	})
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(partPath, path)
	}

	if err != nil {
		os.Remove(partPath)
		return err
	}
	return nil
}

// downloadPieces downloads the pieces of the torrent with the given indexes
// from the swarm's peers. It calls save with each piece once the piece has
// been downloaded and verified against its hash. The calls are made one at a
// time, in whichever order the pieces happen to complete.
//
// Pieces are downloaded from every peer at once: each peer has a worker that
// takes pieces off a shared queue. A piece that a worker fails to download,
// for whatever reason, goes back on the queue for any worker to try again.
func (s *swarm) downloadPieces(torrent *Torrent, indexes []int, save func(index int, piece []byte) error) error {
	if len(indexes) == 0 {
		return nil
	}

	peers, err := s.discover()
	if err != nil {
		return err
	}

	// Returning cancels the context, which stops the workers, and then waits
	// for them to finish.
	var workers sync.WaitGroup
	defer workers.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The queue has room for every piece, so putting a piece back on it never
	// blocks.
	queue := make(chan int, len(indexes))
	for _, index := range indexes {
		queue <- index
	}
	downloaded := make(chan downloadedPiece)
	gaveUp := make(chan error, len(peers)) // Receives the reason from each worker that gives up on its peer

	for _, peer := range peers {
		workers.Go(func() {
			worker := pieceWorker{
				swarm:      s,
				torrent:    torrent,
				address:    peer.String(),
				queue:      queue,
				downloaded: downloaded,
			}
			if err := worker.run(ctx); err != nil {
				gaveUp <- fmt.Errorf("peer %s: %w", peer, err)
			}
		})
	}

	var failures []error
	for remaining := len(indexes); remaining > 0; {
		select {
		case piece := <-downloaded:
			if err := save(piece.index, piece.data); err != nil {
				return err
			}
			remaining--

		case err := <-gaveUp:
			failures = append(failures, err)
			if len(failures) == len(peers) {
				return fmt.Errorf("ran out of peers with %d piece(s) left to download:\n%w",
					remaining, errors.Join(failures...))
			}
		}
	}
	return nil
}

// downloadedPiece is a piece that has been downloaded and verified.
type downloadedPiece struct {
	index int
	data  []byte
}

// pieceWorker downloads pieces of a torrent from a single peer.
type pieceWorker struct {
	swarm   *swarm
	torrent *Torrent
	address string // The peer's address

	queue      chan int               // The indexes of the pieces that are waiting to be downloaded
	downloaded chan<- downloadedPiece // Where to deliver downloaded pieces

	// peer is the connection to the peer. It's only opened once there's a
	// piece to download, so that no connection is made to a peer that turns
	// out not to be needed.
	peer *peerConn
}

// run downloads pieces from the queue until the context is cancelled, which
// is how the worker is told that the download is over.
//
// If the peer fails to deliver a piece, the piece goes back on the queue, and
// the worker starts over with a new connection after a pause. An error is
// returned once the peer has failed several times in a row, at which point
// it's not worth bothering with any more.
func (w *pieceWorker) run(ctx context.Context) error {
	defer w.disconnect()

	for failures := 0; ; {
		var index int
		select {
		case <-ctx.Done():
			return nil
		case index = <-w.queue:
		}

		piece, err := w.download(ctx, index)
		if err != nil {
			w.queue <- index
			w.disconnect() // There's no telling what state the connection is in

			failures++
			if failures == maxPeerFailures {
				return err
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(peerRetryDelay):
			}
			continue
		}
		failures = 0

		select {
		case <-ctx.Done():
			return nil
		case w.downloaded <- downloadedPiece{index, piece}:
		}
	}
}

// download downloads the piece with the given index from the worker's peer,
// connecting to the peer first if need be.
func (w *pieceWorker) download(ctx context.Context, index int) ([]byte, error) {
	if w.peer == nil {
		peer, err := w.swarm.dial(ctx, w.address)
		if err != nil {
			return nil, err
		}
		w.peer = peer
	}
	return w.peer.downloadPiece(w.torrent, index)
}

// disconnect closes the connection to the worker's peer, if there is one.
func (w *pieceWorker) disconnect() {
	if w.peer != nil {
		w.peer.Close()
		w.peer = nil
	}
}
