package workersvc

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// judgeEligibleKinds is the explicit allowlist of run kinds a judge may review (PRD
// #46 Decision 2). It is an ALLOWLIST, never a denylist: a future kind must opt in
// deliberately, and judge/self_improve/chat stay out (no recursion, no self-feeding
// improvement loop; audit M4).
var judgeEligibleKinds = map[string]bool{RunKindIssue: true, RunKindCIFix: true}

// maybeEnqueueJudgeByID reloads a run by id and runs the judge gate. Used by the
// sweeper, whose swept rows do not carry the full run shape the gate needs.
func (s *Service) maybeEnqueueJudgeByID(ctx context.Context, runID uuid.UUID) {
	run, err := s.q.GetRunByID(ctx, runID)
	if err != nil {
		slog.Warn("judge enqueue: reload swept run", "run", runID, "error", err)
		return
	}
	s.maybeEnqueueJudge(ctx, run)
}

// maybeEnqueueJudge enqueues a judge run for a just-committed terminal run when every
// gate passes (PRD #46 Decision 2). Called at each COMMITTED terminal transition — the
// worker's SetState (rows>0) and the sweeper's terminal sweeps — so a timed-out or
// worker-lost run (exactly the runs worth judging) is covered too, and the lossy
// notify seam is never trusted for a spend decision. It is BEST-EFFORT: it never
// errors the transition that triggered it; every failure is logged and swallowed.
//
// Gates, cheap-first: eligible terminal status (completed|failed only — a
// user-cancelled run is not judged, Decision 2), eligible kind (allowlist — no
// recursion), global judge_enabled, owner opted in, owner has an Anthropic token. The
// one-active-judge-per-target unique index is the final guard: a duplicate raises
// 23505, treated as "already being judged".
func (s *Service) maybeEnqueueJudge(ctx context.Context, run store.Run) {
	// Gate 0: only completed/failed are judged. cancelled and non-terminal never are.
	if run.Status != "completed" && run.Status != "failed" {
		return
	}
	// Gate 1: eligible kind allowlist — never judge/self_improve/chat (no recursion).
	if !judgeEligibleKinds[run.Kind] {
		return
	}
	// Gate 2: global kill-switch. No settings wired ⇒ feature off (tests / dormant).
	if s.settings == nil {
		return
	}
	enabled, err := s.settings.JudgeEnabled(ctx)
	if err != nil {
		slog.Warn("judge enqueue: read judge_enabled", "run", run.ID, "error", err)
		return
	}
	if !enabled {
		return
	}
	// Gate 3: owner opted in (users.judge_enabled) — UNLESS the admin has enforced the
	// judge for every run (PRD #69), in which case the per-user opt-in is bypassed. The
	// enforce read is best-effort: an error reads as false (opt-in still required), so a
	// settings hiccup never forces token spend on. The owner load is still needed for the
	// Gate 4 token check even in enforced mode.
	owner, err := s.q.GetUserByID(ctx, run.UserID)
	if err != nil {
		slog.Warn("judge enqueue: load owner", "run", run.ID, "error", err)
		return
	}
	enforceAll, err := s.settings.JudgeEnforceAll(ctx)
	if err != nil {
		slog.Warn("judge enqueue: read judge_enforce_all", "run", run.ID, "error", err)
		enforceAll = false
	}
	if !enforceAll && !owner.JudgeEnabled {
		return
	}
	// Gate 4: owner has an Anthropic token. Presence only, not decryptability — a
	// locked vault just makes the judge run wait 'queued' like any run.
	if _, err := s.q.GetUserSecretCiphertext(ctx, store.GetUserSecretCiphertextParams{
		UserID: run.UserID,
		Kind:   store.KindAnthropicToken,
	}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("judge enqueue: token presence check", "run", run.ID, "error", err)
		}
		return // no token ⇒ nothing to spend ⇒ no judge run
	}
	// Enqueue. The judge run is owned by the SAME user (never cross-user) and targets
	// this run. A concurrent duplicate trips the one-active-judge-per-target index.
	if _, err := s.q.CreateJudgeRun(ctx, store.CreateJudgeRunParams{
		UserID:           run.UserID,
		TargetRunID:      pgUUID(run.ID),
		IssueTitle:       judgeRunTitle(run),
		IssueDescription: "",
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return // a judge run is already active for this target — expected, not an error
		}
		slog.Warn("judge enqueue: create judge run", "run", run.ID, "error", err)
	}
}

// judgeRunTitle synthesizes a display title for a judge run (it has no issue of its own).
func judgeRunTitle(target store.Run) string {
	if target.IssueTitle != "" {
		return "Judge: " + target.IssueTitle
	}
	return "Judge of run " + target.ID.String()
}
