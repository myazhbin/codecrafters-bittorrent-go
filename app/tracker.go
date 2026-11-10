package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// listenPort is the port that trackers are told this client accepts
	// connections on. The client never actually listens: it only downloads,
	// and does so over connections that it initiates itself.
	listenPort = 6881

	// trackerTimeout bounds how long a whole tracker request may take.
	trackerTimeout = 30 * time.Second

	// maxTrackerResponseSize caps how much of a tracker's response is read, so
	// that a misbehaving tracker can't exhaust memory. A megabyte has room for
	// well over 100,000 peers.
	maxTrackerResponseSize = 1 << 20
)

// newPeerID returns a new random ID for this client to identify itself with
// to trackers and peers. It follows the usual convention of a client code and
// version between dashes, followed by random characters.
func newPeerID() [20]byte {
	var id [20]byte
	copy(id[:], "-CC0001-"+rand.Text())
	return id
}

// discoverPeers asks the tracker at trackerURL for the peers sharing the
// torrent identified by infoHash. left is the number of bytes of the torrent
// that this client still has to download.
func discoverPeers(trackerURL string, infoHash, peerID [20]byte, left int64) ([]netip.AddrPort, error) {
	announceURL, err := buildAnnounceURL(trackerURL, infoHash, peerID, left)
	if err != nil {
		return nil, err
	}

	client := http.Client{Timeout: trackerTimeout}
	response, err := client.Get(announceURL)
	if err != nil {
		return nil, fmt.Errorf("tracker: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxTrackerResponseSize))
	if err != nil {
		return nil, fmt.Errorf("tracker: reading response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tracker: unexpected HTTP status %q: %s", response.Status, summarize(body))
	}

	return parseAnnounceResponse(body)
}

// buildAnnounceURL returns the URL to request from a tracker in order to
// announce this client and get a list of peers back.
func buildAnnounceURL(trackerURL string, infoHash, peerID [20]byte, left int64) (string, error) {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return "", fmt.Errorf("invalid tracker URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported tracker URL %q: only HTTP trackers are supported", trackerURL)
	}

	query := u.Query() // Keeps any parameters that are already in the URL, such as a passkey
	query.Set("info_hash", string(infoHash[:]))
	query.Set("peer_id", string(peerID[:]))
	query.Set("port", strconv.Itoa(listenPort))
	query.Set("uploaded", "0")
	query.Set("downloaded", "0")
	query.Set("left", strconv.FormatInt(left, 10))
	query.Set("compact", "1") // Asks for the peer list in its compact form

	// Encode escapes spaces as "+", like an HTML form would. The BitTorrent
	// specification calls for "%20" instead, and not every tracker accepts
	// "+". Any "+" in the encoded query is an escaped space (a literal "+" is
	// escaped as "%2B"), so they can simply all be replaced.
	u.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20")

	return u.String(), nil
}

// parseAnnounceResponse extracts the peers from a tracker's response to an
// announce request. The response is a bencoded dictionary with these keys:
//
//	interval: how long the client should wait between announces, in seconds
//	peers:    the peers, in compact form (see parseCompactPeers)
//
// If the tracker rejects the request, the dictionary instead has a single
// "failure reason" key that holds a human-readable explanation.
func parseAnnounceResponse(body []byte) ([]netip.AddrPort, error) {
	decoded, err := decodeBencode(string(body))
	if err != nil {
		return nil, fmt.Errorf("tracker: invalid response: %w", err)
	}
	response, ok := decoded.(map[string]interface{})
	if !ok {
		return nil, errors.New("tracker: invalid response: not a dictionary")
	}

	if reason, failed := response["failure reason"]; failed {
		return nil, fmt.Errorf("tracker: request failed: %v", reason)
	}

	peers, err := dictString(response, "peers")
	if err != nil {
		return nil, fmt.Errorf("tracker: invalid response: %w", err)
	}
	return parseCompactPeers(peers)
}

// parseCompactPeers parses a list of peers in the compact format, which uses
// 6 bytes per peer: a 4-byte IPv4 address followed by a 2-byte port number in
// network (big-endian) byte order.
func parseCompactPeers(compact string) ([]netip.AddrPort, error) {
	const peerSize = 6

	if len(compact)%peerSize != 0 {
		return nil, fmt.Errorf("tracker: peer list is %d bytes long, which is not a multiple of %d",
			len(compact), peerSize)
	}

	peers := make([]netip.AddrPort, 0, len(compact)/peerSize)
	for ; compact != ""; compact = compact[peerSize:] {
		ip := netip.AddrFrom4([4]byte{compact[0], compact[1], compact[2], compact[3]})
		port := uint16(compact[4])<<8 | uint16(compact[5])
		peers = append(peers, netip.AddrPortFrom(ip, port))
	}
	return peers, nil
}

// summarize returns a short, printable excerpt of a response body, for use
// in error messages.
func summarize(body []byte) string {
	const maxLength = 200

	body = bytes.TrimSpace(body)
	if len(body) > maxLength {
		return fmt.Sprintf("%q...", body[:maxLength])
	}
	return fmt.Sprintf("%q", body)
}
