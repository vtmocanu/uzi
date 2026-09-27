package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Issue #1751 M2 driver tests for RefHead across all three drivers: the head of a FULL ref
// (the forge checkpoint ref a live predecessor settle proves against). The invariants: a
// found ref returns its commit id; a 404 is ErrRefNotFound; an answer naming another ref, a
// non-commit object or a malformed sha is an error (never a head); a malformed ref name makes
// no request; a redirect is never followed.

const refHeadRef = "refs/uzi-checkpoints/agent/issue-7"

func refObj(ref, typ, sha string) map[string]any {
	return map[string]any{"ref": ref, "object": map[string]any{"type": typ, "sha": sha}}
}

type refHeadCase struct {
	name    string
	handler http.HandlerFunc
	wantSHA string
	wantNF  bool
}

// assertRefHead checks one RefHead answer against a case.
func assertRefHead(t *testing.T, tc refHeadCase, got string, err error) {
	t.Helper()
	assertNoPAT(t, err)
	if tc.wantSHA != "" {
		if err != nil || got != tc.wantSHA {
			t.Fatalf("RefHead = (%q, %v), want %q", got, err, tc.wantSHA)
		}
	} else if err == nil {
		t.Fatalf("RefHead = %q, want an error", got)
	}
	if errors.Is(err, ErrRefNotFound) != tc.wantNF {
		t.Fatalf("errors.Is(ErrRefNotFound) = %v, want %v (err %v)", errors.Is(err, ErrRefNotFound), tc.wantNF, err)
	}
}

func TestGitHubRefHead(t *testing.T) {
	cases := []refHeadCase{
		{"found", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, refObj(refHeadRef, "commit", ancHead))
		}, ancHead, false},
		{"missing", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 404, map[string]any{"message": "Not Found"})
		}, "", true},
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 500, map[string]any{"message": "boom " + ancPAT})
		}, "", false},
		{"echo names another ref", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, refObj(refHeadRef+"0", "commit", ancHead))
		}, "", false},
		{"echo missing ref", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"object": map[string]any{"type": "commit", "sha": ancHead}})
		}, "", false},
		{"annotated tag object", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, refObj(refHeadRef, "tag", ancHead))
		}, "", false},
		{"malformed sha", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, refObj(refHeadRef, "commit", strings.Repeat("A", 40)))
		}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &pathRecorder{}
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/git/ref/": func(w http.ResponseWriter, r *http.Request) {
					rec.add(r)
					tc.handler(w, r)
				},
			})
			got, err := newGitHubDriver(t, m, ancPAT).RefHead(context.Background(), 7, refHeadRef)
			assertRefHead(t, tc, got, err)
			if p := rec.list(); len(p) != 1 || p[0] != "/api/v3/repos/acme/widgets/git/ref/uzi-checkpoints/agent/issue-7" {
				t.Fatalf("requested paths = %v, want the per-segment ref path once", p)
			}
		})
	}
}

func TestGitLabRefHead(t *testing.T) {
	cases := []refHeadCase{
		{"found", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"id": ancHead, "short_id": "1111111"})
		}, ancHead, false},
		{"missing", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 404, map[string]any{"message": "404 Commit Not Found"})
		}, "", true},
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 500, map[string]any{"message": ancPAT})
		}, "", false},
		{"malformed id", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"id": "abc"})
		}, "", false},
		{"missing id", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, map[string]any{"short_id": "1111111"})
		}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &pathRecorder{}
			m := newMockGitLab(t, map[string]http.HandlerFunc{
				"/api/v4/projects/7/repository/commits/": func(w http.ResponseWriter, r *http.Request) {
					rec.add(r)
					tc.handler(w, r)
				},
			})
			got, err := newTestDriver(t, m, ancPAT).RefHead(context.Background(), 7, refHeadRef)
			assertRefHead(t, tc, got, err)
			if p := rec.list(); len(p) != 1 || p[0] != "/api/v4/projects/7/repository/commits/refs%2Fuzi-checkpoints%2Fagent%2Fissue-7" {
				t.Fatalf("requested paths = %v, want the single-segment encoded ref path once", p)
			}
		})
	}
}

func TestForgejoRefHead(t *testing.T) {
	cases := []refHeadCase{
		{"found as array with a longer prefix sibling", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, []any{
				refObj(refHeadRef+"0", "commit", ancCand),
				refObj(refHeadRef, "commit", ancHead),
			})
		}, ancHead, false},
		{"found as single object", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, refObj(refHeadRef, "commit", ancHead))
		}, ancHead, false},
		{"missing", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 404, map[string]any{"message": "not found"})
		}, "", true},
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 500, map[string]any{"message": ancPAT})
		}, "", false},
		{"only a longer prefix ref", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, []any{refObj(refHeadRef+"0", "commit", ancHead)})
		}, "", false},
		{"single object names another ref", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, refObj("refs/heads/main", "commit", ancHead))
		}, "", false},
		{"non-commit object", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, []any{refObj(refHeadRef, "tag", ancHead)})
		}, "", false},
		{"malformed sha", func(w http.ResponseWriter, _ *http.Request) {
			ancWriteJSON(w, 200, []any{refObj(refHeadRef, "commit", "abc")})
		}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &pathRecorder{}
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/git/refs/": func(w http.ResponseWriter, r *http.Request) {
					rec.add(r)
					tc.handler(w, r)
				},
			})
			got, err := newForgejoDriver(t, m, ancPAT).RefHead(context.Background(), 7, refHeadRef)
			assertRefHead(t, tc, got, err)
			if p := rec.list(); len(p) != 1 || p[0] != "/api/v1/repos/acme/widgets/git/refs/uzi-checkpoints/agent/issue-7" {
				t.Fatalf("requested paths = %v, want the per-segment ref path once", p)
			}
		})
	}
}

