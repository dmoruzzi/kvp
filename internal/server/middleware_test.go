package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func newIPTestServer(trusted ...netip.Prefix) *Server {
	return newServer(nil, Options{TrustedProxies: trusted})
}

func reqWithPeer(peer string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/k", nil)
	r.RemoteAddr = peer
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestClientIP(t *testing.T) {
	trusted := netip.MustParsePrefix("10.255.0.0/24")
	proxy := "10.255.0.5:41234"
	client := "203.0.113.7"

	cases := []struct {
		name    string
		trusted []netip.Prefix
		peer    string
		headers map[string]string
		want    string
	}{
		{
			name: "untrusted peer: forwarded headers ignored",
			peer: "192.0.2.1:1234",
			headers: map[string]string{
				"X-Forwarded-For":  client,
				"CF-Connecting-IP": client,
			},
			want: "192.0.2.1",
		},
		{
			name:    "no trusted proxies configured: XFF ignored even if present",
			peer:    proxy,
			headers: map[string]string{"X-Forwarded-For": client},
			want:    "10.255.0.5",
		},
		{
			name:    "trusted peer: first XFF entry wins",
			trusted: []netip.Prefix{trusted},
			peer:    proxy,
			headers: map[string]string{"X-Forwarded-For": client + ", 10.0.0.1, 10.0.0.2"},
			want:    client,
		},
		{
			name:    "trusted peer: CF-Connecting-IP used when no XFF",
			trusted: []netip.Prefix{trusted},
			peer:    proxy,
			headers: map[string]string{"CF-Connecting-IP": " " + client + " "},
			want:    client,
		},
		{
			name:    "trusted peer: XFF preferred over CF-Connecting-IP",
			trusted: []netip.Prefix{trusted},
			peer:    proxy,
			headers: map[string]string{"X-Forwarded-For": client, "CF-Connecting-IP": "198.51.100.9"},
			want:    client,
		},
		{
			name:    "trusted peer without headers falls back to the peer itself",
			trusted: []netip.Prefix{trusted},
			peer:    proxy,
			want:    "10.255.0.5",
		},
		{
			name:    "trusted peer with blank XFF entries falls through to CF then peer",
			trusted: []netip.Prefix{trusted},
			peer:    proxy,
			headers: map[string]string{"X-Forwarded-For": " , , "},
			want:    "10.255.0.5",
		},
		{
			name:    "RemoteAddr without a port is returned verbatim",
			trusted: []netip.Prefix{trusted},
			peer:    "weird-addr",
			want:    "weird-addr",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newIPTestServer(tc.trusted...)
			if got := s.clientIP(reqWithPeer(tc.peer, tc.headers)); got != tc.want {
				t.Errorf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPeerTrusted(t *testing.T) {
	trusted := netip.MustParsePrefix("10.255.0.0/24")

	cases := []struct {
		name    string
		trusted []netip.Prefix
		peer    string
		want    bool
	}{
		{"ip inside prefix", []netip.Prefix{trusted}, "10.255.0.99", true},
		{"network address matches", []netip.Prefix{trusted}, "10.255.0.0", true},
		{"ip outside prefix", []netip.Prefix{trusted}, "10.255.1.1", false},
		{"similar but different prefix", []netip.Prefix{trusted}, "10.255.2.5", false},
		{"unparseable peer", []netip.Prefix{trusted}, "not-an-ip", false},
		{"empty trusted list", nil, "10.255.0.5", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newIPTestServer(tc.trusted...)
			if got := s.peerTrusted(tc.peer); got != tc.want {
				t.Errorf("peerTrusted(%q) = %v, want %v", tc.peer, got, tc.want)
			}
		})
	}
}

func TestRateLimiterPruneRemovesIdleBuckets(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rl := newRateLimiter(10, 20)

	if ok, _ := rl.allow("old", now); !ok {
		t.Fatal("allow(old): denied, want allowed")
	}
	if ok, _ := rl.allow("fresh", now.Add(30*time.Minute)); !ok {
		t.Fatal("allow(fresh): denied, want allowed")
	}

	rl.prune(now.Add(2 * time.Hour)) // "old" idle > 1h, "fresh" idle 90m... also pruned
	rl.mu.Lock()
	nAfterLongIdle := len(rl.buckets)
	rl.mu.Unlock()
	if nAfterLongIdle != 0 {
		t.Errorf("buckets after pruning both idle = %d, want 0", nAfterLongIdle)
	}

	// A recently used bucket survives.
	if ok, _ := rl.allow("keep", now); !ok {
		t.Fatal("allow(keep): denied, want allowed")
	}
	rl.prune(now.Add(30 * time.Minute))
	rl.mu.Lock()
	_, keepExists := rl.buckets["keep"]
	rl.mu.Unlock()
	if !keepExists {
		t.Error("recently-used bucket was pruned")
	}
}
