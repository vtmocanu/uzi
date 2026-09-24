package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_account_action_test.go pins PRD #1590 D6's derived codex_account_action without a
// database: one row per action string of the D6 table, plus the documented relogin_required
// fallback for states the table does not name.

// heldLinkedActionInputs is a held run on a linked alias whose account passes the release
// predicate (heldReleasableInputs): the resuming baseline every case mutates.
func heldLinkedActionInputs(t *testing.T) codexAccountActionInputs {
	t.Helper()
	return codexAccountActionInputs{
		aliasPresent: true, aliasStatus: "linked", accountLinked: true,
		release: heldReleasableInputs(t),
	}
}

func TestDeriveCodexAccountAction(t *testing.T) {
	quarantine := func(in *codexAccountActionInputs) { in.release.coordState = codexCoordQuarantined }
	for _, tc := range []struct {
		name   string
		mutate func(in *codexAccountActionInputs)
		want   string
	}{
		// D6 table rows.
		{"account not quarantined, predicate passes", func(*codexAccountActionInputs) {}, CodexAccountActionResuming},
		{"account committed, predicate passes", func(in *codexAccountActionInputs) { in.release.coordState = codexCoordCommitted },
			CodexAccountActionResuming},
		{"same-identity relogin ahead, idle (re-admitted next tick)", func(in *codexAccountActionInputs) {
			in.release.currentMaterialRev = 5
		}, CodexAccountActionResuming},
		{"quarantined, recovery material at the current generation", func(in *codexAccountActionInputs) {
			quarantine(in)
			in.recoveryAtGeneration = true
		}, CodexAccountActionReconciling},
		{"quarantined, live lease", func(in *codexAccountActionInputs) {
			quarantine(in)
			in.leaseLive = true
		}, CodexAccountActionReconciling},
		{"quarantined, no material and no lease", quarantine, CodexAccountActionReloginRequired},
		// Recovery material outranks the reauth flag: the survivor pass promotes it and
		// PromoteCodexRecovery clears reauth_required, so no owner action is needed.
		{"quarantined, recovery material but reauth flag set", func(in *codexAccountActionInputs) {
			quarantine(in)
			in.recoveryAtGeneration, in.reauthRequired = true, true
		}, CodexAccountActionReconciling},
		{"quarantined, reauth flag set, no material", func(in *codexAccountActionInputs) {
			quarantine(in)
			in.reauthRequired = true
		}, CodexAccountActionReloginRequired},
		{"quarantined, reauth flag set with a live lease", func(in *codexAccountActionInputs) {
			quarantine(in)
			in.reauthRequired, in.leaseLive = true, true
		}, CodexAccountActionReloginRequired},
		{"quarantined, material ahead (no re-admission while quarantined), recovering", func(in *codexAccountActionInputs) {
			quarantine(in)
			in.release.currentMaterialRev, in.recoveryAtGeneration = 5, true
		}, CodexAccountActionReconciling},
		{"alias staging", func(in *codexAccountActionInputs) {
			in.aliasStatus, in.accountLinked = "staging", false
		}, CodexAccountActionVerifyingLogin},
		{"alias failed", func(in *codexAccountActionInputs) {
			in.aliasStatus, in.accountLinked = "failed", false
		}, CodexAccountActionReloginRequired},
		{"alias deleted", func(in *codexAccountActionInputs) {
			in.aliasDeleted, in.aliasPresent, in.aliasStatus, in.accountLinked = true, false, "", false
		}, CodexAccountActionReloginRequired},

		// Documented extension: a refresh in flight (in_progress, live lease) holding back the
		// re-admission is reconciling, not a re-login.
		{"in_progress with live lease, material ahead", func(in *codexAccountActionInputs) {
			in.release.coordState, in.release.currentMaterialRev, in.leaseLive = codexCoordInProgress, 5, true
		}, CodexAccountActionReconciling},

		// FALLBACK: any other state is relogin_required.
		{"fallback: pending terminal identity change", func(in *codexAccountActionInputs) {
			in.release.currentProviderUserID = "user-2"
		}, CodexAccountActionReloginRequired},
		{"fallback: pending terminal credential revision change", func(in *codexAccountActionInputs) {
			in.release.currentCredentialRev = 8
		}, CodexAccountActionReloginRequired},
		{"fallback: identity change hides quarantine recovery", func(in *codexAccountActionInputs) {
			quarantine(in)
			in.release.currentProviderUserID, in.recoveryAtGeneration = "user-2", true
		}, CodexAccountActionReloginRequired},
		{"fallback: linked alias with no account", func(in *codexAccountActionInputs) { in.accountLinked = false },
			CodexAccountActionReloginRequired},
		{"fallback: missing alias state row", func(in *codexAccountActionInputs) { in.aliasPresent = false },
			CodexAccountActionReloginRequired},
		{"fallback: static alias", func(in *codexAccountActionInputs) { in.aliasStatus = "static" },
			CodexAccountActionReloginRequired},
		// An expired in_progress lease is reaped into quarantine by the survivor pass on its
		// next tick, so it is reconciling, not an owner action.
		{"in_progress with expired lease, material ahead", func(in *codexAccountActionInputs) {
			in.release.coordState, in.release.currentMaterialRev, in.leaseExpired = codexCoordInProgress, 5, true
		}, CodexAccountActionReconciling},
		{"fallback: in_progress with no lease, material ahead", func(in *codexAccountActionInputs) {
			in.release.coordState, in.release.currentMaterialRev = codexCoordInProgress, 5
		}, CodexAccountActionReloginRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := heldLinkedActionInputs(t)
			tc.mutate(&in)
			if got := deriveCodexAccountAction(in); got != tc.want {
				t.Fatalf("action = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCodexAccountActionInputsFromRow pins the row projection's derived flags: the lease is live
// only strictly before its deadline at the read's now, a NULL alias id is a deleted alias, and
// NULL account columns (a LEFT join miss) never read as a linked account.
func TestCodexAccountActionInputsFromRow(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	acct := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	row := store.ListCodexAccountActionInputsRow{
		ID:                     uuid.New(),
		CodexSecretID:          pgtype.UUID{Bytes: uuid.New(), Valid: true},
		AliasStatus:            pgtype.Text{String: "linked", Valid: true},
		AliasProviderAccountID: acct,
		AccountID:              acct,
		LeaseDeadline:          pgtype.Timestamptz{Time: now.Add(time.Second), Valid: true},
		ReauthRequired:         pgtype.Bool{Bool: true, Valid: true},
	}
	in := codexAccountActionInputsFromRow(row, now)
	if in.aliasDeleted || !in.aliasPresent || !in.accountLinked || !in.leaseLive || in.leaseExpired || !in.reauthRequired {
		t.Fatalf("linked row projected as %+v", in)
	}
	row.LeaseDeadline.Time = now
	if in := codexAccountActionInputsFromRow(row, now); in.leaseLive || !in.leaseExpired {
		t.Fatalf("a lease at its deadline is expired, not live: %+v", in)
	}
	row.LeaseDeadline = pgtype.Timestamptz{}
	if in := codexAccountActionInputsFromRow(row, now); in.leaseLive || in.leaseExpired {
		t.Fatalf("a NULL lease is neither live nor expired: %+v", in)
	}
	row.CodexSecretID, row.AliasStatus, row.AliasProviderAccountID, row.AccountID = pgtype.UUID{}, pgtype.Text{}, pgtype.UUID{}, pgtype.UUID{}
	in = codexAccountActionInputsFromRow(row, now)
	if !in.aliasDeleted || in.aliasPresent || in.accountLinked {
		t.Fatalf("deleted-alias row projected as %+v", in)
	}
}

func TestIsCodexAccountHold(t *testing.T) {
	cause, other := recoveryCauseCodexAccountUnavailable, "forge_unreachable"
	for _, tc := range []struct {
		status string
		cause  *string
		want   bool
	}{
		{"recovery_wait", &cause, true},
		{"recovery_wait", &other, false},
		{"recovery_wait", nil, false},
		{"queued", &cause, false},
	} {
		if got := IsCodexAccountHold(tc.status, tc.cause); got != tc.want {
			t.Errorf("IsCodexAccountHold(%q, %v) = %v, want %v", tc.status, tc.cause, got, tc.want)
		}
	}
}
