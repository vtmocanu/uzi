package redirectguard

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func mustReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return &http.Request{URL: u}
}

func TestSameOriginTable(t *testing.T) {
	const origin = "https://forge.example.com/api/v4/user"
	cases := []struct {
		name   string
		target string
		ok     bool
	}{
		{"same origin other path", "https://forge.example.com/api/v4/users/1", true},
		{"explicit default port", "https://forge.example.com:443/x", true},
		{"host case and root dot", "https://FORGE.example.COM./x", true},
		{"scheme downgrade", "http://forge.example.com/x", false},
		{"scheme downgrade on 443", "http://forge.example.com:443/x", false},
		{"other port", "https://forge.example.com:8443/x", false},
		{"other host", "https://evil.example.net/x", false},
		{"subdomain", "https://api.forge.example.com/x", false},
		{"userinfo trick", "https://forge.example.com@evil.example.net/x", false},
		{"non-http scheme", "file:///etc/passwd", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SameOrigin(mustReq(t, tc.target), []*http.Request{mustReq(t, origin)})
			if tc.ok && err != nil {
				t.Fatalf("SameOrigin(%s) = %v, want nil", tc.target, err)
			}
			if !tc.ok {
				if !errors.Is(err, ErrCrossOrigin) {
					t.Fatalf("SameOrigin(%s) = %v, want ErrCrossOrigin", tc.target, err)
				}
				if strings.Contains(err.Error(), "/x") {
					t.Fatalf("error %q leaks the target path", err)
				}
			}
		})
	}
}

// The origin is the FIRST request of the chain, not the previous hop, so a chain
// that has already been allowed cannot pivot off-origin from a later hop.
func TestSameOriginIPv6(t *testing.T) {
	via := []*http.Request{mustReq(t, "https://[::1]/a")}
	if err := SameOrigin(mustReq(t, "https://[::1]:443/b"), via); err != nil {
		t.Fatalf("same IPv6 origin = %v, want nil", err)
	}
	if err := SameOrigin(mustReq(t, "https://[::1]:8443/b"), via); !errors.Is(err, ErrCrossOrigin) {
		t.Fatalf("other IPv6 port = %v, want ErrCrossOrigin", err)
	}
}

func TestSameOriginComparesAgainstChainStart(t *testing.T) {
	via := []*http.Request{
		mustReq(t, "https://forge.example.com/a"),
		mustReq(t, "https://forge.example.com/b"),
	}
	if err := SameOrigin(mustReq(t, "https://forge.example.com/c"), via); err != nil {
		t.Fatalf("same-origin third hop refused: %v", err)
	}
	if err := SameOrigin(mustReq(t, "https://evil.example.net/c"), via); !errors.Is(err, ErrCrossOrigin) {
		t.Fatalf("off-origin third hop = %v, want ErrCrossOrigin", err)
	}
}

func TestSameOriginHopCeiling(t *testing.T) {
	via := make([]*http.Request, MaxHops)
	for i := range via {
		via[i] = mustReq(t, "https://forge.example.com/loop")
	}
	// via holds the original request plus every redirect already followed, so
	// len(via) == MaxHops means req is redirect number MaxHops (net/http's rule).
	err := SameOrigin(mustReq(t, "https://forge.example.com/loop"), via)
	if err == nil {
		t.Fatalf("redirect %d followed, want the ceiling to stop it", MaxHops)
	}
	if err := SameOrigin(mustReq(t, "https://forge.example.com/loop"), via[:MaxHops-1]); err != nil {
		t.Fatalf("redirect %d refused: %v", MaxHops-1, err)
	}
}

func TestSameOriginEmptyHostNeverMatches(t *testing.T) {
	via := []*http.Request{{URL: &url.URL{Scheme: "https", Path: "/a"}}}
	if err := SameOrigin(&http.Request{URL: &url.URL{Scheme: "https", Path: "/b"}}, via); !errors.Is(err, ErrCrossOrigin) {
		t.Fatalf("empty-host redirect = %v, want ErrCrossOrigin", err)
	}
}

type countingRT struct{ n int }

func (c *countingRT) RoundTrip(*http.Request) (*http.Response, error) {
	c.n++
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

func TestOriginPin(t *testing.T) {
	next := &countingRT{}
	pin := NewOriginPin(next)
	if err := pin.Allow("https://api.example.com/", "https://uploads.example.com/"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	for _, ok := range []string{"https://api.example.com/repos", "https://uploads.example.com:443/x"} {
		if _, err := pin.RoundTrip(mustReq(t, ok)); err != nil {
			t.Fatalf("RoundTrip(%s) = %v, want sent", ok, err)
		}
	}
	for _, bad := range []string{"http://api.example.com/repos", "https://api.example.com:8443/x", "https://evil.example.net/x"} {
		if _, err := pin.RoundTrip(mustReq(t, bad)); !errors.Is(err, ErrCrossOrigin) {
			t.Fatalf("RoundTrip(%s) = %v, want ErrCrossOrigin", bad, err)
		}
	}
	if next.n != 2 {
		t.Fatalf("next transport saw %d requests, want exactly the 2 allowed", next.n)
	}
}

func TestOriginPinEmptyRefusesAndAllowRejectsRelative(t *testing.T) {
	next := &countingRT{}
	pin := NewOriginPin(next)
	if _, err := pin.RoundTrip(mustReq(t, "https://api.example.com/")); !errors.Is(err, ErrCrossOrigin) || next.n != 0 {
		t.Fatalf("empty pin: err=%v sent=%d, want ErrCrossOrigin and nothing sent", err, next.n)
	}
	if err := pin.Allow("/api/v3/"); err == nil {
		t.Fatal("Allow accepted a relative URL")
	}
}

func TestStopOffOrigin(t *testing.T) {
	via := []*http.Request{mustReq(t, "https://forge.example.com/a")}
	if err := StopOffOrigin(mustReq(t, "https://forge.example.com/b"), via); err != nil {
		t.Fatalf("same origin = %v, want nil", err)
	}
	if err := StopOffOrigin(mustReq(t, "http://forge.example.com/b"), via); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("off origin = %v, want http.ErrUseLastResponse", err)
	}
}
