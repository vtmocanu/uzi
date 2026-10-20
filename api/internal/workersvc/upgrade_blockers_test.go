package workersvc

import (
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func blockerText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

func TestUpgradeBlockerStatuses(t *testing.T) {
	tests := []struct{ status, want string }{
		{"awaiting_approval", "owner approves or reviews"},
		{"awaiting_input", "owner answers"},
		{"awaiting_followup", "owner follows up or ends"},
		{"paused", "owner resumes"},
		{"claimed", "run must finish"}, {"running", "run must finish"},
		{"pool_wait", "a pooled token becomes spendable, or the owner adds or selects a usable token / runs `uzi run resume-now`"},
		{"limit_wait", "gates promotion"}, {"recovery_wait", "automatic retry not before"},
		{"queued", "exact fence unavailable, inspect run"}, {"new_status", "releasing action unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			w, owner, run := uuid.New(), uuid.New(), uuid.New()
			rows := []store.ListWorkerUpgradeBlockersRow{{
				WorkerID: w, WorkerOwner: owner, SafeCount: 1, RunID: pgtype.UUID{Bytes: run, Valid: true},
				Kind: blockerText("chat"), Status: blockerText(tc.status), RecoveryRetryNotBefore: pgtype.Timestamptz{Valid: true, Time: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}, HealthReason: blockerText("vault_locked"),
				HoldReason: blockerText("credential_disabled"), RecoveryWaitCause: blockerText("forge_unreachable"),
			}}
			got := GroupUpgradeBlockers(rows)[w].Summary()
			for _, want := range []string{run.String(), tc.want, "assignment releases only when the run ends or its assignment is removed"} {
				if !strings.Contains(got, want) {
					t.Fatalf("%q missing %q", got, want)
				}
			}
			if tc.status == "pool_wait" && strings.Contains(got, "capacity") {
				t.Fatal(got)
			}
			if tc.status == "queued" && (!strings.Contains(got, "vault_locked") || !strings.Contains(got, "credential_disabled")) {
				t.Fatal(got)
			}
			if tc.status == "recovery_wait" && !strings.Contains(got, "forge_unreachable") {
				t.Fatal(got)
			}
		})
	}
}

func TestUpgradeBlockerRecoveryRetryActions(t *testing.T) {
	for _, cause := range []string{"", "vault_locked", "forge_unreachable"} {
		t.Run(cause, func(t *testing.T) {
			row := store.ListWorkerUpgradeBlockersRow{Status: blockerText("recovery_wait"), RecoveryWaitCause: blockerText(cause), RecoveryRetryNotBefore: pgtype.Timestamptz{Valid: true, Time: time.Now().Add(time.Hour)}}
			action := blockerAction(row)
			if !strings.Contains(action, "automatic retry") || strings.Contains(action, "owner makes the recovery decision") {
				t.Fatalf("recovery wait misstates automatic retry: %s", action)
			}
			if cause == "vault_locked" && !strings.Contains(action, "owner unlocks the vault") {
				t.Fatalf("missing known owner action: %s", action)
			}
		})
	}
}

func TestUpgradeBlockerRecoveryHoldsHaveNoTimer(t *testing.T) {
	for _, cause := range []string{"codex_account_unavailable", "worker_requeue_exhausted"} {
		for _, retry := range []pgtype.Timestamptz{{}, {Valid: true, Time: time.Now().Add(time.Hour)}} {
			row := store.ListWorkerUpgradeBlockersRow{Status: blockerText("recovery_wait"), RecoveryWaitCause: blockerText(cause), RecoveryRetryNotBefore: retry}
			action := blockerAction(row)
			if strings.Contains(action, "automatic retry") || !strings.Contains(action, "no timer") || !strings.Contains(action, cause) {
				t.Fatalf("hold advertises timer: %s", action)
			}
		}
	}
	row := store.ListWorkerUpgradeBlockersRow{Status: blockerText("recovery_wait")}
	if action := blockerAction(row); strings.Contains(action, "automatic retry") || !strings.Contains(action, "retry timing unavailable") {
		t.Fatalf("NULL timing invented: %s", action)
	}
	row.RecoveryRetryNotBefore = pgtype.Timestamptz{Valid: true, Time: time.Now().Add(time.Hour)}
	if action := blockerAction(row); !strings.Contains(action, "automatic retry") || !strings.Contains(action, "cause unavailable") {
		t.Fatalf("SQL NULL cause lost timed recovery: %s", action)
	}
}

