package pushbroker_test

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/cgi" //nolint:gosec // G504: test-only CGI host for git http-backend on a fixed modern Go toolchain; httpoxy (CVE-2016-5386) affects Go < 1.6.3 only.
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// Regression tests for the forge-credential redirect boundary on every go-git HTTP
// operation the broker performs: reference listing, fetch, manual receive-pack
// (publish, CAS delete, and CreateRef and the CAS Delete of a salvage ref) and the
// refspec delete push.
// The remote is a real smart-HTTP server (git http-backend behind an httptest TLS proxy) so each
// operation's earlier discovery steps succeed and the redirect lands on exactly the
// request under test; the forbidden destination counts requests and must see ZERO.

// otherHost is a hostname the httptest certificate covers; trustLoopbackTLS dials
// it to loopback.
const otherHost = "forbidden.example.com"

// trustLoopbackTLS makes the process-wide default transport trust the httptest
// certificate and dial otherHost to loopback. go-git's HTTP clients all send
// through the http.DefaultTransport OBJECT (captured when they are built), so the
// fields are mutated in place and restored afterwards. Not parallel-safe.
func trustLoopbackTLS(t *testing.T) {
	t.Helper()
	tr := http.DefaultTransport.(*http.Transport)
	probe := httptest.NewTLSServer(http.NotFoundHandler())
	pool := probe.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	probe.Close()

	prevTLS, prevDial := tr.TLSClientConfig, tr.DialContext
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	var d net.Dialer
	tr.TLSClientConfig = cfg
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(addr); err == nil && host == otherHost {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
		return d.DialContext(ctx, network, addr)
	}
	tr.CloseIdleConnections()
	t.Cleanup(func() {
		tr.TLSClientConfig, tr.DialContext = prevTLS, prevDial
		tr.CloseIdleConnections()
	})
}

// gitHTTPRemote serves the fixture's bare repo over smart HTTP (TLS) and, when
// told to, answers the nth info/refs discovery for one service with a redirect.
type gitHTTPRemote struct {
	srv *httptest.Server

	mu         sync.Mutex
	seen       map[string]int // service -> info/refs requests so far
	redirSvc   string
	redirNth   int
	redirTo    func(path, rawQuery string) string
	redirected atomic.Int64
}

// gitHTTPBackendStatus reports whether git-http-backend is an executable regular
// file under execPath. When it is not, fatal says whether the caller must fail
// (CI) rather than skip, and reason is the message for either.
func gitHTTPBackendStatus(execPath string, inCI bool) (path string, ok, fatal bool, reason string) {
	path = filepath.Join(execPath, "git-http-backend")
	fi, err := os.Stat(path)
	if err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
		return path, true, false, ""
	}
	problem := "not an executable file"
	if err != nil {
		problem = err.Error()
	}
	if inCI {
		return path, false, true, "git-http-backend unavailable at " + path + " (" + problem +
			"); the credential-redirect regression suite must not be skipped in CI"
	}
	return path, false, false, "git-http-backend unavailable at " + path + " (" + problem +
		"); install it (Alpine: apk add git-daemon; Debian/Ubuntu: ships with git)"
}

// requireGitHTTPBackend returns the git-http-backend path, skipping the test
// locally and failing it in CI when the CGI binary is missing (Alpine's git
// package omits it, which otherwise surfaces as opaque HTTP 500s).
func requireGitHTTPBackend(t *testing.T) string {
	t.Helper()
	execPath := strings.TrimSpace(run(t, "", "git", "--exec-path"))
	path, ok, fatal, reason := gitHTTPBackendStatus(execPath, os.Getenv("CI") != "")
	if !ok {
		if fatal {
			t.Fatal(reason)
		}
		t.Skip(reason)
	}
	return path
}

func newGitHTTPRemote(t *testing.T, f *gitFixture, backendPath string) *gitHTTPRemote {
	t.Helper()
	run(t, "", "git", "-C", f.bare, "config", "http.receivepack", "true")
	backend := &cgi.Handler{
		Path: backendPath,
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Dir(f.bare),
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	r := &gitHTTPRemote{seen: map[string]int{}}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// A same-origin alias prefix, used by the positive controls.
		req.URL.Path = strings.TrimPrefix(req.URL.Path, "/alias")
		if strings.HasSuffix(req.URL.Path, "/info/refs") {
			svc := req.URL.Query().Get("service")
			r.mu.Lock()
			r.seen[svc]++
			hit := r.redirTo != nil && svc == r.redirSvc && r.seen[svc] == r.redirNth
			r.mu.Unlock()
			if hit {
				r.redirected.Add(1)
				http.Redirect(w, req, r.redirTo(req.URL.Path, req.URL.RawQuery), http.StatusFound)
				return
			}
		}
		backend.ServeHTTP(w, req)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *gitHTTPRemote) cloneURL(f *gitFixture) string {
	return r.srv.URL + "/" + filepath.Base(f.bare)
}

type forbidden struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newForbiddenRemote(t *testing.T, useTLS bool) *forbidden {
	t.Helper()
	f := &forbidden{}
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.hits.Add(1)
		http.Error(w, "should never be reached", http.StatusTeapot)
	})
	if useTLS {
		f.srv = httptest.NewTLSServer(h)
	} else {
		f.srv = httptest.NewServer(h)
	}
	t.Cleanup(f.srv.Close)
	return f
}

