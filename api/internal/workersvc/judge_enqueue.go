package workersvc

import (
	"context"
	"errors"
	"log/slog"
	"time"

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
	// Gate 5: per-user spend guards (PRD #69 M5, Decision 9). Count-based, best-effort,
	// applied in EVERY mode (a runaway loop is a footgun even for an opted-in user). On
	// trip: skip silently (like a Gate 3/4 miss), debug-logged — no defer, no queue, no
	// notification. Deliberately FAIL-OPEN, the opposite of Gates 2–4: those gate
	// correctness/consent and fail closed; these are a soft anti-runaway cost backstop,
	// so on ANY read error (settings or query) we do NOT trip the guard — we proceed to
	// enqueue (a query error is logged at warn). A transient DB/settings hiccup must
	// never silently disable judging; the generous defaults keep the guard to genuine
	// runaways.
	if !s.judgeSpendGuardsAllow(ctx, run) {
		return
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

// judgeSpendGuardsAllow is Gate 5 (PRD #69 M5, Decision 9): the per-user, count-based,
// best-effort spend guards. It reports whether the judge may be enqueued — false skips
// it silently (debug-logged). Called in every mode, after the correctness/consent gates
// and before the idempotency insert.
//
// FAIL-OPEN by design (opposite to Gates 2–4): on ANY read error, settings or query, it
// returns true so the enqueue proceeds — the guards are a soft cost backstop, and a
// transient DB/settings hiccup must never silently disable judging. A query error is
// logged at warn; the generous defaults keep the guard to genuine runaways. s.settings
// is non-nil here (Gate 2 returned early otherwise).
func (s *Service) judgeSpendGuardsAllow(ctx context.Context, run store.Run) bool {
	// Cooldown: skip if this user had a judge enqueued within the last N seconds.
	if cooldown, err := s.settings.JudgeCooldownSeconds(ctx); err == nil && cooldown > 0 {
		last, lerr := s.q.LastJudgeEnqueuedAt(ctx, run.UserID)
		if lerr != nil {
			slog.Warn("judge enqueue: cooldown lookup failed, proceeding (fail-open)", "run", run.ID, "user", run.UserID, "error", lerr)
		} else if last.Valid && time.Since(last.Time) < time.Duration(cooldown)*time.Second {
			slog.Debug("judge enqueue: within cooldown, skipping", "run", run.ID, "user", run.UserID, "cooldown_s", cooldown)
			return false
		}
	}
	// Daily budget: skip if this user already had >= N judge runs in the rolling 24h.
	if budget, err := s.settings.JudgeDailyBudget(ctx); err == nil && budget > 0 {
		count, cerr := s.q.CountJudgesSince(ctx, store.CountJudgesSinceParams{
			UserID: run.UserID,
			Since:  pgTime(time.Now().Add(-24 * time.Hour)),
		})
		if cerr != nil {
			slog.Warn("judge enqueue: budget lookup failed, proceeding (fail-open)", "run", run.ID, "user", run.UserID, "error", cerr)
		} else if count >= int64(budget) {
			slog.Debug("judge enqueue: daily budget reached, skipping", "run", run.ID, "user", run.UserID, "budget", budget)
			return false
		}
	}
	return true
}

// judgeRunTitle synthesizes a display title for a judge run (it has no issue of its own).
func judgeRunTitle(target store.Run) string {
	if target.IssueTitle != "" {
		return "Judge: " + target.IssueTitle
	}
	return "Judge of run " + target.ID.String()
}
