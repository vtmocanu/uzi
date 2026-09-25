package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// runs_dto.go holds the run and message DTO mappers that translate store rows into
// the wire apitypes shapes, shared by the run lifecycle and listing handlers.

// isPlanningPhase reports whether a run is in its pre-approval PLANNING turn — a
// display-only predicate meaningful only while status=="running" (issue #321). A run
// is planning iff it is a planning-capable kind (chat/judge never plan), is running,
// has not yet entered the implement loop (iteration_count 0), and has no persisted
// plan yet (plan_md empty). Planning-capability delegates to runkind.Listed, which is
// pinned to ListRunsForUser's NOT IN ('chat','judge') filter by runkind_sql_test.go,
// so issue/ci_fix/self_improve are planning-capable.
func isPlanningPhase(kind, status string, iterationCount int32, planMdPresent bool) bool {
	if !runkind.Listed(kind) {
		return false
	}
	return status == "running" && iterationCount == 0 && !planMdPresent
}

// runPriorityClass resolves a run's display class via the ONE SQL function (D8),
// never re-deriving the demotion predicate in Go. is_stale is the D4 fail-open flag
// (created before now-RUN_BACKGROUND_GRACE). Best-effort: on error, the display
// defaults to "normal" (a read must not fail because the pill class couldn't be
// computed).
func (h *Handler) runPriorityClass(ctx context.Context, r store.Run) string {
	// Best-effort: a Handler assembled without a direct store (the wsvc-routed fake
	// used by the runs handler tests) has no query surface here, so default to the
	// safe class rather than dereferencing a nil store — the same "on error → normal"
	// contract this helper already promises.
	if h.q == nil {
		return "normal"
	}
	cutoff := h.clock().Add(-h.cfg.WorkerBackgroundGrace)
	isStale := r.CreatedAt.Valid && r.CreatedAt.Time.Before(cutoff)
	class, err := h.q.RunPriorityClass(ctx, store.RunPriorityClassParams{
		RunKind: r.Kind, Priority: r.Priority, IsStale: isStale,
	})
	if err != nil {
		return "normal"
	}
	return class
}

// pauseRequestedRule is the SINGLE, server-side pause-boundary decision (PRD #1190 M1,
// Decision 4): given the run's pause mode and its frozen/completed milestone counts, whether
// the worker holding it should park at its next boundary. The worker honors this boolean off
// the running-report ACK, so the comparison lives in exactly one place and one test table.
//   - "now"       → park immediately (drop the in-flight turn).
//   - "wall"      → park immediately, exactly like "now" (PRD #1497 M1): the system's wall
//     request means the deadline is reached, so the worker drops the in-flight turn and takes
//     the capture-first wall park at once.
//   - "milestone" → park once there is no frozen list to wait on (park at the next turn
//     boundary) OR the in-flight milestone has completed (completed count now exceeds the
//     count captured at request time).
//   - any other value (empty / no pause pending) → false.
func pauseRequestedRule(pauseMode string, frozenLen, completedLen, afterCount int) bool {
	switch pauseMode {
	case "now", "wall":
		return true
	case "milestone":
		return frozenLen == 0 || completedLen > afterCount
	default:
		return false
	}
}

// completionPhaseRule is the SINGLE, server-side completion-phase decision (PRD #1226 M5,
// D8): the one derived label the web and CLI both render, so the two surfaces cannot
// disagree about which of D8's three states an interlocked run is in. Like pauseRequestedRule
// it lives in exactly one place and one test table.
//   - not interlocked → "" (a legacy / non-interlocked run has no completion phase).
//   - held ("completion_blocked") → "blocked" (takes precedence over the running states below).
//   - parked in the LIVE completion-question window (awaiting_input, interlocked, with the
//     dedicated completion-question marker set, before the hold so hold_reason is still empty) →
//     "blocked". Without this arm such a run derives "" — indistinguishable from a legacy run — so
//     the web/CLI would show nothing while the run is in fact blocked pending the owner's continue
//     decision. It keys on completionQuestionOpen (run.CompletionQuestionAt.Valid), the SAME
//     dedicated marker the decision endpoint admits on, and shows the SAME honest "blocked" label
//     as the paused hold. The marker is authored ONLY by a worker completion-question report, so
//     an ORDINARY ask_user clarification — even on an interlocked run past a completion attempt —
//     no longer matches this arm (the imprecision the old interlock+attempts proxy carried is
//     gone: the arm is now precise).
//   - running past a first attempt, some criteria still unmet → "reworking".
//   - running past a first attempt, none unmet → "checking".
//   - anything else (no attempt yet, not running) → "".
func completionPhaseRule(interlocked bool, status, holdReason string, completionQuestionOpen bool, attempts, unmetCount int) string {
	if !interlocked {
		return ""
	}
	if holdReason == "completion_blocked" {
		return "blocked"
	}
	if status == "awaiting_input" && completionQuestionOpen {
		return "blocked"
	}
	if status == "running" && attempts > 0 {
		if unmetCount > 0 {
			return "reworking"
		}
		return "checking"
	}
	return ""
}

