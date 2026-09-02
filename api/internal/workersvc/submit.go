package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// SubmitInput records a steering input (approve/reject/follow-up/cancel) for a
// run the user owns. When the target is a cancel or plan rejection and no live
// poller exists (the run is still queued, or its worker has gone stale), the
// transition is applied server-side so the input is never stranded waiting for a
// GET /inputs poll that will never come. Otherwise the input is enqueued for the
// worker to consume.
//
// sel is the PRD #37 agent selection and is legal ONLY on approve_plan (nil
// everywhere else, including the Slack approve path, which offers no picker — such
// a run keeps whatever default its worker resolves). It is validated against the
// run's actual roster, then persisted to the run row and JSON-encoded into the
// input body in one statement.
//
// approve_plan deliberately has no server-side no-poller branch: a run can only be
// awaiting approval because a live worker put it there, so an approve with no
// poller is a race the worker's own gate timeout resolves. Only cancel/reject_plan
// need the branch below.
//
// The capability approval gate (PRD #84 M4 4c) is ENFORCED here — use
// SubmitInputWithCapabilityOverride for the owner "run without the capability" override.
func (s *Service) SubmitInput(ctx context.Context, userID, runID uuid.UUID, kind, body string, sel *AgentSelection) (SubmitInputResult, error) {
	return s.submitInput(ctx, userID, runID, kind, body, sel, false)
}

// SubmitInputWithCapabilityOverride is SubmitInput for the PRD #84 M4 4c owner override
// ("run without the capability", Decision 12). It BYPASSES the capability approval gate and
// clears the run's inferred/hinted required_capabilities — but ATOMICALLY with a successful
// approve: the clear runs ONLY after the approve's own validation (selection roster check)
// and enqueue succeed, so a FAILED approve (e.g. an invalid agent selection) leaves
// required_capabilities INTACT and the retry stays gated. This closes the non-atomic drop
// the old handler-side pre-clear had, where a failed approve permanently dropped the
// requirement. The override is meaningful only for approve_plan; on any other kind the gate
// never runs, so the flag is inert.
func (s *Service) SubmitInputWithCapabilityOverride(ctx context.Context, userID, runID uuid.UUID, kind, body string, sel *AgentSelection) (SubmitInputResult, error) {
	return s.submitInput(ctx, userID, runID, kind, body, sel, true)
}

