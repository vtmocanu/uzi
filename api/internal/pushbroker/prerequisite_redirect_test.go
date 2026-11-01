package pushbroker_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// Each independent Git request uses a real HTTP Git backend. The redirect target
// is on the same origin and would remain eligible under the ordinary policy.
func TestPrerequisiteNoRedirectEveryGitPhase(t *testing.T) {
	for _, stage := range []string{"prepare", "create-discovery", "create-post", "create-readback", "cleanup-list", "delete-list", "delete-discovery", "delete-post", "delete-readback", "final-list"} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			t.Run(fmt.Sprintf("%s/%d", stage, status), func(t *testing.T) {
				f, remote, url, base, _, _ := heldHTTPFixture(t)
				var active atomic.Bool
				var uploads, targets, redirects atomic.Int32
				var proxyURL string
				proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/redirect-target" {
						targets.Add(1)
						http.Error(w, "wrong repository", 500)
						return
					}
					hit := false
					if active.Load() {
						if r.URL.Query().Get("service") == "git-upload-pack" {
							n := uploads.Add(1)
							hit = stage == "prepare" && n == 1 || stage == "create-readback" && n == 1 ||
								stage == "cleanup-list" && n == 1 || stage == "delete-list" && n == 2 ||
								stage == "delete-readback" && n == 3 || stage == "final-list" && n == 4
						}
						if r.URL.Query().Get("service") == "git-receive-pack" {
							hit = stage == "create-discovery" || stage == "delete-discovery"
						}
						if r.Method == "POST" {
							hit = stage == "create-post" || stage == "delete-post"
						}
					}
					if hit {
						redirects.Add(1)
						w.Header().Set("Location", proxyURL+"/redirect-target")
						w.WriteHeader(status)
						return
					}
					remote.srv.Config.Handler.ServeHTTP(w, r)
				}))
				defer proxy.Close()
				proxyURL = proxy.URL
				clone := proxy.URL + strings.TrimPrefix(url, remote.srv.URL)
				ctx := pushbroker.WithoutRedirects(context.Background())
				o := prereqOpts(clone)
				p, err := pushbroker.PreparePrereqCreate(ctx, o)
				if err != nil {
					t.Fatal(err)
				}
				cleanup := strings.HasPrefix(stage, "delete") || stage == "cleanup-list" || stage == "final-list"
				if cleanup {
					f.git("push", "origin", base+":"+p.Ref())
				}
				active.Store(true)
				switch {
				case stage == "prepare":
					_, err = pushbroker.PreparePrereqCreate(ctx, o)
				case cleanup:
					opts := pushbroker.ListRefsOptions{CloneURL: clone}
					_, err = pushbroker.ListRefTips(ctx, opts, p.Ref())
					if err == nil {
						err = pushbroker.Delete(ctx, pushbroker.DeleteOptions{CloneURL: clone, Ref: p.Ref(), ExpectedOldTip: base})
					}
					if err == nil {
						_, err = pushbroker.ListRefTips(ctx, opts, p.Ref())
					}
				default:
					out, sendErr := pushbroker.SendPrereqCreate(ctx, p)
					err = sendErr
					if out != pushbroker.PrereqCreateUnknown {
						t.Errorf("redirect yielded conclusive create outcome %v", out)
					}
				}
				if err == nil || redirects.Load() != 1 || targets.Load() != 0 {
					t.Fatalf("err=%v redirects=%d target requests=%d", err, redirects.Load(), targets.Load())
				}
			})
		}
	}
}

// The marker is local to the prerequisite call. Ordinary broker clients keep
// following a same-origin redirect through the same native HTTP Git backend.
func TestPrerequisiteUnmarkedSameOriginRedirect(t *testing.T) {
	_, remote, original, base, _, _ := heldHTTPFixture(t)
	var targets, redirects atomic.Int32
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/redirect-target/") {
			targets.Add(1)
			copied := r.Clone(r.Context())
			copied.URL.Path = strings.TrimPrefix(r.URL.Path, "/redirect-target")
			remote.srv.Config.Handler.ServeHTTP(w, copied)
			return
		}
		redirects.Add(1)
		w.Header().Set("Location", "/redirect-target"+r.URL.RequestURI())
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer proxy.Close()
	clone := proxy.URL + strings.TrimPrefix(original, remote.srv.URL)
	ctx := context.Background()
	p, err := pushbroker.PreparePrereqCreate(ctx, prereqOpts(clone))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := pushbroker.SendPrereqCreate(ctx, p)
	if err != nil || outcome != pushbroker.PrereqCreateCreated {
		t.Fatalf("create outcome=%v err=%v", outcome, err)
	}
	if err := pushbroker.Delete(ctx, pushbroker.DeleteOptions{CloneURL: clone, Ref: p.Ref(), ExpectedOldTip: base}); err != nil {
		t.Fatal(err)
	}
	tips, err := pushbroker.ListRefTips(ctx, pushbroker.ListRefsOptions{CloneURL: clone}, p.Ref())
	if err != nil || tips[p.Ref()] != "" {
		t.Fatalf("postdelete tips=%v err=%v", tips, err)
	}
	// go-git may retain the redirected endpoint for later POSTs, so those
	// target requests need not have a new discovery redirect of their own.
	if redirects.Load() == 0 || targets.Load() < redirects.Load() {
		t.Fatalf("redirects=%d targets=%d", redirects.Load(), targets.Load())
	}
}
