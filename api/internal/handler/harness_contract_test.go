package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// harness_contract_test.go is the PRD #1429 (M1 / D2, D5) POSITIVE acceptance for the harness
// seam, replacing M5A's dark unknown-field NEGATIVES (which asserted a public creation body
// carrying `harness` was rejected at decode). M1 builds the seam but deliberately does NOT wire
// the production handlers to accept `harness` — that is M2/M3 — so the contract this file pins
// now is the #1247 EFFECTIVE-HARNESS override rule (D5), the one exported seam meaningfully
// testable at this layer: workersvc.ResolveCredentialOverride REFUSES an Anthropic override when
// the effective harness is Codex (a typed 422) and ACCEPTS it when the effective harness is
// Claude. "Effective harness" is the exact D11 result the create transaction resolves —
// createRunAtomic hands the insert closure that harness, so the M2 handler validates the override
// against it, NOT against users.default_harness. So the two #1247 cases (an explicit-Claude
// request that has a Codex default; an unusable-Codex-default that falls through to Claude) both
// reduce to "effective harness = claude → accept" here.
//
// The resolver's enum-acceptance / explicit-unavailable(no_credential_for_harness) /
// implicit-fallback matrix and the atomic freeze/rollback are proven end to end against a real
// Postgres in workersvc's create_run_atomic_livedb_test.go + harness_resolver_livedb_test.go.

// overrideContractDB answers ONLY GetUserSecretMetaByIDOfKind (the owner-scoped pinned-token
// lookup ResolveCredentialOverride makes for a pinned request) as a HIT, so a pinned Anthropic
// override on a Claude harness resolves to a real override. Any other query is a test failure —
// it would mean the validator advanced somewhere these tests do not probe.
type overrideContractDB struct{ t *testing.T }

func (overrideContractDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (overrideContractDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}

func (d overrideContractDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "name: GetUserSecretMetaByIDOfKind") {
		// A token the caller owns of kind anthropic_token: err==nil (the row is discarded by
		// ResolveCredentialOverride, which only distinguishes found vs pgx.ErrNoRows).
		return fakeScanRow{func(...any) error { return nil }}
	}
	d.t.Errorf("unexpected query reached: %s", sql)
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

func overrideContractService(t *testing.T) *workersvc.Service {
	return workersvc.New(store.New(overrideContractDB{t: t}), nil, workersvc.Params{})
}

// TestResolveCredentialOverrideCodexRefused: an effective Codex harness refuses an Anthropic
// override with the typed sentinel the handler maps to 422 (D5/D9), for EVERY mode — the harness
// check precedes the per-mode secret lookup, so even inherit or auto on a Codex run is refused
// rather than silently clearing/accepting. Codex is a separate runtime and no cross-harness
// resume exists, so an Anthropic-only override on a Codex run is an explicit error, not a no-op.
func TestResolveCredentialOverrideCodexRefused(t *testing.T) {
	svc := overrideContractService(t)
	userID := uuid.New()
	secretID := uuid.New()

	for _, mode := range []string{
		workersvc.CredentialOverrideModePinned,
		workersvc.CredentialOverrideModeInherit,
		workersvc.CredentialOverrideModeAuto,
		workersvc.CredentialOverrideModeDefault,
	} {
		_, err := svc.ResolveCredentialOverride(context.Background(), userID, "issue", string(workersvc.HarnessCodex), mode, &secretID)
		if !errors.Is(err, workersvc.ErrCredentialOverrideHarnessUnsupported) {
			t.Fatalf("mode %q on codex harness: err = %v, want ErrCredentialOverrideHarnessUnsupported (422)", mode, err)
		}
	}
}

// TestResolveCredentialOverrideClaudeAccepts pins the #1247 effective-harness cases (D5): when
// the D11 result is Claude, a valid Anthropic override is accepted — a pinned override resolves
// to the owned token, auto/default resolve to their mode, and inherit clears to nil. This is the
// exact acceptance an explicit-Claude-with-a-Codex-default request and an unusable-Codex-default
// falling through to Claude both get, because the seam feeds ResolveCredentialOverride the
// resolved harness rather than the stored default.
func TestResolveCredentialOverrideClaudeAccepts(t *testing.T) {
	svc := overrideContractService(t)
	userID := uuid.New()
	secretID := uuid.New()
	claude := string(workersvc.HarnessClaude)

	// pinned: the owned anthropic token resolves to a pinned override.
	ov, err := svc.ResolveCredentialOverride(context.Background(), userID, "issue", claude, workersvc.CredentialOverrideModePinned, &secretID)
	if err != nil {
		t.Fatalf("pinned override on claude harness: %v", err)
	}
	if ov == nil || ov.Mode != workersvc.CredentialOverrideModePinned || ov.SecretID == nil || *ov.SecretID != secretID {
		t.Fatalf("pinned override = %+v, want {pinned, %s}", ov, secretID)
	}

	// auto: accepted, no secret lookup.
	ov, err = svc.ResolveCredentialOverride(context.Background(), userID, "issue", claude, workersvc.CredentialOverrideModeAuto, nil)
	if err != nil {
		t.Fatalf("auto override on claude harness: %v", err)
	}
	if ov == nil || ov.Mode != workersvc.CredentialOverrideModeAuto {
		t.Fatalf("auto override = %+v, want {auto}", ov)
	}

	// inherit: accepted, and clears both columns (a nil override).
	ov, err = svc.ResolveCredentialOverride(context.Background(), userID, "issue", claude, workersvc.CredentialOverrideModeInherit, nil)
	if err != nil {
		t.Fatalf("inherit override on claude harness: %v", err)
	}
	if ov != nil {
		t.Fatalf("inherit override = %+v, want nil (inherit clears both columns)", ov)
	}
}
