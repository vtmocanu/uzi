package agentsource

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// --- FINDING 2: redirect must not escape the allowlist -------------------------

// TestRedirectGuardPredicate unit-tests the CheckRedirect predicate directly: a
// redirect target that still passes the allowlist is followed, one that does not is
// refused, a nil predicate refuses all redirects (fail-closed), and the hop ceiling
// fires.
func TestRedirectGuardPredicate(t *testing.T) {
	allowOnly := func(host string) func(string) bool {
		return func(raw string) bool { return strings.Contains(raw, host) }
	}
	mkReq := func(rawURL string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		return r
	}

	// Same allowlisted host, http->https / path change: allowed.
	guard := redirectGuard(allowOnly("git.example.com"))
	if err := guard(mkReq("https://git.example.com/roster.git/info/refs?service=git-upload-pack"), nil); err != nil {
		t.Errorf("same-host allowlisted redirect must be allowed; got %v", err)
	}

	// Cross-host hop to a link-local / metadata address: refused.
	if err := guard(mkReq("http://169.254.169.254/latest/meta-data/"), nil); err == nil {
		t.Errorf("redirect to a non-allowlisted host must be refused")
	}

	// Nil predicate refuses everything.
	if err := redirectGuard(nil)(mkReq("https://git.example.com/x"), nil); err == nil {
		t.Errorf("a nil allowlist predicate must refuse all redirects (fail-closed)")
	}

	// Hop ceiling.
	via := make([]*http.Request, maxCloneRedirects)
	if err := guard(mkReq("https://git.example.com/x"), via); err == nil {
		t.Errorf("exceeding the redirect hop ceiling must be refused")
	}
}

// TestFetchRefusesRedirectToNonAllowlistedHost drives the whole clone transport against
// an httptest server that 302s the initial ref advertisement to a non-allowlisted host,
// proving redirectGuard is actually wired into the go-git client used by the fetch — not
// just unit-correct in isolation.
func TestFetchRefusesRedirectToNonAllowlistedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A well-formed git info/refs target so go-git's own redirect policy permits the
		// hop; our guard is what must refuse it because the host is off-allowlist.
		http.Redirect(w, r, "http://evil.internal/roster.git/info/refs?service=git-upload-pack", http.StatusFound)
	}))
	defer srv.Close()

	// Allow only the httptest server's own host; the redirect target (evil.internal) is
	// therefore off-allowlist.
	allowSelf := func(raw string) bool { return strings.Contains(raw, srv.Listener.Addr().String()) }

	_, _, err := FetchRoleFiles(context.Background(), CloneOptions{
		CloneURL:        srv.URL + "/roster.git",
		RedirectAllowed: allowSelf,
	})
	if err == nil {
		t.Fatalf("clone that redirects to a non-allowlisted host must fail")
	}
	// Require the guard's OWN message, not merely a failure that mentions the target
	// host: if the guard were unwired, go-git would FOLLOW the redirect and fail later
	// with a downstream dial/DNS error that also contains "evil.internal" — so a
	// host-substring assertion would pass vacuously. "refusing redirect" is emitted only
	// by redirectGuard, so this reddens if the guard is disabled.
	if !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("error should be the redirect guard's own refusal (not a downstream dial failure); got %q", err.Error())
	}
}

// --- FINDING 1: the wire-size cap errors an oversized response cleanly ----------

// TestBoundedBodyTripsCleanlyOverCap feeds more bytes than the budget allows through the
// bounded body and asserts it returns errCloneWireBudget (a clean, distinct error) and
// sets the trip flag — rather than silently truncating at EOF, which would surface as an
// opaque pack-corruption error.
func TestBoundedBodyTripsCleanlyOverCap(t *testing.T) {
	const capBytes = 1024
	budget := &wireBudget{remaining: capBytes}
	body := &boundedBody{
		rc:     io.NopCloser(strings.NewReader(strings.Repeat("x", capBytes*4))),
		budget: budget,
	}
	buf := make([]byte, 256)
	var tripped bool
	for i := 0; i < 100; i++ {
		_, err := body.Read(buf)
		if errors.Is(err, errCloneWireBudget) {
			tripped = true
			break
		}
		if err != nil {
			t.Fatalf("unexpected read error before cap: %v", err)
		}
	}
	if !tripped {
		t.Fatalf("reading past the cap must return errCloneWireBudget")
	}
	if !budget.tripped() {
		t.Errorf("the wire budget trip flag must be set after the cap is crossed")
	}
}

