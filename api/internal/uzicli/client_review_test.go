package uzicli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// TestUndoFindingSoft404: a 404 is softened to the plain ErrFindingNothingToUndo sentinel (not an
// *ExitError), while any other non-2xx keeps its real exit code.
func TestUndoFindingSoft404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
}