func TestUpgradeBlockerGroupingCountEmptyMismatch(t *testing.T) {
	w, owner := uuid.New(), uuid.New()
	a := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	z := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	rows := []store.ListWorkerUpgradeBlockersRow{
		{WorkerID: w, WorkerOwner: owner, SafeCount: 9, RunID: pgtype.UUID{Bytes: z, Valid: true}, Status: blockerText("running")},
		{WorkerID: w, WorkerOwner: owner, SafeCount: 9, RunID: pgtype.UUID{Bytes: a, Valid: true}, Status: blockerText("paused"), OwnerMismatch: true},
	}
	b := GroupUpgradeBlockers(rows)[w]
	if b.Owner != owner || b.Count != 9 || !strings.Contains(b.Runs[0], a.String()) {
		t.Fatalf("%+v", b)
	}
	got := b.Summary()
	if !strings.Contains(got, "7 additional") || !strings.Contains(got, "unavailable") || strings.Contains(got, "No assigned") {
		t.Fatal(got)
	}
	for _, mismatch := range []bool{false, true} {
		b = GroupUpgradeBlockers([]store.ListWorkerUpgradeBlockersRow{{WorkerID: w, WorkerOwner: owner, OwnerMismatch: mismatch}})[w]
		got = b.Summary()
		if mismatch && (!strings.Contains(got, "unavailable") || strings.Contains(got, "No assigned")) {
			t.Fatal(got)
		}
		if !mismatch && !strings.Contains(got, "custody clearance is not established") {
			t.Fatal(got)
		}
	}
	if got := (UpgradeBlockers{Unavailable: true}).Summary(); !strings.Contains(got, "unavailable") || strings.Contains(got, "No assigned") {
		t.Fatal(got)
	}
}

func TestUpgradeBlockerUntrustedFactsAndTimes(t *testing.T) {
	w := uuid.New()
	hostile := blockerText("\n\t\u001b\u202e" + strings.Repeat("界", 200))
	b := GroupUpgradeBlockers([]store.ListWorkerUpgradeBlockersRow{{
		WorkerID: w, SafeCount: 1, RunID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		Status: blockerText("queued"), Kind: hostile, HealthReason: hostile, HoldReason: hostile,
	}})[w]
	for _, r := range b.Summary() {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Fatalf("unsafe %q", r)
		}
	}
	if strings.Contains(b.Summary(), strings.Repeat("界", 96)) {
		t.Fatal("unbounded fact")
	}
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		stamp pgtype.Timestamptz
		want  string
	}{
		{pgtype.Timestamptz{}, "unavailable"},
		{pgtype.Timestamptz{Valid: true}, "unavailable"},
		{pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.Infinity}, "unavailable"},
		{pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.NegativeInfinity}, "unavailable"},
		{pgtype.Timestamptz{Valid: true, Time: future}, "2099-01-01T00:00:00Z"},
	}
	for _, tc := range tests {
		got := GroupUpgradeBlockers([]store.ListWorkerUpgradeBlockersRow{{
			WorkerID: w, SafeCount: 1, RunID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Status: blockerText("limit_wait"),
			RetryNotBefore: tc.stamp, LimitResetsAt: tc.stamp,
		}})[w].Summary()
		if !strings.Contains(got, "retry_not_before "+tc.want) || !strings.Contains(got, "limit reset "+tc.want+" is informational") || !strings.Contains(got, "no roll time promised") {
			t.Fatal(got)
		}
	}
}