func (s *Service) submitInput(ctx context.Context, userID, runID uuid.UUID, kind, body string, sel *AgentSelection, overrideCapabilities bool) (SubmitInputResult, error) {
	run, err := s.GetRun(ctx, userID, runID)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if terminalStatuses[run.Status] {
		return SubmitInputResult{}, ErrRunTerminal
	}
	// A chat run's turns MUST ride SubmitChatMessage, which counts persisted
	// follow_ups against ChatMaxTurns before enqueuing (chat.go). A follow_up posted
	// to the generic /inputs endpoint would skip that count and burn spend past the
	// cap, so reject it here — at the service boundary, before any row is written, so
	// the guard covers HTTP/CLI/future Slack. Only follow_up is blocked: cancel (which
	// EndChat rides), reject_plan, approve_plan and answer stay legal on a chat run.
	if run.Kind == RunKindChat && kind == "follow_up" {
		return SubmitInputResult{}, ErrChatInputNotAllowed
	}
	if sel != nil && kind != "approve_plan" {
		return SubmitInputResult{}, fmt.Errorf("%w: an agent selection is only valid when approving a plan", ErrInvalidSelection)
	}
	// PRD #84 M4 4c: the AUTHORITATIVE capability approval gate runs here for EVERY
	// approve_plan — both the selection-bearing dispatch and the nil-selection plain-enqueue
	// path — so neither can approve a plan onto a worker that cannot run it. See
	// capabilityGate.
	//
	// The owner OVERRIDE ("run without the capability", Decision 12) instead BYPASSES the
	// gate and clears the run's required_capabilities — but the clear is ATOMIC with a
	// successful approve: it runs ONLY after the approve's own validation (submitApproval's
	// roster check) and enqueue have succeeded, so a FAILED approve (e.g. an invalid
	// selection) leaves the requirement INTACT and the retry stays gated. Doing the clear
	// here, after the enqueue, rather than in the handler BEFORE this call, is the fix for
	// the non-atomic drop.
	if kind == "approve_plan" {
		if !overrideCapabilities {
			if err := s.capabilityGate(ctx, run); err != nil {
				return SubmitInputResult{}, err
			}
		}
		var res SubmitInputResult
		if sel != nil {
			res, err = s.submitApproval(ctx, run, *sel)
		} else {
			res, err = s.enqueueRunInput(ctx, runID, kind, body)
		}
		if err != nil {
			return SubmitInputResult{}, err
		}
		if overrideCapabilities {
			// Only reached once the approve fully succeeded, so a failed approve above never
			// clears the requirement. Owner- and awaiting_approval-scoped in SQL.
			if err := s.OverrideRunRequiredCapabilities(ctx, userID, runID); err != nil {
				return SubmitInputResult{}, err
			}
		}
		return res, nil
	}

	// An `answer` resolves the clarification question the run is CURRENTLY parked on
	// (PRD #88 M1). Unlike every other kind it is rejected outright unless the run is
	// actually asking: SubmitInput otherwise accepts any non-terminal run, so an
	// answer posted before any ask_user would be enqueued, consumed by the steering
	// poll, and auto-resolve the first question the moment it opened — the user would
	// never see the question, and the feed would show it answered by text written
	// before it existed. The run row is already loaded here, so the guard is free.
	//
	// It is belt-and-braces rather than the primary control (the identity check below
	// independently rejects an answer that does not name the open question), but it
	// rejects at the earliest point and with an error the caller can act on.
	if kind == "answer" {
		return s.submitAnswer(ctx, run, body)
	}

	// PRD #634 M2: decode the run's milestone facts once, shared by the `scope` branch and by
	// the milestone-run `stop` remap below. `run` is a GetRun SELECT *, so both jsonb columns
	// are populated; a decode error degrades to an empty slice (len 0), which correctly makes
	// milestoneIssueRun false rather than failing the input. len(frozen) is the ceiling upper
	// bound; len(completed) the current floor (already-done milestones can never be un-done).
	frozen, _ := DecodeMilestones(run.MilestonesFrozen)
	completed, _ := DecodeMilestoneIDs(run.MilestonesCompleted)
	milestoneIssueRun := run.Kind == RunKindIssue && len(frozen) > 0

	// A `scope` directive (PRD #634 M2) bounds how many of the run's frozen milestones it may
	// complete. It writes runs.scope_ceiling (the control the worker honors on its ACK) plus a
	// kind='scope' audit row in one statement, and NEVER a stop_kind — the scope-capped
	// disposition is stamped at finalize by the worker (m3/m4). The desired ceiling is clamped
	// into [len(completed), len(frozen)] and reported back (never rejected for range), so an
	// operator can only ever bound the run between "nothing further" and "the whole list".
	if kind == "scope" {
		if !milestoneIssueRun {
			return SubmitInputResult{}, ErrScopeNotMilestoneRun
		}
		n, err := strconv.Atoi(strings.TrimSpace(body))
		if err != nil {
			return SubmitInputResult{}, ErrInvalidScopeCeiling
		}
		ceiling := clampInt(n, len(completed), len(frozen))
		auditBody := fmt.Sprintf("scope ceiling → complete through milestone %d of %d", ceiling, len(frozen))
		if ceiling != n {
			auditBody += fmt.Sprintf(" (clamped from %d)", n)
		}
		return s.submitScopeCeiling(ctx, run, ceiling, auditBody)
	}

	// A graceful `stop` (PRD #517 M4) is the interactive-run wind-down: unlike cancel/
	// reject_plan it has NO server-side !live transition branch, because only the worker can
	// finalize it (push + open MR iff open_mr) and report `completed` with stop_kind='stopped'.
	// So a stop ALWAYS enqueues via CreateStopVerdictInput, stamping stop_kind='stopped' in the
	// same statement. A live parked/running worker consumes it and finalizes; a dead-worker
	// parked run is requeued by M2's stale-heartbeat sweep (awaiting_followup is in
	// RequeueRunsOfStaleWorkers) and honors the pending stop on resume. The terminal guard at
	// the top of SubmitInput already 409s a stop on a finished run. Never routes through
	// CancelRunServerSide/RejectRunServerSide.
	//
	// stop_reason carries the operator's OPTIONAL message (like a cancel's — a stop reason is
	// helpful, not mandatory); the same message is co-written to run_user_inputs.body, NUL-
	// stripped to avoid Postgres 22021 aborting the CTE.
	if kind == "stop" {
		// PRD #634 M2: on a milestone-structured issue run a `stop` means "finalize what is
		// already complete, start no further milestone" — which is exactly a scope write with
		// ceiling = len(completed). Map it to a scope write BEFORE the interactive-task guard
		// below, so a stop on an issue run is accepted rather than 409'd. It does NOT go through
		// CreateStopVerdictInput (no stop_kind='stopped') — the scope-capped disposition is
		// stamped at finalize by the worker in m3, not here.
		if milestoneIssueRun {
			reason, _ := stripNUL(body)
			reason = strings.TrimSpace(reason)
			auditBody := fmt.Sprintf("stop → finalize %d completed milestone(s), start no further", len(completed))
			if reason != "" {
				auditBody += ": " + reason
			}
			return s.submitScopeCeiling(ctx, run, len(completed), auditBody)
		}
		// Only an interactive task run has a park that reads the stop flag, so a stop is
		// meaningful ONLY there. Reject it on any other run BEFORE stamping, so a
		// non-interactive-task / chat / plan-gated run cannot acquire a spurious permanent
		// stop_kind='stopped' and return a misleading success. `run` came from GetRun (a
		// SELECT *), so Kind and Interactive are populated. The guard is on kind+interactive
		// only — a RUNNING interactive task (not yet parked) is still a legal stop target.
		// The owner-scope (GetRun→404) and terminal (ErrRunTerminal→409) guards above run
		// first and are unchanged.
		if run.Kind != RunKindTask || !run.Interactive {
			return SubmitInputResult{}, ErrStopNotInteractive
		}
		cleanBody, _ := stripNUL(body)
		if _, err := s.q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
			RunID: runID, Kind: kind, Body: pgconv.TextOrNull(cleanBody), StopKind: pgconv.TextOrNull(stopKindFor(kind)), StopReason: stopReasonParam(body),
		}); err != nil {
			return SubmitInputResult{}, err
		}
		return SubmitInputResult{ServerSide: false}, nil
	}

	if kind == "cancel" || kind == "reject_plan" {
		live, err := s.hasLivePoller(ctx, run)
		if err != nil {
			return SubmitInputResult{}, err
		}
		if !live {
			status := "cancelled"
			if kind == "cancel" {
				// PRD #503 M3: persist the operator's OPTIONAL cancel reason; an empty
				// body stores NULL (a cancel reason is helpful, not mandatory).
				_, err = s.q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{
					ID: runID, UserID: userID, StopReason: stopReasonParam(body),
				})
			} else {
				status = "failed"
				// PRD #503 M2 — persist the operator's reject reason as failure_reason
				// instead of the hardcoded literal; the CLI now requires it, but keep a
				// fallback for non-CLI callers that may still send an empty body. Sanitize
				// like the worker-reported failure_reason (strip NUL — a NUL in a text column
				// raises 22021 and would abort the reject — then cap length).
				reason, _ := stripNUL(body)
				reason = strings.TrimSpace(reason)
				if reason == "" {
					reason = "plan rejected"
				}
				reason = truncateRunes(reason, maxFailureReasonRunes)
				_, err = s.q.RejectRunServerSide(ctx, store.RejectRunServerSideParams{
					ID: runID, UserID: userID, FailureReason: pgconv.TextOrNull(reason),
				})
			}
			if err != nil {
				return SubmitInputResult{}, err
			}
			if s.bcast != nil {
				s.bcast.PublishState(runID, status)
			}
			s.notify(runID, status) // cancelled → origin restore; failed (reject) → origin restore
			// PRD #46 Decision 2: a server-side plan REJECT commits the run to 'failed',
			// a judged status — enqueue a judge on it. A server-side CANCEL commits
			// 'cancelled', which the enqueue gate filters out. Best-effort, gated inside.
			s.maybeEnqueueJudgeByID(ctx, runID)
			// PRD #634 follow-up: a server-side cancel/reject on a scope-directed run commits
			// terminal outside SetState, so settle the pending scope audit row here too (it would
			// otherwise stay "active" forever). Best-effort — must never fail the operator's cancel.
			// Guarded on the run carrying a directive so it is a no-op when there is none.
			if run.ScopeCeiling.Valid {
				if _, setErr := s.q.SettleScopeInputDisposition(ctx, store.SettleScopeInputDispositionParams{
					RunID: runID, Disposition: pgconv.TextOrNull("declined"),
				}); setErr != nil {
					slog.Warn("settle scope input disposition (server-side cancel/reject)", "run", runID, "error", setErr)
				}
			}
			return SubmitInputResult{ServerSide: true}, nil
		}
		// Live poller: the worker will consume this verdict. Enqueue it AND stamp the
		// deliberate-stop signal in one statement (PRD #33 Decision 3) via the
		// dedicated CreateStopVerdictInput CTE, so the signal is never lost
		// independently of the input that requested it. stopKindFor is always non-empty
		// here (kind is cancel/reject_plan). The stamp lands while the run is still
		// non-terminal; the client's terminal-guarded isStoppedRun ignores it until the
		// run reaches failed/cancelled.
		// PRD #503 M3: the shared CTE stamps stop_reason unconditionally, but the reason
		// belongs on a CANCEL only. Pass the operator's optional reason for a cancel;
		// NULL for reject_plan, whose reason lives in failure_reason via the M2 path (the
		// server-side reject branch above) — double-writing would contradict that split.
		var stopReason pgtype.Text // NULL for reject_plan
		if kind == "cancel" {
			stopReason = stopReasonParam(body)
		}
		// Strip NUL from the body co-written to run_user_inputs.body in the SAME INSERT:
		// a NUL in a text column raises Postgres 22021, which would abort the whole CTE and
		// silently drop the cancel/reject verdict (the stop_reason sanitizing above would be
		// moot if this INSERT never lands). NUL is never meaningful in an operator message.
		cleanBody, _ := stripNUL(body)
		if _, err := s.q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
			RunID: runID, Kind: kind, Body: pgconv.TextOrNull(cleanBody), StopKind: pgconv.TextOrNull(stopKindFor(kind)), StopReason: stopReason,
		}); err != nil {
			return SubmitInputResult{}, err
		}
		return SubmitInputResult{ServerSide: false}, nil
	}

	// A revise_plan (PRD #41) is a plain enqueue like follow_up/approve_plan — no
	// stop signal, no server-side transition — but it is capped: at most
	// PlanMaxRevisions persisted revisions per run. The cap spans ALL revise_plan rows
	// (no consumed_at filter), so a consumed revision still counts toward it.
	//
	// The cap check and the enqueue are ONE statement, and — since #106 — the check is
	// a predicate on runs.revise_count inside the UPDATE that bumps it, not a count of
	// run_user_inputs. That distinction is the fix, not a detail: a lock on `runs` never
	// covered a count of a DIFFERENT table, because READ COMMITTED gives the blocked
	// caller a refreshed view of the LOCKED ROW only, not a refreshed snapshot. Two
	// concurrent submits — e.g. web + Slack on the same single-owner gate racing at
	// N-1 — could both slip past and persist an N+1th row, measured 100/100 with the
	// interleave forced. They now cannot. No row = the cap is already reached. The
	// terminal-run guard above already blocks a revise on a finished run.
	//
	// This branch is the SOLE writer of revise_plan rows, which is what keeps the counter
	// and the rows in step; a second writer added later would defeat the cap silently —
	// rows the counter never sees, with nothing going red.
	//
	// 🔴 THAT INVARIANT IS ONLY PARTLY GUARDED, and the parts are worth naming exactly,
	// because an earlier version of this comment credited a test that cannot see it at
	// all. Measured 2026-07-29:
	//
	//   - a second writer added INSIDE this branch, or replacing the capped call, is
	//     caught by TestSubmitInputRevisePlanEnqueuesPlain (service_test.go) — it asserts
	//     CreateRunInput was not called for a revise_plan;
	//   - a NEW SQL query that inserts a 'revise_plan' literal is caught by
	//     store.TestOnlyOneQueryInsertsRevisePlanRows;
	//   - a writer added ELSEWHERE IN GO, reusing the generic CreateRunInput query, is
	//     caught by NOTHING. Adding one left the whole `go test -count=1 ./...` gate green
	//     (43 packages ok, 0 FAIL). Note 00074's CHECK permits 'revise_plan' through
	//     CreateRunInput, whose kind is a bare parameter, so the only thing between the
	//     two writers is the early return below.
	//
	// If you are adding a revise_plan write anywhere else: bump runs.revise_count in the
	// same statement, or the cap stops meaning anything.
	if kind == "revise_plan" {
		row, err := s.q.CreateRunReviseInputIfUnderCap(ctx, store.CreateRunReviseInputIfUnderCapParams{
			RunID: runID, Body: pgconv.TextOrNull(body), MaxRevisions: int32(s.p.PlanMaxRevisions), //nolint:gosec // G115: PlanMaxRevisions is a small bounded config int (env PLAN_MAX_REVISIONS), never near int32 range
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return SubmitInputResult{}, ErrReviseCapReached
		}
		if err != nil {
			return SubmitInputResult{}, err
		}
		return SubmitInputResult{ServerSide: false, ID: row.ID, CreatedAt: row.CreatedAt.Time}, nil
	}

	// A plain steering input (follow_up, or a nil-selection approve_plan handled above):
	// enqueue for the worker with no stop signal and no runs-row touch.
	return s.enqueueRunInput(ctx, runID, kind, body)
}

