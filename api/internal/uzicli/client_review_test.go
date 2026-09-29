package uzicli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMarkFindingDoneWire: resolve POSTs to /api/findings/{id}/done with the id path-escaped and
// decodes the {status, disposition_id} reply (issue #1723).
func TestMarkFindingDoneWire(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"done","disposition_id":"disp-9"}`))
	}))
	defer srv.Close()

	res, err := newTestClient(srv).MarkFindingDone(context.Background(), "f/1")
	if err != nil {
		t.Fatalf("MarkFindingDone: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/findings/f%2F1/done" {
		t.Errorf("path = %q, want /api/findings/f%%2F1/done (id escaped)", gotPath)
	}
	if gotAuth != "Bearer uzc_test" {
		t.Errorf("auth = %q, want Bearer uzc_test", gotAuth)
	}
	if res.Status != "done" || res.DispositionID != "disp-9" {
		t.Errorf("decoded result wrong: %+v", res)
	}
}

// TestMarkFindingDoneStatusMapping: a 404 (unknown/foreign id) is exit 4 and a 409 (being filed)
// is exit 5, straight from statusError, exactly like dismiss.
func TestMarkFindingDoneStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   int
	}{
		{"not-found-404", http.StatusNotFound, ExitNotFound},
		{"filing-409", http.StatusConflict, ExitConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"nope"}`))
			}))
			defer srv.Close()
			_, err := newTestClient(srv).MarkFindingDone(context.Background(), "f-1")
			if got := ExitCodeFor(err); got != tc.want {
				t.Errorf("exit = %d, want %d (err: %v)", got, tc.want, err)
			}
		})
	}
}

// TestUndoFindingWire: undo DELETEs the neutral /api/findings/{id}/disposition route (not the
// dismissed-only /dismiss one) and decodes the undone row, whose status is where it landed.
func TestUndoFindingWire(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"disposition_id":"disp-1","status":"filed"}`))
	}))
	defer srv.Close()

	row, err := newTestClient(srv).UndoFinding(context.Background(), "disp-1")
	if err != nil {
		t.Fatalf("UndoFinding: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/findings/disp-1/disposition" {
		t.Errorf("path = %q, want /api/findings/disp-1/disposition", gotPath)
	}
	if row.Status != "filed" {
		t.Errorf("decoded status = %q, want filed", row.Status)
	}
}

// TestUndoFindingLegacyFallback: against a server built before #1723, DELETE .../disposition
// is a router 404 (the route does not exist). Undo must retry once on the legacy DELETE
// .../dismiss route (keyed on the same disposition id) rather than report "already undone"
// while the dismissal remains. The mux registers ONLY the legacy route, like an old server.
func TestUndoFindingLegacyFallback(t *testing.T) {
	var paths []string
	mux := http.NewServeMux()
	// The route is registered for the exact id so the router still 404s any other one; the body
	// is a constant (no request value is echoed back into the response).
	mux.HandleFunc("DELETE /api/findings/disp-1/dismiss", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"disposition_id":"disp-1","status":"open"}`))
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.EscapedPath())
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	row, err := newTestClient(srv).UndoFinding(context.Background(), "disp-1")
	if err != nil {
		t.Fatalf("UndoFinding against a pre-#1723 server: %v", err)
	}
	if row.DispositionID != "disp-1" || row.Status != "open" {
		t.Errorf("decoded row = %+v, want disp-1/open", row)
	}
	want := []string{"DELETE /api/findings/disp-1/disposition", "DELETE /api/findings/disp-1/dismiss"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", paths, want)
	}
}

