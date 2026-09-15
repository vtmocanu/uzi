package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// run_credential.go is the service side of `uzi run set-token` for the NON-HELD run
// states (PRD #1247 M4, D4/D6/D12): it writes the per-run credential override and, for a
// parked run, performs the state-specific early promote so the next claim spends the
// chosen token. The HELD-state switch protocol (awaiting_*/running) is M5 — this file
// refuses those with a typed 409 and writes NOTHING; M5 replaces that branch with the
// real quiesce → release → reclaim protocol and the credential_switch capability check.

// The typed refusals SetRunCredential returns for a run whose CURRENT state cannot take a
// parked-state switch. The handler maps each to 409 with a state-specific message; they
// are distinct sentinels so the handler (and M5) can tell them apart:
//
//   - ErrCredentialSwitchHeldStateUnsupported → a worker holds the session
//     (awaiting_approval/awaiting_input/awaiting_followup/running). M4 refuses and does
//     NOT write the override; M5 replaces this branch with the held-state switch protocol.
//   - ErrCredentialSwitchClaimAssembling → the run is `claimed`; the claim payload is being
//     assembled, so retry in a moment.
//   - ErrCredentialSwitchRunTerminal → the run is completed/failed/cancelled.
//   - ErrCredentialSwitchRaced → the run moved out of the parked state between the read and
//     the early-promote write (a concurrent claim/cancel), so the promote found 0 rows.
var (
	ErrCredentialSwitchHeldStateUnsupported = errors.New("credential switch not supported in a held state")
	ErrCredentialSwitchClaimAssembling      = errors.New("credential switch: claim being assembled")
	ErrCredentialSwitchRunTerminal          = errors.New("credential switch: run has finished")
	ErrCredentialSwitchRaced                = errors.New("credential switch: run state changed, retry")
)

// SetRunCredentialResult is what SetRunCredential returns on success: the re-read run (so
// the caller renders the resumed status + the written override) and a best-effort D6
// warning string, empty when there is nothing to warn about. The warning NEVER refuses the
// verb — D6 warns, never blocks, on a headroom concern.
type SetRunCredentialResult struct {
	Run     store.Run
	Warning string
}