type offOrigin struct {
	name   string
	useTLS bool
	host   func(forbiddenURL string) string
}

func offOrigins() []offOrigin {
	return []offOrigin{
		{"other host", true, func(u string) string { return strings.Replace(u, "127.0.0.1", otherHost, 1) }},
		{"https to http", false, func(u string) string { return u }},
		{"other port", true, func(u string) string { return u }},
	}
}

// brokerOp is one go-git HTTP operation, identified by the info/refs discovery
// request that starts it.
type brokerOp struct {
	name string
	svc  string
	nth  int
	// setup prepares origin through the file:// remote and returns the call that
	// drives the operation over cloneURL.
	setup func(t *testing.T, f *gitFixture) func(cloneURL string) error
}

func redirectTestPAT() string {
	return strings.Join([]string{"broker", "redirect", "credential", "42"}, "-")
}

func brokerOps() []brokerOp {
	publishSetup := func(t *testing.T, f *gitFixture) func(string) error {
		t.Helper()
		base := f.commit("a.txt", "base\n", "base")
		f.pushMain()
		tip := f.commit("b.txt", "one\n", "c1")
		pack := f.pack(tip, base)
		return func(u string) error {
			_, err := pushbroker.Publish(context.Background(), pushbroker.Options{
				CloneURL: u, Branch: "main", DefaultBranch: "main", DeclaredTip: tip, Pack: pack,
				Username: "uzi-bot", PAT: redirectTestPAT(),
			})
			return err
		}
	}
	// deleteSetup publishes a checkpoint over file:// so there is a ref to delete.
	deleteSetup := func(cas bool) func(*testing.T, *gitFixture) func(string) error {
		return func(t *testing.T, f *gitFixture) func(string) error {
			t.Helper()
			base := f.commit("a.txt", "base\n", "base")
			f.pushMain()
			tip := f.commit("b.txt", "one\n", "c1")
			if _, err := pushbroker.Publish(context.Background(), pushbroker.Options{
				CloneURL: f.cloneURL(), Branch: "main", DefaultBranch: "main", DeclaredTip: tip, Pack: f.pack(tip, base),
			}); err != nil {
				t.Fatalf("seed Publish: %v", err)
			}
			expected := ""
			if cas {
				expected = tip
			}
			return func(u string) error {
				return pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{
					CloneURL: u, Branch: "main", Username: "uzi-bot", PAT: redirectTestPAT(), ExpectedOldTip: expected,
				})
			}
		}
	}
	// salvageCreateSetup puts a run tip on origin's checkpoint ref over file://, so
	// CreateRef creates the salvage ref (PRD #1867) from it over the HTTP remote: a list,
	// a receive-pack session, then the read-back list.
	salvageCreateSetup := func(t *testing.T, f *gitFixture) func(string) error {
		t.Helper()
		f.commit("a.txt", "base\n", "base")
		f.pushMain()
		tip := f.commit("b.txt", "one\n", "c1")
		f.git("push", "origin", tip+":refs/uzi-checkpoints/main")
		return func(u string) error {
			return pushbroker.CreateRef(context.Background(), pushbroker.CreateRefOptions{
				CloneURL: u, Ref: pushbroker.SalvageRef(salvageRunID), Tip: tip, SourceRef: "refs/uzi-checkpoints/main",
				Username: "uzi-bot", PAT: redirectTestPAT(),
			})
		}
	}
	// salvageRefSetup puts a salvage ref on origin over file:// for the salvage CAS Delete
	// and ListRefTips to reach.
	salvageRefSetup := func(list bool) func(*testing.T, *gitFixture) func(string) error {
		return func(t *testing.T, f *gitFixture) func(string) error {
			t.Helper()
			f.commit("a.txt", "base\n", "base")
			f.pushMain()
			tip := f.commit("b.txt", "one\n", "c1")
			ref := pushbroker.SalvageRef(salvageRunID)
			f.git("push", "origin", tip+":"+ref)
			return func(u string) error {
				if list {
					_, err := pushbroker.ListRefTips(context.Background(), pushbroker.ListRefsOptions{
						CloneURL: u, Username: "uzi-bot", PAT: redirectTestPAT(),
					}, ref)
					return err
				}
				return pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{
					CloneURL: u, Ref: ref, ExpectedOldTip: tip, Username: "uzi-bot", PAT: redirectTestPAT(),
				})
			}
		}
	}
	return []brokerOp{
		{"publish list", "git-upload-pack", 1, publishSetup},
		{"publish fetch", "git-upload-pack", 2, publishSetup},
		{"publish receive-pack", "git-receive-pack", 1, publishSetup},
		{"delete list", "git-upload-pack", 1, deleteSetup(false)},
		{"delete push", "git-receive-pack", 1, deleteSetup(false)},
		{"cas delete list", "git-upload-pack", 1, deleteSetup(true)},
		{"cas delete receive-pack", "git-receive-pack", 1, deleteSetup(true)},
		{"salvage create ref list", "git-upload-pack", 1, salvageCreateSetup},
		{"salvage create ref receive-pack", "git-receive-pack", 1, salvageCreateSetup},
		{"salvage create ref read-back list", "git-upload-pack", 2, salvageCreateSetup},
		{"salvage cas delete list", "git-upload-pack", 1, salvageRefSetup(false)},
		{"salvage cas delete receive-pack", "git-receive-pack", 1, salvageRefSetup(false)},
		{"salvage list ref tips", "git-upload-pack", 1, salvageRefSetup(true)},
	}
}

