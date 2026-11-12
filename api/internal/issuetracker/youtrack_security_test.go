package issuetracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"unicode"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// tlsDriver builds a bound driver over a TLS test server running h.
func tlsDriver(t *testing.T, h http.Handler) (*youTrack, *httptest.Server) {
	t.Helper()
	return tlsDriverToken(t, h, credential())
}

// tlsDriverToken is tlsDriver with a chosen token.
func tlsDriverToken(t *testing.T, h http.Handler, token string) (*youTrack, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	y, err := newYouTrackOver(Options{
		BaseURL:        srv.URL,
		Token:          token,
		Project:        ProjectRef{ID: "0-1", ShortName: "UZI"},
		ProjectID:      testProjectID,
		EligibilityTag: fakeEligibility,
		OpenState:      "Open",
		ClosedState:    "Fixed",
	}, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return y, srv
}

func tokenForms() []string {
	c := credential()
	j, _ := json.Marshal(c)
	return []string{c, url.QueryEscape(c), url.PathEscape(c), string(j[1 : len(j)-1])}
}

func assertScrubbed(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, f := range tokenForms() {
		if strings.Contains(msg, f) {
			t.Errorf("error contains a token form %q: %q", f, msg)
		}
	}
	for _, r := range msg {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Errorf("error contains control or format character %U: %q", r, msg)
			break
		}
	}
}

func TestRedirectsAreRefused(t *testing.T) {
	for _, code := range []int{301, 302, 307, 308} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var hits atomic.Int64
			other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
			t.Cleanup(other.Close)
			y, _ := tlsDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL+r.URL.Path, code) //nolint:gosec // G710: test redirect to a second httptest server, the redirect-refusal behaviour under test
			}))
			ctx := context.Background()
			calls := map[string]func() error{
				"GetIssue":    func() error { _, err := y.GetIssue(ctx, 1, 1); return err },
				"VerifyToken": func() error { _, err := y.VerifyToken(ctx); return err },
			}
			if code == 307 || code == 308 {
				calls["CreateIssue"] = func() error { _, err := y.CreateIssue(ctx, 1, "t", "d", nil); return err }
			}
			for name, call := range calls {
				err := call()
				if !errors.Is(err, ErrRedirectRefused) {
					t.Errorf("%s: %v, want ErrRedirectRefused", name, err)
				}
				if err != nil && !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", code)) {
					t.Errorf("%s: message %q does not name the status", name, err)
				}
				assertScrubbed(t, err)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("redirect target received %d requests", n)
			}
		})
	}
}

func TestOversizedResponses(t *testing.T) {
	pad := func(n int) []byte { return append([]byte("["), bytes.Repeat([]byte(" "), n)...) }
	t.Run("list page", func(t *testing.T) {
		y, _ := tlsDriver(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(pad(int(listPageBytes) + 10))
		}))
		if _, err := y.ListProjects(context.Background()); !errors.Is(err, forge.ErrResponseTooLarge) {
			t.Fatalf("got %v, want ErrResponseTooLarge", err)
		}
	})
	t.Run("GetIssue", func(t *testing.T) {
		y, _ := tlsDriver(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(pad(int(objectBytes) + 10))
		}))
		_, err := y.GetIssue(context.Background(), 1, 1)
		if !errors.Is(err, forge.ErrResponseTooLarge) || !forge.IsIssueResponseTooLarge(err) {
			t.Fatalf("got %v, want an issue-provenance ErrResponseTooLarge", err)
		}
	})
}

