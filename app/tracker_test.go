package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

// compactPeers returns the compact encoding of the given "ip:port" addresses:
// 4 bytes of IPv4 address and 2 bytes of big-endian port for each.
func compactPeers(t *testing.T, addresses ...string) string {
	t.Helper()

	var compact strings.Builder
	for _, address := range addresses {
		peer, err := netip.ParseAddrPort(address)
		if err != nil {
			t.Fatal(err)
		}
		ip := peer.Addr().As4()
		compact.Write(ip[:])
		compact.WriteByte(byte(peer.Port() >> 8))
		compact.WriteByte(byte(peer.Port()))
	}
	return compact.String()
}

// announceResponse returns a tracker's bencoded response to a successful
// announce.
func announceResponse(compactPeers string) string {
	return fmt.Sprintf("d8:intervali60e5:peers%d:%se", len(compactPeers), compactPeers)
}

// testTracker is a fake HTTP tracker that answers every announce with the
// same list of peers.
type testTracker struct {
	AnnounceURL string

	// Requests receives each announce request that the tracker gets.
	Requests chan *http.Request
}

// startTestTracker starts a fake tracker that hands out the given peers. The
// tracker is shut down when the test ends.
func startTestTracker(t *testing.T, peers ...string) *testTracker {
	t.Helper()

	response := announceResponse(compactPeers(t, peers...))
	requests := make(chan *http.Request, 16)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r:
		default: // Nobody is checking the requests
		}
		io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)

	return &testTracker{AnnounceURL: server.URL + "/announce", Requests: requests}
}

// startTestTrackerFunc starts a fake tracker that answers every request with
// the given status code and body.
func startTestTrackerFunc(t *testing.T, status int, body string) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	return server.URL + "/announce"
}

func TestDiscoverPeers(t *testing.T) {
	// An info hash full of bytes that are awkward to put in a URL.
	infoHash := [20]byte{0x00, ' ', '+', '&', '=', '%', '#', '?', '/', '~', 0xff, 0x7f, 'a', 'Z', '0', '-', '.', '_', 0x80, '\n'}
	peerID := [20]byte([]byte("-TS0001- 123456789ab"))

	tracker := startTestTracker(t, "165.232.41.73:51556", "165.232.38.164:51493", "165.232.35.114:51476")

	peers, err := discoverPeers(tracker.AnnounceURL, infoHash, peerID, 92063)
	if err != nil {
		t.Fatalf("discoverPeers returned error: %v", err)
	}

	want := []netip.AddrPort{
		netip.MustParseAddrPort("165.232.41.73:51556"),
		netip.MustParseAddrPort("165.232.38.164:51493"),
		netip.MustParseAddrPort("165.232.35.114:51476"),
	}
	if !reflect.DeepEqual(peers, want) {
		t.Errorf("discoverPeers = %v, want %v", peers, want)
	}

	request := <-tracker.Requests
	if request.Method != http.MethodGet {
		t.Errorf("request method = %s, want GET", request.Method)
	}
	if request.URL.Path != "/announce" {
		t.Errorf("request path = %q, want /announce", request.URL.Path)
	}

	query := request.URL.Query()
	wantParams := map[string]string{
		"info_hash":  string(infoHash[:]),
		"peer_id":    string(peerID[:]),
		"port":       "6881",
		"uploaded":   "0",
		"downloaded": "0",
		"left":       "92063",
		"compact":    "1",
	}
	for name, want := range wantParams {
		if got := query[name]; len(got) != 1 || got[0] != want {
			t.Errorf("query parameter %s = %q, want [%q]", name, got, want)
		}
	}
	if len(query) != len(wantParams) {
		t.Errorf("request has query parameters %q, want only %d of them", query, len(wantParams))
	}

	// The info hash must be sent as raw bytes with everything but unreserved
	// characters percent-encoded: in particular, a space is "%20" rather than
	// "+", and the hash is never sent as 40 hexadecimal characters.
	rawQuery := request.URL.RawQuery
	if want := "info_hash=%00%20%2B%26%3D%25%23%3F%2F~%FF%7FaZ0-._%80%0A"; !strings.Contains("&"+rawQuery+"&", "&"+want+"&") {
		t.Errorf("raw query %q doesn't contain %q", rawQuery, want)
	}
	if want := "peer_id=-TS0001-%20123456789ab"; !strings.Contains("&"+rawQuery+"&", "&"+want+"&") {
		t.Errorf("raw query %q doesn't contain %q", rawQuery, want)
	}
}

// Parameters that the tracker URL already has (private trackers put a
// passkey there) must be kept.
func TestDiscoverPeersKeepsQueryParameters(t *testing.T) {
	tracker := startTestTracker(t, "10.0.0.1:6881")

	_, err := discoverPeers(tracker.AnnounceURL+"?passkey=s3cret&a=b+c", [20]byte{1}, newPeerID(), 1)
	if err != nil {
		t.Fatalf("discoverPeers returned error: %v", err)
	}

	query := (<-tracker.Requests).URL.Query()
	if got := query.Get("passkey"); got != "s3cret" {
		t.Errorf("passkey = %q, want %q", got, "s3cret")
	}
	if got := query.Get("a"); got != "b c" {
		t.Errorf("a = %q, want %q", got, "b c")
	}
	if got := query.Get("compact"); got != "1" {
		t.Errorf("compact = %q, want %q", got, "1")
	}
}

