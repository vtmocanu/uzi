package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Issue #1582 M1 driver tests for BranchHead and CompareAncestry across all three drivers.
// The invariant under test: only an explicit, recognized positive answer is
// AncestryAncestor; every error, rate limit, oversize body, or unrecognized response is
// AncestryUnknown, and head == candidate is answered without any request.

const (
	ancHead = "1111111111111111111111111111111111111111"
	ancCand = "2222222222222222222222222222222222222222"
	ancMB   = "3333333333333333333333333333333333333333"
	// ancPAT is a deliberately NON-provider-shaped secret: the tests assert it never
	// survives into an error string.
	ancPAT = "ancestry-test-secret-value-0001"
)

// pathRecorder records every request path (escaped) + raw query a mock received.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (p *pathRecorder) add(r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		s += "?" + r.URL.RawQuery
	}
	p.paths = append(p.paths, s)
}

func (p *pathRecorder) list() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

func ancWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// oversizeBody is a JSON object whose recognizable field sits BEHIND padding larger than
// every ancestry read ceiling, so a driver that read past the ceiling would see it.
func oversizeBody(field string, value any) []byte {
	pad := strings.Repeat("x", forgejoCompareBodyLimit+16)
	b, _ := json.Marshal(map[string]any{"padding": pad, field: value})
	return b
}

// slowHandler blocks until the client gives up (or a generous cap), for the timeout case.
func slowHandler(w http.ResponseWriter, r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(3 * time.Second):
	}
	w.WriteHeader(http.StatusOK)
}

func assertNoPAT(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), ancPAT) {
		t.Fatalf("error leaks the PAT: %v", err)
	}
}

// ── GitHub ─────────────────────────────────────────────────────────────────────────────

func TestGitHubBranchHead(t *testing.T) {
	const branch = "agent/issue-7"
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantSHA string
		wantNF  bool
	}{
		{"found", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"name": branch, "commit": map[string]any{"sha": ancHead}})
		}, ancHead, false},
		{"missing", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 404, map[string]any{"message": "Branch not found"})
		}, "", true},
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 500, map[string]any{"message": "boom " + ancPAT})
		}, "", false},
		{"rate limited 429", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			ancWriteJSON(w, 429, map[string]any{"message": "slow down"})
		}, "", false},
		{"renamed branch resolves another name", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"name": "other", "commit": map[string]any{"sha": ancHead}})
		}, "", false},
		{"malformed sha", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"name": branch, "commit": map[string]any{"sha": "ABC"}})
		}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &pathRecorder{}
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/branches/": func(w http.ResponseWriter, r *http.Request) {
					rec.add(r)
					tc.handler(w, r)
				},
			})
			d := newGitHubDriver(t, m, ancPAT)
			got, err := d.BranchHead(context.Background(), 7, branch)
			assertNoPAT(t, err)
			if tc.wantSHA != "" {
				if err != nil || got != tc.wantSHA {
					t.Fatalf("BranchHead = (%q, %v), want %q", got, err, tc.wantSHA)
				}
			} else if err == nil {
				t.Fatalf("BranchHead = %q, want an error", got)
			}
			if errors.Is(err, ErrRefNotFound) != tc.wantNF {
				t.Fatalf("errors.Is(ErrRefNotFound) = %v, want %v (err %v)", errors.Is(err, ErrRefNotFound), tc.wantNF, err)
			}
			if p := rec.list(); len(p) != 1 || p[0] != "/api/v3/repos/acme/widgets/branches/agent%2Fissue-7" {
				t.Fatalf("requested paths = %v, want the escaped branch path once", p)
			}
		})
	}
}

