package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// These are independent policy literals, not aliases of production constants.
type assessmentLimitCase struct {
	driver, name, path, body string
	ceiling                  int64
}

var assessmentLimitCases = []assessmentLimitCase{
	{"github", "issue", "/repos/o/r/issues/9", `{"number":9,"title":"title","body":"description","user":{"id":5,"login":"alice"}}`, 2097152},
	{"github", "slug", "/repositories/7", `{"id":7,"name":"r","owner":{"login":"o"}}`, 1048576},
	{"github", "user", "/user/5", `{"id":5,"login":"alice"}`, 1048576},
	{"github", "repository", "/repositories/7", `{"id":7,"name":"r","owner":{"login":"o"}}`, 1048576},
	{"github", "collaborators", "/repos/o/r/collaborators", `[{"id":5,"login":"alice","permissions":{"pull":true,"triage":true,"push":false,"maintain":false,"admin":false}}]`, 1048576},
	{"github", "comments", "/repos/o/r/issues/9/comments", `[{"body":"comment","user":{"id":5,"login":"alice"}}]`, 8388608},
	{"gitlab", "issue", "/projects/7/issues/9", `{"iid":9,"title":"title","description":"description","author":{"id":5,"username":"alice"}}`, 2097152},
	{"gitlab", "user", "/users/5", `{"id":5,"username":"alice","state":"active"}`, 1048576},
	{"gitlab", "member", "/projects/7/members/all/5", `{"id":5,"username":"alice","state":"active","access_level":30}`, 1048576},
	{"gitlab", "project", "/projects/7", `{"id":7}`, 1048576},
	{"gitlab", "comments", "/projects/7/issues/9/notes", `[{"body":"comment","author":{"id":5,"username":"alice"}}]`, 8388608},
}

func assessmentDefault(path string) (string, int) {
	for _, c := range assessmentLimitCases {
		if strings.HasSuffix(path, c.path) {
			return c.body, 200
		}
	}
	return "{}", 404
}

func assessmentCall(t *testing.T, d Forge, name string) (any, error) {
	t.Helper()
	ctx := context.Background()
	switch name {
	case "issue":
		v, err := d.GetIssue(ctx, 7, 9)
		if err == nil && (v.IID != 9 || v.Title != "title" || v.Description != "description") {
			t.Fatalf("decoded issue: %+v", v)
		}
		if err != nil && (v.IID != 0 || v.Title != "") {
			t.Fatalf("partial issue: %+v", v)
		}
		return v, err
	case "comments":
		v, err := d.ListIssueComments(ctx, 7, 9)
		if err == nil && (len(v) != 1 || v[0].Body != "comment" || v[0].AuthorForgeUserID != 5 || v[0].AuthorUsername != "alice") {
			t.Fatalf("decoded comments: %+v", v)
		}
		if err != nil && v != nil {
			t.Fatalf("partial comments: %+v", v)
		}
		return v, err
	case "slug":
		v, err := d.(*github).repoSlugFor(ctx, 7)
		if err == nil && (v.owner != "o" || v.repo != "r") {
			t.Fatalf("decoded slug: %+v", v)
		}
		return v, err
	default:
		v, err := d.RepositoryAuthorEligibility(ctx, 7, 5)
		want := AuthorEligible
		if name == "project" {
			want = AuthorNotEligible
		}
		if err == nil && v != want {
			t.Fatalf("eligibility: %v want %v", v, want)
		}
		if err != nil && v != AuthorUnknown {
			t.Fatalf("partial authorization: %v", v)
		}
		return v, err
	}
}

func assessmentRefusal(t *testing.T, err error) {
	t.Helper()
	var large *ResponseTooLargeError
	if !errors.As(err, &large) || !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("want typed refusal: %v", err)
	}
	if !strings.HasSuffix(err.Error(), "forge response body exceeds size limit") {
		t.Fatalf("unsafe error: %v", err)
	}
}

