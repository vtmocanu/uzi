package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Stable ID A can move to new while old is reused by B. Both Git routes expose
// exactly the same default and pin SHAs; only metadata can distinguish them.
type prerequisiteIdentityFixture struct {
	s            *Service
	m            *prerequisiteMemory
	h            *prerequisiteHTTP
	current      atomic.Value
	mu           sync.Mutex
	entries      []string
	kind         string
	git, lookups atomic.Int32
	uploads      atomic.Int32
	onGit        func(string)
	afterGit     func(string)
	onCompare    func()
	metadata     func(map[string]any)
	compare      string
	url          string
}

func newPrerequisiteIdentityFixture(t *testing.T, kind string) *prerequisiteIdentityFixture {
	t.Helper()
	s, m, h, _ := newPrerequisiteMemory(t, 1)
	f := &prerequisiteIdentityFixture{s: s, m: m, h: h, compare: "behind", kind: kind}
	f.current.Store("old")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if len(f.entries) < 128 {
			f.entries = append(f.entries, prerequisiteDriverEntry(r.Method, r.URL.Path, r.URL.Query().Encode()))
		} else {
			t.Error("identity HTTP observation limit exceeded")
		}
		f.mu.Unlock()
		if m.inTX.Load() {
			t.Error("identity fixture HTTP inside transaction")
		}
		if strings.Contains(r.URL.Path, ".git/") {
			validGit := false
			for _, slug := range []string{"old", "new"} {
				root := "/acme/" + slug + ".git/"
				validGit = validGit || r.Method == "GET" && r.URL.Path == root+"info/refs" && (r.URL.RawQuery == "service=git-upload-pack" || r.URL.RawQuery == "service=git-receive-pack") || r.Method == "POST" && r.URL.Path == root+"git-receive-pack" && r.URL.RawQuery == ""
			}
			if !validGit {
				t.Errorf("unexpected identity Git entry %s", prerequisiteDriverEntry(r.Method, r.URL.Path, r.URL.Query().Encode()))
				http.NotFound(w, r)
				return
			}
			f.git.Add(1)
			stage := "receive"
			if r.Method == "POST" {
				stage = "post"
			} else if r.URL.Query().Get("service") == "git-upload-pack" {
				stage = fmt.Sprintf("upload%d", f.uploads.Add(1))
			}
			if f.onGit != nil {
				f.onGit(stage)
			}
			h.serve(w, r)
			if f.afterGit != nil {
				f.afterGit(stage)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		lookupPath := "/api/v3/repositories/1"
		if kind == "forgejo" {
			lookupPath = "/api/v1/repositories/1"
		}
		if kind == "gitlab" {
			lookupPath = "/api/v4/projects/1"
		}
		if r.Method == "GET" && r.URL.RawQuery == "" && r.URL.Path == lookupPath {
			f.lookups.Add(1)
			slug := "acme/" + f.current.Load().(string)
			p := map[string]any{"id": 1, "name": f.current.Load().(string), "full_name": slug, "path": f.current.Load().(string), "path_with_namespace": slug, "html_url": f.url + "/" + slug, "web_url": f.url + "/" + slug, "owner": map[string]any{"login": "acme"}, "namespace": map[string]any{"full_path": "acme"}}
			if f.metadata != nil {
				f.metadata(p)
			}
			_ = json.NewEncoder(w).Encode(p)
			return
		}
		prefix := "/api/v3/repos/acme/"
		if kind == "forgejo" {
			prefix = "/api/v1/repos/acme/"
		}
		branch := strings.TrimSuffix(r.URL.Path, "/branches/main")
		if r.Method == "GET" && r.URL.RawQuery == "" && (branch == prefix+"old" || branch == prefix+"new" || r.URL.Path == "/api/v4/projects/1/repository/branches/main") && strings.HasSuffix(r.URL.Path, "/branches/main") {
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]any{"sha": prerequisiteTestTip, "id": prerequisiteTestTip}})
			return
		}
		sha := m.row.PrerequisiteShas[0]
		forward := prerequisiteTestTip + "..." + sha
		reverse := sha + "..." + prerequisiteTestTip
		validCompare := r.Method == "GET" && r.URL.RawQuery == "" && (r.URL.Path == prefix+"old/compare/"+forward || r.URL.Path == prefix+"new/compare/"+forward || kind == "forgejo" && (r.URL.Path == prefix+"old/compare/"+reverse || r.URL.Path == prefix+"new/compare/"+reverse))
		if kind == "gitlab" {
			validCompare = r.Method == "GET" && r.URL.Path == "/api/v4/projects/1/repository/merge_base" && r.URL.Query().Encode() == (url.Values{"refs[]": {prerequisiteTestTip, sha}}).Encode()
		}
		if !validCompare {
			t.Errorf("unexpected identity HTTP entry %s", prerequisiteDriverEntry(r.Method, r.URL.Path, r.URL.Query().Encode()))
			http.NotFound(w, r)
			return
		}
		if f.onCompare != nil {
			f.onCompare()
		}
		switch kind {
		case "gitlab":
			sha := m.row.PrerequisiteShas[0]
			if f.compare == "ahead" {
				sha = prerequisiteTestTip
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": sha})
		case "forgejo":
			n := 0
			if strings.HasSuffix(r.URL.Path, "..."+prerequisiteTestTip) {
				n = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_commits": n})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": f.compare})
		}
	}))
	t.Cleanup(server.Close)
	f.url = server.URL
	m.conn.BaseUrl, m.conn.ForgeType = server.URL, kind
	m.repo.WebUrl = server.URL + "/acme/old"
	s.SetForgeBaseURLAllowed(func(base string) bool { return base == server.URL })
	s.SetForges(&prerequisiteDriverBuilder{s: s})
	return f
}