// TestBoundedBodyUnderCapReadsClean confirms a response that stays under the cap reads to
// EOF without tripping, so a legitimate small clone is never rejected.
func TestBoundedBodyUnderCapReadsClean(t *testing.T) {
	budget := &wireBudget{remaining: 1 << 20}
	body := &boundedBody{
		rc:     io.NopCloser(strings.NewReader(strings.Repeat("y", 4096))),
		budget: budget,
	}
	n, err := io.Copy(io.Discard, body)
	if err != nil {
		t.Fatalf("under-cap read must not error; got %v", err)
	}
	if n != 4096 {
		t.Errorf("read %d bytes, want 4096", n)
	}
	if budget.tripped() {
		t.Errorf("under-cap read must not trip the budget")
	}
	if atomic.LoadInt64(&budget.remaining) != (1<<20)-4096 {
		t.Errorf("remaining budget = %d, want %d", budget.remaining, (1<<20)-4096)
	}
}

// TestBoundedRoundTripperWrapsBody confirms the round tripper installs the bounded body
// so reads draw the shared budget down.
func TestBoundedRoundTripperWrapsBody(t *testing.T) {
	budget := &wireBudget{remaining: 10}
	rt := &boundedRoundTripper{base: fakeRT{body: "0123456789ABCDEF"}, budget: budget}
	resp, err := rt.RoundTrip(&http.Request{})
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	if !errors.Is(err, errCloneWireBudget) {
		t.Errorf("reading a body larger than the budget must trip the cap; got %v", err)
	}
	if !budget.tripped() {
		t.Errorf("trip flag must be set")
	}
}

type fakeRT struct{ body string }

func (f fakeRT) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

// TestFetchRefusesRedirectToOtherAllowlistedOrigin: the allowlist alone would let the clone
// credential follow a redirect to ANOTHER allowlisted origin (a second listed host, or the same
// host on another listed port). A credentialed redirect must also stay on the origin of the
// request that started the chain, so the second origin receives nothing.
func TestFetchRefusesRedirectToOtherAllowlistedOrigin(t *testing.T) {
	var otherHits atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherHits.Add(1)
		http.NotFound(w, nil)
	}))
	defer other.Close()
	var sameOriginHits atomic.Int64
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/moved/"):
			sameOriginHits.Add(1)
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/xhost.git/"):
			// Another hostname for the same allowlisted listener: a cross-host redirect.
			http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/roster.git/info/refs?service=git-upload-pack", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/self.git/"):
			http.Redirect(w, r, "/moved/roster.git/info/refs?service=git-upload-pack", http.StatusFound)
		default:
			http.Redirect(w, r, other.URL+"/roster.git/info/refs?service=git-upload-pack", http.StatusFound)
		}
	}))
	defer src.Close()
	// The clone credential, assembled at runtime so no credential-shaped literal is committed.
	cloneCred := strings.Join([]string{"clone", "credential", "0123456789"}, "-")
	// Both origins are allowlisted, under either hostname.
	port := func(s *httptest.Server) string {
		return s.Listener.Addr().(*net.TCPAddr).AddrPort().String()[len("127.0.0.1"):]
	}
	allowBoth := func(raw string) bool {
		return strings.Contains(raw, port(src)+"/") || strings.Contains(raw, port(other)+"/")
	}

	for _, c := range []struct{ name, repo string }{
		{"same host, other port", "/roster.git"},
		{"other host", "/xhost.git"},
	} {
		_, _, err := FetchRoleFiles(context.Background(), CloneOptions{
			CloneURL: src.URL + c.repo, Token: cloneCred, RedirectAllowed: allowBoth,
		})
		if err == nil {
			t.Errorf("%s: clone redirected to another allowlisted origin must fail", c.name)
		}
		if n := otherHits.Load(); n != 0 {
			t.Errorf("%s: the other allowlisted origin received %d request(s); want 0", c.name, n)
			otherHits.Store(0)
		}
	}

	// Positive control: a same-origin redirect is still followed (the moved path is reached).
	_, _, _ = FetchRoleFiles(context.Background(), CloneOptions{
		CloneURL: src.URL + "/self.git", Token: cloneCred, RedirectAllowed: allowBoth,
	})
	if n := sameOriginHits.Load(); n != 1 {
		t.Fatalf("same-origin redirect target hits = %d, want 1", n)
	}
}
