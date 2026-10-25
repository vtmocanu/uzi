package forge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAssessmentLedgerBypassesETag(t *testing.T) {
	for _, lane := range []string{"comments", "collaborators"} {
		for _, warm := range []bool{false, true} {
			name := lane + "/fresh"
			if warm {
				name = lane + "/cached"
			}
			t.Run(name, func(t *testing.T) {
				old := githubETagCache
				githubETagCache = newETagCache()
				defer func() { githubETagCache = old }()
				warming := warm
				conditional, live := 0, 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, code := assessmentDefault(strings.ReplaceAll(r.URL.Path, "pulls-demo", "r"))
					if strings.HasSuffix(r.URL.Path, "/repositories/7") {
						body = `{"id":7,"name":"pulls-demo","owner":{"login":"o"}}`
					}
					if strings.HasSuffix(r.URL.Path, "/"+lane) {
						w.Header().Set("ETag", "\"v1\"")
						if r.Header.Get("If-None-Match") != "" {
							conditional++
							w.WriteHeader(304)
							return
						}
						if !warming {
							live++
							body += strings.Repeat(" ", 1024-len(body))
						}
					}
					w.WriteHeader(code)
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, body)
				}))
				defer srv.Close()
				track := &trackRT{next: forgeHTTPTransport}
				installOverride(t, track)
				d := assessmentDriver(t, "github", srv.URL, func(r *http.Request, next http.RoundTripper) (*http.Response, error) {
					s := responseLimitFrom(r.Context())
					if s.ledger == nil {
						return next.RoundTrip(r)
					}
					if warming {
						// Two ordinary calls prove cache population and conditional replay still work.
						for i := 0; i < 2; i++ {
							resp, err := next.RoundTrip(r.WithContext(context.Background()))
							if err != nil {
								t.Fatal(err)
							}
							data, err := io.ReadAll(resp.Body)
							_ = resp.Body.Close()
							if err != nil || resp.StatusCode != 200 || len(data) == 0 {
								t.Fatalf("ordinary replay: %v %v", resp, err)
							}
						}
						warming = false
					}
					s.ledger.remaining = 512
					resp, err := next.RoundTrip(r)
					if resp != nil {
						t.Fatal("ETag path exposed overflowing response")
					}
					assessmentRefusal(t, err)
					track.mu.Lock()
					input := track.bodies[len(track.bodies)-1]
					track.mu.Unlock()
					if input.read.Load() != 513 {
						t.Fatalf("ETag read ahead: %d", input.read.Load())
					}
					return resp, err
				})
				if lane == "comments" {
					got, err := d.ListIssueComments(context.Background(), 7, 9)
					assessmentRefusal(t, err)
					if got != nil {
						t.Fatal("partial comments")
					}
				} else {
					got, err := d.RepositoryAuthorEligibility(context.Background(), 7, 5)
					assessmentRefusal(t, err)
					if got != AuthorUnknown {
						t.Fatal("partial eligibility")
					}
				}
				wantConditional := 0
				if warm {
					wantConditional = 1
				}
				if conditional != wantConditional || live != 1 {
					t.Fatalf("conditional=%d live=%d", conditional, live)
				}
			})
		}
	}
}
