package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Pre-DB validation of the issue #1723 Mark done endpoints: each rejects before the queries are
// reached, so a nil-q Handler is sufficient (reaching the store would nil-panic, which is exactly
// the "it validated first" property pinned). The 404/409 and owner-scoping behaviours need the SQL
// and live in findings_done_livedb_test.go.

func TestMarkFindingDoneValidation(t *testing.T) {
	h := &Handler{}
	user := store.User{ID: uuid.New()}

	rec := httptest.NewRecorder()
	h.MarkFindingDone(rec, findingsPathReq(http.MethodPost, nil, uuid.NewString()))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no user = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.MarkFindingDone(rec, findingsPathReq(http.MethodPost, &user, "not-a-uuid"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed id = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBulkMarkFindingsDoneValidation(t *testing.T) {
	h := &Handler{}
	user := store.User{ID: uuid.New()}

	rec := httptest.NewRecorder()
	h.BulkMarkFindingsDone(rec, findingsBodyReq(http.MethodPost, nil, `{"ids":[]}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no user = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.BulkMarkFindingsDone(rec, findingsBodyReq(http.MethodPost, &user, `{"ids":`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// Exactly the cap is NOT rejected by the cap check; a malformed id inside it proves the cap let
	// it through (a 400 naming the id, not the cap) without reaching the store.
	ids := make([]string, 0, 101)
	for i := 0; i < 99; i++ {
		ids = append(ids, `"`+uuid.NewString()+`"`)
	}
	ids = append(ids, `"not-a-uuid"`)
	rec = httptest.NewRecorder()
	h.BulkMarkFindingsDone(rec, findingsBodyReq(http.MethodPost, &user, fmt.Sprintf(`{"ids":[%s]}`, strings.Join(ids, ","))))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid finding id") {
		t.Fatalf("100 ids with one malformed = %d %s, want 400 invalid finding id", rec.Code, rec.Body.String())
	}

	// 101 ids → 400 naming the cap.
	ids = append(ids[:99], `"`+uuid.NewString()+`"`, `"`+uuid.NewString()+`"`)
	rec = httptest.NewRecorder()
	h.BulkMarkFindingsDone(rec, findingsBodyReq(http.MethodPost, &user, fmt.Sprintf(`{"ids":[%s]}`, strings.Join(ids, ","))))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "too many ids (max 100)") {
		t.Fatalf("101 ids = %d %s, want 400 too many ids (max 100)", rec.Code, rec.Body.String())
	}
}

func TestUndoFindingDispositionValidation(t *testing.T) {
	h := &Handler{}
	user := store.User{ID: uuid.New()}

	rec := httptest.NewRecorder()
	h.UndoFindingDisposition(rec, findingsPathReq(http.MethodDelete, nil, uuid.NewString()))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no user = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.UndoFindingDisposition(rec, findingsPathReq(http.MethodDelete, &user, "not-a-uuid"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed id = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