// enqueueRunInput writes a plain worker-bound input row (no stop signal, no runs-row
// touch) and returns the created row (PRD #95 S2) so the handler can surface id +
// created_at for a follow_up's optimistic reconcile. Shared by the follow_up path and the
// nil-selection approve_plan path so both go through one enqueue.
func (s *Service) enqueueRunInput(ctx context.Context, runID uuid.UUID, kind, body string) (SubmitInputResult, error) {
	row, err := s.q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: runID, Kind: kind, Body: pgconv.TextOrNull(body),
	})
	if err != nil {
		return SubmitInputResult{}, err
	}
	return SubmitInputResult{ServerSide: false, ID: row.ID, CreatedAt: row.CreatedAt.Time}, nil
}

// submitApproval enqueues an approve_plan carrying an agent selection (PRD #37):
// validate against the run's real roster, then write the run's agent_source /
// agent_exclusions and the worker-bound input body in one statement, so the row can
// never disagree with what the worker was told to use.
//
// The body is the SERVER's canonical encoding of the validated selection, never the
// client's text: the worker parses it back with parseAgentSelection, and a raw
// pass-through would hand an unvalidated string to the process that builds the
// agent map.
func (s *Service) submitApproval(ctx context.Context, run store.Run, sel AgentSelection) (SubmitInputResult, error) {
	roster, err := s.rosterFor(ctx, run, sel.Source, nil)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if err := validateSelection(sel, roster); err != nil {
		return SubmitInputResult{}, err
	}
	// The capability approval gate (capabilityGate) is enforced UPSTREAM in SubmitInput for
	// every approve_plan — both the selection-bearing path that reaches here and the
	// nil-selection plain-enqueue path — so a plan can never be approved onto a worker that
	// cannot run it, whichever path the client used.
	exclusions, err := encodeJSONArray(sel.Exclusions)
	if err != nil {
		return SubmitInputResult{}, fmt.Errorf("encode agent exclusions: %w", err)
	}
	body, err := json.Marshal(AgentSelection{Source: sel.Source, Exclusions: orEmpty(sel.Exclusions)})
	if err != nil {
		return SubmitInputResult{}, fmt.Errorf("encode agent selection: %w", err)
	}
	// Issue #260 instrumentation: capture the live milestone freeze state on both sides of
	// the approve-time freeze so a future human-gated dev-cluster run reveals what
	// CreateApprovePlanInput saw. Best-effort: a snapshot read error is logged at Warn and
	// NEVER aborts the approve.
	before, beforeErr := s.q.GetRunMilestoneFreezeSnapshot(ctx, run.ID)
	if beforeErr != nil {
		slog.Warn("workersvc: approve-freeze pre-read failed", "run_id", run.ID, "error", beforeErr)
	}
	if _, err := s.q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
		RunID: run.ID, Body: pgconv.TextOrNull(string(body)), AgentSource: pgconv.TextOrNull(sel.Source), AgentExclusions: exclusions,
		// PRD #122 M2 (Decision 5/5b): the budget-scaling config the freeze reads to derive
		// this run's effective budget from its frozen milestone count, atomically with the
		// candidate→frozen copy. IDEMPOTENT via COALESCE — a re-gate resume re-supplies the
		// same config and never changes a budget frozen once.
		RunMaxIterations:         int32(s.p.RunMaxIterations), //nolint:gosec // G115: RunMaxIterations is a small bounded config int (env RUN_MAX_ITERATIONS), never near int32 range
		RunTimeoutSeconds:        int32(s.p.RunTimeout.Seconds()),
		MilestoneBudgetCap:       milestoneBudgetCap,
		BudgetWallCeilingSeconds: budgetWallCeilingSeconds,
	}); err != nil {
		return SubmitInputResult{}, err
	}
	after, afterErr := s.q.GetRunMilestoneFreezeSnapshot(ctx, run.ID)
	if afterErr != nil {
		slog.Warn("workersvc: approve-freeze post-read failed", "run_id", run.ID, "error", afterErr)
	}
	// Issue #260: emit ONE structured log capturing what the approve-time freeze saw at the
	// live instant. Approves are human-gated and rare, so this logs unconditionally. The
	// pathological signature is the #260 bug SHAPE specifically — a candidate WAS present at
	// the pre-read yet frozen came out NULL after the freeze — raised to Warn with a stable
	// signature field for alerting. A 0-milestone run correctly freezes NULL from a NULL
	// candidate (see CreateApprovePlanInput's own comment), so it must NOT trip the signature,
	// or every no-milestone approve would drown the real signal; hence the before-candidate
	// guard, not a bare after-frozen-empty test.
	logArgs := []any{
		"run_id", run.ID,
		"before_frozen", string(before.MilestonesFrozen),
		"before_candidate", string(before.MilestonesCandidate),
		"before_updated_at", before.UpdatedAt.Time,
		"after_frozen", string(after.MilestonesFrozen),
		"after_updated_at", after.UpdatedAt.Time,
	}
	if afterErr == nil && beforeErr == nil && len(before.MilestonesCandidate) > 0 && len(after.MilestonesFrozen) == 0 {
		slog.Warn("workersvc: approve-time milestone freeze", append(logArgs, "signature", "approve_froze_null")...)
	} else {
		slog.Info("workersvc: approve-time milestone freeze", logArgs...)
	}
	// Populated AFTER validateSelection accepted the selection: only a valid, accepted
	// guard-role exclusion warrants the owner heads-up (PRD #319 M3).
	return SubmitInputResult{ServerSide: false, ExcludedGuardRoles: excludedGuardRoles(sel)}, nil
}