// assessmentDriver puts observation immediately above the real limiter; the
// override only counts reads below it. It never changes state at the override.
func assessmentDriver(t *testing.T, kind, base string, observe func(*http.Request, http.RoundTripper) (*http.Response, error)) Forge {
	t.Helper()
	if kind == "github" {
		d, err := newGitHub(base, testToken(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		hc := d.client.Client()
		next := hc.Transport
		hc.Transport = githubLabelTransport(func(r *http.Request) (*http.Response, error) { return observe(r, next) })
		d.client, err = gh.NewClient(gh.WithHTTPClient(hc), gh.WithAuthToken(testToken()), gh.WithEnterpriseURLs(base, base))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	hc := timeoutClient(5 * time.Second)
	hc.Transport = responseLimitTransport{base: ciRepairTransport{base: hc.Transport}}
	next := hc.Transport
	hc.Transport = githubLabelTransport(func(r *http.Request) (*http.Response, error) { return observe(r, next) })
	c, err := gitlab.NewClient(testToken(), gitLabClientOptions(base, hc, zeroBackoff)...)
	if err != nil {
		t.Fatal(err)
	}
	return &gitLab{client: c, redact: newRedactor(testToken())}
}

// Every server response flushes headers before writing, forcing chunked framing.
// trackedBody measures reader calls at the limiter input, not socket readahead or
// the standard HTTP transport's close drain.
func TestAssessmentResponseLimits(t *testing.T) {
	for _, c := range assessmentLimitCases {
		t.Run(c.driver+"/"+c.name, func(t *testing.T) {
			for _, shape := range []string{"exact", "plus-one", "malformed", "trailing"} {
				t.Run(shape, func(t *testing.T) {
					const ceiling = 512
					payload := c.body + strings.Repeat(" ", ceiling-len(c.body))
					if shape == "plus-one" {
						payload += " "
					}
					if shape == "malformed" {
						payload = "!" + strings.Repeat("x", ceiling)
					}
					if shape == "trailing" {
						payload = c.body + strings.Repeat(" ", ceiling-len(c.body)) + "!"
					}
					calls := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.ProtoMajor != 1 || r.Header.Get("Accept-Encoding") != "identity" {
							t.Errorf("protocol/encoding: %s %q", r.Proto, r.Header.Get("Accept-Encoding"))
						}
						body, status := assessmentDefault(r.URL.Path)
						if c.name == "project" && strings.HasSuffix(r.URL.Path, "/members/all/5") {
							body, status = "{}", 404
						}
						if strings.HasSuffix(r.URL.Path, c.path) {
							body = payload
							calls++
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						w.(http.Flusher).Flush()
						_, _ = io.WriteString(w, body)
					}))
					defer srv.Close()
					track := &trackRT{next: forgeHTTPTransport}
					installOverride(t, track)
					observed := 0
					var states []*responseLimitState
					d := assessmentDriver(t, c.driver, srv.URL, func(r *http.Request, next http.RoundTripper) (*http.Response, error) {
						state := responseLimitFrom(r.Context())
						if state == nil {
							t.Fatal("unmarked assessment request")
						}
						for _, previous := range states {
							if state == previous {
								t.Fatal("state shared between SDK calls")
							}
						}
						states = append(states, state)
						target := strings.HasSuffix(r.URL.Path, c.path)
						if target {
							if state.ceiling != c.ceiling {
								t.Fatalf("production ceiling=%d want literal %d", state.ceiling, c.ceiling)
							}
							state.ceiling = ceiling
						}
						resp, err := next.RoundTrip(r)
						if target {
							track.mu.Lock()
							input := track.bodies[len(track.bodies)-1]
							track.mu.Unlock()
							wantRead := int64(len(payload))
							if wantRead > ceiling+1 {
								wantRead = ceiling + 1
							}
							if input.read.Load() != wantRead || !input.closed.Load() {
								t.Fatalf("limiter input read=%d want=%d closed=%v", input.read.Load(), wantRead, input.closed.Load())
							}
							observed++
							if shape != "exact" && calls == 1 {
								if resp != nil {
									t.Fatalf("overflow exposed decoder-facing response: %v", resp.StatusCode)
								}
								assessmentRefusal(t, err)
								if !state.overflow.Load() {
									t.Fatal("missing overflow state")
								}
							}
						}
						return resp, err
					})
					_, err := assessmentCall(t, d, c.name)
					if shape == "exact" {
						if err != nil {
							t.Fatal(err)
						}
					} else {
						assessmentRefusal(t, err)
						if IsIssueResponseTooLarge(err) != (c.name == "issue") {
							t.Fatalf("successful response provenance: issue=%v err=%v", IsIssueResponseTooLarge(err), err)
						}
					}
					if calls != 1 || observed != 1 {
						t.Fatalf("attempts=%d observed=%d", calls, observed)
					}
					if !track.allClosed() || track.maxRead() > ceiling+1 {
						t.Fatalf("input read=%d closed=%v", track.maxRead(), track.allClosed())
					}
					// Same driver recovers with fresh states. An overflowing slug was not cached.
					payload = c.body
					_, err = assessmentCall(t, d, c.name)
					if err != nil {
						t.Fatalf("recovery: %v", err)
					}
					if c.name == "slug" {
						wantCalls := 1
						if shape != "exact" {
							wantCalls = 2
						}
						if calls != wantCalls {
							t.Fatalf("slug cache calls=%d want %d", calls, wantCalls)
						}
					}
				})
			}
		})
	}
}

// No replacement SDK clients here: these calls prove both production
// constructors actually install the limiter with their auth/origin chains.
func TestAssessmentProductionConstructorWiring(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, status := assessmentDefault(r.URL.Path)
				if strings.Contains(r.URL.Path, "issues/9") {
					if r.Header.Get("Accept-Encoding") != "identity" {
						t.Error("missing limiter encoding")
					}
					if kind == "github" && r.Header.Get("Authorization") != "Bearer "+testToken() {
						t.Error("missing auth")
					}
					if kind == "gitlab" && r.Header.Get("PRIVATE-TOKEN") != testToken() {
						t.Error("missing auth")
					}
					body += strings.Repeat(" ", 2097153-len(body))
				}
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			d, err := NewWithGitLabBackoff(Type(kind), srv.URL, testToken(), 5*time.Second, zeroBackoff)
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.GetIssue(context.Background(), 7, 9)
			assessmentRefusal(t, err)
		})
	}
}

