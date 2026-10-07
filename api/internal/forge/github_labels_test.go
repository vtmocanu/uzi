package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gh "github.com/google/go-github/v92/github"
)

type githubLabelTransport func(*http.Request) (*http.Response, error)

func (f githubLabelTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// labelState serializes requests and observations, including a concurrent writer
// simulated between snapshot serialization and replacement/addition.
type labelState struct {
	mu     sync.Mutex
	labels map[string]bool
	calls  []string
	race   bool
}

func (s *labelState) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r.Method+" "+r.RequestURI)
	issue := "/api/v3/repos/acme/widgets/issues/5"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == issue:
		labels := []map[string]string{}
		for name := range s.labels {
			labels = append(labels, map[string]string{"name": name})
		}
		snapshot, _ := json.Marshal(map[string]any{"number": 5, "labels": labels})
		if s.race {
			delete(s.labels, "on-deck")
		}
		_, _ = w.Write(snapshot)
	case r.Method == http.MethodDelete:
		name, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), issue+"/labels/"))
		if err != nil {
			http.Error(w, "bad escape", 400)
			return
		}
		if !s.labels[name] {
			http.Error(w, `{"message":"delete original"}`, 404)
			return
		}
		delete(s.labels, name)
		w.WriteHeader(204)
	case r.Method == http.MethodPut || r.Method == http.MethodPost:
		var names []string
		if err := json.NewDecoder(r.Body).Decode(&names); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if r.Method == http.MethodPut {
			s.labels = map[string]bool{}
		} else if s.race {
			delete(s.labels, "on-deck")
		}
		for _, name := range names {
			s.labels[name] = true
		}
		_, _ = w.Write([]byte("[]"))
	default:
		http.Error(w, "unexpected request", 500)
	}
}
func labelStateDriver(t *testing.T, s *labelState) Forge {
	t.Helper()
	m := newMockGitHub(t, map[string]http.HandlerFunc{"/repos/acme/widgets/issues/": s.serve})
	return newGitHubDriver(t, m, "test-token")
}
func TestGitHubUpdateIssueLabelsDoesNotResurrectOnDeck(t *testing.T) {
	s := &labelState{labels: map[string]bool{"on-deck": true, "Todo": true, "unrelated": true}, race: true}
	d := labelStateDriver(t, s)
	if err := d.UpdateIssueLabels(context.Background(), 7, 5, []string{"In Progress"}, []string{"Todo"}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.labels["on-deck"] {
		t.Error("resurrected on-deck after concurrent selector removal")
	}
	if !s.labels["In Progress"] || !s.labels["unrelated"] || s.labels["Todo"] {
		t.Errorf("incorrect final labels: %v", s.labels)
	}
}

func TestGitHubUpdateIssueLabelsDeltas(t *testing.T) {
	for _, tc := range []struct {
		name        string
		add, remove []string
		want        map[string]bool
		methods     []string
	}{
		{"add-only", []string{"new"}, nil, map[string]bool{"keep": true, "old": true, "new": true}, []string{"POST"}},
		{"remove-only", nil, []string{"old"}, map[string]bool{"keep": true}, []string{"DELETE"}},
		{"overlap", []string{"old"}, []string{"old"}, map[string]bool{"keep": true, "old": true}, []string{"DELETE", "POST"}},
		{"repeated-absent", nil, []string{"missing", "missing"}, map[string]bool{"keep": true, "old": true}, []string{"DELETE", "GET", "DELETE", "GET"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &labelState{labels: map[string]bool{"keep": true, "old": true}}
			d := labelStateDriver(t, s)
			if err := d.UpdateIssueLabels(context.Background(), 7, 5, tc.add, tc.remove); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.labels) != len(tc.want) {
				t.Fatalf("labels: %v", s.labels)
			}
			for name := range tc.want {
				if !s.labels[name] {
					t.Errorf("missing %q", name)
				}
			}
			if len(s.calls) != len(tc.methods) {
				t.Fatalf("calls: %v", s.calls)
			}
			for i, method := range tc.methods {
				if !strings.HasPrefix(s.calls[i], method+" ") {
					t.Errorf("calls: %v", s.calls)
				}
			}
		})
	}
}
func TestGitHubUpdateIssueLabelsEmpty(t *testing.T) {
	m := newMockGitHub(t, nil)
	d := newGitHubDriver(t, m, "test-token")
	if err := d.UpdateIssueLabels(context.Background(), -1, -1, nil, nil); err != nil {
		t.Fatal(err)
	}
	if m.reqCount.Load() != 0 {
		t.Fatal("empty delta resolved slug")
	}
}
func TestGitHubUpdateIssueLabelsEscaping(t *testing.T) {
	for _, tc := range []struct{ name, escaped string }{
		{"In Progress", "In%20Progress"}, {"area::forge", "area::forge"},
		{"slash/name", "slash%2Fname"}, {"percent%", "percent%25"}, {"question?", "question%3F"},
		{"hash#", "hash%23"}, {"%2F", "%252F"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &labelState{labels: map[string]bool{tc.name: true, "keep": true}}
			d := labelStateDriver(t, s)
			if err := d.UpdateIssueLabels(context.Background(), 7, 5, nil, []string{tc.name}); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			want := "DELETE /api/v3/repos/acme/widgets/issues/5/labels/" + tc.escaped
			if len(s.calls) != 1 || s.calls[0] != want {
				t.Fatalf("raw URI: %v, want %s", s.calls, want)
			}
			if s.labels[tc.name] || !s.labels["keep"] {
				t.Fatalf("decoded once state: %v", s.labels)
			}
		})
	}
	t.Run("decoded-parent-guard", func(t *testing.T) {
		m := newMockGitHub(t, nil)
		d := newGitHubDriver(t, m, "test-token")
		err := d.UpdateIssueLabels(context.Background(), 7, 5, []string{"new"}, []string{"../escape"})
		if err == nil {
			t.Fatal("SDK decoded parent path must be rejected")
		}
		if m.reqCount.Load() != 1 {
			t.Fatalf("guard made label calls: %d", m.reqCount.Load())
		}
	})
}
func TestGitHubUpdateIssueLabelsDelete404Verification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		success bool
	}{
		{"absent", 200, `{"number":5,"labels":[null,{"name":"keep"}]}`, true},
		{"present", 200, `{"number":5,"labels":[{"name":"old"}]}`, false},
		{"forbidden", 403, `{"message":"read failed"}`, false},
		{"not-found", 404, `{"message":"read failed"}`, false},
		{"rate-limit", 429, `{"message":"read failed"}`, false},
		{"server-error", 500, `{"message":"read failed"}`, false},
		{"bad-gateway", 502, `{"message":"read failed"}`, false},
		{"malformed", 200, "{", false}, {"empty", 200, "", false}, {"whitespace", 200, " \n\t", false},
		{"null", 200, "null", false}, {"wrong-number", 200, `{"number":6,"labels":[]}`, false},
		{"missing-number", 200, `{"labels":[]}`, false},
		{"transport", 0, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			token := "ghp_" + "abcdefghijklmnopqrstuvwxyz1234567890"
			m := newMockGitHub(t, map[string]http.HandlerFunc{"/repos/acme/widgets/issues/": func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, r.Method)
				switch r.Method {
				case http.MethodDelete:
					http.Error(w, `{"message":"delete original `+token+`"}`, 404)
				case http.MethodGet:
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				case http.MethodPost:
					_, _ = w.Write([]byte("[]"))
				default:
					t.Errorf("unexpected method %s", r.Method)
				}
			}})
			d := newGitHubRawDriver(t, m, token)
			if tc.status == 0 {
				client := d.client.Client()
				next := client.Transport
				client.Transport = githubLabelTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/5") {
						mu.Lock()
						calls = append(calls, r.Method)
						mu.Unlock()
						return nil, errors.New("read transport failed")
					}
					return next.RoundTrip(r)
				})
				var err error
				d.client, err = gh.NewClient(gh.WithHTTPClient(client), gh.WithEnterpriseURLs(m.srv.URL, m.srv.URL))
				if err != nil {
					t.Fatal(err)
				}
			}
			err := d.UpdateIssueLabels(context.Background(), 7, 5, []string{"new"}, []string{"old", "later"})
			// Each removal gets its own bounded verification, even if already absent.
			if tc.success {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "delete original") {
					t.Fatalf("must retain original delete error: %v", err)
				}
				if strings.Contains(err.Error(), token) {
					t.Fatal("token leaked")
				}
			}
			mu.Lock()
			defer mu.Unlock()
			want := "DELETE GET"
			if tc.success {
				want = "DELETE GET DELETE GET POST"
			}
			if strings.Join(calls, " ") != want {
				t.Fatalf("calls %v, want %s", calls, want)
			}
		})
	}
}
func TestGitHubUpdateIssueLabelsPartialFailure(t *testing.T) {
	for _, status := range []int{403, 404, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := &labelState{labels: map[string]bool{"old": true, "keep": true}}
			m := newMockGitHub(t, map[string]http.HandlerFunc{"/repos/acme/widgets/issues/": func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					s.mu.Lock()
					s.calls = append(s.calls, "POST")
					s.mu.Unlock()
					http.Error(w, `{"message":"add failed"}`, status)
					return
				}
				s.serve(w, r)
			}})
			d := newGitHubDriver(t, m, "test-token")
			err := d.UpdateIssueLabels(context.Background(), 7, 5, []string{"new"}, []string{"old"})
			if err == nil || !strings.Contains(err.Error(), "add failed") {
				t.Fatalf("addition error: %v", err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.labels) != 1 || !s.labels["keep"] {
				t.Fatalf("partial state must retain removal: %v", s.labels)
			}
			if len(s.calls) != 2 || !strings.HasPrefix(s.calls[0], "DELETE ") || s.calls[1] != "POST" {
				t.Fatalf("compensation or extra calls: %v", s.calls)
			}
		})
	}
}
func TestGitHubUpdateIssueLabelsDeleteFailureAborts(t *testing.T) {
	for _, status := range []int{403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int64
			m := newMockGitHub(t, map[string]http.HandlerFunc{"/repos/acme/widgets/issues/": func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodDelete {
					t.Errorf("unexpected %s", r.Method)
				}
				http.Error(w, `{"message":"remove failed"}`, status)
			}})
			d := newGitHubDriver(t, m, "test-token")
			err := d.UpdateIssueLabels(context.Background(), 7, 5, []string{"new"}, []string{"old", "later"})
			if err == nil || !strings.Contains(err.Error(), "remove failed") {
				t.Fatalf("error: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("continued after failure: %d", calls.Load())
			}
		})
	}
}