func TestGitHubCompareAncestry(t *testing.T) {
	status := func(s string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"status": s, "ahead_by": 0, "behind_by": 3, "commits": []any{}})
		}
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		ctxTO   time.Duration
		want    Ancestry
	}{
		{"identical", status("identical"), 0, AncestryAncestor},
		{"behind", status("behind"), 0, AncestryAncestor},
		{"ahead", status("ahead"), 0, AncestryNotAncestor},
		{"diverged", status("diverged"), 0, AncestryNotAncestor},
		{"unrecognized status", status("sideways"), 0, AncestryUnknown},
		{"missing status with a commits array", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"total_commits": 0, "commits": []any{map[string]any{"sha": ancCand}}})
		}, 0, AncestryUnknown},
		{"not json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }, 0, AncestryUnknown},
		{"404", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 404, map[string]any{"message": "Not Found"})
		}, 0, AncestryUnknown},
		{"422", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 422, map[string]any{"message": "No common ancestor"})
		}, 0, AncestryUnknown},
		{"500", func(w http.ResponseWriter, _ *http.Request) { ancWriteJSON(w, 500, map[string]any{"message": "x"}) }, 0, AncestryUnknown},
		{"429", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "60")
			ancWriteJSON(w, 429, map[string]any{"message": "slow"})
		}, 0, AncestryUnknown},
		{"rate-limit 403", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Reset", "4102444800")
			ancWriteJSON(w, 403, map[string]any{"message": "API rate limit exceeded"})
		}, 0, AncestryUnknown},
		{"oversize", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(oversizeBody("status", "behind"))
		}, 0, AncestryUnknown},
		{"timeout", slowHandler, 100 * time.Millisecond, AncestryUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &pathRecorder{}
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/compare/": func(w http.ResponseWriter, r *http.Request) {
					rec.add(r)
					tc.handler(w, r)
				},
			})
			d := newGitHubDriver(t, m, ancPAT)
			ctx := context.Background()
			if tc.ctxTO > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.ctxTO)
				defer cancel()
			}
			got, err := d.CompareAncestry(ctx, 7, ancHead, ancCand)
			assertNoPAT(t, err)
			if got != tc.want {
				t.Fatalf("CompareAncestry = (%q, %v), want %q", got, err, tc.want)
			}
			if tc.want == AncestryUnknown && err == nil {
				t.Fatalf("an unknown answer must carry an error")
			}
			if tc.want != AncestryUnknown && err != nil {
				t.Fatalf("a conclusive answer must not carry an error: %v", err)
			}
			if tc.name == "rate-limit 403" {
				var rle *RateLimitError
				if !errors.As(err, &rle) {
					t.Fatalf("rate-limit 403 error = %T %v, want *RateLimitError", err, err)
				}
			}
			if p := rec.list(); len(p) != 1 || p[0] != "/api/v3/repos/acme/widgets/compare/"+ancHead+"..."+ancCand {
				t.Fatalf("requested paths = %v, want exactly compare/{head}...{candidate}", p)
			}
		})
	}
}

func TestGitHubCompareAncestryNoRequestCases(t *testing.T) {
	m := newMockGitHub(t, nil)
	d := newGitHubDriver(t, m, ancPAT)
	if a, err := d.CompareAncestry(context.Background(), 7, ancHead, ancHead); a != AncestryAncestor || err != nil {
		t.Fatalf("head == candidate = (%q, %v), want ancestor", a, err)
	}
	for _, bad := range []string{"main", strings.ToUpper("abcdef" + ancCand[6:]), ancCand[:39], ancCand + "0", ""} {
		if a, err := d.CompareAncestry(context.Background(), 7, ancHead, bad); a != AncestryUnknown || err == nil {
			t.Fatalf("candidate %q = (%q, %v), want unknown + error", bad, a, err)
		}
	}
	if n := m.reqCount.Load(); n != 0 {
		t.Fatalf("made %d requests, want 0 (equal SHAs and malformed args never reach the forge)", n)
	}
}

