package forge

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func boundedIssueDriver(t *testing.T, driver string, rt http.RoundTripper) Forge {
	t.Helper()
	installOverride(t, rt)
	switch driver {
	case "github":
		g, err := newGitHub("https://github.com", "fixture", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		g.slugs[7] = repoSlug{owner: "acme", repo: "widgets"}
		return g
	case "forgejo":
		f := newForgejo("https://forge.example.com", "fixture", time.Second)
		f.slugs[7] = repoSlug{owner: "acme", repo: "widgets"}
		return f
	default:
		g, err := newGitLab("https://forge.example.com", "fixture", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
}

// Serial: the transport override and lowered ceiling are package globals.
func TestGetIssueBounded(t *testing.T) {
	successLimit := int(issueResponseBytes)
	for _, driver := range []string{"github", "gitlab", "forgejo"} {
		for _, tc := range []struct {
			name              string
			status            int
			body              string
			encoding          bool
			overflow, missing bool
		}{
			{name: "valid", status: 200, body: `{"iid":17,"number":17,"title":"small"}`},
			{name: "exact success cap", status: 200, body: `{"iid":17,"number":17,"title":"small"}` + strings.Repeat(" ", successLimit-len(`{"iid":17,"number":17,"title":"small"}`))},
			{name: "exact error cap", status: 404, body: `{"message":"not found"}` + strings.Repeat(" ", (4<<10)-len(`{"message":"not found"}`)), missing: true},
			{name: "missing", status: 404, body: `{"message":"not found"}`, missing: true},
			{name: "over success", status: 200, body: `{"iid":17,"number":17,"body":"` + strings.Repeat("x", successLimit) + `"}`, overflow: true},
			{name: "over error", status: 400, body: `{"message":"private-` + strings.Repeat("x", 5<<10) + `"}`, overflow: true},
			{name: "over missing", status: 404, body: `{"message":"private-` + strings.Repeat("x", 5<<10) + `"}`, overflow: true},
			{name: "over retryable", status: 500, body: strings.Repeat("x", 5<<10), overflow: true},
			{name: "encoded success", status: 200, body: "unread compressed bytes", encoding: true},
			{name: "encoded missing", status: 404, body: "unread compressed bytes", encoding: true},
		} {
			t.Run(driver+"/"+tc.name, func(t *testing.T) {
				rt := &fabRT{}
				rt.fn = func(_ int, req *http.Request) (*http.Response, error) {
					if req.Header.Get("Accept-Encoding") != "identity" {
						t.Error("bounded request must ask for identity encoding")
					}
					resp := rt.resp(req, tc.status, strings.NewReader(tc.body))
					if tc.encoding {
						resp.Header["Content-Encoding"] = []string{"identity", "gzip"}
					}
					return resp, nil
				}
				f := boundedIssueDriver(t, driver, rt)
				// An inherited CI marker must not bypass continuation validation.
				ctx := markCIJSON(WithCIRepairReadLimits(context.Background()))
				issue, err := GetIssueBounded(ctx, f, 7, 17)
				var missing *IssueNotFoundError
				if tc.overflow {
					var large *ResponseTooLargeError
					if !errors.As(err, &large) || err.Error() != "forge response body exceeds size limit" {
						t.Fatalf("unsafe overflow error: %v", err)
					}
					if errors.As(err, &missing) {
						t.Fatal("overflow classified as missing")
					}
					if IsIssueResponseTooLarge(err) != (tc.status == http.StatusOK) || Class(err) != classifyError(nil, tc.status) {
						t.Fatalf("lost response provenance or class: %+v class=%s", large, Class(err))
					}
					limit := issueResponseBytes
					if tc.status != 200 {
						limit = 4 << 10
					}
					if rt.totalRead() != limit+1 {
						t.Fatalf("read=%d want cap+1=%d", rt.totalRead(), limit+1)
					}
				} else if tc.encoding {
					if err == nil || err.Error() != "forge response body read failed: unsupported content encoding" || rt.totalRead() != 0 {
						t.Fatalf("encoded response read=%d err=%v", rt.totalRead(), err)
					}
				} else if tc.missing {
					if !errors.As(err, &missing) {
						t.Fatalf("missing error=%v", err)
					}
				} else if err != nil || issue.IID != 17 || issue.Title != "small" {
					t.Fatalf("valid issue=%+v err=%v", issue, err)
				}
				if err != nil && !reflect.DeepEqual(issue, Issue{}) {
					t.Fatal("partial issue returned on failure")
				}
				if rt.calls.Load() != 1 || !rt.allClosed() {
					t.Fatalf("calls=%d closed=%v", rt.calls.Load(), rt.allClosed())
				}
			})
		}
	}
}

func TestGetIssueBoundedPreservesDriverLimits(t *testing.T) {
	for _, driver := range []string{"github", "gitlab", "forgejo"} {
		t.Run(driver, func(t *testing.T) {
			body := `{"iid":17,"number":17,"title":"` + strings.Repeat("x", int(issueResponseBytes)) + `"}`
			rt := &fabRT{}
			rt.fn = func(_ int, req *http.Request) (*http.Response, error) {
				return rt.resp(req, 200, strings.NewReader(body)), nil
			}
			f := boundedIssueDriver(t, driver, rt)
			issue, err := f.GetIssue(context.Background(), 7, 17)
			if driver == "forgejo" {
				if err != nil || issue.IID != 17 || len(issue.Title) != int(issueResponseBytes) {
					t.Fatalf("ordinary Forgejo read changed: iid=%d title bytes=%d err=%v", issue.IID, len(issue.Title), err)
				}
			} else if !IsIssueResponseTooLarge(err) {
				t.Fatalf("ordinary direct-issue ceiling/provenance changed: %v", err)
			}
			_, err = GetIssueBounded(context.Background(), f, 7, 17)
			if !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("bounded read=%v", err)
			}
			// The failed bounded call must not poison a subsequent ordinary call.
			body = `{"iid":17,"number":17,"title":"small"}`
			issue, err = f.GetIssue(context.Background(), 7, 17)
			if err != nil || issue.IID != 17 {
				t.Fatalf("subsequent ordinary read=%+v err=%v", issue, err)
			}
		})
	}
}

func TestGetIssueBoundedSkipsConditionalCacheBuffering(t *testing.T) {
	lowerGitLabLimit(t, 128)
	rt := &fabRT{}
	rt.fn = func(_ int, req *http.Request) (*http.Response, error) {
		resp := rt.resp(req, 200, strings.NewReader(strings.Repeat("x", 1024)))
		resp.Header.Set("ETag", "\"hostile\"")
		return resp, nil
	}
	cache := newETagTransportWithCache(rt, "fixture", &etagCache{})
	transport := responseLimitTransport{base: cache}
	state := newResponseLimitState(128)
	state.continuationValidation = true
	ctx := withResponseLimit(context.Background(), state)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/acme/widgets/pulls/17", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if !errors.Is(err, ErrResponseTooLarge) || resp != nil || rt.totalRead() != 129 || !rt.allClosed() {
		t.Fatalf("cache buffered before limit: read=%d err=%v", rt.totalRead(), err)
	}
}