func TestAssessmentOversizedErrors(t *testing.T) {
	for _, c := range assessmentLimitCases {
		for _, status := range []int{401, 403, 404, 429, 500, 501, 503} {
			t.Run(fmt.Sprintf("%s/%s/%d", c.driver, c.name, status), func(t *testing.T) {
				attempts, projectReads := 0, 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, code := assessmentDefault(r.URL.Path)
					if c.name == "project" && strings.HasSuffix(r.URL.Path, "/members/all/5") {
						body, code = "{}", 404
					}
					if strings.HasSuffix(r.URL.Path, "/projects/7") {
						projectReads++
					}
					if strings.HasSuffix(r.URL.Path, c.path) {
						attempts++
						body, code = strings.Repeat("REMOTE", 683), status
					}
					w.WriteHeader(code)
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, body)
				}))
				defer srv.Close()
				track := &trackRT{next: forgeHTTPTransport}
				installOverride(t, track)
				d := assessmentDriver(t, c.driver, srv.URL, func(r *http.Request, next http.RoundTripper) (*http.Response, error) {
					state := responseLimitFrom(r.Context())
					if state == nil {
						t.Fatal("unmarked call")
					}
					if strings.HasSuffix(r.URL.Path, c.path) {
						if state.ceiling != c.ceiling {
							t.Fatalf("ceiling=%d want %d", state.ceiling, c.ceiling)
						}
						// Above the error cap: this must independently enforce 4 KiB.
						state.ceiling = 8192
					}
					resp, err := next.RoundTrip(r)
					if strings.HasSuffix(r.URL.Path, c.path) && statusRetriedBySDK(status) {
						if resp != nil {
							t.Fatal("retriable overflow exposed a response")
						}
						assessmentRefusal(t, err)
					}
					return resp, err
				})
				_, err := assessmentCall(t, d, c.name)
				assessmentRefusal(t, err)
				wantClass := ErrorClassOther
				switch status {
				case 401, 403:
					wantClass = ErrorClassAuth
				case 429:
					wantClass = ErrorClassRateLimited
				case 500, 501, 503:
					wantClass = ErrorClassServerError
				}
				if IsIssueResponseTooLarge(err) || Class(err) != wantClass {
					t.Fatalf("error-status provenance: permanent=%v class=%q want=%q", IsIssueResponseTooLarge(err), Class(err), wantClass)
				}
				if strings.Contains(err.Error(), "REMOTE") {
					t.Fatalf("remote error text: %v", err)
				}
				if attempts != 1 {
					t.Fatalf("actual attempts=%d", attempts)
				}
				if c.name == "member" && projectReads != 0 {
					t.Fatal("oversized membership 404 fell back to project")
				}
				if !track.allClosed() || track.maxRead() > 4097 {
					t.Fatalf("input read=%d closed=%v", track.maxRead(), track.allClosed())
				}
			})
		}
	}
}

