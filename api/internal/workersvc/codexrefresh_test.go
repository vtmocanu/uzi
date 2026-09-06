package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

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
// generation → reconciled (recoverable, not total loss); generation unchanged with no
// recovery copy → unrecoverable (re-login required).
func TestCodexRefreshResolution(t *testing.T) {
	intent := store.CodexRefreshIntent{FromGeneration: 3}

	t.Run("landed commit reconciles", func(t *testing.T) {
		acct := store.CodexProviderAccount{Generation: 4}
		if got := codexRefreshResolution(acct, intent); got != codexIntentReconciled {
			t.Fatalf("resolution = %q, want reconciled (generation advanced)", got)
		}
	})

	t.Run("recovery copy reconciles", func(t *testing.T) {
		acct := store.CodexProviderAccount{
			Generation:         3,
			RecoverySealed:     []byte("sealed"),
			RecoveryGeneration: pgtype.Int8{Int64: 3, Valid: true},
		}
		if got := codexRefreshResolution(acct, intent); got != codexIntentReconciled {
			t.Fatalf("resolution = %q, want reconciled (recovery copy survives)", got)
		}
	})

	t.Run("no recovery copy is unrecoverable", func(t *testing.T) {
		acct := store.CodexProviderAccount{Generation: 3}
		if got := codexRefreshResolution(acct, intent); got != codexIntentUnrecoverable {
			t.Fatalf("resolution = %q, want unrecoverable (no recovery, generation unchanged)", got)
		}
	})

	t.Run("recovery copy at a different generation is unrecoverable", func(t *testing.T) {
		acct := store.CodexProviderAccount{
			Generation:         3,
			RecoverySealed:     []byte("sealed"),
			RecoveryGeneration: pgtype.Int8{Int64: 99, Valid: true},
		}
		if got := codexRefreshResolution(acct, intent); got != codexIntentUnrecoverable {
			t.Fatalf("resolution = %q, want unrecoverable (recovery copy is for another generation)", got)
		}
	})
}