func TestGitHubCompareAncestryScrubsPAT(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/compare/": func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 500, map[string]any{"message": "token " + ancPAT + " rejected"})
		},
	})
	d := newGitHubDriver(t, m, ancPAT)
	a, err := d.CompareAncestry(context.Background(), 7, ancHead, ancCand)
	if a != AncestryUnknown || err == nil {
		t.Fatalf("got (%q, %v), want unknown + error", a, err)
	}
	if strings.Contains(err.Error(), ancPAT) || !strings.Contains(err.Error(), redactPlaceholder) {
		t.Fatalf("error %q: want the echoed PAT redacted", err.Error())
	}
}

// ── GitLab ─────────────────────────────────────────────────────────────────────────────

func TestGitLabBranchHead(t *testing.T) {
	const branch = "agent/issue-7"
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantSHA string
		wantNF  bool
	}{
		{"found", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"name": branch, "commit": map[string]any{"id": ancHead}})
		}, ancHead, false},
		{"missing", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 404, map[string]any{"message": "404 Branch Not Found"})
		}, "", true},
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 500, map[string]any{"message": ancPAT})
		}, "", false},
		{"rate limited 429", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "60")
			ancWriteJSON(w, 429, map[string]any{"message": "slow"})
		}, "", false},
		{"malformed id", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"name": branch, "commit": map[string]any{"id": "abc"}})
		}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &pathRecorder{}
			m := newMockGitLab(t, map[string]http.HandlerFunc{
				"/api/v4/projects/7/repository/branches/": func(w http.ResponseWriter, r *http.Request) {
					rec.add(r)
					tc.handler(w, r)
				},
			})
			d := newTestDriver(t, m, ancPAT)
			got, err := d.BranchHead(context.Background(), 7, branch)
			assertNoPAT(t, err)
			if tc.wantSHA != "" {
				if err != nil || got != tc.wantSHA {
					t.Fatalf("BranchHead = (%q, %v), want %q", got, err, tc.wantSHA)
				}
			} else if err == nil {
				t.Fatalf("BranchHead = %q, want an error", got)
			}
			if errors.Is(err, ErrRefNotFound) != tc.wantNF {
				t.Fatalf("errors.Is(ErrRefNotFound) = %v, want %v (err %v)", errors.Is(err, ErrRefNotFound), tc.wantNF, err)
			}
			// Exactly one request: a 429 is answered immediately, never retried.
			if p := rec.list(); len(p) != 1 || p[0] != "/api/v4/projects/7/repository/branches/agent%2Fissue-7" {
				t.Fatalf("requested paths = %v, want the encoded branch path exactly once", p)
			}
			if m.gotToken != ancPAT {
				t.Fatalf("PRIVATE-TOKEN = %q, want the PAT", m.gotToken)
			}
		})
	}
}

func TestGitLabCompareAncestry(t *testing.T) {
	mb := func(id any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"id": id, "short_id": "x"})
		}
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    Ancestry
	}{
		{"merge base is the candidate", mb(ancCand), AncestryAncestor},
		{"merge base differs", mb(ancMB), AncestryNotAncestor},
		{"merge base is head (candidate ahead)", mb(ancHead), AncestryNotAncestor},
		{"missing id", func(w http.ResponseWriter, _ *http.Request) { ancWriteJSON(w, 200, map[string]any{"short_id": "x"}) }, AncestryUnknown},
		{"malformed id", mb("not-a-sha"), AncestryUnknown},
		{"404", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 404, map[string]any{"message": "404 Not found"})
		}, AncestryUnknown},
		{"400", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 400, map[string]any{"message": "Could not find merge base"})
		}, AncestryUnknown},
		{"500", func(w http.ResponseWriter, _ *http.Request) { ancWriteJSON(w, 500, map[string]any{"message": ancPAT}) }, AncestryUnknown},
		{"429", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "60")
			ancWriteJSON(w, 429, map[string]any{"message": "slow"})
		}, AncestryUnknown},
		{"oversize", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(oversizeBody("id", ancCand)) }, AncestryUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &pathRecorder{}
			m := newMockGitLab(t, map[string]http.HandlerFunc{
				"/api/v4/projects/7/repository/merge_base": func(w http.ResponseWriter, r *http.Request) {
					rec.add(r)
					tc.handler(w, r)
				},
			})
			d := newTestDriver(t, m, ancPAT)
			got, err := d.CompareAncestry(context.Background(), 7, ancHead, ancCand)
			assertNoPAT(t, err)
			if got != tc.want {
				t.Fatalf("CompareAncestry = (%q, %v), want %q", got, err, tc.want)
			}
			if (tc.want == AncestryUnknown) != (err != nil) {
				t.Fatalf("error presence %v does not match an unknown answer (%q)", err, got)
			}
			if p := rec.list(); len(p) != 1 {
				t.Fatalf("requested %v, want exactly one merge_base request (no retry)", p)
			}
			if refs := m.lastQuery["refs[]"]; len(refs) != 2 || refs[0] != ancHead || refs[1] != ancCand {
				t.Fatalf("refs[] = %v, want [head candidate]", refs)
			}
		})
	}
}