// SetRunCredential is the `uzi run set-token` verb for the NON-HELD states (PRD #1247 M4,
// D4). It reads the run owner-scoped (a missing/foreign run is ErrRunNotFound → 404),
// validates the requested override through the ONE validator (typed 404/409/422/400), then
// dispatches on the run's CURRENT status:
//
//   - queued / limit_wait / pool_wait / recovery_wait / paused: write the override columns
//     FIRST (idempotent), then the state-specific transition (an early promote for a parked
//     run; nothing more for queued). A 0-row promote (a raced claim/cancel) is
//     ErrCredentialSwitchRaced → 409.
//   - awaiting_approval / awaiting_input / awaiting_followup / running: a worker holds the
//     session → ErrCredentialSwitchHeldStateUnsupported, and NO override is written (M5's
//     switch protocol owns that write).
//   - claimed: ErrCredentialSwitchClaimAssembling → 409.
//   - completed / failed / cancelled: ErrCredentialSwitchRunTerminal → 409.
//
// It returns the re-read run plus the D6 warning. Validation runs BEFORE the status
// dispatch, so an invalid override on any run (held or terminal included) is refused with
// the validator's error and nothing is written — the validator itself performs no write.
func (s *Service) SetRunCredential(ctx context.Context, userID, runID uuid.UUID, mode string, secretID *uuid.UUID) (SetRunCredentialResult, error) {
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SetRunCredentialResult{}, ErrRunNotFound
		}
		return SetRunCredentialResult{}, fmt.Errorf("set run credential: read run: %w", err)
	}

	// The effective harness of THIS run: runs.harness is NOT NULL DEFAULT 'claude'
	// (migration 00226), so a persisted run always carries its authoritative harness and no
	// users.default_harness fallback is needed here (that fallback is the create case, where
	// no run row exists yet — the handler's createRunEffectiveHarness). An empty value is
	// defended as claude, the safe direction (only a codex harness is refused, D9).
	harness := run.Harness
	if harness == "" {
		harness = harnessClaude
	}

	// The ONE validator (D5/D9/D10): lane (409) → harness (422) → mode (404/400). It never
	// writes, so validating before the status dispatch is safe; a refusal returns a typed
	// error the handler maps to a status and no override is written.
	resolved, err := s.validateCredentialOverride(ctx, userID, run.Kind, harness, mode, secretID)
	if err != nil {
		return SetRunCredentialResult{}, err
	}

	switch run.Status {
	case "queued", "limit_wait", "pool_wait", "recovery_wait", "paused":
		// Write the override columns FIRST (idempotent, harmless if the transition then
		// finds 0 rows), then the state-specific transition.
		if _, werr := s.q.SetRunCredentialOverride(ctx, store.SetRunCredentialOverrideParams{
			Mode:     pgOverrideMode(resolved),
			SecretID: pgOverrideSecretID(resolved),
			ID:       runID,
			UserID:   userID,
		}); werr != nil {
			return SetRunCredentialResult{}, fmt.Errorf("set run credential override: %w", werr)
		}
		if terr := s.applyCredentialTransition(ctx, userID, runID, run.Status); terr != nil {
			return SetRunCredentialResult{}, terr
		}
	case "awaiting_approval", "awaiting_input", "awaiting_followup", "running":
		// M5 replaces this branch with the held-state switch protocol; M4 refuses and
		// writes nothing.
		return SetRunCredentialResult{}, ErrCredentialSwitchHeldStateUnsupported
	case "claimed":
		return SetRunCredentialResult{}, ErrCredentialSwitchClaimAssembling
	default:
		// completed / failed / cancelled, and any status not otherwise handled.
		return SetRunCredentialResult{}, ErrCredentialSwitchRunTerminal
	}

	// D6 warnings (best-effort): computed from the PRE-transition run row —
	// limit_dead_secret_id and the retry cadence are preserved through the promote, so the
	// pre-transition row carries them. A warning-computation read error is logged and
	// dropped, never fails the verb.
	warning := s.credentialSwitchWarning(ctx, run, resolved)

	updated, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: userID})
	if err != nil {
		return SetRunCredentialResult{}, fmt.Errorf("set run credential: re-read run: %w", err)
	}
	return SetRunCredentialResult{Run: updated, Warning: warning}, nil
}

// applyCredentialTransition performs the state-specific transition after the override
// columns are written (PRD #1247 M4, D4). queued does nothing more; each parked state runs
// its own owner-scoped promote (the limit_wait/recovery_wait early promotes return the run
// to queued WITHOUT waiting for its retry window; pool_wait uses the existing
// PromotePoolWaitRun; paused uses ResumePausedRun, which banks the budget and keeps the
// wall). A 0-row promote is a raced claim/cancel → ErrCredentialSwitchRaced (409).
//
// Each of the four promote transitions writes a kind='resume' audit row exactly as
// ResumeRunNow does — a server-only kind excluded from ConsumeRunInputs, so the worker
// never drains it. 'set_token' is deliberately NOT reused as a new kind: it is absent from
// the run_inputs kind CHECK, so a distinct audit kind would need a migration (out of M4's
// scope), and 'resume' already means exactly "this run was moved back to queued". The
// queued branch performs no transition, so it writes no audit row.
func (s *Service) applyCredentialTransition(ctx context.Context, userID, runID uuid.UUID, status string) error {
	switch status {
	case "queued":
		// The next claim honours the override; nothing else to do.
		return nil
	case "limit_wait":
		rows, err := s.q.PromoteLimitWaitRunNow(ctx, store.PromoteLimitWaitRunNowParams{ID: runID, UserID: userID})
		if err != nil {
			return fmt.Errorf("promote limit_wait run: %w", err)
		}
		if rows == 0 {
			return ErrCredentialSwitchRaced
		}
	case "pool_wait":
		rows, err := s.q.PromotePoolWaitRun(ctx, store.PromotePoolWaitRunParams{ID: runID, UserID: userID})
		if err != nil {
			return fmt.Errorf("promote pool_wait run: %w", err)
		}
		if rows == 0 {
			return ErrCredentialSwitchRaced
		}
	case "recovery_wait":
		rows, err := s.q.PromoteRecoveryWaitRunNow(ctx, store.PromoteRecoveryWaitRunNowParams{ID: runID, UserID: userID})
		if err != nil {
			return fmt.Errorf("promote recovery_wait run: %w", err)
		}
		if rows == 0 {
			return ErrCredentialSwitchRaced
		}
	case "paused":
		if _, err := s.q.ResumePausedRun(ctx, store.ResumePausedRunParams{ID: runID, UserID: userID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCredentialSwitchRaced
			}
			return fmt.Errorf("resume paused run: %w", err)
		}
	}
	// Best-effort audit row for the four promote transitions (the transition already
	// committed, so a failed audit write must not fail the verb).
	if _, err := s.q.CreateRunInput(ctx, store.CreateRunInputParams{RunID: runID, Kind: "resume", Body: pgtype.Text{}}); err != nil {
		slog.Warn("set run credential: write resume audit row", "run", runID, "error", err)
	}
	return nil
}