// capabilityGate is the AUTHORITATIVE, server-side PRD #84 M4 4c approval gate. A run at
// awaiting_approval is owned by exactly one worker (run.WorkerID); if that worker's EFFECTIVE
// capabilities do not satisfy the run's plan-time-inferred (and M2 repo-hinted)
// required_capabilities, the approve is BLOCKED (a *CapabilityUnmetError → 409) so a plan can
// never be approved onto a worker that cannot run it. The effective-caps fold —
// worker.capabilities ∪ {docker if docker_enabled} — is the SAME one fn_worker_can_claim
// (migration 00142) and CountOnlineWorkersSatisfyingCaps apply, so approve and claim never
// disagree. Gated by the capability-aware kill-switch (default ON), identically to the
// claim/health paths: with the flag OFF the fleet claims best-effort, so there is no
// eligibility to enforce and this stays silent. Called from submitInput for every approve_plan
// (both the selection and nil-selection paths) so the gate has no bypass — EXCEPT the owner
// override path (SubmitInputWithCapabilityOverride), which skips this gate deliberately and
// clears required_capabilities AFTER a successful approve (OverrideRunRequiredCapabilities).
func (s *Service) capabilityGate(ctx context.Context, run store.Run) error {
	if len(run.RequiredCapabilities) == 0 || !s.capabilityAwareOn(ctx) {
		return nil
	}
	effective, err := s.effectiveOwningWorkerCaps(ctx, run)
	if err != nil {
		return err
	}
	if unmet := capability.Unmet(run.RequiredCapabilities, effective); len(unmet) > 0 {
		return &CapabilityUnmetError{Unmet: unmet}
	}
	return nil
}

