package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// codexActionStore is a minimal workersvc.Store answering only the D6 batched read (PRD #1590);
// anything else panics through the embedded nil interface.
type codexActionStore struct {
	workersvc.Store
	rows  []store.ListCodexAccountActionInputsRow
	err   error
	calls [][]uuid.UUID
}

func (s *codexActionStore) ListCodexAccountActionInputs(_ context.Context, ids []uuid.UUID) ([]store.ListCodexAccountActionInputsRow, error) {
	s.calls = append(s.calls, ids)
	return s.rows, s.err
}

func heldDTO(id uuid.UUID) apitypes.RunDTO {
	cause := "codex_account_unavailable"
	return apitypes.RunDTO{ID: id.String(), Status: "recovery_wait", RecoveryWaitCause: &cause}
}

// TestOverlayCodexAccountActions: the overlay queries only the held codex_account_unavailable
// runs, in one batched read, sets the derived action and the run's own label on them, leaves every other run (and a held
// run the read did not return) null, costs no query when nothing is held, and degrades a read
// error to null without failing.
func TestOverlayCodexAccountActions(t *testing.T) {
	heldA, heldB, other := uuid.New(), uuid.New(), uuid.New()
	forge := "forge_unreachable"
	st := &codexActionStore{rows: []store.ListCodexAccountActionInputsRow{{
		ID:            heldA,
		CodexSecretID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		AliasStatus:   pgtype.Text{String: "staging", Valid: true},
		Status:        "recovery_wait",
		// The run's own snapshotted alias label (PRD #1590 D6).
		CodexSecretLabel: pgtype.Text{String: "work-laptop", Valid: true},
	}}}
	h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{})}

	a, b := heldDTO(heldA), heldDTO(heldB)
	c := apitypes.RunDTO{ID: other.String(), Status: "recovery_wait", RecoveryWaitCause: &forge}
	d := apitypes.RunDTO{ID: uuid.NewString(), Status: "running"}
	h.overlayCodexAccountActions(context.Background(), &a, &b, &c, &d)

	if len(st.calls) != 1 || len(st.calls[0]) != 2 || st.calls[0][0] != heldA || st.calls[0][1] != heldB {
		t.Fatalf("reads = %v, want one read of exactly the two held runs", st.calls)
	}
	if a.CodexAccountAction == nil || *a.CodexAccountAction != workersvc.CodexAccountActionVerifyingLogin {
		t.Fatalf("held staging run action = %v, want verifying_login", a.CodexAccountAction)
	}
	if b.CodexAccountAction != nil || c.CodexAccountAction != nil || d.CodexAccountAction != nil {
		t.Fatalf("actions = %v %v %v, want null for an unreturned hold and for non-held runs",
			b.CodexAccountAction, c.CodexAccountAction, d.CodexAccountAction)
	}
	if a.CodexSecretLabel == nil || *a.CodexSecretLabel != "work-laptop" {
		t.Fatalf("held run label = %v, want the run's own snapshot work-laptop", a.CodexSecretLabel)
	}
	if b.CodexSecretLabel != nil || c.CodexSecretLabel != nil || d.CodexSecretLabel != nil {
		t.Fatalf("labels = %v %v %v, want null wherever the action is null",
			b.CodexSecretLabel, c.CodexSecretLabel, d.CodexSecretLabel)
	}

	// No held run: no query at all.
	st.calls = nil
	h.overlayCodexAccountActions(context.Background(), &c, &d)
	if len(st.calls) != 0 {
		t.Fatalf("reads = %v, want none for a page with no held run", st.calls)
	}

	// A read error leaves the action null and does not fail.
	st.err = errors.New("boom")
	e := heldDTO(heldA)
	h.overlayCodexAccountActions(context.Background(), &e)
	if len(st.calls) != 1 || e.CodexAccountAction != nil || e.CodexSecretLabel != nil {
		t.Fatalf("reads=%d action=%v label=%v, want one failed read and a null action and label",
			len(st.calls), e.CodexAccountAction, e.CodexSecretLabel)
	}
}

// heldRunsStore is the runsStore handler fake plus the D6 batched read, answering only for the
// ids the handler actually asks about, as the real query does.
type heldRunsStore struct {
	*runsStore
	actionRows []store.ListCodexAccountActionInputsRow
}