func TestAssessmentPageTwoDiscardsEvidence(t *testing.T) {
	for _, c := range assessmentLimitCases {
		if c.name != "comments" && c.name != "collaborators" {
			continue
		}
		t.Run(c.driver+"/"+c.name, func(t *testing.T) {
			pages := 0
			recoverPage := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, code := assessmentDefault(r.URL.Path)
				if strings.HasSuffix(r.URL.Path, c.path) {
					pages++
					if r.URL.Query().Get("page") == "2" {
						body += strings.Repeat(" ", 513-len(body))
					} else if !recoverPage {
						w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
						w.Header().Set("X-Next-Page", "2")
					}
				}
				w.WriteHeader(code)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			var states []*responseLimitState
			d := assessmentDriver(t, c.driver, srv.URL, func(r *http.Request, next http.RoundTripper) (*http.Response, error) {
				s := responseLimitFrom(r.Context())
				if s == nil {
					t.Fatal("unmarked page")
				}
				for _, prior := range states {
					if s == prior {
						t.Fatal("reused page state")
					}
				}
				states = append(states, s)
				if strings.HasSuffix(r.URL.Path, c.path) {
					if s.ceiling != c.ceiling {
						t.Fatalf("ceiling=%d want %d", s.ceiling, c.ceiling)
					}
					s.ceiling = 512
				}
				return next.RoundTrip(r)
			})
			_, err := assessmentCall(t, d, c.name)
			assessmentRefusal(t, err)
			if pages != 2 {
				t.Fatalf("pages=%d", pages)
			}
			recoverPage = true
			_, err = assessmentCall(t, d, c.name)
			if err != nil {
				t.Fatalf("recovery: %v", err)
			}
		})
	}
}

func TestAssessmentGitLabRetryPolicyPreserved(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				if attempts == 1 {
					w.WriteHeader(status)
					w.(http.Flusher).Flush()
					// Exactly the independent 4 KiB error ceiling still reaches SDK policy.
					_, _ = io.WriteString(w, "{}"+strings.Repeat(" ", 4094))
					return
				}
				_, _ = io.WriteString(w, assessmentLimitCases[6].body)
			}))
			defer srv.Close()
			d, err := newGitLab(srv.URL, testToken(), 5*time.Second, zeroBackoff)
			if err != nil {
				t.Fatal(err)
			}
			_, err = assessmentCall(t, d, "issue")
			if err != nil || attempts != 2 {
				t.Fatalf("attempts=%d err=%v", attempts, err)
			}
		})
	}
}

func TestAssessmentGitHubSlugCIPrecedence(t *testing.T) {
	for _, size := range []int{1048577, 4194304, 4194305} {
		for _, ci := range []bool{false, true} {
			t.Run(fmt.Sprintf("size=%d/ci=%v", size, ci), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body := assessmentLimitCases[1].body
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, body+strings.Repeat(" ", size-len(body)))
				}))
				defer srv.Close()
				d, err := newGitHub(srv.URL, testToken(), 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				if ci {
					ctx = markCIJSON(WithCIRepairReadLimits(ctx))
				}
				slug, err := d.repoSlugFor(ctx, 7)
				if !ci {
					assessmentRefusal(t, err)
				} else if size > 4194304 {
					if !errors.Is(err, ErrCIReadBudget) {
						t.Fatalf("want CI budget: %v", err)
					}
				} else if err != nil || slug.owner != "o" || slug.repo != "r" {
					t.Fatalf("CI slug=%+v err=%v", slug, err)
				}
			})
		}
	}
}