// effectiveOwningWorkerCaps folds the run's OWNING worker's effective capability set the
// SAME way fn_worker_can_claim and CountOnlineWorkersSatisfyingCaps do — via the shared
// capability.EffectiveWorkerCaps (the Go mirror of SQL fn_effective_worker_caps, single
// source since #512 M5) — the worker's stored capabilities plus `docker` when
// docker_enabled — so the PRD #84 M4 4c
// approval gate and the claim gate evaluate the identical set and can never disagree. A run
// with no owning worker (no awaiting_approval run reaches submitApproval without one) folds
// to the empty set, which fails CLOSED: every required capability is then unmet, matching
// the claim path's fail direction. A GetWorkerByID error (including the worker having been
// deleted) propagates as an error rather than silently opening the gate.
func (s *Service) effectiveOwningWorkerCaps(ctx context.Context, run store.Run) ([]string, error) {
	if !run.WorkerID.Valid {
		return nil, nil
	}
	wkr, err := s.q.GetWorkerByID(ctx, uuid.UUID(run.WorkerID.Bytes))
	if err != nil {
		return nil, err
	}
	return capability.EffectiveWorkerCaps(wkr.Capabilities, wkr.DockerEnabled.Valid && wkr.DockerEnabled.Bool), nil
}

// OverrideRunRequiredCapabilities backs the PRD #84 M4 4c user override ("run without the
// capability", Decision 12): it clears the run's inferred/hinted required_capabilities. The
// clear is owner- AND awaiting_approval-scoped in SQL, so a non-owner runID or a run outside
// the plan gate is a silent no-op (0 rows). It is called from submitInput's override path
// AFTER the approve has validated and enqueued (the gate is bypassed for that path), so the
// clear runs ONLY on a successful approve — a failed approve leaves the requirement intact
// and the retry stays gated. v1 clears the WHOLE set (repo hint + inferred); a
// hint-vs-inference split is a future refinement (Decision 6/12), and no runtime security
// boundary is bypassed — the §300 guardrail still denies docker USE on a daemon-less worker
// at run time.
func (s *Service) OverrideRunRequiredCapabilities(ctx context.Context, userID, runID uuid.UUID) error {
	_, err := s.q.ClearRunRequiredCapabilities(ctx, store.ClearRunRequiredCapabilitiesParams{ID: runID, UserID: userID})
	return err
}

