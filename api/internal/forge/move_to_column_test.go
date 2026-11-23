package forge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// moveWire is an httptest server that records every request as
// "METHOD request-uri body" and answers from a stateless per-dialect responder,
// so two fresh drivers fed the same calls see the same wire.
type moveWire struct {
	mu   sync.Mutex
	seen []string
	srv  *httptest.Server
}

func newMoveWire(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *moveWire {
	t.Helper()
	m := &moveWire{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.seen = append(m.seen, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		respond(w, r)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *moveWire) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seen...)
}

func respondGitLab(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
		_, _ = io.WriteString(w, `[]`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
		_, _ = io.WriteString(w, `{"id":1,"name":"x"}`)
	default:
		_, _ = io.WriteString(w, `{"id":1,"iid":5,"labels":[]}`)
	}
}

func respondGitHub(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v3/repositories/7":
		_, _ = io.WriteString(w, `{"id":7,"name":"widgets","owner":{"id":1,"login":"acme"}}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels") && !strings.Contains(r.URL.Path, "/issues/"):
		_, _ = io.WriteString(w, `{"id":1,"name":"x"}`)
	default:
		_, _ = io.WriteString(w, `[]`)
	}
}

func respondForgejo(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/version"):
		_, _ = io.WriteString(w, `{"version":"11.0.0"}`)
	case r.URL.Path == "/api/v1/repositories/7":
		_, _ = io.WriteString(w, `{"id":7,"name":"widgets","owner":{"id":1,"login":"acme"}}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels") && !strings.Contains(r.URL.Path, "/issues/"):
		_, _ = io.WriteString(w, `{"id":9,"name":"x"}`)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/") && strings.HasSuffix(r.URL.Path, "/labels"):
		_, _ = io.WriteString(w, `[{"id":3,"name":"Planned"}]`)
	default:
		_, _ = io.WriteString(w, `[]`)
	}
}

func TestMoveToColumnWireEqualsEnsureThenUpdate(t *testing.T) {
	dialects := []struct {
		name    string
		typ     Type
		respond func(http.ResponseWriter, *http.Request)
	}{
		{"gitlab", TypeGitLab, respondGitLab},
		{"github", TypeGitHub, respondGitHub},
		{"forgejo", TypeForgejo, respondForgejo},
	}
	moves := []struct {
		name string
		move ColumnMove
	}{
		{"full move", ColumnMove{
			Column: "In Progress",
			Ensure: []Label{{Name: "In Progress", Color: "#123456"}},
			Add:    []string{"In Progress"},
			Remove: []string{"Planned"},
		}},
		{"move to Open: no ensure, nil add", ColumnMove{Column: "", Remove: []string{"Planned"}}},
		{"nothing to do", ColumnMove{Column: "Planned"}},
	}
	for _, d := range dialects {
		for _, mv := range moves {
			t.Run(d.name+"/"+mv.name, func(t *testing.T) {
				ctx := context.Background()
				const p, i = int64(7), int64(5)
				fresh := func() (Forge, *moveWire) {
					w := newMoveWire(t, d.respond)
					drv, err := New(d.typ, w.srv.URL, "tok", 5*time.Second)
					if err != nil {
						t.Fatalf("New: %v", err)
					}
					return drv, w
				}

				viaMove, wireMove := fresh()
				if err := viaMove.MoveToColumn(ctx, p, i, mv.move); err != nil {
					t.Fatalf("MoveToColumn: %v", err)
				}

				viaParts, wireParts := fresh()
				// The drivers' EnsureLabels lists the project's labels even for an
				// empty set, so an empty Ensure means no EnsureLabels call at all
				// (the pre-MoveToColumn AutoMove skipped it the same way).
				if len(mv.move.Ensure) > 0 {
					if err := viaParts.EnsureLabels(ctx, p, mv.move.Ensure); err != nil {
						t.Fatalf("EnsureLabels: %v", err)
					}
				}
				if err := viaParts.UpdateIssueLabels(ctx, p, i, mv.move.Add, mv.move.Remove); err != nil {
					t.Fatalf("UpdateIssueLabels: %v", err)
				}

				got, want := wireMove.requests(), wireParts.requests()
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("MoveToColumn wire differs from EnsureLabels+UpdateIssueLabels\n got: %q\nwant: %q", got, want)
				}
			})
		}
	}
}

// spyTracker records the IssueTracker calls MoveByLabels makes.
type spyTracker struct {
	IssueTracker // nil: any other method would panic
	calls        []string
	ensure       []Label
	add, remove  []string
	ensureErr    error
}

func (s *spyTracker) EnsureLabels(_ context.Context, _ int64, l []Label) error {
	s.calls = append(s.calls, "EnsureLabels")
	s.ensure = l
	return s.ensureErr
}

func (s *spyTracker) UpdateIssueLabels(_ context.Context, _, _ int64, add, remove []string) error {
	s.calls = append(s.calls, "UpdateIssueLabels")
	s.add, s.remove = add, remove
	return nil
}

func TestMoveByLabels(t *testing.T) {
	ctx := context.Background()
	t.Run("ensure then one update", func(t *testing.T) {
		s := &spyTracker{}
		m := ColumnMove{Ensure: []Label{{Name: "a"}}, Add: []string{"a"}, Remove: []string{"b"}}
		if err := MoveByLabels(ctx, s, 1, 2, m); err != nil {
			t.Fatal(err)
		}
		if want := []string{"EnsureLabels", "UpdateIssueLabels"}; !reflect.DeepEqual(s.calls, want) {
			t.Fatalf("calls = %v, want %v", s.calls, want)
		}
		if !reflect.DeepEqual(s.add, []string{"a"}) || !reflect.DeepEqual(s.remove, []string{"b"}) {
			t.Fatalf("add/remove = %v/%v", s.add, s.remove)
		}
	})
	t.Run("no ensure call when Ensure is empty, nil stays nil", func(t *testing.T) {
		s := &spyTracker{}
		if err := MoveByLabels(ctx, s, 1, 2, ColumnMove{Remove: []string{"b"}}); err != nil {
			t.Fatal(err)
		}
		if want := []string{"UpdateIssueLabels"}; !reflect.DeepEqual(s.calls, want) {
			t.Fatalf("calls = %v, want %v", s.calls, want)
		}
		if s.add != nil {
			t.Fatalf("add = %#v, want nil", s.add)
		}
	})
	t.Run("empty but non-nil slices stay non-nil", func(t *testing.T) {
		s := &spyTracker{}
		if err := MoveByLabels(ctx, s, 1, 2, ColumnMove{Add: []string{}, Remove: []string{}}); err != nil {
			t.Fatal(err)
		}
		if s.add == nil || s.remove == nil {
			t.Fatalf("add/remove = %#v/%#v, want non-nil", s.add, s.remove)
		}
	})
	t.Run("ensure failure stops before the update", func(t *testing.T) {
		boom := errors.New("boom")
		s := &spyTracker{ensureErr: boom}
		err := MoveByLabels(ctx, s, 1, 2, ColumnMove{Ensure: []Label{{Name: "a"}}, Add: []string{"a"}})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if want := []string{"EnsureLabels"}; !reflect.DeepEqual(s.calls, want) {
			t.Fatalf("calls = %v, want %v", s.calls, want)
		}
	})
}
