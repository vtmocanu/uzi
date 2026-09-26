package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// credential_disabled_d5_test.go pins PRD #1732 D5 on the binding surfaces a fake store can
// drive: a NEW explicit assignment onto a disabled credential is refused with a 409 naming
// Settings, before any write. The live-DB half (run create, schedule create/edit, set-token)
// is credential_disabled_d5_livedb_test.go.

func assertSettings409(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Settings") {
		t.Fatalf("body = %s, want a refusal naming Settings", rec.Body.String())
	}
}

// TestPatchWorkerRefusesDisabledToken: binding a worker to a disabled token is a 409 and the
// binding is never written.
//
// MUTATION: drop the row.Disabled check in SetWorkerAnthropicToken; the bind is then written
// with a 200 and this test fails.
func TestPatchWorkerRefusesDisabledToken(t *testing.T) {
	owner := store.User{ID: uuid.New(), IsActive: true}
	secretID, workerID := uuid.New(), uuid.New()
	st := &bindStore{
		secrets:  map[uuid.UUID]uuid.UUID{secretID: owner.ID},
		labels:   map[string]uuid.UUID{owner.ID.String() + "|parked": secretID},
		disabled: map[uuid.UUID]bool{secretID: true},
	}
	h := newBindHandler(t, st)
	rec := httptest.NewRecorder()
	h.PatchWorker(rec, patchWorkerReq(t, owner, workerID, `{"anthropic_token":"parked"}`))
	assertSettings409(t, rec)
	if st.setCalled {
		t.Fatal("a binding onto a disabled token was written")
	}
}

// TestSetJudgeRefusesDisabledTokenAllOrNothing: a Judge PUT naming a disabled token is a 409
// and writes NOTHING, neither the binding nor the opt-in flip that precedes it.
//
// MUTATION: drop the handler's CheckCredentialEnabled pre-check; the opt-in is then flipped
// before the binding writer refuses, and this test fails on the half-applied request.
func TestSetJudgeRefusesDisabledTokenAllOrNothing(t *testing.T) {
	owner, secretID := uuid.New(), uuid.New()
	st := &judgeBindStore{
		secrets:  map[uuid.UUID]uuid.UUID{secretID: owner},
		labels:   map[string]uuid.UUID{owner.String() + "|parked": secretID},
		disabled: map[uuid.UUID]bool{secretID: true},
	}
	db := &fakeUserDB{}
	h := newJudgeBindHandler(t, db, st)
	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"anthropic_token":"parked"}`))
	assertSettings409(t, rec)
	if db.called || st.setCalled {
		t.Fatalf("writes on a refused request: opt-in=%t binding=%t, want neither", db.called, st.setCalled)
	}
}

// TestSetRunCredentialDisabledIs409: the set-token surface maps the validator's D5 refusal to
// the same 409 as run create (the shared writeCredentialOverrideError).
func TestSetRunCredentialDisabledIs409(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.writeSetRunCredentialError(rec, workersvc.ErrCredentialDisabled)
	assertSettings409(t, rec)
}