func TestGitLabCompareAncestryNoRequestCases(t *testing.T) {
	var n atomic.Int64
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, _ *http.Request) { n.Add(1); w.WriteHeader(500) },
	})
	d := newTestDriver(t, m, ancPAT)
	if a, err := d.CompareAncestry(context.Background(), 7, ancCand, ancCand); a != AncestryAncestor || err != nil {
		t.Fatalf("head == candidate = (%q, %v), want ancestor", a, err)
	}
	if a, err := d.CompareAncestry(context.Background(), 7, "main", ancCand); a != AncestryUnknown || err == nil {
		t.Fatalf("ref-name head = (%q, %v), want unknown + error", a, err)
	}
	if n.Load() != 0 {
		t.Fatalf("made %d requests, want 0", n.Load())
	}
}

// ── Forgejo ────────────────────────────────────────────────────────────────────────────

func TestForgejoBranchHead(t *testing.T) {
	const branch = "agent/issue-7"
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantSHA string
		wantNF  bool
	}{
		{"found", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"name": branch, "commit": map[string]any{"id": ancHead}})
		}, ancHead, false},
		{"missing", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 404, map[string]any{"message": "branch does not exist"})
		}, "", true},
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 500, map[string]any{"message": ancPAT})
		}, "", false},
		{"rate limited 429", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 429, map[string]any{"message": "slow"})
		}, "", false},
		{"missing commit", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"name": branch})
		}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &pathRecorder{}
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/branches/": func(w http.ResponseWriter, r *http.Request) {
					rec.add(r)
					tc.handler(w, r)
				},
			})
			d := newForgejoDriver(t, m, ancPAT)
			got, err := d.BranchHead(context.Background(), 7, branch)
			assertNoPAT(t, err)
			if tc.wantSHA != "" {
				if err != nil || got != tc.wantSHA {
					t.Fatalf("BranchHead = (%q, %v), want %q", got, err, tc.wantSHA)
				}
			} else if err == nil {
				t.Fatalf("BranchHead = %q, want an error", got)
			}
			if errors.Is(err, ErrRefNotFound) != tc.wantNF {
				t.Fatalf("errors.Is(ErrRefNotFound) = %v, want %v (err %v)", errors.Is(err, ErrRefNotFound), tc.wantNF, err)
			}
			if p := rec.list(); len(p) != 1 || p[0] != "/api/v1/repos/acme/widgets/branches/agent/issue-7" {
				t.Fatalf("requested paths = %v, want the per-segment branch path once", p)
			}
		})
	}
}

// forgejoCompareSide is one direction's scripted answer: a status and a body.
type forgejoCompareSide struct {
	status int
	body   any
}

func total(n int) forgejoCompareSide {
	return forgejoCompareSide{200, map[string]any{"total_commits": n, "commits": []any{}}}
}

