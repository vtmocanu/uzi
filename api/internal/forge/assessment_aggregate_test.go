package forge

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestAssessmentAggregateBoundaries(t *testing.T) {
	for _, lane := range []string{"github-comments", "gitlab-comments", "github-collaborators"} {
		kind := strings.Split(lane, "-")[0]
		collaborators := strings.HasSuffix(lane, "collaborators")
		for _, shape := range []string{"below", "exact", "plus-one", "continuation"} {
			t.Run(lane+"/"+shape, func(t *testing.T) {
				pages := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, code := assessmentDefault(r.URL.Path)
					if (!collaborators && (strings.HasSuffix(r.URL.Path, "/comments") || strings.HasSuffix(r.URL.Path, "/notes"))) || (collaborators && strings.HasSuffix(r.URL.Path, "/collaborators")) {
						pages++
						if collaborators && pages == 2 {
							body = strings.ReplaceAll(strings.ReplaceAll(body, `"id":5`, `"id":6`), "alice", "bob")
						}
						size := 256
						if pages == 2 && shape == "below" {
							size--
						}
						if pages == 2 && shape == "plus-one" {
							size++
						}
						body += strings.Repeat(" ", size-len(body))
						if pages == 1 || shape == "continuation" {
							w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=%d>; rel=\"next\"", r.Host, r.URL.Path, pages+1))
							w.Header().Set("X-Next-Page", strconv.Itoa(pages+1))
						}
					}
					w.WriteHeader(code)
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, body)
				}))
				defer srv.Close()
				var ledger *responseLedger
				d := assessmentDriver(t, kind, srv.URL, func(r *http.Request, next http.RoundTripper) (*http.Response, error) {
					s := responseLimitFrom(r.Context())
					if s.ledger != nil {
						if ledger == nil {
							wantCeiling := int64(4194304)
							if collaborators {
								wantCeiling = 1048576
							}
							if s.ceiling != wantCeiling || commentPageResponseBytes != 8388608 {
								t.Fatalf("configured/effective ceiling=%d/%d", commentPageResponseBytes, s.ceiling)
							}
							ledger = s.ledger
							ledger.remaining = 512
						} else if ledger != s.ledger {
							t.Fatal("page ledger was reset")
						}
						if isCIJSON(r.Context()) {
							t.Fatal("CI marker survived ledger context")
						}
						if r.Context().Value(aggregateContextKey{}) != "retained" {
							t.Fatal("context values lost")
						}
					}
					return next.RoundTrip(r)
				})
				ctx := context.WithValue(context.Background(), aggregateContextKey{}, "retained")
				callctx := markCIJSON(ctx)
				var got []IssueComment
				var err error
				eligibility := AuthorUnknown
				if collaborators {
					eligibility, err = d.RepositoryAuthorEligibility(callctx, 7, 5)
				} else {
					got, err = d.ListIssueComments(callctx, 7, 9)
				}
				if shape == "plus-one" || shape == "continuation" {
					assessmentRefusal(t, err)
					if got != nil || eligibility != AuthorUnknown {
						t.Fatal("partial list or permission")
					}
				} else if err != nil || (!collaborators && len(got) != 2) || (collaborators && eligibility != AuthorEligible) {
					t.Fatalf("terminal boundary: len=%d err=%v", len(got), err)
				}
				if pages != 2 {
					t.Fatalf("fetched beyond exhaustion: %d", pages)
				}
			})
		}
	}
}

type aggregateContextKey struct{}

func TestAssessmentGitLabEligibilityLedger(t *testing.T) {
	for _, shape := range []string{"user", "member", "fallback", "fallback-guard", "exact", "retry", "retry-guard"} {
		t.Run(shape, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, code := assessmentDefault(r.URL.Path)
				size := 128
				if strings.Contains(r.URL.Path, "/members/") && strings.HasPrefix(shape, "fallback") {
					body, code = "{}", 404
				}
				if shape == "retry" || shape == "retry-guard" {
					if calls == 1 {
						body, code = "{}", 503
					}
				}
				body += strings.Repeat(" ", size-len(body))
				w.WriteHeader(code)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			budget := int64(256)
			wantCalls := 2
			switch shape {
			case "user":
				budget, wantCalls = 127, 1
			case "member":
				budget = 255
			case "fallback":
				budget, wantCalls = 383, 3
			case "fallback-guard":
				budget = 256
			case "retry":
				budget = 255
			case "retry-guard":
				budget, wantCalls = 128, 1
			}
			var ledger *responseLedger
			d := assessmentDriver(t, "gitlab", srv.URL, func(r *http.Request, next http.RoundTripper) (*http.Response, error) {
				s := responseLimitFrom(r.Context())
				if s.ledger == nil {
					t.Fatal("unbudgeted eligibility request")
				}
				if ledger == nil {
					ledger = s.ledger
					if ledger.remaining != 8388608 || s.ceiling != 1048576 {
						t.Fatal("eligibility policy changed")
					}
					ledger.remaining = budget
				} else if ledger != s.ledger {
					t.Fatal("eligibility ledger reset")
				}
				return next.RoundTrip(r)
			})
			got, err := d.RepositoryAuthorEligibility(context.Background(), 7, 5)
			if shape == "exact" {
				if err != nil || got != AuthorEligible {
					t.Fatalf("exact: %v %v", got, err)
				}
			} else {
				assessmentRefusal(t, err)
				if got != AuthorUnknown {
					t.Fatalf("partial eligibility=%v", got)
				}
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d", calls, wantCalls)
			}
		})
	}
}
