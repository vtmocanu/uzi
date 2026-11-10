package pushbroker_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// Characterization of PrepareHeldPack on the production ci_fix "null coordinate" shape and of
// what it reports when its context ends while the HTTP exchange is in flight. It supports
// reading hosted logs in which every "held publication: pack pre-verify failed" line carries
// "context canceled" and none carries a deadline. It does not attribute any hosted attempt.
// The 60 s maxHeldPrepareDuration ceiling is deliberately not timed here.

// promptReturn bounds how long PrepareHeldPack may take to return after its context ends.
const promptReturn = 5 * time.Second

var wantLine = regexp.MustCompile(`want ([0-9a-f]{40})`)

// gatedHeldRemote is a smart-HTTP remote over the fixture's bare repo that records the
// git-upload-pack want lines and can block one stage until the request context ends.
type gatedHeldRemote struct {
	srv *httptest.Server

	// gate is "", "list" (first info/refs for git-upload-pack) or "post" (the upload-pack POST).
	gate    string
	entered chan struct{}

	mu       sync.Mutex
	wants    []string
	infoReqs int
	once     sync.Once
}

// gitUploadPack serves one stateless smart-HTTP upload-pack exchange with the real git binary
// (git-http-backend is not installed everywhere), so the wire bytes are git's own.
func gitUploadPack(w http.ResponseWriter, req *http.Request, bare string, body []byte) {
	args := []string{"upload-pack", "--stateless-rpc"}
	if strings.HasSuffix(req.URL.Path, "/info/refs") {
		args = append(args, "--advertise-refs")
	}
	args = append(args, bare)
	cmd := exec.CommandContext(req.Context(), "git", args...) //nolint:gosec // G204: test helper invoking the real git against a test-owned temp repo path, not attacker input.
	cmd.Env = cleanGitEnv()
	cmd.Stdin = bytes.NewReader(body)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	if strings.HasSuffix(req.URL.Path, "/info/refs") {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = w.Write([]byte("001e# service=git-upload-pack\n0000"))
	} else {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	}
	_, _ = w.Write(out.Bytes())
}

func newGatedHeldRemote(t *testing.T, f *gitFixture, gate string) *gatedHeldRemote {
	t.Helper()
	r := &gatedHeldRemote{gate: gate, entered: make(chan struct{})}
	release := make(chan struct{})
	block := func(req *http.Request) {
		r.once.Do(func() { close(r.entered) })
		select {
		case <-req.Context().Done():
		case <-release:
		}
	}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/info/refs"):
			r.mu.Lock()
			r.infoReqs++
			first := r.infoReqs == 1
			r.mu.Unlock()
			if r.gate == "list" && first && req.URL.Query().Get("service") == "git-upload-pack" {
				block(req)
				return
			}
			gitUploadPack(w, req, f.bare, nil)
		case strings.HasSuffix(req.URL.Path, "/git-upload-pack"):
			body, _ := io.ReadAll(req.Body)
			plain := body
			if req.Header.Get("Content-Encoding") == "gzip" {
				if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
					plain, _ = io.ReadAll(zr)
				}
			}
			r.mu.Lock()
			for _, m := range wantLine.FindAllSubmatch(plain, -1) {
				r.wants = append(r.wants, string(m[1]))
			}
			r.mu.Unlock()
			if r.gate == "post" {
				block(req)
				return
			}
			gitUploadPack(w, req, f.bare, plain)
		default:
			http.NotFound(w, req)
		}
	}))
	// Cleanups run last-in-first-out: release any handler still parked before srv.Close waits.
	t.Cleanup(r.srv.Close)
	t.Cleanup(func() { close(release) })
	return r
}

func (r *gatedHeldRemote) cloneURL(f *gitFixture) string {
	return r.srv.URL + "/" + filepath.Base(f.bare)
}

func (r *gatedHeldRemote) wantedTips() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.wants...)
}

// heldCISetup builds origin with main, decoy agent/checkpoint refs at DIFFERENT commits, and a
// worker pack on top of main. It returns the HeldPackOptions in the production ci_fix shape:
// Branch, WorkBranch and CheckpointKey empty, only DefaultBranch set.
func heldCISetup(t *testing.T, f *gitFixture) (opts pushbroker.HeldPackOptions, mainTip string, decoys []string) {
	t.Helper()
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	f.git("checkout", "-q", "-b", "decoy")
	d1 := f.commit("d1.txt", "decoy one\n", "decoy heads")
	d2 := f.commit("d2.txt", "decoy two\n", "decoy checkpoint")
	f.git("push", "origin", d1+":refs/heads/agent/issue-4140")
	f.git("push", "origin", d2+":refs/uzi-checkpoints/agent/issue-4140")
	f.git("checkout", "-q", "main")
	tip := f.commit("b.txt", "one\n", "c1")
	return pushbroker.HeldPackOptions{
		Username: "uzi-bot", PAT: redirectTestPAT(), RunID: salvageRunID, Generation: 1,
		DefaultBranch: "main", Tip: tip, Pack: f.pack(tip, base),
	}, base, []string{d1, d2}
}

