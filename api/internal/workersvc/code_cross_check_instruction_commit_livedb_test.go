package workersvc

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodeCrossCheckInstructionCommitFrozenLiveDB(t *testing.T) {
	f := codeFixture(t)
	w := wkrRow(t, f.env, f.workerID)
	base, head, pin := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	input := CodeCrossCheckInstructions{RepoInstructionsCommit: &pin}
	cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, head, input)
	if err != nil || !cc.RepoInstructionsCommit.Valid || cc.RepoInstructionsCommit.String != pin {
		t.Fatalf("insert pin: %+v %v", cc, err)
	}
	status, err := f.svc.CodeCrossCheckStatus(f.env.ctx, w, f.lead, 1)
	if err != nil || status.RepoInstructionsCommit != cc.RepoInstructionsCommit {
		t.Fatalf("read pin: %+v %v", status, err)
	}
	retry, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, head, input)
	if err != nil || retry.ID != cc.ID || !bytes.Equal(retry.CandidateDigest, cc.CandidateDigest) {
		t.Fatalf("retry: %v", err)
	}
	changed := strings.Repeat("d", 40)
	if _, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, head, CodeCrossCheckInstructions{RepoInstructionsCommit: &changed}); !errors.Is(err, ErrCrossCheckInterrupted) {
		t.Fatalf("changed retry: %v", err)
	}
	if _, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, head); !errors.Is(err, ErrCrossCheckInterrupted) {
		t.Fatalf("omitted retry: %v", err)
	}
	f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
	claim := laneClaim(t, f, "cross_check", nil)
	if claim == nil || claim.CrossCheck == nil || claim.CrossCheck.RepoInstructionsCommit == nil || *claim.CrossCheck.RepoInstructionsCommit != pin {
		t.Fatal("child lost pin")
	}
	if err := ValidateCrossCheckClaim(claim); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{nil, changed} {
		if _, err := f.env.pool.Exec(f.env.ctx, "UPDATE cross_checks SET repo_instructions_commit=$2 WHERE id=$1", cc.ID, value); err == nil {
			t.Fatal("frozen pin changed")
		}
	}
	if _, err := f.env.pool.Exec(f.env.ctx, "UPDATE cross_checks SET repo_instructions_commit=repo_instructions_commit WHERE id=$1", cc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.DecideCodeCrossCheck(f.env.ctx, w, f.runID, claim.ClaimGeneration, "failed", "model_error", nil); err != nil {
		t.Fatal(err)
	}
	failed, err := f.svc.CodeCrossCheckStatus(f.env.ctx, w, f.lead, 1)
	if err != nil || failed.RepoInstructionsCommit != cc.RepoInstructionsCommit {
		t.Fatalf("later failure lost pin: %v", err)
	}
}

func TestCodeCrossCheckInstructionCommitFailedInsertLiveDB(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_snapshot", true: "snapshot"}[snapshot], func(t *testing.T) {
			f := codeFixture(t)
			w := wkrRow(t, f.env, f.workerID)
			pin := strings.Repeat("c", 40)
			var cc store.CrossCheck
			var err error
			if snapshot {
				cc, err = f.svc.RecordCodeCrossCheckFailure(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40), "checker_unavailable", CodeCrossCheckInstructions{RepoInstructionsCommit: &pin})
			} else {
				if _, err := f.svc.RecordCodeCrossCheckFailure(f.env.ctx, w, f.lead, 1, "", "", "snapshot_failed", CodeCrossCheckInstructions{RepoInstructionsCommit: &pin}); !errors.Is(err, ErrCrossCheckRefused) {
					t.Fatalf("pre-snapshot pin: %v", err)
				}
				cc, err = f.svc.RecordCodeCrossCheckFailure(f.env.ctx, w, f.lead, 1, "", "", "snapshot_failed")
			}
			if err != nil || cc.RepoInstructionsCommit.Valid != snapshot {
				t.Fatalf("failed insert: %+v %v", cc, err)
			}
			// Disable only this private fixture's immutability trigger to exercise the
			// independent column CHECK, then restore it even on assertion failure.
			f.env.exec("ALTER TABLE cross_checks DISABLE TRIGGER cross_checks_code_snapshot")
			defer f.env.exec("ALTER TABLE cross_checks ENABLE TRIGGER cross_checks_code_snapshot")
			values := []string{"", strings.Repeat("A", 40), strings.Repeat("a", 39), strings.Repeat("g", 40)}
			if !snapshot {
				values = append(values, pin)
			}
			for _, value := range values {
				if _, err := f.env.pool.Exec(f.env.ctx, "UPDATE cross_checks SET repo_instructions_commit=$2 WHERE id=$1", cc.ID, value); err == nil {
					t.Fatalf("CHECK accepted %q", value)
				}
			}
		})
	}
}