func TestForgejoCompareAncestry(t *testing.T) {
	missing := forgejoCompareSide{200, map[string]any{"commits": []any{}}}
	e404 := forgejoCompareSide{404, map[string]any{"message": "nope"}}
	e500 := forgejoCompareSide{500, map[string]any{"message": ancPAT}}
	e429 := forgejoCompareSide{429, map[string]any{"message": "slow"}}
	pathA := "/api/v1/repos/acme/widgets/compare/" + ancHead + "..." + ancCand
	pathB := "/api/v1/repos/acme/widgets/compare/" + ancCand + "..." + ancHead
	cases := []struct {
		name      string
		a, b      forgejoCompareSide
		want      Ancestry
		wantPaths []string
	}{
		{"A=0 and B>0 is ancestor", total(0), total(3), AncestryAncestor, []string{pathA, pathB}},
		{"unrelated histories A=0 and B=0", total(0), total(0), AncestryUnknown, []string{pathA, pathB}},
		{"A>0 is unknown, never not_ancestor", total(2), total(3), AncestryUnknown, []string{pathA}},
		{"A missing total_commits", missing, total(3), AncestryUnknown, []string{pathA}},
		{"B missing total_commits", total(0), missing, AncestryUnknown, []string{pathA, pathB}},
		{"A 404", e404, total(3), AncestryUnknown, []string{pathA}},
		{"B 404", total(0), e404, AncestryUnknown, []string{pathA, pathB}},
		{"A 500", e500, total(3), AncestryUnknown, []string{pathA}},
		{"B 500", total(0), e500, AncestryUnknown, []string{pathA, pathB}},
		{"A 429", e429, total(3), AncestryUnknown, []string{pathA}},
		{"B 429", total(0), e429, AncestryUnknown, []string{pathA, pathB}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &pathRecorder{}
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/compare/": func(w http.ResponseWriter, r *http.Request) {
					rec.add(r)
					side := tc.b
					if strings.HasSuffix(r.URL.Path, ancHead+"..."+ancCand) {
						side = tc.a
					}
					ancWriteJSON(w, side.status, side.body)
				},
			})
			d := newForgejoDriver(t, m, ancPAT)
			got, err := d.CompareAncestry(context.Background(), 7, ancHead, ancCand)
			assertNoPAT(t, err)
			if got != tc.want {
				t.Fatalf("CompareAncestry = (%q, %v), want %q", got, err, tc.want)
			}
			if got == AncestryNotAncestor {
				t.Fatalf("forgejo must never answer not_ancestor")
			}
			if (tc.want == AncestryUnknown) != (err != nil) {
				t.Fatalf("error presence %v does not match an unknown answer (%q)", err, got)
			}
			p := rec.list()
			if strings.Join(p, ",") != strings.Join(tc.wantPaths, ",") {
				t.Fatalf("requested paths = %v, want %v", p, tc.wantPaths)
			}
		})
	}
}

func TestForgejoCompareAncestryOversizeAndNoRequest(t *testing.T) {
	var n atomic.Int64
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/compare/": func(w http.ResponseWriter, _ *http.Request) {
			n.Add(1)
			_, _ = w.Write(oversizeBody("total_commits", 0))
		},
	})
	d := newForgejoDriver(t, m, ancPAT)
	if a, err := d.CompareAncestry(context.Background(), 7, ancHead, ancCand); a != AncestryUnknown || err == nil {
		t.Fatalf("oversize = (%q, %v), want unknown + error", a, err)
	}
	before := n.Load()
	if a, err := d.CompareAncestry(context.Background(), 7, ancHead, ancHead); a != AncestryAncestor || err != nil {
		t.Fatalf("head == candidate = (%q, %v), want ancestor", a, err)
	}
	if n.Load() != before {
		t.Fatalf("head == candidate made a compare request")
	}
}