func TestPrepareHeldPackNullCoordinatesRequestsOnlyDefaultBranch(t *testing.T) {
	trustLoopbackTLS(t)
	f := newGitFixture(t)
	opts, mainTip, _ := heldCISetup(t, f)
	for _, ref := range []string{"refs/heads/agent/issue-4140", "refs/uzi-checkpoints/agent/issue-4140"} {
		if f.originRef(ref) == "" {
			t.Fatalf("decoy ref %s missing on origin", ref)
		}
	}
	remote := newGatedHeldRemote(t, f, "")
	opts.CloneURL = remote.cloneURL(f)

	if _, err := pushbroker.PrepareHeldPack(context.Background(), opts); err != nil {
		t.Fatalf("PrepareHeldPack: %v", err)
	}
	wants := remote.wantedTips()
	if len(wants) != 1 || wants[0] != mainTip {
		t.Fatalf("upload-pack want lines = %v, want exactly the default branch tip %s", wants, mainTip)
	}
}

// causeKept reports whether the context cause is visible to errors.Is at a stage. LIMITATION
// (go-git as pinned): at the "list" stage (info/refs) the transport error wraps the context
// error, so errors.Is works. At the "post" stage (the git-upload-pack POST inside fetch) go-git
// wraps the client error in a *plumbing.UnexpectedError, which holds the original error in its Err
// field but has no Unwrap method, so errors.Is(err, context.Canceled) and
// errors.Is(err, context.DeadlineExceeded) are BOTH false on the returned error. The cause is
// still recoverable via errors.As to *plumbing.UnexpectedError and errors.Is on its Err, and by
// the error text. Anything classifying a PrepareHeldPack error by errors.Is alone therefore
// misses the post stage; the hosted "pack pre-verify failed ... context canceled" text is
// consistent with either stage.
var causeKept = map[string]bool{"list": true, "post": false}

// assertCause checks the cause at a stage: through errors.Is where go-git keeps it, and by
// error text everywhere. The opposite cause must never appear.
func assertCause(t *testing.T, stage string, err error, want error, wantText string, other error, otherText string) {
	t.Helper()
	if causeKept[stage] {
		if !errors.Is(err, want) {
			t.Fatalf("errors.Is(err, %v) = false at stage %s; err = %v", want, stage, err)
		}
	} else if errors.Is(err, want) {
		t.Fatalf("errors.Is(err, %v) = true at stage %s: go-git now keeps the cause; update causeKept and the limitation note; err = %v", want, stage, err)
	}
	if errors.Is(err, other) || strings.Contains(err.Error(), otherText) {
		t.Fatalf("stage %s reported the wrong cause (%s): %v", stage, otherText, err)
	}
	if !strings.Contains(err.Error(), wantText) {
		t.Fatalf("stage %s error text lacks %q: %v", stage, wantText, err)
	}
	if stage == "post" {
		var ue *plumbing.UnexpectedError
		if !errors.As(err, &ue) {
			t.Fatalf("stage post error is not a *plumbing.UnexpectedError: %T %v", err, err)
		}
		if !errors.Is(ue.Err, want) || errors.Is(ue.Err, other) {
			t.Fatalf("UnexpectedError.Err = %v, want cause %v and not %v", ue.Err, want, other)
		}
	}
}

func TestPrepareHeldPackReportsContextCause(t *testing.T) {
	for _, stage := range []string{"list", "post"} {
		t.Run(stage+"/parent cancelled", func(t *testing.T) {
			trustLoopbackTLS(t)
			f := newGitFixture(t)
			opts, _, _ := heldCISetup(t, f)
			remote := newGatedHeldRemote(t, f, stage)
			opts.CloneURL = remote.cloneURL(f)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				err error
				at  time.Time
			}
			done := make(chan result, 1)
			go func() {
				_, err := pushbroker.PrepareHeldPack(ctx, opts)
				done <- result{err, time.Now()}
			}()
			select {
			case <-remote.entered:
			case <-time.After(30 * time.Second):
				t.Fatal("handler gate never entered")
			}
			cancelledAt := time.Now()
			cancel()
			select {
			case res := <-done:
				elapsed := res.at.Sub(cancelledAt)
				t.Logf("stage=%s elapsed after cancel=%v err=%v", stage, elapsed, res.err)
				if res.err == nil {
					t.Fatal("PrepareHeldPack succeeded against a blocked remote")
				}
				if elapsed > promptReturn {
					t.Fatalf("returned %v after cancel, want < %v", elapsed, promptReturn)
				}
				assertCause(t, stage, res.err, context.Canceled, "context canceled", context.DeadlineExceeded, "deadline exceeded")
			case <-time.After(promptReturn + 10*time.Second):
				t.Fatal("PrepareHeldPack did not return after the parent context was cancelled")
			}
		})

		t.Run(stage+"/parent deadline", func(t *testing.T) {
			trustLoopbackTLS(t)
			f := newGitFixture(t)
			opts, _, _ := heldCISetup(t, f)
			remote := newGatedHeldRemote(t, f, stage)
			opts.CloneURL = remote.cloneURL(f)

			// The parent deadline is far below the 60 s internal ceiling, so it is the
			// deadline that fires. It starts at call time; the gate is reached within it only
			// if the fixture is fast, so the budget is generous relative to a local CGI hop.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			start := time.Now()
			_, err := pushbroker.PrepareHeldPack(ctx, opts)
			elapsed := time.Since(start)
			t.Logf("stage=%s elapsed=%v err=%v", stage, elapsed, err)
			select {
			case <-remote.entered:
			default:
				t.Fatalf("handler gate never entered before the deadline; err = %v", err)
			}
			if err == nil {
				t.Fatal("PrepareHeldPack succeeded against a blocked remote")
			}
			if elapsed > 3*time.Second+promptReturn {
				t.Fatalf("returned after %v, want shortly after the 3s parent deadline", elapsed)
			}
			assertCause(t, stage, err, context.DeadlineExceeded, "context deadline exceeded", context.Canceled, "context canceled")
		})
	}
}
