package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

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

// runToDTO maps a bare run row to its wire DTO. priorityClass is the D8 display class
// (from fn_run_priority_class via a list column or h.runPriorityClass), passed in
// explicitly so this mapper stays a PURE function of its inputs — no now()/config
// reaches into it.
func runToDTO(r store.Run, priorityClass string) apitypes.RunDTO {
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
		HasPRDLink:     r.IssueIid.Valid && forgesvc.HasPRDLink(r.IssueDescription),
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
		PlanMd:        textPtrValue(r.PlanMd.Valid, r.PlanMd.String),
		PlanSource:    r.PlanSource,
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
	if r.BudgetMaxIterations.Valid {
		v := int(r.BudgetMaxIterations.Int32)
		dto.BudgetMaxIterations = &v
	}
	if r.BudgetWallSeconds.Valid {
		v := int(r.BudgetWallSeconds.Int32)
		dto.BudgetWallSeconds = &v
	}
	// PRD #634 M2: the operator scope ceiling rides the running-report ACK and the claim
	// payload (both built here) so the worker honors it at the loop top and across a
	// re-claim. pgtype.Int4 → *int, null (unbounded) when no scope directive was written.
	if r.ScopeCeiling.Valid {
		v := int(r.ScopeCeiling.Int32)
		dto.ScopeCeiling = &v
	}
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
