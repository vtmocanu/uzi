package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

const ownerHeldGenericText = "this owner hold prevents switching the token; inspect the run's recovery options: Resume may re-hold it, Cancel ends the run"

func ownerHeldRow(status, cause string) store.LockRunForCredentialSwitchRow {
	row := store.LockRunForCredentialSwitchRow{Status: status}
	if cause != "" {
		row.RecoveryWaitCause = pgtype.Text{String: cause, Valid: true}
	}
	return row
}

func TestRecoveryWaitOwnerHold(t *testing.T) {
	policyHold := ownerHeldRow("recovery_wait", "provider_outage")
	policyHold.HoldReason = pgtype.Text{String: "codex_policy_hold", Valid: true}
	pending := ownerHeldRow("recovery_wait", "")
	pending.PolicyOutcomeID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	resolved := ownerHeldRow("recovery_wait", "provider_outage")
	resolved.PolicyOutcomeID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	resolved.PolicyResolvedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	notRecovery := ownerHeldRow("queued", "worker_memory_pressure")
	notRecovery.HoldReason = pgtype.Text{String: "codex_policy_hold", Valid: true}

	for _, tc := range []struct {
		name  string
		row   store.LockRunForCredentialSwitchRow
		label string
		held  bool
	}{
		{"policy hold", policyHold, "policy hold", true},
		{"pending outcome", pending, "pending policy outcome", true},
		{"resolved outcome", resolved, "", false},
		{"codex account", ownerHeldRow("recovery_wait", "codex_account_unavailable"), "codex account unavailable", true},
		{"requeue exhausted", ownerHeldRow("recovery_wait", "worker_requeue_exhausted"), "worker requeue exhausted", true},
		{"memory pressure", ownerHeldRow("recovery_wait", "worker_memory_pressure"), "worker memory pressure", true},
		{"adoption failed", ownerHeldRow("recovery_wait", "retained_recovery_adoption_failed"), "retained recovery adoption failed", true},
		{"unwitnessed", ownerHeldRow("recovery_wait", "retained_continuation_unwitnessed"), "retained continuation unwitnessed", true},
		{"retained exhausted", ownerHeldRow("recovery_wait", "retained_recovery_exhausted"), "retained recovery exhausted", true},
		{"unknown cause", ownerHeldRow("recovery_wait", "something_new"), "", false},
		{"null cause", ownerHeldRow("recovery_wait", ""), "", false},
		{"provider outage", ownerHeldRow("recovery_wait", "provider_outage"), "", false},
		{"not recovery_wait", notRecovery, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			label, held := recoveryWaitOwnerHold(tc.row)
			if label != tc.label || held != tc.held {
				t.Fatalf("got (%q, %v), want (%q, %v)", label, held, tc.label, tc.held)
			}
		})
	}
}

func TestRunOwnerHeldErrorText(t *testing.T) {
	if got := (&RunOwnerHeldError{}).Error(); got != ownerHeldGenericText {
		t.Fatalf("empty label text = %q", got)
	}
	want := ownerHeldGenericText + " (hold: x)"
	if got := (&RunOwnerHeldError{Label: "x"}).Error(); got != want {
		t.Fatalf("labelled text = %q, want %q", got, want)
	}
	var err error = &RunOwnerHeldError{Label: "x"}
	if !errors.Is(err, ErrRunOwnerHeld) {
		t.Fatal("does not unwrap to ErrRunOwnerHeld")
	}
	if errors.Is(err, ErrCredentialSwitchRaced) {
		t.Fatal("unexpectedly matches ErrCredentialSwitchRaced")
	}
}