func (f *prerequisiteIdentityFixture) drift()           { f.current.Store("new") }
func (f *prerequisiteIdentityFixture) run(t *testing.T) { prerequisiteRun(t, f.s, f.m) }
func (f *prerequisiteIdentityFixture) pin(t *testing.T) {
	f.run(t)
	if f.m.row.State != "pinned" {
		t.Fatalf("setup state=%s", f.m.row.State)
	}
	f.uploads.Store(0)
	f.git.Store(0)
}
func (f *prerequisiteIdentityFixture) retained(t *testing.T, state string) {
	t.Helper()
	if f.m.row.State != state || !f.m.row.LiveRunID.Valid {
		t.Fatalf("state=%s live=%v want=%s with retention", f.m.row.State, f.m.row.LiveRunID.Valid, state)
	}
}

func TestPrerequisiteStableIDRenameAndReuse(t *testing.T) {
	for _, kind := range []string{"github", "gitlab", "forgejo"} {
		for _, refreshed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/refreshed=%v", kind, refreshed), func(t *testing.T) {
				f := newPrerequisiteIdentityFixture(t, kind)
				f.drift()
				if refreshed {
					f.m.repo.WebUrl = f.url + "/acme/new"
				}
				f.run(t)
				if refreshed {
					f.retained(t, "pinned")
					f.assertNewTraffic(t)
					if f.h.sends != 1 {
						t.Fatal("refreshed route did not create")
					}
				} else if f.git.Load() != 0 || f.m.row.State != "pending" || f.m.marked != 0 {
					t.Fatalf("stale route traffic=%d state=%s markers=%d", f.git.Load(), f.m.row.State, f.m.marked)
				} else {
					// A mismatched preflight must stop before any ancestry request.
					f.mu.Lock()
					entries := append([]string(nil), f.entries...)
					f.mu.Unlock()
					if len(entries) != 1 || f.lookups.Load() != 1 {
						t.Fatalf("mismatched preflight HTTP=%v lookups=%d", entries, f.lookups.Load())
					}
				}
			})
		}
	}
}