// TestRefHeadRejectsMalformedRefWithoutRequest: a ref that is not a well-formed FULL ref name
// is an error on every driver and never reaches the forge.
func TestRefHeadRejectsMalformedRefWithoutRequest(t *testing.T) {
	bad := []string{"", "agent/issue-7", "refs/", "heads/main", "refs/../x", "refs/a//b", "refs/a/.x",
		"refs/a/", "refs/a.lock", "refs/a@{0}", "refs/a b", "refs/a?x", "refs/a\x01", "refs/a~1", "refs/a:b",
		"refs/" + strings.Repeat("x", 256)}
	for _, ref := range bad {
		name := fmt.Sprintf("%q", ref)
		if len(name) > 40 {
			name = name[:40]
		}
		t.Run(name, func(t *testing.T) {
			rec := &pathRecorder{}
			record := func(w http.ResponseWriter, r *http.Request) {
				rec.add(r)
				ancWriteJSON(w, 200, refObj(ref, "commit", ancHead))
			}
			gh := newMockGitHub(t, map[string]http.HandlerFunc{"/repos/": record})
			gl := newMockGitLab(t, map[string]http.HandlerFunc{"/api/v4/projects/": record})
			fj := newMockForgejo(t, map[string]http.HandlerFunc{"/repos/": record})
			for driver, call := range map[string]func() (string, error){
				"github":  func() (string, error) { return newGitHubDriver(t, gh, ancPAT).RefHead(context.Background(), 7, ref) },
				"gitlab":  func() (string, error) { return newTestDriver(t, gl, ancPAT).RefHead(context.Background(), 7, ref) },
				"forgejo": func() (string, error) { return newForgejoDriver(t, fj, ancPAT).RefHead(context.Background(), 7, ref) },
			} {
				if got, err := call(); err == nil || errors.Is(err, ErrRefNotFound) {
					t.Fatalf("%s RefHead(%q) = (%q, %v), want a non-ErrRefNotFound error", driver, ref, got, err)
				}
			}
			for _, p := range rec.list() {
				if strings.Contains(p, "/git/ref") || strings.Contains(p, "/repository/commits/") {
					t.Fatalf("a malformed ref reached the forge: %v", rec.list())
				}
			}
		})
	}
}

// TestRefHeadRedirectsAreRefused: a 3xx on the ref read is an error on every driver, never
// ErrRefNotFound and never the redirect target's answer.
func TestRefHeadRedirectsAreRefused(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect} {
		t.Run(fmt.Sprintf("github %d", status), func(t *testing.T) {
			o := newOffHostTarget(t, refHeadRef)
			m := newMockGitHub(t, map[string]http.HandlerFunc{"/repos/acme/widgets/git/ref/": redirectTo(o, status)})
			got, err := newGitHubDriver(t, m, ancPAT).RefHead(context.Background(), 7, refHeadRef)
			assertNoPAT(t, err)
			if err == nil || errors.Is(err, ErrRefNotFound) {
				t.Fatalf("RefHead across a %d = (%q, %v), want a non-ErrRefNotFound error", status, got, err)
			}
			o.assertUntouched(t)
		})
		t.Run(fmt.Sprintf("gitlab %d", status), func(t *testing.T) {
			o := newOffHostTarget(t, refHeadRef)
			m := newMockGitLab(t, map[string]http.HandlerFunc{"/api/v4/projects/7/repository/commits/": redirectTo(o, status)})
			got, err := newTestDriver(t, m, ancPAT).RefHead(context.Background(), 7, refHeadRef)
			assertNoPAT(t, err)
			if err == nil || errors.Is(err, ErrRefNotFound) {
				t.Fatalf("RefHead across a %d = (%q, %v), want a non-ErrRefNotFound error", status, got, err)
			}
			o.assertUntouched(t)
		})
		t.Run(fmt.Sprintf("forgejo %d", status), func(t *testing.T) {
			o := newOffHostTarget(t, refHeadRef)
			m := newMockForgejo(t, map[string]http.HandlerFunc{"/repos/acme/widgets/git/refs/": redirectTo(o, status)})
			got, err := newForgejoDriver(t, m, ancPAT).RefHead(context.Background(), 7, refHeadRef)
			assertNoPAT(t, err)
			if err == nil || errors.Is(err, ErrRefNotFound) {
				t.Fatalf("RefHead across a %d = (%q, %v), want a non-ErrRefNotFound error", status, got, err)
			}
			o.assertUntouched(t)
		})
	}
}
