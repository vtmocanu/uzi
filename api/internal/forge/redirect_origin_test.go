package forge

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the forge-credential redirect boundary: a driver must never
// send a request (credentialed or not) to an origin other than the allowlisted one
// it was built for, however the forge answers. Each test counts requests at the
// forbidden destination and asserts ZERO; an absent Authorization header is not
// enough, because GitLab's PRIVATE-TOKEN is a custom header net/http forwards to
// any host and go-github re-adds its Bearer header on every hop.

// wireLog records every request that reaches the process's outbound transport, the
// layer below every SDK auth wrapper. A request refused before this point never
// left the process.
type wireLog struct {
	mu   sync.Mutex
	next http.RoundTripper
	reqs []wireReq
}

type wireReq struct {
	scheme, host string
	credentialed bool
}

func (w *wireLog) RoundTrip(r *http.Request) (*http.Response, error) {
	w.mu.Lock()
	w.reqs = append(w.reqs, wireReq{
		scheme:       r.URL.Scheme,
		host:         r.URL.Host,
		credentialed: r.Header.Get("Authorization") != "" || r.Header.Get("PRIVATE-TOKEN") != "",
	})
	w.mu.Unlock()
	return w.next.RoundTrip(r)
}

func (w *wireLog) count(scheme string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, r := range w.reqs {
		if r.scheme == scheme {
			n++
		}
	}
	return n
}

// installTrustingWire swaps http.DefaultTransport for one that trusts the
// httptest certificate (every httptest TLS server shares it) and records the wire.
// Drivers capture DefaultTransport when they are built, so call this first. Tests
// using it must not run in parallel: DefaultTransport is process-global.
func installTrustingWire(t *testing.T) *wireLog {
	t.Helper()
	tlsSrv := httptest.NewTLSServer(http.NotFoundHandler())
	trusting := tlsSrv.Client().Transport.(*http.Transport).Clone()
	tlsSrv.Close()
	// forbidden.example.com (a name the httptest certificate covers) dials loopback,
	// so the "other host" case really is another hostname on a reachable server.
	var d net.Dialer
	trusting.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(addr); err == nil && host == otherHost {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
		return d.DialContext(ctx, network, addr)
	}
	w := &wireLog{next: trusting}
	prev := http.DefaultTransport
	http.DefaultTransport = w
	t.Cleanup(func() { http.DefaultTransport = prev })
	return w
}

// forbiddenServer answers every request with a plausible identity so that, on a
// vulnerable client, the call SUCCEEDS against it and the test's hit count is the
// only signal.
type forbiddenServer struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newForbidden(t *testing.T, tls bool) *forbiddenServer {
	t.Helper()
	f := &forbiddenServer{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if strings.HasSuffix(r.URL.Path, "/version") {
			_ = json.NewEncoder(w).Encode(map[string]any{"version": "16.0.0+gitea-1.22.0"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 666, "login": "attacker", "username": "attacker"})
	})
	if tls {
		f.srv = httptest.NewTLSServer(h)
	} else {
		f.srv = httptest.NewServer(h)
	}
	t.Cleanup(f.srv.Close)
	return f
}

// otherHost is a hostname the httptest certificate is valid for; the trusting wire
// dials it to loopback.
const otherHost = "forbidden.example.com"

type redirectCase struct {
	name string
	// target builds the Location from the forbidden server's URL and the path the
	// driver asked for.
	tls    bool
	target func(forbiddenURL, path string) string
}

func redirectCases() []redirectCase {
	return []redirectCase{
		{"other host", true, func(u, p string) string {
			return strings.Replace(u, "127.0.0.1", otherHost, 1) + p
		}},
		{"https to http", false, func(u, p string) string { return u + p }},
		{"other port", true, func(u, p string) string { return u + p }},
	}
}

// driverUnderTest wires one driver type: the primary server's route table (with
// /user redirecting via redirect), and a call that makes the driver GET /user.
type driverUnderTest struct {
	name     string
	userPath string
	build    func(t *testing.T, baseURL string) Forge
	extra    map[string]http.HandlerFunc
}

func driversUnderTest() []driverUnderTest {
	build := func(typ Type) func(*testing.T, string) Forge {
		return func(t *testing.T, base string) Forge {
			t.Helper()
			d, err := New(typ, base, redirectTestToken(), 5*time.Second)
			if err != nil {
				t.Fatalf("New(%s): %v", typ, err)
			}
			return d
		}
	}
	return []driverUnderTest{
		{name: "github", userPath: "/api/v3/user", build: build(TypeGitHub)},
		{name: "gitlab", userPath: "/api/v4/user", build: build(TypeGitLab)},
		{name: "forgejo", userPath: "/api/v1/user", build: build(TypeForgejo), extra: map[string]http.HandlerFunc{
			"/api/v1/version": versionHandler("16.0.0+gitea-1.22.0"),
		}},
	}
}

// redirectTestToken is assembled at runtime so no complete token literal is
// committed (check:token-literals).
func redirectTestToken() string {
	return strings.Join([]string{"redirect", "test", "credential", "0123456789"}, "-")
}

