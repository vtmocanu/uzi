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

// This file deliberately uses only symbols available before M1 so it can be
// copied unchanged into the base export for an executable regression proof.
func TestAssessmentAggregateRealDrivers(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		for _, lane := range []string{"comments", "collaborators", "system-notes"} {
			if (kind == "gitlab" && lane == "collaborators") || (kind == "github" && lane == "system-notes") {
				continue
			}
			t.Run(kind+"/"+lane, func(t *testing.T) {
				pageBytes, refusedPage, remaining := 1572864, 3, int64(1048576)
				if lane == "collaborators" {
					pageBytes, refusedPage, remaining = 786432, 11, 524288
				}
				pages := 0
				recoverCall := false
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, code := assessmentDefault(r.URL.Path)
					target := strings.HasSuffix(r.URL.Path, "/comments") || strings.HasSuffix(r.URL.Path, "/notes") || strings.HasSuffix(r.URL.Path, "/collaborators")
					if target {
						pages++
						page, _ := strconv.Atoi(r.URL.Query().Get("page"))
						if page == 0 {
							page = 1
						}
						if !recoverCall {
							if lane == "system-notes" {
								body = `[{"body":"hidden","system":true}]`
							}
							if lane == "collaborators" && page > 1 {
								body = strings.ReplaceAll(strings.ReplaceAll(body, `"id":5`, fmt.Sprintf(`"id":%d`, page+5)), "alice", fmt.Sprintf("user%d", page))
							}
							if page%2 == 0 {
								// Discarded JSON fields consume the same ledger as whitespace.
								body = strings.TrimSuffix(body, "}]") + `,"ignored":"` + strings.Repeat("x", pageBytes-len(body)-len(`,"ignored":""`)) + `"}]`
							} else {
								body += strings.Repeat(" ", pageBytes-len(body))
							}
							// Bound the old-code proof to one extra advertised page.
							if page <= refusedPage {
								w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=%d>; rel=\"next\"", r.Host, r.URL.Path, page+1))
								w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
							}
						}
					}
					w.WriteHeader(code)
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, body)
				}))
				defer srv.Close()
				track := &trackRT{next: forgeHTTPTransport}
				installOverride(t, track)
				d := assessmentDriver(t, kind, srv.URL, func(r *http.Request, next http.RoundTripper) (*http.Response, error) {
					resp, err := next.RoundTrip(r)
					if !recoverCall && pages == refusedPage {
						if resp != nil {
							t.Fatal("refused bytes exposed a decoder-facing response")
						}
						assessmentRefusal(t, err)
						track.mu.Lock()
						input := track.bodies[len(track.bodies)-1]
						track.mu.Unlock()
						if input.read.Load() > remaining+1 || !input.closed.Load() {
							t.Fatalf("refused input=%d closed=%v", input.read.Load(), input.closed.Load())
						}
					}
					return resp, err
				})
				ctx := BeginAuthorAssessment(context.Background())
				call := func(ctx context.Context) error {
					if lane == "collaborators" {
						got, err := d.RepositoryAuthorEligibility(ctx, 7, 5)
						if err != nil && got != AuthorUnknown {
							t.Fatalf("partial eligibility=%v", got)
						}
						return err
					}
					got, err := d.ListIssueComments(ctx, 7, 9)
					if err != nil && got != nil {
						t.Fatal("partial comments")
					}
					return err
				}
				assessmentRefusal(t, call(ctx))
				if pages != refusedPage {
					t.Fatalf("pages=%d want=%d", pages, refusedPage)
				}
				recoverCall = true
				if lane == "collaborators" {
					assessmentRefusal(t, call(ctx))
					if pages != refusedPage {
						t.Fatal("failed assessment snapshot was fetched again")
					}
				}
				if err := call(context.Background()); err != nil {
					t.Fatalf("new call did not reset: %v", err)
				}
			})
		}
	}
}