// AnswerBody is the wire shape of an `answer` steering input (PRD #88 M1). It is
// JSON rather than the bare prose every other kind carries, because an answer must
// name the QUESTION it answers — `approve_plan` already establishes the JSON-body
// idiom, so this is in keeping rather than novel.
//
// One shape, every surface: the web and CLI construct it directly, and the Slack
// replier (whose inbound is free text) resolves the thread anchor to the open
// question id and constructs the same JSON server-side. There is deliberately not a
// second, text-only contract for Slack.
type AnswerBody struct {
	// QuestionID names the question this answers. Compared for equality against the
	// run's open_question_id; never parsed for meaning.
	QuestionID string `json:"question_id"`
	// Answers is index-aligned with the question payload's `questions` array. Free
	// text is always allowed, including where the question offered options.
	Answers []string `json:"answers"`
}

// submitAnswer records an answer to the clarification question a run is parked on
// (PRD #88 M1). Four things happen here that do not happen for any other kind, and
// each closes a failure the others do not:
//
//  1. The run must actually be parked. See the caller's comment.
//  2. A malformed body is REJECTED, deliberately unlike parseAgentSelection's
//     fallback-to-a-safe-default. There, a default is genuinely safe (the run's own
//     agents). Here the whole point of the payload is to identify what is being
//     answered, so accepting an unidentifiable answer IS the harm — it would resolve
//     whatever question happens to be open.
//  3. The named question must be the one currently open. This is the stale-answer
//     guard, and it is keyed on identity precisely because it must survive a requeue:
//     a worker death re-queues and re-parks the run, so any clock- or arrival-ordinal
//     key would reject an answer the user correctly submitted before the death.
//  4. The text is scrubbed and bounded (D-G). #88 is the feature that makes the agent
//     ASK the user for information, which is exactly the prompt that elicits a
//     credential paste — and the question text itself is attacker-influenceable, so an
//     injected repo file can make the lead ask for a PAT "to continue". Slack's
//     inbound path already scrubbed; web and CLI did not. Doing it here means every
//     surface inherits it.
func (s *Service) submitAnswer(ctx context.Context, run store.Run, body string) (SubmitInputResult, error) {
	if run.Status != "awaiting_input" {
		return SubmitInputResult{}, ErrRunNotAwaitingInput
	}
	// NOTE on calling this "belt-and-braces": it is only redundant with the identity
	// check below BECAUSE the worker clears its open-question id when a park settles
	// (RunRunner.askUser's settle) and SetRunRunning clears the column. If either
	// stopped, a run could sit non-parked while still naming a question, and this
	// status check would become the only thing rejecting an answer to it.
	var ab AnswerBody
	if err := json.Unmarshal([]byte(body), &ab); err != nil {
		return SubmitInputResult{}, fmt.Errorf("%w: body must be JSON {question_id, answers}", ErrInvalidAnswer)
	}
	qid := strings.TrimSpace(ab.QuestionID)
	if qid == "" {
		return SubmitInputResult{}, fmt.Errorf("%w: question_id is required", ErrInvalidAnswer)
	}
	// A run parked with no open question id cannot be resumed by any answer
	// (SetRunRunning's guard is unsatisfiable), so an equality test against "" must
	// never pass. SetState rejects an empty id at the park, making this unreachable —
	// it is here because "unreachable" is a claim about another function.
	if !run.OpenQuestionID.Valid || run.OpenQuestionID.String == "" || qid != run.OpenQuestionID.String {
		return SubmitInputResult{}, ErrStaleAnswer
	}
	if len(ab.Answers) > maxAnswerCount {
		return SubmitInputResult{}, fmt.Errorf("%w: at most %d answers", ErrInvalidAnswer, maxAnswerCount)
	}
	answers := make([]string, 0, len(ab.Answers))
	for _, a := range ab.Answers {
		clean, _ := stripNUL(a)
		answers = append(answers, truncateRunes(secretscrub.Scrub(clean), maxAnswerBodyRunes))
	}
	// Re-encode from the server's own validated values rather than storing the
	// caller's raw text, the same rule submitApproval follows: what the worker reads
	// back is what the server checked, not what the client sent.
	encoded, err := json.Marshal(AnswerBody{QuestionID: qid, Answers: answers})
	if err != nil {
		return SubmitInputResult{}, fmt.Errorf("encode answer: %w", err)
	}
	row, err := s.q.CreateRunAnswerInput(ctx, store.CreateRunAnswerInputParams{
		RunID: run.ID, Body: pgconv.TextOrNull(string(encoded)), QuestionID: pgconv.TextOrNull(qid),
	})
	if err != nil {
		return SubmitInputResult{}, err
	}
	return SubmitInputResult{ServerSide: false, ID: row.ID, CreatedAt: row.CreatedAt.Time}, nil
}