func (s *heldRunsStore) ListCodexAccountActionInputs(_ context.Context, ids []uuid.UUID) ([]store.ListCodexAccountActionInputsRow, error) {
	var out []store.ListCodexAccountActionInputsRow
	for _, row := range s.actionRows {
		if slices.Contains(ids, row.ID) {
			out = append(out, row)
		}
	}
	return out, nil
}

// TestCodexAccountActionReadHandlersWiring pins the PRD #1590 D6 overlay in every read that
// serves a RunDTO to a held run's viewer: the owner's GetRun and ListRuns, the admin's GetRun,
// and AdminListRuns. Each response carries the derived action and the run's own label for the
// held run and null for its non-held neighbour. The overlay helper has its own unit test above;
// this one fails when a handler stops calling it. Mutation-checked, one call at a time:
// deleting the overlay call in ListRuns, AdminListRuns or GetRun reddened exactly that
// handler's subtest(s) (GetRun: both the owner and the admin read).
func TestCodexAccountActionReadHandlersWiring(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	heldID, otherID := uuid.New(), uuid.New()
	held := store.Run{ID: heldID, UserID: owner.ID, Status: "recovery_wait",
		RecoveryWaitCause: pgtype.Text{String: "codex_account_unavailable", Valid: true}}
	other := store.Run{ID: otherID, UserID: owner.ID, Status: "running"}
	newStore := func() *heldRunsStore {
		return &heldRunsStore{
			runsStore: &runsStore{
				ownerID:    owner.ID,
				run:        held,
				userRuns:   []store.ListRunsForUserRow{{Run: held}, {Run: other}},
				activeRuns: []store.ListActiveRunsAllRow{{Run: held, OwnerEmail: "o@x.io"}, {Run: other, OwnerEmail: "o@x.io"}},
			},
			actionRows: []store.ListCodexAccountActionInputsRow{{
				ID:               heldID,
				CodexSecretID:    pgtype.UUID{Bytes: uuid.New(), Valid: true},
				AliasStatus:      pgtype.Text{String: "staging", Valid: true},
				Status:           "recovery_wait",
				CodexSecretLabel: pgtype.Text{String: "work-laptop", Valid: true},
			}},
		}
	}
	type runJSON struct {
		ID                 string  `json:"id"`
		CodexAccountAction *string `json:"codex_account_action"`
		CodexSecretLabel   *string `json:"codex_secret_label"`
	}
	assertHeld := func(t *testing.T, r runJSON) {
		t.Helper()
		if r.CodexAccountAction == nil || *r.CodexAccountAction != workersvc.CodexAccountActionVerifyingLogin {
			t.Errorf("held run %s action = %v, want verifying_login", r.ID, r.CodexAccountAction)
		}
		if r.CodexSecretLabel == nil || *r.CodexSecretLabel != "work-laptop" {
			t.Errorf("held run %s label = %v, want work-laptop", r.ID, r.CodexSecretLabel)
		}
	}
	decodeList := func(t *testing.T, rec *httptest.ResponseRecorder) map[string]runJSON {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Runs []runJSON `json:"runs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := make(map[string]runJSON, len(body.Runs))
		for _, r := range body.Runs {
			out[r.ID] = r
		}
		if len(out) != 2 {
			t.Fatalf("runs = %v, want the held run and its neighbour", out)
		}
		return out
	}
	assertList := func(t *testing.T, byID map[string]runJSON) {
		t.Helper()
		assertHeld(t, byID[heldID.String()])
		if o := byID[otherID.String()]; o.CodexAccountAction != nil || o.CodexSecretLabel != nil {
			t.Errorf("non-held run action=%v label=%v, want both null", o.CodexAccountAction, o.CodexSecretLabel)
		}
	}
	getRun := func(t *testing.T, viewer store.User) {
		t.Helper()
		rec := httptest.NewRecorder()
		newRunsHandler(t, newStore()).GetRun(rec, runReq(viewer, heldID))
		if rec.Code != http.StatusOK {
			t.Fatalf("GetRun = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Run runJSON `json:"run"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		assertHeld(t, body.Run)
	}

	t.Run("owner GetRun", func(t *testing.T) { getRun(t, owner) })
	t.Run("admin GetRun", func(t *testing.T) { getRun(t, admin) })
	t.Run("owner ListRuns", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
		newRunsHandler(t, newStore()).ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), owner)))
		assertList(t, decodeList(t, rec))
	})
	t.Run("AdminListRuns", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/runs", nil)
		newRunsHandler(t, newStore()).AdminListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), admin)))
		assertList(t, decodeList(t, rec))
	})
}
