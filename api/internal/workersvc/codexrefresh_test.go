package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexRefreshLeaseLeavesWorkerHTTPMargin(t *testing.T) {
	if codexRefreshLeaseTTL != 7*time.Second {
		t.Fatalf("refresh lease = %s, want 7s below the worker's 8s HTTP budget", codexRefreshLeaseTTL)
	}
}

// These are the DB-free unit tests for the coordinated Codex refresher's two pure
// decision functions (PRD #1147 M2, B6): the merged-reseal rule and the reconcile
// resolution rule. They need no Postgres, so they run in the ordinary `go test` sweep.

// TestMergeCodexLoginRetainsPreviousRefreshTokenWhenOmitted proves the rev-4 fix 1 merge
// rule: when the provider OMITS a rotated refresh token (RefreshResult.RefreshToken nil),
// the merged blob installs the NEW access token but RETAINS the previous refresh token —
// never a stale access token, never a dropped refresh token.
func TestMergeCodexLoginRetainsPreviousRefreshTokenWhenOmitted(t *testing.T) {
	prev := codexLoginBlob{AccessToken: "old-access", RefreshToken: "keep-me"}
	merged := mergeCodexLogin(prev, codexauth.RefreshResult{AccessToken: "new-access", RefreshToken: nil})

	if merged.AccessToken != "new-access" {
		t.Fatalf("access token = %q, want the freshly-exchanged new-access", merged.AccessToken)
	}
	if merged.RefreshToken != "keep-me" {
		t.Fatalf("refresh token = %q, want the previous keep-me (provider omitted a rotation)", merged.RefreshToken)
	}
}

// TestMergeCodexLoginReplacesRefreshTokenWhenProvided proves the other half: a provider
// that DOES return a rotated refresh token replaces the previous one.
func TestMergeCodexLoginReplacesRefreshTokenWhenProvided(t *testing.T) {
	rotated := "rotated-refresh"
	prev := codexLoginBlob{AccessToken: "old-access", RefreshToken: "old-refresh"}
	merged := mergeCodexLogin(prev, codexauth.RefreshResult{AccessToken: "new-access", RefreshToken: &rotated})

	if merged.AccessToken != "new-access" {
		t.Fatalf("access token = %q, want new-access", merged.AccessToken)
	}
	if merged.RefreshToken != rotated {
		t.Fatalf("refresh token = %q, want the rotated %q", merged.RefreshToken, rotated)
	}
}