// TestUndoFindingSoft404: a 404 from BOTH the /disposition route and the legacy /dismiss
// fallback is softened to the plain ErrFindingNothingToUndo sentinel (not an *ExitError), while
// any other non-2xx on the first request keeps its real exit code and triggers no fallback.
func TestUndoFindingSoft404(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no dismissed or done finding to undo"}`))
	}))
	defer srv.Close()
	_, err := newTestClient(srv).UndoFinding(context.Background(), "disp-1")
	if !errors.Is(err, ErrFindingNothingToUndo) {
		t.Fatalf("404 err = %v, want ErrFindingNothingToUndo", err)
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		t.Errorf("the softened 404 must be a plain error, not an *ExitError (%v)", ee)
	}
	want := []string{"/api/findings/disp-1/disposition", "/api/findings/disp-1/dismiss"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", paths, want)
	}

	var paths401 []string
	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths401 = append(paths401, r.URL.EscapedPath())
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid CLI token"}`))
	}))
	defer srv401.Close()
	_, err = newTestClient(srv401).UndoFinding(context.Background(), "disp-1")
	if errors.Is(err, ErrFindingNothingToUndo) {
		t.Fatal("a 401 must not be softened to the nothing-to-undo sentinel")
	}
	if got := ExitCodeFor(err); got != ExitAuth {
		t.Errorf("401 exit = %d, want %d", got, ExitAuth)
	}
	if len(paths401) != 1 {
		t.Errorf("a 401 must not trigger the legacy fallback; requests = %v", paths401)
	}
}

// TestUndoFindingFallbackHardError: a /disposition 404 followed by a non-404 failure on the
// legacy /dismiss fallback is a real *ExitError, never the nothing-to-undo sentinel.
func TestUndoFindingFallbackHardError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.EscapedPath(), "/disposition") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer srv.Close()
	_, err := newTestClient(srv).UndoFinding(context.Background(), "disp-1")
	if errors.Is(err, ErrFindingNothingToUndo) {
		t.Fatal("a fallback 500 must not be softened to the nothing-to-undo sentinel")
	}
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("fallback 500 err = %v, want an *ExitError", err)
	}
}

func TestFindingIssueDraftWire(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"disposition_id":"d-3","title":"t"}`))
	}))
	defer srv.Close()
	d, err := newTestClient(srv).FindingIssueDraft(context.Background(), "e/1")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/findings/e%2F1/issue-draft" || d.DispositionID != "d-3" {
		t.Errorf("method=%q path=%q draft=%+v", gotMethod, gotPath, d)
	}
}

func TestFileFindingGroupWire(t *testing.T) {
	for _, tc := range []struct {
		status   int
		body     string
		accepted bool
	}{
		{http.StatusCreated, `{"operation_id":"op-1","disposition_ids":["a","b"],"phase":"settled","issue":{"iid":5,"web_url":"u","title":"t"}}`, false},
		{http.StatusAccepted, `{"operation_id":"op-2","disposition_ids":["a","b"],"phase":"in_flight","warning":"w"}`, true},
	} {
		var gotMethod, gotPath, gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		res, accepted, err := newTestClient(srv).FileFindingGroup(context.Background(), []string{"a", "b"})
		srv.Close()
		if err != nil {
			t.Fatalf("%d: %v", tc.status, err)
		}
		if gotMethod != http.MethodPost || gotPath != "/api/findings/issue" || gotBody != `{"ids":["a","b"]}` {
			t.Errorf("%d: method=%q path=%q body=%q", tc.status, gotMethod, gotPath, gotBody)
		}
		if accepted != tc.accepted || res.OperationID == "" || (tc.accepted == (res.Issue != nil)) {
			t.Errorf("%d: accepted=%v res=%+v", tc.status, accepted, res)
		}
	}
}

func TestFileFindingGroupStatusMapping(t *testing.T) {
	for status, want := range map[int]int{http.StatusConflict: ExitConflict, http.StatusBadRequest: ExitUsage, http.StatusNotFound: ExitNotFound} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"x"}`))
		}))
		_, _, err := newTestClient(srv).FileFindingGroup(context.Background(), []string{"a"})
		srv.Close()
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != want {
			t.Errorf("status %d: err=%v want exit %d", status, err, want)
		}
	}
}

func TestReleaseFindingGroupWire(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.EscapedPath()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"operation_id":"op/1","phase":"released"}`))
	}))
	defer srv.Close()
	res, err := newTestClient(srv).ReleaseFindingGroup(context.Background(), "op/1")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/findings/filing-operations/op%2F1/release" || gotBody != `{"confirmed_absent":true}` || res.Phase != "released" {
		t.Errorf("method=%q path=%q body=%q res=%+v", gotMethod, gotPath, gotBody, res)
	}
}