func TestErrorBodiesAreScrubbed(t *testing.T) {
	c := credential()
	nasty := "\r\n\x1b[31m\u202e"
	j, _ := json.Marshal(c)
	bodies := map[string][]byte{
		"raw":     []byte("echo " + c + nasty),
		"query":   []byte("echo " + url.QueryEscape(c) + nasty),
		"json":    []byte(`{"error":"bad","error_description":"echo ` + string(j[1:len(j)-1]) + `\r\n\u001b[31m\u202e"}`),
		"marshal": mustJSON(t, map[string]string{"error": c + nasty, "error_description": url.QueryEscape(c) + nasty}),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			y, _ := tlsDriver(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write(body)
			}))
			_, err := y.VerifyToken(context.Background())
			assertScrubbed(t, err)
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEveryMethodScrubsItsErrors(t *testing.T) {
	y, _ := tlsDriver(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(mustJSON(t, map[string]string{"error": "boom", "error_description": "token " + credential() + " \u202e"}))
	}))
	ctx := context.Background()
	calls := map[string]func() error{
		"ListLabels":             func() error { _, err := y.ListLabels(ctx, 1); return err },
		"EnsureLabels":           func() error { return y.EnsureLabels(ctx, 1, []forge.Label{{Name: "a"}}) },
		"ListIssues":             func() error { _, err := y.ListIssues(ctx, 1, forge.ListIssuesOptions{}); return err },
		"GetIssue":               func() error { _, err := y.GetIssue(ctx, 1, 1); return err },
		"CreateIssue":            func() error { _, err := y.CreateIssue(ctx, 1, "t", "d", nil); return err },
		"UpdateIssueLabels":      func() error { return y.UpdateIssueLabels(ctx, 1, 1, []string{"a"}, nil) },
		"UpdateIssueDescription": func() error { return y.UpdateIssueDescription(ctx, 1, 1, "d") },
		"SetIssueState":          func() error { return y.SetIssueState(ctx, 1, 1, forge.StateClosed) },
		"ListIssueLabelEvents":   func() error { _, err := y.ListIssueLabelEvents(ctx, 1, 1); return err },
		"ListIssueComments":      func() error { _, err := y.ListIssueComments(ctx, 1, 1); return err },
		"CreateIssueNote":        func() error { _, err := y.CreateIssueNote(ctx, 1, 1, "n"); return err },
		"VerifyToken":            func() error { _, err := y.VerifyToken(ctx); return err },
		"ListProjects":           func() error { _, err := y.ListProjects(ctx); return err },
		"ListStates":             func() error { _, err := y.ListStates(ctx, "0-1"); return err },
	}
	if len(calls) != 14 {
		t.Fatalf("table covers %d methods, want 14", len(calls))
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			assertScrubbed(t, err)
			if forge.Class(err) != forge.ErrorClassServerError {
				t.Errorf("class = %v, want server error", forge.Class(err))
			}
		})
	}
}

func TestMovedIssueIsNeverWritten(t *testing.T) {
	y, srv := newFakeDriver(t)
	ctx := context.Background()
	n := srv.SeedIssue("moves", "d")
	srv.MoveIssue(n, youtracktest.OtherProjectID)
	srv.ResetRequests()
	calls := map[string]func() error{
		"GetIssue":               func() error { _, err := y.GetIssue(ctx, 1, n); return err },
		"UpdateIssueLabels":      func() error { return y.UpdateIssueLabels(ctx, 1, n, []string{"a"}, []string{"b"}) },
		"UpdateIssueDescription": func() error { return y.UpdateIssueDescription(ctx, 1, n, "d") },
		"SetIssueState":          func() error { return y.SetIssueState(ctx, 1, n, forge.StateClosed) },
		"ListIssueLabelEvents":   func() error { _, err := y.ListIssueLabelEvents(ctx, 1, n); return err },
		"ListIssueComments":      func() error { _, err := y.ListIssueComments(ctx, 1, n); return err },
		"CreateIssueNote":        func() error { _, err := y.CreateIssueNote(ctx, 1, n, "x"); return err },
	}
	for name, call := range calls {
		var nf *forge.IssueNotFoundError
		if err := call(); !errors.As(err, &nf) {
			t.Errorf("%s = %v, want *forge.IssueNotFoundError", name, err)
		}
	}
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			t.Errorf("write after move: %s %s", r.Method, r.Path)
		}
	}
}
