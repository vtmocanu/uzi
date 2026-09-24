package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
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