// orEmpty makes a nil slice marshal as `[]`, never `null` — the worker's
// parseAgentSelection accepts both, but the persisted body is also what a human
// reads in the inputs table.
func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// stopKindFor maps a deliberate-stop steering kind to the stop signal stamped on the
// run (PRD #33): a cancel verdict is 'cancelled', a plan reject is 'plan_rejected', a
// graceful interactive wind-down (PRD #517 M4) is 'stopped'. Only cancel/reject_plan/
// stop reach it (the stop-verdict branches of SubmitInput); the server owns this mapping
// so the signal never depends on the reason string the worker later reports.
// clampInt clamps v into [lo, hi]. Callers guarantee lo <= hi (PRD #634 M2 passes
// len(completed) <= len(frozen)); if not, hi wins (the upper bound is returned).
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// submitScopeCeiling writes runs.scope_ceiling + the kind='scope' audit row in one
// statement (PRD #634 M2) and returns the resolved ceiling so m5's CLI can report the
// clamped value. The ceiling is written as a REAL value even when 0 (complete nothing
// further) — pgInt4 nulls a 0 (which would read as "unbounded"), so the Int4 is built
// directly here. The audit body is NUL-stripped to avoid Postgres 22021 aborting the CTE;
// disposition is left NULL (the worker settles it in m4). No stop_kind is stamped.
func (s *Service) submitScopeCeiling(ctx context.Context, run store.Run, ceiling int, auditBody string) (SubmitInputResult, error) {
	cleanBody, _ := stripNUL(auditBody)
	// ceiling is clampInt'd into [len(completed), len(frozen)] by the caller, so it is a
	// small non-negative milestone count. The explicit int32 bound makes that provable to
	// the integer-conversion analyzers (gosec G115 / CodeQL go/incorrect-integer-conversion)
	// at the cast, mirroring task.go's budget-iters guard; the >MaxInt32 arm is unreachable
	// for a milestone count and falls back to 0 ("complete nothing further"), the safe direction.
	ceil32 := int32(0)
	if ceiling >= 0 && ceiling <= math.MaxInt32 {
		ceil32 = int32(ceiling)
	}
	if _, err := s.q.CreateScopeCeilingInput(ctx, store.CreateScopeCeilingInputParams{
		RunID:        run.ID,
		ScopeCeiling: pgtype.Int4{Int32: ceil32, Valid: true},
		Body:         pgconv.TextOrNull(cleanBody),
	}); err != nil {
		return SubmitInputResult{}, err
	}
	c := ceiling
	return SubmitInputResult{ServerSide: false, ScopeCeiling: &c}, nil
}

func stopKindFor(kind string) string {
	switch kind {
	case "cancel":
		return "cancelled"
	case "reject_plan":
		return "plan_rejected"
	case "stop":
		return "stopped"
	default:
		return ""
	}
}