// decodeLatestUnmet pulls the bounded unmet milestone-id list out of a run's
// latest_completion_attempt jsonb summary (PRD #1226 M5, D8, key "unmet"). It ALWAYS returns a
// NON-NIL slice — `[]` for a run with no attempt (empty jsonb), a malformed summary, or an
// attempt with nothing unmet — so completion_unmet is a stable array on the wire, never null.
// The ids are server-validated milestone keys; a decode error degrades to `[]` rather than
// failing the read of an otherwise-fine run (the summary is server-built via jsonb_build_object).
func decodeLatestUnmet(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	var summary struct {
		Unmet []string `json:"unmet"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil || summary.Unmet == nil {
		return []string{}
	}
	return summary.Unmet
}

// credentialEpochsToDTO maps a run's credential-epoch journal rows to the DTO slice
// (PRD #1247 M1, D7), oldest generation first (the query orders them). Always returns a
// non-nil slice ([] over null) so the wire field is never null once enriched. secret_id,
// label and select_reason are all null-tolerant so a deleted token's history stays readable;
// secret_id is mapped INDEPENDENTLY of label (the FK nulls the id on a delete while the
// snapshotted label stays), exactly as runToDTO maps the run-level id/label pair.
func credentialEpochsToDTO(rows []store.RunCredentialEpoch) []apitypes.CredentialEpochDTO {
	out := make([]apitypes.CredentialEpochDTO, 0, len(rows))
	for _, e := range rows {
		dto := apitypes.CredentialEpochDTO{
			ClaimGeneration: e.ClaimGeneration,
			AppliedAt:       e.AppliedAt.Time,
		}
		if e.SecretID.Valid {
			s := uuid.UUID(e.SecretID.Bytes).String()
			dto.SecretID = &s
		}
		if e.Label.Valid {
			l := e.Label.String
			dto.Label = &l
		}
		if e.SelectReason.Valid {
			sr := e.SelectReason.String
			dto.SelectReason = &sr
		}
		out = append(out, dto)
	}
	return out
}

// credentialSwitchState derives RunDTO.credential_switch from the run row (PRD #1247
// M1, D14): null when no held-state switch is pending, "released" once the release
// transition has stamped claim_released_at at-or-after the request, else "requested".
//
// M4/M5 make this LIVE: SetRunCredential stamps the columns on a held-state switch, and
// (PRD #1247 D11 fix round) every terminal transition CLEARS them, so a completed/failed/
// cancelled run never carries a stale switch state.
//
// NOT YET IMPLEMENTED (deferred, issue #1422): clearing the stamp on successful APPLICATION
// at the next epoch write (D14). Until then, after a release+reclaim (the run's
// claim_generation has advanced PAST credential_switch_generation) this reads the stale
// pre-reclaim state ("requested"/"released") rather than null. It is NOT a worker-signal leak
// — PendingCredentialSwitchSignal's generation guard already returns nil there — only this
// DTO field, and no UI renders it yet (PRD m7/m8), so it is a JSON-contract wart, not a
// user-visible one.
func credentialSwitchState(r store.Run) *string {
	if !r.CredentialSwitchRequestedAt.Valid {
		return nil
	}
	state := "requested"
	if r.ClaimReleasedAt.Valid && !r.ClaimReleasedAt.Time.Before(r.CredentialSwitchRequestedAt.Time) {
		state = "released"
	}
	return &state
}

// runToDTO maps a bare run row to its wire DTO. priorityClass is the D8 display class
// (from fn_run_priority_class via a list column or h.runPriorityClass), passed in
// explicitly so this mapper stays a PURE function of its inputs — no now()/config
// reaches into it. globalTimeout is the instance RUN_TIMEOUT (h.cfg.RunTimeout),
// passed in the same way, so RunDeadline can be computed here without config access
// (PRD #1170). extensionCapSeconds is the effective admin extension cap (PRD #1189,
// RunExtensionCapSeconds; 0 = extending disabled) and now is the wall-clock instant used
// for budget_used_seconds — both passed in so this mapper stays pure.
// forgeParkMax is the effective forge-unreachable park cap (PRD #1392 M1,
// RUN_FORGE_UNREACHABLE_MAX_PARKS; 0 = unlimited), passed in the same way as
// extensionCapSeconds so this mapper stays pure — no config reaches into it. It surfaces on
// the DTO as ForgeParkMax so the forge-park pill can render "N of MAX".
func runToDTO(r store.Run, priorityClass string, globalTimeout time.Duration, extensionCapSeconds int, forgeParkMax int, now time.Time) apitypes.RunDTO {
	dto := apitypes.RunDTO{
		ID:               r.ID.String(),
		Kind:             r.Kind,
		IssueTitle:       r.IssueTitle,
		IssueDescription: r.IssueDescription,
		// PRD #764 M2: server-computed PRD presence for the runs view, derived
		// label-independently from the snapshotted issue description via the same
		// detector the board card uses. Only issue-backed runs can link a PRD, so an
		// issue-less run (chat / self-improve) whose description happens to mention a
		// prds/*.md path never shows a spurious PRD badge.
		HasPRDLink: r.IssueIid.Valid && forgesvc.HasPRDLink(r.IssueDescription),
		// PRD #1429 M1 (D2): the run's actual stored harness, surfaced read-only. NOT NULL
		// DEFAULT 'claude', so always a definite value ("claude" for every current run).
		Harness:        r.Harness,
		Title:          textPtrValue(r.Title.Valid, r.Title.String),
		Status:         r.Status,
		RequeueCount:   r.RequeueCount,
		IterationCount: r.IterationCount,
		IsPlanning: isPlanningPhase(r.Kind, r.Status, r.IterationCount,
			r.PlanMd.Valid && strings.TrimSpace(r.PlanMd.String) != ""),
		AutoApprove:   r.AutoApprove,
		TriggerSource: r.TriggerSource,
		Branch:        textPtrValue(r.Branch.Valid, r.Branch.String),
		BaseBranch:    textPtrValue(r.BaseBranch.Valid, r.BaseBranch.String),
		OpenMr:        r.OpenMr,
		Interactive:   r.Interactive,
		// PRD #400 Decision 6: when the task run's dispatch gate was stamped (null until
		// then, and on every non-task run). Mapped like ClaimedAt.
		DispatchedAt:  timePtr(r.DispatchedAt.Valid, r.DispatchedAt.Time),
		MrWebURL:      textPtrValue(r.MrWebUrl.Valid, r.MrWebUrl.String),
		MrState:       textPtrValue(r.MrState.Valid, r.MrState.String),
		FailureReason: textPtrValue(r.FailureReason.Valid, r.FailureReason.String),
		StopKind:      textPtrValue(r.StopKind.Valid, r.StopKind.String),
		StopReason:    textPtrValue(r.StopReason.Valid, r.StopReason.String),
		Health:        r.Health,
		HealthReason:  textPtrValue(r.HealthReason.Valid, r.HealthReason.String),
		HealthSince:   timePtr(r.HealthSince.Valid, r.HealthSince.Time),
		// PRD #1170: the server-computed wall-clock deadline the near-timeout badge
		// counts down to. RunDeadline returns nil for a run with no wall deadline
		// (not running, chat/judge/interactive, or no started_at).
		DeadlineAt: workersvc.RunDeadline(r.StartedAt, r.BudgetWallSeconds, r.BudgetPausedSeconds, r.Kind, r.Interactive, r.Status, globalTimeout, r.BudgetExtensionSeconds, r.BudgetFinalizeSeconds),
		PlanMd:     textPtrValue(r.PlanMd.Valid, r.PlanMd.String),
		PlanSource: r.PlanSource,
		// PRD #362 M1: plain-English summaries. Intent/plan are nullable text; deltas
		// are decoded below (tolerate-on-read) so a malformed value cannot fail the read.
		SummaryIntent: textPtrValue(r.SummaryIntent.Valid, r.SummaryIntent.String),
		SummaryPlan:   textPtrValue(r.SummaryPlan.Valid, r.SummaryPlan.String),
		PipelineRef:   textPtrValue(r.PipelineRef.Valid, r.PipelineRef.String),
		FixVerdict:    textPtrValue(r.FixVerdict.Valid, r.FixVerdict.String),
		ReportOnly:    r.ReportOnly,
		ReportMd:      textPtrValue(r.ReportMd.Valid, r.ReportMd.String),
		// PRD #377 M1: the preserved agent diff on a workflow_scope_missing failed run.
		PreservedPatch: textPtrValue(r.PreservedPatch.Valid, r.PreservedPatch.String),
		// issue #974: the run's typed fail_origin (already coerced/allowlisted at write
		// time), surfaced read-only so a diagnosis keys on it instead of failure_reason
		// free text. Null when the run never set one.
		FailOrigin: textPtrValue(r.FailOrigin.Valid, r.FailOrigin.String),
		// issue #1418: the server-derived landing bucket. runToDTO is CAPTURE-UNAWARE
		// (hasAvailableCapture=false), which is correct for every non-read caller and the
		// default for the read callers before they overlay the capture-aware value. A
		// preserved_patch alone is enough to reach needs_landing here.
		LandingState: workersvc.DeriveLandingState(textPtrValue(r.FailOrigin.Valid, r.FailOrigin.String), r.PreservedPatch.Valid, false),
		// PRD-link reconciliation (read-only): the path the run declared it archived a
		// completed PRD to, and when that patch lifecycle settled (null = still pending).
		PrdDonePath:       textPtrValue(r.PrdDonePath.Valid, r.PrdDonePath.String),
		PrdPatchSettledAt: timePtr(r.PrdPatchSettledAt.Valid, r.PrdPatchSettledAt.Time),
		ClaimedAt:         timePtr(r.ClaimedAt.Valid, r.ClaimedAt.Time),
		StartedAt:         timePtr(r.StartedAt.Valid, r.StartedAt.Time),
		FinishedAt:        timePtr(r.FinishedAt.Valid, r.FinishedAt.Time),
		CreatedAt:         r.CreatedAt.Time,
		UpdatedAt:         r.UpdatedAt.Time,
		// PRD #35 usage-limit park. Mapped INDEPENDENTLY of each other, like the
		// PRD #111 credential fields below: WaitOnLimit is set on every run from
		// creation while the other four stay null/zero until a first park, so a run
		// legitimately carries the opt-in with no park data. Never branch on the group.
		WaitOnLimit: r.WaitOnLimit,
		// PRD #841 M1: the per-run MR-rework override, tri-state. boolPtrValue maps the
		// nullable column to *bool so null (inherit) survives to the wire distinct from
		// an explicit true/false.
		MrReworkEnabled: boolPtrValue(r.MrReworkEnabled),
		LimitResetsAt:   timePtr(r.LimitResetsAt.Valid, r.LimitResetsAt.Time),
		RetryNotBefore:  timePtr(r.RetryNotBefore.Valid, r.RetryNotBefore.Time),
		LimitWaitCount:  r.LimitWaitCount,
		RateLimitType:   textPtrValue(r.RateLimitType.Valid, r.RateLimitType.String),
		// PRD #1392 M1: the forge pre-clone park surface. RecoveryWaitCause is the typed cause
		// (null = untyped/legacy park); RecoveryRetryNotBefore is the recovery-park promotion
		// stamp (the recovery-park analog of RetryNotBefore, a distinct column); ForgeParkCount
		// is the forge-only lifetime counter; ForgeParkMax is the effective cap passed in
		// (0 = unlimited). All four surface for every run (SC5), independent of each other.
		RecoveryWaitCause:      textPtrValue(r.RecoveryWaitCause.Valid, r.RecoveryWaitCause.String),
		RecoveryRetryNotBefore: timePtr(r.RecoveryRetryNotBefore.Valid, r.RecoveryRetryNotBefore.Time),
		ForgeParkCount:         int(r.ForgeParkCount),
		ForgeParkMax:           forgeParkMax,
		// PRD #300: the per-schedule model a schedule froze onto this run at fire time.
		// nil (NULL column) for every run that inherited the owner's per-user default.
		Model: textPtrValue(r.Model.Valid, r.Model.String),
		// PRD #305: the frozen "apply model also to agents" flag; false for every run
		// that did not opt in (the default).
		OverrideSubagentModel: r.OverrideSubagentModel,
		// PRD #320 D8: the display priority class, supplied by the caller (a list-query
		// column or h.runPriorityClass) so this mapper stays pure.
		Priority: priorityClass,
		// PRD #84: the run's inferred/hinted scheduling requirements, surfaced RAW for the
		// web/CLI (4d) readiness/mismatch display. Capability/tool slices are normalized to
		// a non-nil empty slice ([] over null), mirroring the repo DTO; size_class is the
		// NOT NULL DEFAULT '' string (empty for a run whose inference never set it).
		RequiredCapabilities: capsOrEmpty(r.RequiredCapabilities),
		RequiredTools:        capsOrEmpty(r.RequiredTools),
		SizeClass:            r.SizeClass,
		// PRD #212: the plan-turn git-status list, non-nil ([] over null) via capsOrEmpty
		// like required_tools. runToDTO is the single chokepoint for every run-detail
		// response, so this one line covers every consumer.
		PlanChangedFiles: capsOrEmpty(r.PlanChangedFiles),
	}
	if r.RepoID.Valid {
		s := uuid.UUID(r.RepoID.Bytes).String()
		dto.RepoID = &s
	}
	if r.ResumeOfRunID.Valid {
		s := uuid.UUID(r.ResumeOfRunID.Bytes).String()
		dto.ResumeOfRunID = &s
	}
	if r.IssueIid.Valid {
		v := r.IssueIid.Int64
		dto.IssueIID = &v
	}
	if r.WorkerID.Valid {
		s := uuid.UUID(r.WorkerID.Bytes).String()
		dto.WorkerID = &s
	}
	if r.MrIid.Valid {
		v := r.MrIid.Int64
		dto.MrIID = &v
	}
	// PRD #111 M1. Mapped INDEPENDENTLY, not as a pair: the FK nulls the id when the
	// credential is deleted while the snapshotted label stays, so a historical run
	// legitimately carries a label with no id and the UI still names the account.
	if r.AnthropicSecretID.Valid {
		s := uuid.UUID(r.AnthropicSecretID.Bytes).String()
		dto.AnthropicSecretID = &s
	}
	dto.AnthropicSecretLabel = textPtrValue(r.AnthropicSecretLabel.Valid, r.AnthropicSecretLabel.String)
	// PRD #111 M5. Mapped independently of BOTH fields above, for the same reason and
	// one more: the reason is present on every M1-era run (all three lanes write one)
	// while the headroom is present only on an auto pick, so a run legitimately
	// carries a reason with no headroom. Rendering must branch on each, never on the
	// pair.
	dto.AnthropicSelectReason = textPtrValue(r.AnthropicSelectReason.Valid, r.AnthropicSelectReason.String)
	if r.AnthropicHeadroomPct.Valid {
		// SMALLINT 0..100 with a CHECK, widened to int for the wire: JSON has one
		// number type and a *int16 would only invite a client to think the range is
		// meaningful to it. The range lives in the database and in autoselect, not here.
		v := int(r.AnthropicHeadroomPct.Int16)
		dto.AnthropicHeadroomPct = &v
	}
	// PRD #1247 M1: the per-run credential override + switch state, mapped from the run
	// row. Mapped INDEPENDENTLY (like the anthropic fields above): a run can carry an
	// override with no pending switch and vice versa. The override Label is resolved by
	// the GetRun enrichment path (runToDTO is pure and cannot read the token row); here it
	// is null. CredentialEpochs is initialized to [] and filled by the same enrichment —
	// [] over null so a client reads it unconditionally, mirroring PlanChangedFiles. Every
	// value stays null/[] in M1 because nothing writes the override columns yet.
	if r.CredentialOverrideMode.Valid && r.CredentialOverrideMode.String != "" {
		dto.CredentialOverride = &apitypes.CredentialOverrideDTO{Mode: r.CredentialOverrideMode.String}
	}
	dto.CredentialSwitch = credentialSwitchState(r)
	dto.CredentialEpochs = []apitypes.CredentialEpochDTO{}
	// PRD #37. A decode error should be impossible (the API validates every write
	// and both columns carry a jsonb_typeof CHECK); it is logged and treated as
	// "not reported" rather than failing the read of an otherwise-fine run.
	if agents, err := workersvc.DecodeRepoAgents(r.RepoAgents); err != nil {
		slog.Error("decode run repo agents", "run_id", r.ID, "error", err)
	} else {
		dto.RepoAgents = agents
	}
	if excl, err := workersvc.DecodeExclusions(r.AgentExclusions); err != nil {
		slog.Error("decode run agent exclusions", "run_id", r.ID, "error", err)
	} else {
		dto.AgentExclusions = excl
	}
	dto.AgentSource = textPtrValue(r.AgentSource.Valid, r.AgentSource.String)
	// PRD #122 M1: the FROZEN milestone list. Degrades gracefully on a decode error
	// (impossible in practice — every write is validated and the column carries a
	// jsonb_typeof CHECK), logged and treated as "no list" rather than failing the read.
	if milestones, err := workersvc.DecodeMilestones(r.MilestonesFrozen); err != nil {
		slog.Error("decode run milestones", "run_id", r.ID, "error", err)
	} else {
		dto.Milestones = milestones
	}
	// PRD #122 M3: the PRE-APPROVAL candidate list, read-only for the plan gate. Decoded
	// like the frozen list above (the column carries the same jsonb_typeof CHECK and every
	// write is validated) and degrades gracefully on a decode error — logged and left nil
	// rather than failing the read of an otherwise-fine run.
	if candidate, err := workersvc.DecodeMilestones(r.MilestonesCandidate); err != nil {
		slog.Error("decode run milestones candidate", "run_id", r.ID, "error", err)
	} else {
		dto.MilestonesCandidate = candidate
	}
	// PRD #122 M2: live progress (id arrays) + the effective per-run budget. Progress
	// degrades to nil on a decode error, same as the frozen list above (the columns
	// carry a jsonb_typeof CHECK). The budget columns are pgtype.Int4 → *int, null when
	// the run is on the global default (a 0/1-milestone run).
	if completed, err := workersvc.DecodeMilestoneIDs(r.MilestonesCompleted); err != nil {
		slog.Error("decode run milestones completed", "run_id", r.ID, "error", err)
	} else {
		dto.MilestonesCompleted = completed
	}
	if inProgress, err := workersvc.DecodeMilestoneIDs(r.MilestonesInProgress); err != nil {
		slog.Error("decode run milestones in progress", "run_id", r.ID, "error", err)
	} else {
		dto.MilestonesInProgress = inProgress
	}
	// PRD #1224: the validated per-milestone agent attribution. Degrades to nil on a decode
	// error (the stored value is the already-validated subset), same as the id arrays above.
	if agents, err := workersvc.DecodeMilestoneAgents(r.MilestonesAgents); err != nil {
		slog.Error("decode run milestones agents", "run_id", r.ID, "error", err)
	} else {
		dto.MilestonesAgents = agents
	}
	if r.BudgetMaxIterations.Valid {
		v := int(r.BudgetMaxIterations.Int32)
		dto.BudgetMaxIterations = &v
	}
	if r.BudgetWallSeconds.Valid {
		v := int(r.BudgetWallSeconds.Int32)
		dto.BudgetWallSeconds = &v
	}
	// PRD #1189: the owner-granted extension (NOT NULL DEFAULT 0 → plain int) and the
	// effective admin cap, so the Extend chooser and the CLI need no second settings call.
	// The cap is the same value RunExtensionCapSeconds returns (0 = extending disabled).
	dto.BudgetExtensionSeconds = int(r.BudgetExtensionSeconds)
	dto.BudgetExtensionCapSeconds = extensionCapSeconds
	// PRD #1497 M1: the finalize allowance (0, or 1800 once Stop granted it); the third
	// budget_total term, outside the extension cap.
	dto.BudgetFinalizeSeconds = int(r.BudgetFinalizeSeconds)
	// PRD #1189 / #1497 M1: budget_total_seconds is the THREE-TERM total,
	// COALESCE(budget_wall_seconds, RUN_TIMEOUT) + budget_extension_seconds +
	// budget_finalize_seconds. It is emitted for every STARTED, TIMED (non-chat/judge,
	// non-interactive), NON-TERMINAL row — INCLUDING paused — decoupled from DeadlineAt (which
	// stays nil while parked). The predicate is the started/timed one RunDeadline uses, minus the
	// running-only status restriction, so a parked run carries its budget.
	timedBudget := r.StartedAt.Valid && !r.Interactive && r.Kind != "chat" && r.Kind != "judge" && !apitypes.IsTerminalRunStatus(r.Status)
	if timedBudget {
		wall := int(globalTimeout / time.Second)
		if r.BudgetWallSeconds.Valid && r.BudgetWallSeconds.Int32 > 0 {
			wall = int(r.BudgetWallSeconds.Int32)
		}
		total := wall + int(r.BudgetExtensionSeconds) + int(r.BudgetFinalizeSeconds)
		dto.BudgetTotalSeconds = &total
	}
	// PRD #1189 / #1497 M1: budget_used_seconds is ACTIVE time so far — end - started_at -
	// budget_paused, clamped at 0 — valid in every started status and nil when the run never
	// started. For a PAUSED row `end` is FROZEN at status_since (the park instant), so the figure
	// does not drift upward while parked (it was banked into budget_paused only at resume, so a
	// live now would otherwise creep — the #1497 fix). For every other started row `end` is now,
	// aged client-side. It subtracts only BANKED pause time (budget_paused_seconds is credited at
	// resume). The sweep and the health arm, which own the kill, run only while status='running'.
	if r.StartedAt.Valid {
		end := now
		if r.Status == "paused" && r.StatusSince.Valid {
			end = r.StatusSince.Time
		}
		used := int(end.Sub(r.StartedAt.Time).Seconds()) - int(r.BudgetPausedSeconds)
		if used < 0 {
			used = 0
		}
		dto.BudgetUsedSeconds = &used
	}
	// PRD #1497 M1: the server-derived Stop gate — true iff a budget_exhausted wall park on a
	// milestone ISSUE run with >= 1 completed milestone and the finalize allowance unused. Read
	// after MilestonesCompleted is decoded above so len() is the real completed count.
	dto.CanStopAtWall = r.Status == "paused" && r.HoldReason.Valid && r.HoldReason.String == "budget_exhausted" &&
		r.Kind == "issue" && !r.Interactive && r.BudgetFinalizeSeconds == 0 && len(dto.MilestonesCompleted) >= 1
	// PRD #634 M2: the operator scope ceiling rides the running-report ACK and the claim
	// payload (both built here) so the worker honors it at the loop top and across a
	// re-claim. pgtype.Int4 → *int, null (unbounded) when no scope directive was written.
	if r.ScopeCeiling.Valid {
		v := int(r.ScopeCeiling.Int32)
		dto.ScopeCeiling = &v
	}
	// PRD #1190 M1: the pending-pause flag columns (owner intent) and the server-decided
	// pause boundary the worker honors on the running-report ACK. checkpoint_tip_at is not
	// owner-gated (just a timestamp). PauseRequested is computed by the ONE boundary rule
	// (pauseRequestedRule), reusing the frozen/completed lists decoded above so the count
	// comparison and the DTO cannot disagree.
	dto.PauseRequestedAt = timePtr(r.PauseRequestedAt.Valid, r.PauseRequestedAt.Time)
	dto.PauseMode = textPtrValue(r.PauseMode.Valid, r.PauseMode.String)
	if r.PauseAfterCount.Valid {
		v := int(r.PauseAfterCount.Int32)
		dto.PauseAfterCount = &v
	}
	dto.CheckpointTipAt = timePtr(r.CheckpointTipAt.Valid, r.CheckpointTipAt.Time)
	afterCount := 0
	if r.PauseAfterCount.Valid {
		afterCount = int(r.PauseAfterCount.Int32)
	}
	mode := ""
	if r.PauseMode.Valid {
		mode = r.PauseMode.String
	}
	dto.PauseRequested = pauseRequestedRule(mode, len(dto.Milestones), len(dto.MilestonesCompleted), afterCount)
	// PRD #1226 M4 (D3): the server-decided served `budget_exhausted` steer rides the running-report
	// ACK exactly like PauseRequested. It is true iff the sweeper has stamped
	// completion_budget_exhausted_at (StampCompletionBudgetExhausted) — a one-shot flag the live
	// post-attempt worker reads to enter the completion hold; false whenever the column is NULL.
	dto.CompletionBudgetExhausted = r.CompletionBudgetExhaustedAt.Valid
	// PRD #1226 M5 (D8): the honest-state completion fields — the wire contract the web + CLI
	// render the interlock states from. CompletionInterlock is the discriminator (non-null
	// completion_contract_version); the rest carry inert defaults on a non-interlocked run.
	// completion_unmet is the bounded unmet-milestone list from the latest attempt's jsonb
	// summary, ALWAYS a non-nil slice ([] over null). hold_context is the constant D8
	// provider-context string, stated ONLY in a completion hold so a surface never claims
	// cross-worker durability. completion_phase is the ONE derived label (completionPhaseRule),
	// so the web and CLI cannot disagree about which of D8's three states the run is in.
	dto.CompletionInterlock = r.CompletionContractVersion.Valid
	dto.CompletionAttempts = int(r.CompletionAttempts)
	dto.CompletionUnmet = decodeLatestUnmet(r.LatestCompletionAttempt)
	dto.HoldReason = textPtrValue(r.HoldReason.Valid, r.HoldReason.String)
	holdReason := ""
	if r.HoldReason.Valid {
		holdReason = r.HoldReason.String
	}
	if holdReason == "completion_blocked" {
		holdCtx := "unavailable(same_worker_only)"
		dto.HoldContext = &holdCtx
	}
	dto.CompletionPhase = completionPhaseRule(dto.CompletionInterlock, r.Status, holdReason, r.CompletionQuestionAt.Valid, dto.CompletionAttempts, len(dto.CompletionUnmet))
	// PRD #1227 M1: the owner-decision contract projection. completion_revision is the run's current
	// contract_revision (null when unfrozen). completion_deferred / completion_accepted are decoded
	// from the frozen completion_contract by the SINGLE shared projector (workersvc.CompletionScopeView),
	// so the wire shape and the contract shape cannot drift; both are STABLE arrays ([] over null),
	// like completion_unmet.
	if r.ContractRevision.Valid {
		rev := int(r.ContractRevision.Int32)
		dto.CompletionRevision = &rev
	}
	dto.CompletionDeferred, dto.CompletionAccepted = workersvc.CompletionScopeView(r.CompletionContract)
	// PRD #362 M1, Decision 6 (tolerate-on-read): decode the summary_deltas jsonb into
	// the typed slice; a malformed or unexpected value renders as NO deltas (nil), logged
	// and never a panic — the deltas are advisory and a prior write's data, not an
	// invariant of this read. Mirrors the milestones decode above.
	if deltas, err := workersvc.DecodeSummaryDeltas(r.SummaryDeltas); err != nil {
		slog.Error("decode run summary deltas", "run_id", r.ID, "error", err)
	} else {
		dto.SummaryDeltas = deltas
	}
	// The failing pipeline's URL rides the frozen snapshot, not a column (the
	// pipeline cache row is transient). Best-effort decode; a ci_fix run always has
	// one, an issue run has no snapshot.
	if url := failureSnapshotWebURL(r.FailureSnapshot); url != "" {
		dto.PipelineWebURL = &url
	}
	return dto
}

// landingStateOverlay resolves the capture-aware landing_state for a single-run detail read
// (issue #1418). On a capture-lookup error we do NOT know whether an available recovery
// capture exists, so for a human-landable origin with no preserved_patch — whose
// capture-unaware seed from runToDTO is "unrecoverable" — we must not emit that definitive
// "work is lost" state on a transient DB blip. Bias to capture-present so the safe degraded
// value is the actionable "needs_landing", which points the operator at `uzi run export`
// (itself the source of truth for capture availability). The run-list path reads the capture
// fact authoritatively from a joined column and never takes this error branch.
func landingStateOverlay(failOrigin *string, hasPreservedPatch, hasAvailableCapture bool, lookupErr error) string {
	if lookupErr != nil {
		return workersvc.DeriveLandingState(failOrigin, hasPreservedPatch, true)
	}
	return workersvc.DeriveLandingState(failOrigin, hasPreservedPatch, hasAvailableCapture)
}

// runExtensionCapSeconds reads the per-run extension cap (PRD #1189) best-effort for the DTO:
// a nil settings cache or a read error reads as 0 (extending disabled / unknown), so a
// momentarily-unreadable setting never fails a run read — the client falls back to plain
// elapsed and the Extend button does not render (rollout-skew safe).
func (h *Handler) runExtensionCapSeconds(ctx context.Context) int {
	if h.settings == nil {
		return 0
	}
	c, _ := h.settings.RunExtensionCapSeconds(ctx)
	return c
}

// failureSnapshotWebURL pulls just the pipeline web URL out of a run's
// failure_snapshot jsonb for the run-view header link. Returns "" for an issue run
// (no snapshot) or a malformed one.
func failureSnapshotWebURL(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var snap struct {
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return ""
	}
	return snap.WebURL
}

func messageToDTO(m store.RunMessage) apitypes.MessageDTO {
	return apitypes.MessageDTO{
		Seq:           m.Seq,
		Kind:          m.Kind,
		Agent:         textPtrValue(m.Agent.Valid, m.Agent.String),
		AgentInstance: textPtrValue(m.AgentInstance.Valid, m.AgentInstance.String),
		AgentLabel:    textPtrValue(m.AgentLabel.Valid, m.AgentLabel.String),
		Payload:       json.RawMessage(m.Payload),
		CreatedAt:     m.CreatedAt.Time,
	}
}

// steerInputToDTO maps a run_user_inputs row to the web/CLI steer-queue DTO (PRD #95,
// #634). The queue carries both follow_up rows (state from consumed_at) and scope
// operator directives (state from disposition), so Kind and Disposition ride along and
// the client picks the right derivation.
func steerInputToDTO(i store.RunUserInput) apitypes.SteerInputDTO {
	return apitypes.SteerInputDTO{
		ID:          i.ID,
		Kind:        i.Kind,
		Body:        textPtrValue(i.Body.Valid, i.Body.String),
		CreatedAt:   i.CreatedAt.Time,
		ConsumedAt:  timePtr(i.ConsumedAt.Valid, i.ConsumedAt.Time),
		Disposition: textPtrValue(i.Disposition.Valid, i.Disposition.String),
	}
}

// overlayCodexAccountActions sets codex_account_action and the run's own codex_secret_label
// (PRD #1590 D6) on the DTOs of runs held on codex_account_unavailable. runToDTO stays pure, so the read handlers overlay it here. Only
// held runs are looked up, in ONE batched read per call, so a response with no held run costs no
// query. Best-effort: a read error logs and leaves every action and label null, never failing the
// read.
func (h *Handler) overlayCodexAccountActions(ctx context.Context, dtos ...*apitypes.RunDTO) {
	var ids []uuid.UUID
	byID := make(map[uuid.UUID][]*apitypes.RunDTO)
	for _, d := range dtos {
		if d == nil || !workersvc.IsCodexAccountHold(d.Status, d.RecoveryWaitCause) {
			continue
		}
		id, err := uuid.Parse(d.ID)
		if err != nil {
			continue
		}
		if _, seen := byID[id]; !seen {
			ids = append(ids, id)
		}
		byID[id] = append(byID[id], d)
	}
	if len(ids) == 0 || h.wsvc == nil {
		return
	}
	actions, err := h.wsvc.CodexAccountActionsForRuns(ctx, ids)
	if err != nil {
		slog.Error("codex account actions", "runs", len(ids), "error", err)
		return
	}
	for id, ds := range byID {
		hold, ok := actions[id]
		if !ok {
			continue
		}
		for _, d := range ds {
			a := hold.Action
			d.CodexAccountAction = &a
			if hold.Label != nil {
				l := *hold.Label
				d.CodexSecretLabel = &l
			}
		}
	}
}