// Require the full ordered traffic, including both ancestry directions on
// Forgejo and the stable numeric ancestry endpoint on GitLab. Old exposes the
// same SHAs, so a successful state alone cannot establish the repository used.
func (f *prerequisiteIdentityFixture) assertNewTraffic(t *testing.T) {
	t.Helper()
	prefix := "/api/v3"
	if f.kind == "forgejo" {
		prefix = "/api/v1"
	}
	lookup := "GET " + prefix + "/repositories/1"
	compare := []string{"GET " + prefix + "/repos/acme/new/compare/" + prerequisiteTestTip + "..." + f.m.row.PrerequisiteShas[0]}
	if f.kind == "forgejo" {
		compare = append(compare, "GET "+prefix+"/repos/acme/new/compare/"+f.m.row.PrerequisiteShas[0]+"..."+prerequisiteTestTip)
	}
	if f.kind == "gitlab" {
		lookup = "GET /api/v4/projects/1"
		compare = []string{prerequisiteDriverEntry("GET", "/api/v4/projects/1/repository/merge_base", (url.Values{"refs[]": {prerequisiteTestTip, f.m.row.PrerequisiteShas[0]}}).Encode())}
	}
	upload := "GET /acme/new.git/info/refs?service=git-upload-pack"
	want := []string{lookup, upload, lookup}
	want = append(want, compare...)
	want = append(want, lookup, lookup, "GET /acme/new.git/info/refs?service=git-receive-pack", "POST /acme/new.git/git-receive-pack", upload, lookup)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.entries, want) {
		for i := 0; i < max(len(f.entries), len(want)); i++ {
			if i >= len(f.entries) || i >= len(want) {
				t.Fatalf("HTTP length got=%d want=%d; first missing/extra at %d", len(f.entries), len(want), i)
			}
			if f.entries[i] != want[i] {
				t.Fatalf("HTTP[%d]=%s want=%s", i, f.entries[i], want[i])
			}
		}
	}
}

