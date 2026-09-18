package workersvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// pgOverrideMode / pgOverrideSecretID convert a *CredentialOverride (nil = inherit) to
// the two nullable override columns the Create*Run inserts and the schedule producers
// write (PRD #1247). A nil override, or an override with an empty mode, yields NULL/NULL
// — byte-identical to a pre-#1247 run.
func pgOverrideMode(o *CredentialOverride) pgtype.Text {
	if o == nil {
		return pgtype.Text{}
	}
	return pgconv.TextOrNull(o.Mode)
}

func pgOverrideSecretID(o *CredentialOverride) pgtype.UUID {
	if o == nil {
		return pgtype.UUID{}
	}
	return pgconv.UUIDPtr(o.SecretID)
}

// CredentialOverride is the per-run Anthropic credential choice (PRD #1247 M1, D1),
// threaded through the Create*Run methods and (in M4/M6) written by the switch verb and
// the schedule producers. A nil *CredentialOverride means INHERIT the worker binding —
// today's behaviour, back-compat by construction — so every M1 caller passes nil and
// behaviour is byte-identical until M2/M6 wire real user input.
//
// Mode is one of the three stored modes (pinned/auto/default), mirroring migration
// 00233's runs.credential_override_mode CHECK; SecretID is set ONLY for a pinned
// override and is the caller's own anthropic_token id. "inherit" is not a stored mode —
// it is expressed as a nil *CredentialOverride (both columns NULL).
type CredentialOverride struct {
	Mode     string
	SecretID *uuid.UUID
}

// The credential-override request modes accepted by the one validator (PRD #1247, D5).
// The first three are the stored modes; "inherit" clears both columns (a nil override).
const (
	CredentialOverrideModePinned  = BindModePinned  // "pinned"
	CredentialOverrideModeAuto    = BindModeAuto    // "auto"
	CredentialOverrideModeDefault = BindModeDefault // "default"
	CredentialOverrideModeInherit = "inherit"
)

// The typed refusals the one validator returns, which the handler layer (M2/M4/M6) maps
// to HTTP statuses. They are the D6/D9/D10 "refuse on a fact" set:
//
//   - ErrCredentialOverrideSecretNotFound → 404: a pinned override whose secret id is not
//     the caller's own user_secrets row of kind anthropic_token (foreign, deleted, or
//     wrong kind). Same shape as a foreign token id anywhere else.
//   - ErrCredentialOverrideLaneNotSwitchable → 409: a chat/judge/self_improve run (D10) —
//     their ladders would ignore an override, so a silently accepted write would
//     misreport intent.
//   - ErrCredentialOverrideHarnessUnsupported → 422: a run/schedule whose effective
//     harness is codex (D9) — the switch keeps the harness, and Codex is a separate
//     runtime; the unsupported case is an explicit error, not a silent no-op.
//   - ErrCredentialOverridePinnedNeedsSecret → 400: a pinned mode with no secret id.
//   - ErrCredentialOverrideInvalidMode → 400: a mode outside the closed set.
var (
	ErrCredentialOverrideSecretNotFound     = errors.New("credential override token not found")
	ErrCredentialOverrideLaneNotSwitchable  = errors.New("credential override lane not switchable")
	ErrCredentialOverrideHarnessUnsupported = errors.New("credential override unsupported on codex harness")
	ErrCredentialOverridePinnedNeedsSecret  = errors.New("credential override pinned mode requires a token")
	ErrCredentialOverrideInvalidMode        = errors.New("invalid credential override mode")
)

// ResolveCredentialOverride is the EXPORTED entry point every override write outside this
// package runs through (PRD #1247 M2): the handler (a different package) has no access to
// the unexported validateCredentialOverride, so this thin wrapper is its door. It performs
// no work of its own beyond delegating — the validation, the D9/D10 refusals and the
// column resolution all live in the one validator — and returns the same resolved
// *CredentialOverride (nil = inherit) or one of the exported typed refusals the handler
// maps to HTTP statuses (404/409/422/400). Wired by CreateRun in M2 and by run set-token /
// schedule create-edit in M4/M6.
func (s *Service) ResolveCredentialOverride(ctx context.Context, userID uuid.UUID, kind, harness, mode string, secretID *uuid.UUID) (*CredentialOverride, error) {
	return s.validateCredentialOverride(ctx, userID, kind, harness, mode, secretID)
}

// validateCredentialOverride is the ONE validator every override write runs through
// (PRD #1247 M1) — at run create, run approve --token, run set-token, and schedule
// create/edit (the handlers wire it in M2/M4/M6; M1 provides the function and its
// tests). It answers "may this run/schedule carry this override, and what columns does
// it write?" and returns the resolved *CredentialOverride (nil = inherit ⇒ clear both
// columns) or one of the typed refusals above.
//
// kind is the run/schedule LANE, harness its effective harness (runs.harness /
// run_schedules.harness, else users.default_harness — the caller resolves it), mode the
// requested mode ("pinned"/"auto"/"default"/"inherit"), and secretID the pinned target.
//
// Check order — lane (409) → harness (422) → mode (404/400) — is deliberate: a lane that
// is fundamentally not switchable, or a harness the switch cannot serve, is refused
// before the per-mode secret lookup, so even an `inherit` on a judge lane or a codex run
// is refused rather than silently clearing (D10/D9). The 404 secret check runs only for
// a pinned request, the only mode that names a credential.
func (s *Service) validateCredentialOverride(ctx context.Context, userID uuid.UUID, kind, harness, mode string, secretID *uuid.UUID) (*CredentialOverride, error) {
	// D10: chat, judge and self_improve follow their own ladders and are not switchable.
	switch kind {
	case runkind.Chat, runkind.Judge, runkind.SelfImprove:
		return nil, fmt.Errorf("%w: %s", ErrCredentialOverrideLaneNotSwitchable, kind)
	}
	// D9: a Codex run/schedule cannot carry an Anthropic-only override; the switch keeps
	// the harness and no cross-harness resume exists (a later PRD lifts this).
	if harness == harnessCodex {
		return nil, ErrCredentialOverrideHarnessUnsupported
	}
	switch mode {
	case CredentialOverrideModeInherit:
		// Clears both columns (D5). A nil override is exactly that: NULL mode, NULL id.
		return nil, nil
	case CredentialOverrideModeAuto:
		return &CredentialOverride{Mode: CredentialOverrideModeAuto}, nil
	case CredentialOverrideModeDefault:
		return &CredentialOverride{Mode: CredentialOverrideModeDefault}, nil
	case CredentialOverrideModePinned:
		if secretID == nil {
			return nil, ErrCredentialOverridePinnedNeedsSecret
		}
		// 404: the secret must be the caller's OWN anthropic_token. The kind-scoped
		// owner-scoped lookup returns pgx.ErrNoRows for a foreign id, a deleted id, or a
		// wrong-kind id alike — the same "unavailable" fact a foreign id produces at open.
		if _, err := s.q.GetUserSecretMetaByIDOfKind(ctx, store.GetUserSecretMetaByIDOfKindParams{
			ID:     *secretID,
			UserID: userID,
			Kind:   store.KindAnthropicToken,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrCredentialOverrideSecretNotFound
			}
			return nil, fmt.Errorf("credential override token lookup: %w", err)
		}
		id := *secretID
		return &CredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &id}, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrCredentialOverrideInvalidMode, mode)
	}
}