func TestBrokerRefusesOffOriginRedirect(t *testing.T) {
	backend := requireGitHTTPBackend(t)
	for _, op := range brokerOps() {
		for _, oo := range offOrigins() {
			t.Run(op.name+"/"+oo.name, func(t *testing.T) {
				trustLoopbackTLS(t)
				f := newGitFixture(t)
				call := op.setup(t, f)
				remote := newGitHTTPRemote(t, f, backend)
				bad := newForbiddenRemote(t, oo.useTLS)
				remote.redirSvc, remote.redirNth = op.svc, op.nth
				remote.redirTo = func(path, q string) string { return oo.host(bad.srv.URL) + path + "?" + q }

				err := call(remote.cloneURL(f))
				if n := remote.redirected.Load(); n != 1 {
					t.Fatalf("the operation under test was not reached (redirects served = %d); an earlier step failed: %v", n, err)
				}
				if got := bad.hits.Load(); got != 0 {
					t.Fatalf("forbidden destination received %d request(s); want 0 (err=%v)", got, err)
				}
				if err == nil {
					t.Fatal("operation succeeded through an off-origin redirect; want an error")
				}
				if strings.Contains(err.Error(), redirectTestPAT()) {
					t.Fatalf("error leaks the credential: %v", err)
				}
			})
		}
	}
}

// Positive control: a same-origin redirect of each operation's discovery request is
// still followed and the operation completes.
func TestBrokerFollowsSameOriginRedirect(t *testing.T) {
	backend := requireGitHTTPBackend(t)
	for _, op := range brokerOps() {
		t.Run(op.name, func(t *testing.T) {
			trustLoopbackTLS(t)
			f := newGitFixture(t)
			call := op.setup(t, f)
			remote := newGitHTTPRemote(t, f, backend)
			remote.redirSvc, remote.redirNth = op.svc, op.nth
			remote.redirTo = func(path, q string) string { return "/alias" + path + "?" + q }

			if err := call(remote.cloneURL(f)); err != nil {
				t.Fatalf("operation through a same-origin redirect: %v", err)
			}
			if n := remote.redirected.Load(); n != 1 {
				t.Fatalf("redirects served = %d, want 1", n)
			}
		})
	}
}

func TestGitHTTPBackendStatus(t *testing.T) {
	cases := []struct {
		name      string
		mode      os.FileMode // 0 = absent
		inCI      bool
		wantOK    bool
		wantFatal bool
	}{
		{"absent locally skips", 0, false, false, false},
		{"absent in CI is fatal", 0, true, false, true},
		{"executable is ok", 0o755, true, true, false},
		{"non-executable is not ok", 0o644, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			want := filepath.Join(dir, "git-http-backend")
			if tc.mode != 0 {
				if err := os.WriteFile(want, []byte("#!/bin/sh\n"), tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			path, ok, fatal, reason := gitHTTPBackendStatus(dir, tc.inCI)
			if path != want || ok != tc.wantOK || fatal != tc.wantFatal {
				t.Fatalf("got (%q, ok=%v, fatal=%v), want (%q, ok=%v, fatal=%v)", path, ok, fatal, want, tc.wantOK, tc.wantFatal)
			}
			if !ok && !strings.Contains(reason, "git-http-backend") {
				t.Fatalf("reason %q does not name git-http-backend", reason)
			}
			if fatal && !strings.Contains(reason, "must not be skipped in CI") {
				t.Fatalf("CI reason %q does not say the suite must not be skipped", reason)
			}
		})
	}
}
