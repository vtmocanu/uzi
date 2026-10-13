package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestForgejoAuthorSharedContextAndPermissionFallback(t *testing.T) {
	for _, expireAt := range []string{"/repositories/7", "/repos/acme/widgets/teams", "/repos/acme/widgets/collaborators"} {
		t.Run(expireAt, func(t *testing.T) {
			parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			assessment := BeginAuthorAssessment(parent)
			lookup := newAuthorDeadline(assessment)
			defer lookup.expire()
			calls := map[string]int{}
			permissions := 0
			d := newForgejo("https://forge.example", "fixture", 0)
			d.client.Transport = githubLabelTransport(func(r *http.Request) (*http.Response, error) {
				if err := r.Context().Err(); err != nil {
					return nil, err
				}
				path := strings.TrimPrefix(r.URL.Path, "/api/v1")
				calls[path]++
				if path == expireAt {
					lookup.expire()
				}
				switch {
				case path == "/users/search":
					id := r.URL.Query().Get("uid")
					return authorResponse(r, fmt.Sprintf("{\"data\":[{\"id\":%s,\"login\":\"user%s\"}],\"ok\":true}", id, id)), nil
				case path == "/repositories/7":
					if r.Context().Done() != parent.Done() {
						t.Error("repository not bound to assessment")
					}
					return authorResponse(r, `{"id":7,"name":"widgets","full_name":"acme/widgets","owner":{"id":1,"login":"acme"}}`), nil
				case path == "/repos/acme/widgets/teams":
					if r.Context().Done() != parent.Done() {
						t.Error("ownership not bound to assessment")
					}
					return authorResponse(r, "[]"), nil
				case path == "/repos/acme/widgets/collaborators":
					if r.Context().Done() != parent.Done() {
						t.Error("collaborators not bound to assessment")
					}
					resp := authorResponse(r, "[]")
					if r.URL.Query().Get("page") == "1" {
						resp.Header.Set("Link", `<https://forge.example/api/v1/repos/acme/widgets/collaborators?page=2>; rel="next"`)
					}
					return resp, nil
				case strings.HasSuffix(path, "/permission"):
					permissions++
					if r.Context().Done() == parent.Done() {
						t.Error("permission not bound to lookup")
					}
					return authorResponse(r, `{"permission":"write","user":{"id":43,"login":"user43"}}`), nil
				default:
					return nil, fmt.Errorf("unexpected request %s", r.URL)
				}
			})
			got, err := d.RepositoryAuthorEligibility(lookup, 7, 42)
			if got != AuthorUnknown || err == nil || !errors.Is(lookup.Err(), context.DeadlineExceeded) {
				t.Fatalf("first author got=%v err=%v", got, err)
			}
			if permissions != 0 {
				t.Fatal("expired permission lookup received a fresh deadline")
			}
			if parent.Err() != nil {
				t.Fatalf("assessment expired: %v", parent.Err())
			}
			fresh, stop := context.WithCancel(assessment)
			defer stop()
			assertAuthor(t, d, fresh, 43, AuthorEligible)
			if calls["/repositories/7"] != 1 || calls["/repos/acme/widgets/teams"] != 1 || calls["/repos/acme/widgets/collaborators"] != 2 || permissions != 1 {
				t.Fatalf("calls=%v permissions=%d", calls, permissions)
			}
		})
	}
}
