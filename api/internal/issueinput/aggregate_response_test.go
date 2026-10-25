package issueinput_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
)

// Only public forge APIs are used here; SDK details stay inside forge.
func TestFetchAggregateRefusal(t *testing.T) {
	for _, kind := range []forge.Type{forge.Type("github"), forge.Type("gitlab")} {
		for _, lane := range []string{"comments", "collaborators"} {
			if kind == forge.Type("gitlab") && lane == "collaborators" {
				continue
			}
			t.Run(string(kind)+"/"+lane, func(t *testing.T) {
				pages := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body := "{}"
					switch {
					case strings.HasSuffix(r.URL.Path, "/repositories/7"):
						body = `{"id":7,"name":"r","owner":{"login":"o"}}`
					case strings.HasSuffix(r.URL.Path, "/user/5"):
						body = `{"id":5,"login":"alice"}`
					case strings.HasSuffix(r.URL.Path, "/users/5"):
						body = `{"id":5,"username":"alice","state":"active"}`
					case strings.Contains(r.URL.Path, "/members/all/5"):
						body = `{"id":5,"username":"alice","state":"active","access_level":30}`
					case strings.HasSuffix(r.URL.Path, "/issues/9"):
						body = `{"number":9,"iid":9,"title":"title","body":"raw","description":"raw","user":{"id":5,"login":"alice"},"author":{"id":5,"username":"alice"}}`
					case strings.HasSuffix(r.URL.Path, "/comments"), strings.HasSuffix(r.URL.Path, "/notes"):
						body = `[{"body":"comment","user":{"id":5,"login":"alice"},"author":{"id":5,"username":"alice"}}]`
					case strings.HasSuffix(r.URL.Path, "/collaborators"):
						body = `[{"id":5,"login":"alice","permissions":{"pull":true,"triage":true,"push":false,"maintain":false,"admin":false}}]`
					}
					target := (lane == "comments" && (strings.HasSuffix(r.URL.Path, "/comments") || strings.HasSuffix(r.URL.Path, "/notes"))) || (lane == "collaborators" && strings.HasSuffix(r.URL.Path, "/collaborators"))
					if target {
						pages++
						size := 1572864
						if lane == "collaborators" {
							size = 786432
						}
						page, _ := strconv.Atoi(r.URL.Query().Get("page"))
						if page == 0 {
							page = 1
						}
						if lane == "collaborators" && page > 1 {
							body = strings.ReplaceAll(strings.ReplaceAll(body, `"id":5`, fmt.Sprintf(`"id":%d`, page+5)), "alice", fmt.Sprintf("user%d", page))
						}
						body += strings.Repeat(" ", size-len(body))
						lastAdvertised := 3
						if lane == "collaborators" {
							lastAdvertised = 11
						}
						if page <= lastAdvertised {
							w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=%d>; rel=\"next\"", r.Host, r.URL.Path, page+1))
							w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
						}
					}
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, body)
				}))
				defer srv.Close()
				d, err := forge.New(kind, srv.URL, "fixture", 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				c, err := issueinput.Fetch(context.Background(), d, 7, 9, 99)
				if err != nil {
					t.Fatal(err)
				}
				if c.Thread == nil || !c.Thread.Unknown || !c.Unknown {
					t.Fatalf("missing unknown projection: %+v", c)
				}
				wantPages := 3
				if lane == "comments" {
					if len(c.Thread.Comments) != 0 || c.Reason != "" {
						t.Fatalf("partial thread or lost author: %+v", c)
					}
				} else {
					wantPages = 11
					if c.Reason != issueinput.Unknown || len(c.Thread.Comments) != 1 || c.Thread.Comments[0].Reason != issueinput.Unknown || c.Thread.Comments[0].Body != issueinput.Placeholder {
						t.Fatalf("unsafe permission projection: capture=%+v thread=%+v", c, c.Thread)
					}
				}
				if pages != wantPages {
					t.Fatalf("pages=%d want=%d", pages, wantPages)
				}
			})
		}
	}
}