func TestAssessmentProductionHTTP1(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 1 {
					t.Errorf("marked call used %s", r.Proto)
				}
				body, code := assessmentDefault(r.URL.Path)
				w.WriteHeader(code)
				_, _ = io.WriteString(w, body)
			}))
			srv.EnableHTTP2 = true
			srv.StartTLS()
			defer srv.Close()
			general := srv.Client().Transport.(*http.Transport).Clone()
			general.ForceAttemptHTTP2 = true
			http1 := newHTTP1Transport(general)
			oldGeneral, oldHTTP1 := forgeHTTPTransport, forgeHTTP1Transport
			forgeHTTPTransport, forgeHTTP1Transport = general, http1
			defer func() {
				forgeHTTPTransport, forgeHTTP1Transport = oldGeneral, oldHTTP1
				general.CloseIdleConnections()
				http1.CloseIdleConnections()
			}()
			d, err := NewWithGitLabBackoff(Type(kind), srv.URL, testToken(), 5*time.Second, zeroBackoff)
			if err != nil {
				t.Fatal(err)
			}
			_, err = assessmentCall(t, d, "issue")
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAssessmentForcedCompressionRefusedUnread(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, code := assessmentDefault(r.URL.Path)
				if strings.Contains(r.URL.Path, "issues/9") {
					w.Header().Set("Content-Encoding", "gzip")
					body = "REMOTE compressed content"
				}
				w.WriteHeader(code)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			track := &trackRT{next: forgeHTTPTransport}
			installOverride(t, track)
			d, err := NewWithGitLabBackoff(Type(kind), srv.URL, testToken(), 5*time.Second, zeroBackoff)
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.GetIssue(context.Background(), 7, 9)
			if err == nil || strings.Contains(err.Error(), "REMOTE") {
				t.Fatalf("compression error: %v", err)
			}
			track.mu.Lock()
			last := track.bodies[len(track.bodies)-1]
			track.mu.Unlock()
			if last.read.Load() != 0 || !last.closed.Load() {
				t.Fatalf("compressed input read=%d closed=%v", last.read.Load(), last.closed.Load())
			}
		})
	}
}

// A repository name containing "/pulls" reaches the existing ETag allowlist.
// Both the initial response and a cached 304 replay must cross the limiter.
func TestAssessmentGitHubCachedReplayBounded(t *testing.T) {
	const issueJSON = `{"number":9,"title":"title","body":"description","user":{"id":5,"login":"alice"}}`
	var replays atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken() {
			t.Error("missing auth")
		}
		if strings.HasSuffix(r.URL.Path, "/repositories/7") {
			_, _ = io.WriteString(w, `{"id":7,"name":"pulls-cache","owner":{"login":"o"}}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/o/pulls-cache/issues/9") {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("If-None-Match") == "cached-issue" {
			replays.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", "cached-issue")
		_, _ = io.WriteString(w, issueJSON)
	}))
	defer srv.Close()
	lower := false
	d := assessmentDriver(t, "github", srv.URL, func(r *http.Request, next http.RoundTripper) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/issues/9") {
			state := responseLimitFrom(r.Context())
			if state == nil || state.ceiling != 2097152 {
				t.Fatal("missing issue ceiling on cached read")
			}
			if lower {
				state.ceiling = int64(len(issueJSON) - 1)
			}
		}
		resp, err := next.RoundTrip(r)
		if lower && strings.HasSuffix(r.URL.Path, "/issues/9") {
			if resp != nil {
				t.Fatal("cached overflow exposed decoder-facing response")
			}
			assessmentRefusal(t, err)
		}
		return resp, err
	})
	for i := 0; i < 2; i++ {
		if _, err := assessmentCall(t, d, "issue"); err != nil {
			t.Fatal(err)
		}
	}
	if replays.Load() != 1 {
		t.Fatalf("successful cached replays=%d want 1", replays.Load())
	}
	lower = true
	_, err := d.GetIssue(context.Background(), 7, 9)
	assessmentRefusal(t, err)
	if replays.Load() != 2 {
		t.Fatalf("cached replays=%d want 2", replays.Load())
	}
}

func TestAssessmentPolicyConstants(t *testing.T) {
	if issueResponseBytes != 2097152 || objectResponseBytes != 1048576 || commentPageResponseBytes != 8388608 {
		t.Fatalf("policy ceilings: %d %d %d", issueResponseBytes, objectResponseBytes, commentPageResponseBytes)
	}
}