func TestForgeDriversRefuseOffOriginRedirect(t *testing.T) {
	for _, drv := range driversUnderTest() {
		for _, rc := range redirectCases() {
			t.Run(drv.name+"/"+rc.name, func(t *testing.T) {
				wire := installTrustingWire(t)
				bad := newForbidden(t, rc.tls)
				mux := http.NewServeMux()
				for p, h := range drv.extra {
					mux.HandleFunc(p, h)
				}
				mux.HandleFunc(drv.userPath, func(w http.ResponseWriter, r *http.Request) {
					// A hostile forge reflects the credential into the Location and the
					// 3xx body; neither may surface in the driver's error (GitLab now
					// reads this 3xx as its answer rather than following it).
					w.Header().Set("Location", rc.target(bad.srv.URL, drv.userPath)+"?echo="+redirectTestToken())
					w.WriteHeader(http.StatusFound)
					_, _ = w.Write([]byte(`{"message":"` + redirectTestToken() + `"}`))
				})
				var userHits atomic.Int64
				primary := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == drv.userPath {
						userHits.Add(1)
					}
					mux.ServeHTTP(w, r)
				}))
				t.Cleanup(primary.Close)

				_, err := drv.build(t, primary.URL).VerifyToken(context.Background())
				// A refused redirect is final: no SDK retry loop re-sends the request
				// (GitLab's retryable client would, five times, on a CheckRedirect error).
				if got := userHits.Load(); got != 1 {
					t.Fatalf("primary saw %d request(s) for %s; want exactly 1 (no retry)", got, drv.userPath)
				}
				if got := bad.hits.Load(); got != 0 {
					t.Fatalf("forbidden destination received %d request(s); want 0", got)
				}
				if err == nil {
					t.Fatal("VerifyToken succeeded through an off-origin redirect; want an error")
				}
				if strings.Contains(err.Error(), redirectTestToken()) {
					t.Fatalf("error leaks the credential: %v", err)
				}
				if rc.name == "https to http" && wire.count("http") != 0 {
					t.Fatalf("a plaintext request left the process: %+v", wire.reqs)
				}
			})
		}
	}
}

// Positive control: a redirect that stays on the origin is still followed.
func TestForgeDriversFollowSameOriginRedirect(t *testing.T) {
	for _, drv := range driversUnderTest() {
		t.Run(drv.name, func(t *testing.T) {
			installTrustingWire(t)
			mux := http.NewServeMux()
			for p, h := range drv.extra {
				mux.HandleFunc(p, h)
			}
			mux.HandleFunc(drv.userPath, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, drv.userPath+"-moved", http.StatusFound)
			})
			mux.HandleFunc(drv.userPath+"-moved", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "login": "uzi-bot-test", "username": "uzi-bot-test"})
			})
			primary := httptest.NewTLSServer(mux)
			t.Cleanup(primary.Close)

			id, err := drv.build(t, primary.URL).VerifyToken(context.Background())
			if err != nil {
				t.Fatalf("VerifyToken through a same-origin redirect: %v", err)
			}
			if id.ForgeUserID != 4242 {
				t.Fatalf("identity = %+v, want id 4242", id)
			}
		})
	}
}

// go-github follows a 301 on its own (bareDoUntilFound, through a client that
// ignores redirects, so no CheckRedirect runs) and compares only Host, so an https
// to http downgrade on the SAME host:port passes its check with the Bearer header
// attached. Nothing may reach the wire in plaintext.
func TestGitHubJobLogTailRefusesSameHostSchemeDowngrade301(t *testing.T) {
	wire := installTrustingWire(t)
	var primary *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repositories/7", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 7, "name": "widgets", "full_name": "acme/widgets",
			"owner": map[string]any{"id": 1, "login": "acme"},
		})
	})
	mux.HandleFunc("/api/v3/repos/acme/widgets/actions/jobs/55/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", strings.Replace(primary.URL, "https://", "http://", 1)+"/api/v3/moved/logs")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	primary = httptest.NewTLSServer(mux)
	t.Cleanup(primary.Close)

	d, err := newGitHub(primary.URL, redirectTestToken(), 5*time.Second)
	if err != nil {
		t.Fatalf("newGitHub: %v", err)
	}
	if _, err := d.JobLogTail(context.Background(), 7, 55, 0); err == nil {
		t.Fatal("JobLogTail followed a scheme-downgrading 301; want an error")
	}
	if n := wire.count("http"); n != 0 {
		t.Fatalf("%d plaintext request(s) left the process: %+v", n, wire.reqs)
	}
}

// Positive control for the 301 path: a same-origin 301 is followed and the
// legitimate 302 to the blob host still works.
func TestGitHubJobLogTailFollowsSameOrigin301(t *testing.T) {
	installTrustingWire(t)
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("the log\n"))
	}))
	t.Cleanup(blob.Close)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/repositories/7", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 7, "name": "widgets", "full_name": "acme/widgets",
			"owner": map[string]any{"id": 1, "login": "acme"},
		})
	})
	mux.HandleFunc("/api/v3/repos/acme/widgets/actions/jobs/55/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/api/v3/repos/acme/renamed/actions/jobs/55/logs")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	mux.HandleFunc("/api/v3/repos/acme/renamed/actions/jobs/55/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", blob.URL+"/log?token=presigned")
		w.WriteHeader(http.StatusFound)
	})
	primary := httptest.NewTLSServer(mux)
	t.Cleanup(primary.Close)

	d, err := newGitHub(primary.URL, redirectTestToken(), 5*time.Second)
	if err != nil {
		t.Fatalf("newGitHub: %v", err)
	}
	d.allowInsecureLogHost = true
	tail, err := d.JobLogTail(context.Background(), 7, 55, 0)
	if err != nil {
		t.Fatalf("JobLogTail: %v", err)
	}
	if tail != "the log\n" {
		t.Fatalf("tail = %q", tail)
	}
}