// TestCodexRefreshResolution proves the reconcile decision rule: a landed commit
// (generation advanced) → reconciled; a surviving recovery copy at the stranded
// generation → reconciled (recoverable, not total loss); a LIVE, validly-leased in-flight
// rotation under this op → pending (leave rotating, PRD #1147 M4 defect 6); generation
// unchanged with no recovery copy and no live lease → unrecoverable (re-login required).
func TestCodexRefreshResolution(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	op := uuid.New()
	intent := store.CodexRefreshIntent{OperationID: op, FromGeneration: 3}

	// pgUUID mirrors how AcquireCodexRefreshLease stamps coord_operation_id.
	pgUUID := func(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

	t.Run("landed commit reconciles", func(t *testing.T) {
		acct := store.CodexProviderAccount{Generation: 4}
		if got := codexRefreshResolution(now, acct, intent); got != codexIntentReconciled {
			t.Fatalf("resolution = %q, want reconciled (generation advanced)", got)
		}
	})

	t.Run("recovery copy reconciles", func(t *testing.T) {
		acct := store.CodexProviderAccount{
			Generation:         3,
			RecoverySealed:     []byte("sealed"),
			RecoveryGeneration: pgtype.Int8{Int64: 3, Valid: true},
		}
		if got := codexRefreshResolution(now, acct, intent); got != codexIntentReconciled {
			t.Fatalf("resolution = %q, want reconciled (recovery copy survives)", got)
		}
	})

	t.Run("live lease of this op is pending", func(t *testing.T) {
		acct := store.CodexProviderAccount{
			Generation:       3,
			CoordState:       codexCoordInProgress,
			CoordOperationID: pgUUID(op),
			LeaseDeadline:    pgtype.Timestamptz{Time: now.Add(5 * time.Second), Valid: true},
		}
		if got := codexRefreshResolution(now, acct, intent); got != codexIntentPending {
			t.Fatalf("resolution = %q, want pending (live in-flight rotation of this op)", got)
		}
	})

	t.Run("expired lease of this op is unrecoverable", func(t *testing.T) {
		// A lease deadline in the PAST is not a live op (the reap would have quarantined it).
		acct := store.CodexProviderAccount{
			Generation:       3,
			CoordState:       codexCoordInProgress,
			CoordOperationID: pgUUID(op),
			LeaseDeadline:    pgtype.Timestamptz{Time: now.Add(-time.Second), Valid: true},
		}
		if got := codexRefreshResolution(now, acct, intent); got != codexIntentUnrecoverable {
			t.Fatalf("resolution = %q, want unrecoverable (expired lease is not live)", got)
		}
	})

	t.Run("in_progress under a DIFFERENT op is unrecoverable", func(t *testing.T) {
		acct := store.CodexProviderAccount{
			Generation:       3,
			CoordState:       codexCoordInProgress,
			CoordOperationID: pgUUID(uuid.New()), // some other op holds the lease
			LeaseDeadline:    pgtype.Timestamptz{Time: now.Add(5 * time.Second), Valid: true},
		}
		if got := codexRefreshResolution(now, acct, intent); got != codexIntentUnrecoverable {
			t.Fatalf("resolution = %q, want unrecoverable (lease belongs to another op)", got)
		}
	})

	t.Run("no recovery copy is unrecoverable", func(t *testing.T) {
		acct := store.CodexProviderAccount{Generation: 3}
		if got := codexRefreshResolution(now, acct, intent); got != codexIntentUnrecoverable {
			t.Fatalf("resolution = %q, want unrecoverable (no recovery, generation unchanged)", got)
		}
	})

	t.Run("recovery copy at a different generation is unrecoverable", func(t *testing.T) {
		acct := store.CodexProviderAccount{
			Generation:         3,
			RecoverySealed:     []byte("sealed"),
			RecoveryGeneration: pgtype.Int8{Int64: 99, Valid: true},
		}
		if got := codexRefreshResolution(now, acct, intent); got != codexIntentUnrecoverable {
			t.Fatalf("resolution = %q, want unrecoverable (recovery copy is for another generation)", got)
		}
	})
}

// TestCodexReturnCommittedRefusesQuarantined pins the B6 §9 guard: the shared
// replay/reconcile tail must NEVER hand back the account's committed token when the
// account is quarantined (a failed rotation left the sealed_login as the pre-rotation,
// expiry-triggering blob), so a quarantined account surfaces CodexRefreshQuarantined
// instead of a stale/expired token. Deleting the guard makes this test fail (it would
// then fall through to openCodexAccountLogin on a nil vault/store).
func TestCodexReturnCommittedRefusesQuarantined(t *testing.T) {
	s := &Service{}
	acct := store.CodexProviderAccount{CoordState: codexCoordQuarantined, Generation: 2}
	res, err := s.codexReturnCommitted(uuid.New(), acct, CodexRefreshReplayed)
	if !errors.Is(err, ErrCodexRefreshQuarantined) {
		t.Fatalf("err = %v, want ErrCodexRefreshQuarantined", err)
	}
	if res.Outcome != CodexRefreshQuarantined {
		t.Fatalf("outcome = %v, want CodexRefreshQuarantined", res.Outcome)
	}
	if res.AccessToken != "" {
		t.Fatalf("token = %q, want NO token from a quarantined account", res.AccessToken)
	}
}