func TestDiscoverPeersNoPeers(t *testing.T) {
	tracker := startTestTracker(t)

	peers, err := discoverPeers(tracker.AnnounceURL, [20]byte{}, newPeerID(), 1)
	if err != nil {
		t.Fatalf("discoverPeers returned error: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("discoverPeers = %v, want no peers", peers)
	}
}

func TestDiscoverPeersErrors(t *testing.T) {
	onePeer := compactPeers(t, "10.0.0.1:6881")

	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"failure reason", http.StatusOK, "d14:failure reason15:unknown torrente", "request failed: unknown torrent"},
		{"HTTP error", http.StatusInternalServerError, "something broke", `unexpected HTTP status "500 Internal Server Error": "something broke"`},
		{"HTTP error with valid body", http.StatusNotFound, announceResponse(onePeer), "unexpected HTTP status"},
		{
			// Only the start of a long error page is quoted.
			"HTTP error with a long body",
			http.StatusBadGateway,
			strings.Repeat("0123456789", 30),
			`unexpected HTTP status "502 Bad Gateway": "` + strings.Repeat("0123456789", 20) + `"...`,
		},
		{"HTTP error with padded body", http.StatusForbidden, "\n  go away  \n", `unexpected HTTP status "403 Forbidden": "go away"`},
		{"empty response", http.StatusOK, "", "invalid response"},
		{"response is not bencode", http.StatusOK, "<html>hello</html>", "invalid response"},
		{"response is not a dictionary", http.StatusOK, "l5:peerse", "not a dictionary"},
		{"response without peers", http.StatusOK, "d8:intervali60ee", `missing "peers"`},
		{"peers are not in compact form", http.StatusOK, "d8:intervali60e5:peersld2:ip8:10.0.0.14:porti6881eeee", `"peers" is not a string`},
		{"truncated peer list", http.StatusOK, announceResponse(onePeer[:5]), "not a multiple of 6"},
		{"peer list with an extra byte", http.StatusOK, announceResponse(onePeer + "x"), "not a multiple of 6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			announceURL := startTestTrackerFunc(t, tt.status, tt.body)

			peers, err := discoverPeers(announceURL, [20]byte{}, newPeerID(), 1)
			if err == nil {
				t.Fatalf("discoverPeers = %v, want an error containing %q", peers, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("discoverPeers error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestDiscoverPeersBadTrackerURL(t *testing.T) {
	// A tracker that was running, but no longer is.
	stopped := httptest.NewServer(http.NotFoundHandler())
	stopped.Close()

	tests := []struct {
		name       string
		trackerURL string
		wantErr    string
	}{
		{"UDP tracker", "udp://tracker.example:6969/announce", "only HTTP trackers are supported"},
		{"no scheme", "tracker.example/announce", "only HTTP trackers are supported"},
		{"empty", "", "only HTTP trackers are supported"},
		{"unparseable", "http://tracker.example:port/announce", "invalid tracker URL"},
		{"unreachable", stopped.URL + "/announce", "tracker: "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peers, err := discoverPeers(tt.trackerURL, [20]byte{}, newPeerID(), 1)
			if err == nil {
				t.Fatalf("discoverPeers = %v, want an error containing %q", peers, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("discoverPeers error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseCompactPeers(t *testing.T) {
	tests := []struct {
		name    string
		compact string
		want    []string
	}{
		{"none", "", nil},
		{"one", "\xa5\xe8\x29\x49\xc9\x64", []string{"165.232.41.73:51556"}},
		{"extreme values", "\x00\x00\x00\x00\x00\x00\xff\xff\xff\xff\xff\xff", []string{"0.0.0.0:0", "255.255.255.255:65535"}},
		{"port byte order", "\x7f\x00\x00\x01\x00\x50\x7f\x00\x00\x01\x50\x00", []string{"127.0.0.1:80", "127.0.0.1:20480"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peers, err := parseCompactPeers(tt.compact)
			if err != nil {
				t.Fatalf("parseCompactPeers returned error: %v", err)
			}

			var got []string
			for _, peer := range peers {
				got = append(got, peer.String())
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCompactPeers = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewPeerID(t *testing.T) {
	first, second := newPeerID(), newPeerID()

	if first == second {
		t.Errorf("newPeerID returned %q twice, want a different ID each time", first)
	}
	for _, id := range [][20]byte{first, second} {
		if !strings.HasPrefix(string(id[:]), "-CC0001-") {
			t.Errorf("peer ID %q doesn't start with the client prefix", id)
		}
		for _, c := range id {
			if c < 0x21 || c > 0x7e {
				t.Errorf("peer ID %q contains the non-printable byte %#x", id, c)
			}
		}
	}
}