func TestPrerequisiteReusedDriverRefreshesAncestrySlug(t *testing.T) {
	for _, kind := range []string{"github", "gitlab", "forgejo"} {
		t.Run(kind, func(t *testing.T) {
			f := newPrerequisiteIdentityFixture(t, kind)
			driver, err := forge.New(forge.Type(kind), f.url, "fixture-credential", 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			project, err := driver.GetProject(context.Background(), 1)
			if err != nil || project.PathWithNamespace != "acme/old" {
				t.Fatalf("seed project=%+v err=%v", project, err)
			}
			tip, err := driver.BranchHead(context.Background(), 1, "main")
			if err != nil || tip != prerequisiteTestTip {
				t.Fatalf("seed branch=%s err=%v", tip, err)
			}
			f.s.SetForges(&prerequisiteTestBuilder{f: driver})
			f.mu.Lock()
			f.entries = nil
			f.mu.Unlock()
			f.drift()
			f.m.repo.WebUrl = f.url + "/acme/new"
			f.run(t)
			f.retained(t, "pinned")
			f.assertNewTraffic(t)
		})
	}
}

func TestPrerequisiteMetadataURLTerminalSlash(t *testing.T) {
	for _, kind := range []string{"github", "gitlab", "forgejo"} {
		for _, suffix := range []string{"/", "//"} {
			t.Run(kind+"/"+suffix, func(t *testing.T) {
				f := newPrerequisiteIdentityFixture(t, kind)
				f.metadata = func(p map[string]any) {
					p["html_url"] = f.m.repo.WebUrl + suffix
					p["web_url"] = f.m.repo.WebUrl + suffix
				}
				f.run(t)
				if suffix == "/" {
					f.retained(t, "pinned")
				} else if f.git.Load() != 0 || f.m.row.State != "pending" || f.m.marked != 0 {
					t.Fatalf("double slash trusted: git=%d row=%+v", f.git.Load(), f.m.row)
				}
			})
		}
	}
}

func TestPrerequisitePostPrepareIdentity(t *testing.T) {
	f := newPrerequisiteIdentityFixture(t, "github")
	f.h.tip = prerequisiteTestTip
	f.afterGit = func(stage string) {
		if stage == "upload1" {
			f.drift()
		}
	}
	f.run(t)
	if f.m.row.State != "pending" || f.m.row.Reason == "pin_foreign" || f.h.sends != 0 {
		t.Fatalf("foreign refusal trusted after drift: %+v", f.m.row)
	}
}

func TestPrerequisitePostProofIdentity(t *testing.T) {
	for _, result := range []string{"behind", "ahead"} {
		t.Run(result, func(t *testing.T) {
			f := newPrerequisiteIdentityFixture(t, "github")
			f.compare = result
			f.onCompare = f.drift
			f.run(t)
			if f.m.row.State != "pending" || f.m.marked != 0 || f.h.sends != 0 {
				t.Fatalf("proof trusted after drift: %+v", f.m.row)
			}
		})
	}
}

func TestPrerequisitePreSendIdentity(t *testing.T) {
	f := newPrerequisiteIdentityFixture(t, "github")
	f.s.SetPrerequisiteStore(f.m, func(ctx context.Context, fn func(PrerequisiteStore) error) error {
		err := f.m.transaction(ctx, fn)
		if f.m.marked != 0 {
			f.drift()
		}
		return err
	})
	f.run(t)
	f.retained(t, "invoked")
	if f.h.sends != 0 {
		t.Fatal("send crossed identity drift after committed marker")
	}
}

func TestPrerequisitePostSendIdentity(t *testing.T) {
	for _, stage := range []string{"receive", "post", "upload2"} {
		t.Run(stage, func(t *testing.T) {
			f := newPrerequisiteIdentityFixture(t, "github")
			f.afterGit = func(got string) {
				if got == stage {
					f.drift()
				}
			}
			f.run(t)
			f.retained(t, "create_unknown")
			if f.h.sends != 1 {
				t.Fatal("fixture did not send exactly once")
			}
			f.run(t)
			f.retained(t, "create_unknown")
			if f.h.sends != 1 {
				t.Fatal("ambiguous invocation resent")
			}
		})
	}
}

func TestPrerequisiteListingIdentity(t *testing.T) {
	for _, state := range []string{"create_unknown", "audit", "cleanup", "lost", "foreign"} {
		for _, when := range []string{"prelist", "postlist"} {
			t.Run(state+"/"+when, func(t *testing.T) {
				f := newPrerequisiteIdentityFixture(t, "github")
				f.pin(t)
				if state == "create_unknown" {
					f.m.row.State = state
				}
				if state == "cleanup" {
					f.m.capture.State = "expired"
					f.h.tip = ""
				}
				if state == "lost" {
					f.h.tip = ""
					f.m.row.AbsentSince = prerequisiteTime(f.m.now.Add(-time.Hour))
				}
				if state == "foreign" {
					f.h.tip = strings.Repeat("b", 40)
				}
				if when == "prelist" {
					f.drift()
				} else {
					f.afterGit = func(stage string) {
						if stage == "upload1" {
							f.drift()
						}
					}
				}
				f.run(t)
				want := "pinned"
				if state == "create_unknown" {
					want = state
				}
				f.retained(t, want)
				if f.h.deletes != 0 {
					t.Fatal("listing drift authorized delete")
				}
				if when == "prelist" && f.git.Load() != 0 {
					t.Fatal("prelist drift reached Git")
				}
			})
		}
	}
}

type prerequisiteIdentityStore struct {
	*prerequisiteMemory
	afterReserve func()
}

func (q *prerequisiteIdentityStore) ReservePrerequisiteCleanup(ctx context.Context, a store.ReservePrerequisiteCleanupParams) (store.RecoveryPrerequisiteClosure, error) {
	row, err := q.prerequisiteMemory.ReservePrerequisiteCleanup(ctx, a)
	if err == nil {
		q.afterReserve()
	}
	return row, err
}

func TestPrerequisitePreDeleteIdentity(t *testing.T) {
	f := newPrerequisiteIdentityFixture(t, "github")
	f.pin(t)
	f.m.capture.State = "expired"
	q := &prerequisiteIdentityStore{prerequisiteMemory: f.m, afterReserve: f.drift}
	f.s.SetPrerequisiteStore(q, func(ctx context.Context, fn func(PrerequisiteStore) error) error {
		return f.m.transaction(ctx, func(PrerequisiteStore) error { return fn(q) })
	})
	f.run(t)
	f.retained(t, "pinned")
	if f.h.deletes != 0 || f.m.row.CleanupAttempts != 1 {
		t.Fatal("reserved delete crossed identity drift")
	}
}

func TestPrerequisitePostDeleteIdentity(t *testing.T) {
	f := newPrerequisiteIdentityFixture(t, "github")
	f.pin(t)
	f.m.capture.State = "expired"
	var returned atomic.Bool
	f.afterGit = func(stage string) {
		if stage == "upload3" {
			f.drift()
			returned.Store(true)
		}
	}
	// The rename is transient: a new observation window would see the old route
	// again. Only validation in the delete window can reject its internal readback.
	f.s.SetClock(func() time.Time {
		if returned.Load() {
			f.current.Store("old")
		}
		return f.m.now
	})
	f.run(t)
	f.retained(t, "pinned")
	if f.h.deletes != 1 {
		t.Fatal("fixture did not delete")
	}
}

func TestPrerequisitePostDeleteListIdentity(t *testing.T) {
	f := newPrerequisiteIdentityFixture(t, "github")
	f.pin(t)
	f.m.capture.State = "expired"
	f.afterGit = func(stage string) {
		if stage == "upload4" {
			f.drift()
		}
	}
	f.run(t)
	f.retained(t, "pinned")
	if f.h.deletes != 1 {
		t.Fatal("fixture did not delete")
	}
}

func TestPrerequisiteIdentityMetadataFailures(t *testing.T) {
	for _, mode := range []string{"wrong-id", "missing-id", "missing-url", "conflicting-url", "path", "query", "userinfo", "fragment", "trailing-slash"} {
		t.Run(mode, func(t *testing.T) {
			f := newPrerequisiteIdentityFixture(t, "github")
			f.metadata = func(p map[string]any) {
				switch mode {
				case "wrong-id":
					p["id"] = 2
				case "missing-id":
					delete(p, "id")
				case "missing-url":
					delete(p, "html_url")
				case "conflicting-url":
					p["html_url"] = f.url + "/acme/new"
				case "path":
					p["full_name"] = "acme/new"
				case "query":
					p["html_url"] = f.m.repo.WebUrl + "?x=1"
				case "userinfo":
					p["html_url"] = strings.Replace(f.m.repo.WebUrl, "://", "://user@", 1)
				case "fragment":
					p["html_url"] = f.m.repo.WebUrl + "#x"
				}
			}
			if mode == "trailing-slash" {
				f.m.repo.WebUrl += "/"
			}
			f.run(t)
			if mode == "trailing-slash" {
				f.retained(t, "pinned")
			} else if f.git.Load() != 0 || f.m.row.State != "pending" {
				t.Fatalf("metadata failure trusted, traffic=%d row=%+v", f.git.Load(), f.m.row)
			}
		})
	}
}

func TestPrerequisiteCachedURLBeforeBuilder(t *testing.T) {
	for _, suffix := range []string{"?x=1", "#x", "?", "/%2e%2e", "/../x"} {
		t.Run(suffix, func(t *testing.T) {
			s, m, h, b := newPrerequisiteMemory(t, 1)
			m.repo.WebUrl += suffix
			prerequisiteRun(t, s, m)
			if b.builds != 0 || h.requests.Load() != 0 || m.marked != 0 {
				t.Fatal("invalid URL reached credentials or Git")
			}
		})
	}
}

func TestPrerequisiteLookupUsesExistingWindow(t *testing.T) {
	// Keep the unused HTTP listener outside the timer bubble: this lookup must
	// expire before Git can start, without adding a real-time wait to the suite.
	s, m, h, b := newPrerequisiteMemory(t, 1)
	synctest.Test(t, func(t *testing.T) {
		b.f = &prerequisiteTestForge{getProject: func(ctx context.Context, _ int64) (forge.Project, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > time.Second {
				t.Error("lookup lacks existing 1s cap")
			}
			if m.inTX.Load() {
				t.Error("lookup inside transaction")
			}
			<-ctx.Done()
			return forge.Project{}, ctx.Err()
		}}
		start := time.Now()
		prerequisiteRun(t, s, m)
		if time.Since(start) != time.Second || h.requests.Load() != 0 || m.row.State != "pending" || m.marked != 0 {
			t.Fatal("slow lookup spent send authority/reserve")
		}
	})
}