// credentialSwitchWarning computes the D6 warning for a set-token that would leave the run
// with a headroom problem (PRD #1247 M4, D6). It NEVER refuses — a warning is returned on a
// 200 and the CLI/web surface it — and it is best-effort: a candidate-list read error is
// logged and dropped (empty warning). Three cases:
//
//  1. Pinning the run's own just-exhausted dead credential (limit_dead_secret_id).
//  2. A pinned token whose usage gauge reads exhausted or stale. A pin need not be pooled,
//     so the pinned token is classified AS IF pooled (AutoEligible forced) to read its
//     gauge alone: only StatusEligible is warning-free.
//  3. auto when the pool minus the dead token is empty (Floor not ok) → the run will hold
//     in pool_wait.
//
// An inherit (nil override) or a default mode carries no per-token headroom concern here.
func (s *Service) credentialSwitchWarning(ctx context.Context, run store.Run, resolved *CredentialOverride) string {
	if resolved == nil {
		return ""
	}
	switch resolved.Mode {
	case BindModePinned:
		if resolved.SecretID == nil {
			return ""
		}
		pinned := *resolved.SecretID
		// (1) The run's just-exhausted dead token — it may re-park while its window is closed.
		if run.LimitDeadSecretID.Valid && uuid.UUID(run.LimitDeadSecretID.Bytes) == pinned {
			return "this token is the one this run just exhausted; it may re-park until its usage window reopens"
		}
		// (2) A pinned token whose gauge reads exhausted or stale.
		rows, err := s.q.ListAutoSelectCandidates(ctx, run.UserID)
		if err != nil {
			slog.Error("set run credential: warning candidate read", "run", run.ID, "error", err)
			return ""
		}
		for _, row := range rows {
			c := autoselectrow.FromCandidateRow(row)
			if c.SecretID != pinned {
				continue
			}
			// A pin does not require pooling, so classify it as if pooled to isolate the
			// gauge signal: stale / no-reading / unmeasured / below-threshold all warn;
			// only a fresh, measurable, in-headroom gauge (StatusEligible) is silent.
			c.AutoEligible = true
			if autoselect.Classify(c, s.p.Autoselect, s.now()).Status != autoselect.StatusEligible {
				return "this token's usage gauge reads low or stale; the run may re-park if the token is exhausted"
			}
			return ""
		}
		return ""
	case BindModeAuto:
		// (3) auto with an empty pool-minus-dead-token → the run holds in pool_wait.
		rows, err := s.q.ListAutoSelectCandidates(ctx, run.UserID)
		if err != nil {
			slog.Error("set run credential: warning candidate read", "run", run.ID, "error", err)
			return ""
		}
		cands := make([]autoselect.Candidate, 0, len(rows))
		for _, row := range rows {
			cands = append(cands, autoselectrow.FromCandidateRow(row))
		}
		if _, ok := autoselect.Floor(cands, s.claimExclude(run), s.now()); !ok {
			return "auto has no pooled token with headroom right now; the run will hold in pool_wait until one is available"
		}
		return ""
	default:
		return ""
	}
}
