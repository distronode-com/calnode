package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serveThroughTrust runs one request through TrustClientIP and reports what remoteIP
// resolved to inside the handler — the value a rate limiter would key on.
func serveThroughTrust(t *testing.T, cidrs []string, peer string, headers map[string]string) string {
	t.Helper()
	trusted, err := ParseTrustedProxies(cidrs)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%v): %v", cidrs, err)
	}
	var got string
	h := TrustClientIP(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = remoteIP(r)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = peer
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

// The default. An untrusted peer's forwarded headers are not read at all, so the spoof
// keys on the connection the spoofer actually made.
func TestTrustClientIP_untrustedPeerIgnoresSpoofedHeaders(t *testing.T) {
	got := serveThroughTrust(t, []string{"10.0.0.0/8"}, "203.0.113.9:1234", map[string]string{
		"X-Forwarded-For":  "198.51.100.1",
		"CF-Connecting-IP": "198.51.100.2",
		"X-Real-IP":        "198.51.100.3",
	})
	if got != "203.0.113.9" {
		t.Errorf("client IP = %q; want the peer 203.0.113.9", got)
	}
}

// With no trusted proxies configured at all, TrustClientIP is a pass-through and the
// behaviour is exactly what it was before the setting existed.
func TestTrustClientIP_noTrustedProxiesIsUnchanged(t *testing.T) {
	got := serveThroughTrust(t, nil, "10.0.0.5:1234", map[string]string{
		"X-Forwarded-For": "198.51.100.1",
	})
	if got != "10.0.0.5" {
		t.Errorf("client IP = %q; want the peer 10.0.0.5", got)
	}
}

func TestTrustClientIP_trustedPeerUsesCFConnectingIP(t *testing.T) {
	got := serveThroughTrust(t, []string{"10.0.0.0/8"}, "10.0.0.5:1234", map[string]string{
		"CF-Connecting-IP": "198.51.100.7",
		// CF-Connecting-IP wins over the chain: one fronting CDN sets it to one value.
		"X-Forwarded-For": "198.51.100.99, 10.0.0.5",
	})
	if got != "198.51.100.7" {
		t.Errorf("client IP = %q; want 198.51.100.7", got)
	}
}

// Two of our own hops appended themselves; the walk goes right to left past both and
// stops at the address the outermost trusted proxy actually saw.
func TestTrustClientIP_walksTrustedChainRightToLeft(t *testing.T) {
	got := serveThroughTrust(t, []string{"10.0.0.0/8", "192.168.0.0/16"}, "10.0.0.5:1234",
		map[string]string{"X-Forwarded-For": "198.51.100.7, 192.168.1.1, 10.0.0.9"})
	if got != "198.51.100.7" {
		t.Errorf("client IP = %q; want 198.51.100.7", got)
	}
}

// The leftmost entry is client-supplied. A client that pre-seeds the header must not be
// able to choose its own key by putting a value to the left of the real one.
func TestTrustClientIP_ignoresEntriesLeftOfTheRealHop(t *testing.T) {
	got := serveThroughTrust(t, []string{"10.0.0.0/8"}, "10.0.0.5:1234",
		map[string]string{"X-Forwarded-For": "1.2.3.4, 198.51.100.7, 10.0.0.9"})
	if got != "198.51.100.7" {
		t.Errorf("client IP = %q; want 198.51.100.7 (not the client-seeded 1.2.3.4)", got)
	}
}

func TestTrustClientIP_malformedHeaderFallsBackToPeer(t *testing.T) {
	cases := map[string]string{
		"garbage":            "not-an-ip",
		"empty entries":      " , ,",
		"address with port":  "198.51.100.7:443",
		"malformed left hop": "not-an-ip, 10.0.0.9",
	}
	for name, xff := range cases {
		t.Run(name, func(t *testing.T) {
			got := serveThroughTrust(t, []string{"10.0.0.0/8"}, "10.0.0.5:1234",
				map[string]string{"X-Forwarded-For": xff})
			if got != "10.0.0.5" {
				t.Errorf("client IP = %q; want the peer 10.0.0.5", got)
			}
		})
	}
}

// A chain of nothing but our own proxies has no client address in it. The peer is the
// only honest answer left.
func TestTrustClientIP_allHopsTrustedFallsBackToPeer(t *testing.T) {
	got := serveThroughTrust(t, []string{"10.0.0.0/8"}, "10.0.0.5:1234",
		map[string]string{"X-Forwarded-For": "10.0.0.7, 10.0.0.9"})
	if got != "10.0.0.5" {
		t.Errorf("client IP = %q; want the peer 10.0.0.5", got)
	}
}

func TestTrustClientIP_ipv6(t *testing.T) {
	// The proxies sit in 2001:db8:0::/48; the client is one prefix over, so the walk
	// skips the trusted hop and stops on it.
	got := serveThroughTrust(t, []string{"2001:db8:0::/48"}, "[2001:db8::1]:1234",
		map[string]string{"X-Forwarded-For": "2001:db8:1::5, 2001:db8::9"})
	if got != "2001:db8:1::5" {
		t.Errorf("client IP = %q; want 2001:db8:1::5", got)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	// A bare address is what an operator naming one proxy writes.
	nets, err := ParseTrustedProxies([]string{"10.0.0.7", "192.168.0.0/16", "2001:db8::1"})
	if err != nil {
		t.Fatalf("err = %v; want nil", err)
	}
	if len(nets) != 3 {
		t.Fatalf("parsed %d entries; want 3", len(nets))
	}
	if !nets[0].Contains(net.ParseIP("10.0.0.7")) || nets[0].Contains(net.ParseIP("10.0.0.8")) {
		t.Errorf("bare IPv4 %v should be exactly one host", nets[0])
	}
	if !nets[2].Contains(net.ParseIP("2001:db8::1")) || nets[2].Contains(net.ParseIP("2001:db8::2")) {
		t.Errorf("bare IPv6 %v should be exactly one host", nets[2])
	}
}

// One typo costs that hop's trust, not the whole list's — and it is reported rather than
// swallowed, because an operator who thinks a proxy is trusted and is wrong gets shared
// rate-limit buckets with no explanation.
func TestParseTrustedProxies_reportsBadEntriesAndKeepsTheRest(t *testing.T) {
	nets, err := ParseTrustedProxies([]string{"10.0.0.0/8", "10.0.0.0/99", "nonsense"})
	if err == nil {
		t.Fatal("err = nil; want the bad entries named")
	}
	if len(nets) != 1 {
		t.Errorf("parsed %d entries; want the 1 good one kept", len(nets))
	}
}
