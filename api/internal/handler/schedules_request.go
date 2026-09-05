package handler

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// schedules_request.go holds request-to-row mapping: validation, defaulting, merge
// and the per-column encoders for schedule create/patch (PRD #1022 file split).

// validateScheduleConfig is the PURE (no store, no clock beyond the passed-in `now`)
// validator behind create/patch/preview, extracted so it is unit-testable without a
// DB. It normalizes the request — a blank timezone becomes "UTC", sweep labels are
// stripped of blanks — and returns the normalized request plus an HTTP status/message
// (status 0 == valid). It never touches the forge or the store; the per-target field
// invariants it enforces mirror the DB CHECK the row would otherwise fail on insert.
//
// allowSelfImprove gates the self_improve target (PRD #590 follow-up, item 1): a
// self_improve schedule is catalog-enable-only, so a DIRECT create (POST /schedules) must
// keep returning the same "target must be one of: issue, sweep, prompt" 400 (pass false),
// while a user-origin CLONE reconfiguring its own row routes its config PATCH through this
// validator and must be accepted. The patch/merge caller passes true ONLY when the row is
// already self_improve (cur.Target == "self_improve"), so editing an existing self_improve
// row is allowed but CONVERTING an issue/sweep/prompt row into one via PATCH stays a 400 —
// a create-by-patch the direct POST path also blocks.
func validateScheduleConfig(req apitypes.ScheduleRequest, now time.Time, allowSelfImprove bool) (apitypes.ScheduleRequest, int, string) {
	n := req
	n.Timezone = strings.TrimSpace(n.Timezone)
	if n.Timezone == "" {
		n.Timezone = "UTC"
	}

	// max_issues (PRD #274 M2), when present, must be a positive integer regardless of
	// target — a zero/negative cap is a nonsense fan-out bound. Its target scoping (sweep
	// only) is enforced per-target below.
	if n.MaxIssues != nil && *n.MaxIssues <= 0 {
		return n, http.StatusBadRequest, "max_issues must be a positive integer"
	}
	if n.MaxIssues != nil && *n.MaxIssues > MaxSweepIssues {
		return n, http.StatusBadRequest, fmt.Sprintf("max_issues must not exceed %d (leave it unset for unlimited)", MaxSweepIssues)
	}

	// guidance (PRD #274 M3): a blank/whitespace value is "none" — normalize it to nil so a
	// cleared web textarea clears the stored guidance rather than persisting whitespace.
	// The size cap applies regardless of target; the per-target scoping (issue/sweep only,
	// rejected on prompt) is enforced in the switch below.
	if n.Guidance != nil && strings.TrimSpace(*n.Guidance) == "" {
		n.Guidance = nil
	}
	if n.Guidance != nil && len(*n.Guidance) > MaxGuidanceBytes {
		return n, http.StatusUnprocessableEntity, "guidance is too large"
	}

	// model (PRD #300): validated with the shared Decision-4 gate (agenttmpl.ValidateModel),
	// the same single-token/≤100-char rule the per-user Worker model, the judge model, and
	// template models use. Applies to EVERY target (a run's model is orthogonal to what it
	// works on), so it is validated here, above the per-target switch, and rejected in no
	// case arm. A blank/whitespace value means inherit — normalized to nil so a cleared
	// control clears the stored model; a malformed token is a 400.
	if n.Model != nil {
		normalized, err := agenttmpl.ValidateModel(*n.Model)
		if err != nil {
			return n, http.StatusBadRequest, "model: " + err.Error()
		}
		if normalized == "" {
			n.Model = nil
		} else {
			n.Model = &normalized
		}
	}

	// output_mode (PRD #929 M1): a blank/whitespace value is "none" — normalize it to nil
	// so a cleared control clears the stored mode back to inherit-the-catalog-default. It is
	// prompt-only; the per-target scoping (rejected on issue/sweep/self_improve, enum-checked
	// on prompt) is enforced in the switch below.
	if n.OutputMode != nil && strings.TrimSpace(*n.OutputMode) == "" {
		n.OutputMode = nil
	}

	switch n.Target {
	case "issue":
		if n.IssueIID == nil || *n.IssueIID <= 0 {
			return n, http.StatusBadRequest, "issue_iid is required and must be a positive integer for an issue schedule"
		}
		if n.MaxIssues != nil {
			return n, http.StatusBadRequest, "max_issues is only valid for a sweep schedule"
		}
		// output_mode is prompt-only (PRD #929 M1, D6): an issue schedule's output is a run
		// on an existing issue, not a proposal, so a set mode is a 422 rather than silently
		// ignored (a silently-ignored option teaches the wrong mental model).
		if n.OutputMode != nil {
			return n, http.StatusUnprocessableEntity, "output_mode is only valid for a prompt schedule"
		}
	case "sweep":
		// Labels are optional; an empty selector defaults to the PRD label at fire time
		// (Decisions 7/9). Drop blanks so a stored [""] cannot defeat that.
		n.Labels = nonBlankLabels(n.Labels)
		// output_mode is prompt-only (PRD #929 M1, D6): a sweep's output is a run on an
		// existing issue, not a proposal — reject a set mode (422).
		if n.OutputMode != nil {
			return n, http.StatusUnprocessableEntity, "output_mode is only valid for a prompt schedule"
		}
	case "prompt":
		if strings.TrimSpace(n.Prompt) == "" {
			return n, http.StatusBadRequest, "prompt is required for a prompt schedule"
		}
		if len(n.Prompt) > workersvc.MaxIssueDescriptionBytes {
			return n, http.StatusUnprocessableEntity, "prompt is too large to run"
		}
		if n.MaxIssues != nil {
			return n, http.StatusBadRequest, "max_issues is only valid for a sweep schedule"
		}
		// Guidance steers HOW an issue/sweep run approaches a task; a prompt schedule
		// carries its own free-form text, so guidance is out of scope there (PRD #274 M3,
		// Out of scope). A blank was already normalized to nil above, so only a real value
		// reaches here.
		if n.Guidance != nil {
			return n, http.StatusBadRequest, "guidance is not valid for a prompt schedule"
		}
		// output_mode (PRD #929 M1) is honored ONLY for a prompt target. A blank was already
		// normalized to nil above, so only a real value reaches here; enum-check it against
		// the mr/issues set (a malformed value is a 400). A nil value inherits the catalog
		// default at fire time.
		if n.OutputMode != nil {
			switch *n.OutputMode {
			case schedtmpl.OutputModeMR, schedtmpl.OutputModeIssues:
			default:
				return n, http.StatusBadRequest, "output_mode must be one of: mr, issues"
			}
		}
	case "self_improve":
		// A self_improve schedule is catalog-enable-only: a DIRECT create (POST /schedules)
		// stays rejected (defense in depth), but a user-origin CLONE (CloneSchedule) is a valid
		// row the owner may reconfigure, so its config PATCH (which routes through this validator)
		// must be accepted. allowSelfImprove is true only on the patch/merge caller.
		// (PRD #590 follow-up, item 1.)
		if !allowSelfImprove {
			return n, http.StatusBadRequest, "target must be one of: issue, sweep, prompt"
		}
		// A self_improve row carries only cadence/model — no issue_iid/labels/prompt and no
		// sweep cap (mirrors the issue/prompt arms rejecting a stray max_issues).
		if n.MaxIssues != nil {
			return n, http.StatusBadRequest, "max_issues is only valid for a sweep schedule"
		}
		// output_mode is prompt-only (PRD #929 M1): a self_improve run produces no proposal,
		// so a set mode is rejected (422), like the issue/sweep arms above.
		if n.OutputMode != nil {
			return n, http.StatusUnprocessableEntity, "output_mode is only valid for a prompt schedule"
		}
		// Item 2: a self_improve run is ALWAYS auto-approved (CreateSelfImproveRun hardcodes
		// auto_approve=true, selfimprove.sql). Force the stored flag true so it can never
		// misrepresent as manual-approve; do not honor a user-set false.
		approve := true
		n.AutoApprove = &approve
	default:
		return n, http.StatusBadRequest, "target must be one of: issue, sweep, prompt"
	}

	switch n.Timing {
	case "recurring":
		if err := schedsvc.ValidateCron(n.CronExpr); err != nil {
			return n, http.StatusBadRequest, "invalid cron expression"
		}
		if _, err := time.LoadLocation(n.Timezone); err != nil {
			return n, http.StatusBadRequest, "invalid timezone"
		}
	case "once":
		if n.RunAt == nil {
			return n, http.StatusBadRequest, "run_at is required for a one-time schedule"
		}
		if _, err := schedsvc.OnceFire(*n.RunAt, now); err != nil {
			return n, http.StatusUnprocessableEntity, "run_at must be in the future"
		}
	default:
		return n, http.StatusBadRequest, "timing must be one of: once, recurring"
	}
	return n, 0, ""
}

// applyCreateDefaults fills the create-time defaults for the three tri-state flags:
// auto_approve ON (PRD #241 Decision 4), wait_on_limit ON (PRD #274 Decision 1a — a
// schedule is unattended, so a fired run should park on the usage limit rather than die),
// enabled on. A nil pointer means the caller omitted the field; a present pointer (even to
// false) is respected. Create-time only: existing rows are not rewritten (PRD #274
// Decision 4).
//
// max_issues (PRD #274 M2) defaults to 10 for a NEW SWEEP with no explicit value — a
// bounded fan-out is the safer default (Decision 2/4). It is left nil for issue/prompt
// targets, where max_issues is not a valid field (validateScheduleConfig rejects it there).
func applyCreateDefaults(req *apitypes.ScheduleRequest) {
	if req.AutoApprove == nil {
		v := true
		req.AutoApprove = &v
	}
	if req.WaitOnLimit == nil {
		v := true
		req.WaitOnLimit = &v
	}
	if req.Enabled == nil {
		v := true
		req.Enabled = &v
	}
	if req.Target == "sweep" && req.MaxIssues == nil {
		v := 10
		req.MaxIssues = &v
	}
}

// onlyEnabled reports whether a PATCH body carries ONLY the enabled flag, so the
// handler can flip pause/resume without re-validating (and re-writing) the whole config.
func onlyEnabled(req apitypes.ScheduleRequest) bool {
	return req.Enabled != nil &&
		req.Target == "" && req.Timing == "" &&
		req.RepoID == "" &&
		req.IssueIID == nil && req.Labels == nil && req.Prompt == "" &&
		req.CronExpr == "" && req.RunAt == nil && req.Timezone == "" &&
		req.AutoApprove == nil && req.WaitOnLimit == nil && req.MrReworkEnabled == nil && req.MaxIssues == nil &&
		req.Guidance == nil && req.Model == nil && req.OutputMode == nil && req.OverrideSubagentModel == nil
}

// mergeSchedule overlays the provided PATCH fields onto the current stored schedule,
// producing the full ScheduleRequest to re-validate. Pointer/empty provided fields keep
// the current value; the three flags always come back non-nil (seeded from cur) so the
// caller can dereference them.
func mergeSchedule(cur store.RunSchedule, req apitypes.ScheduleRequest) apitypes.ScheduleRequest {
	autoApprove := cur.AutoApprove
	waitOnLimit := cur.WaitOnLimit
	enabled := cur.Enabled
	m := apitypes.ScheduleRequest{
		Target:      cur.Target,
		Labels:      scheduleLabelsToSlice(cur.Labels),
		Timing:      cur.Timing,
		Timezone:    cur.Timezone,
		AutoApprove: &autoApprove,
		WaitOnLimit: &waitOnLimit,
		Enabled:     &enabled,
	}
	if cur.IssueIid.Valid {
		v := cur.IssueIid.Int64
		m.IssueIID = &v
	}
	if cur.Prompt.Valid {
		m.Prompt = cur.Prompt.String
	}
	if cur.CronExpr.Valid {
		m.CronExpr = cur.CronExpr.String
	}
	if cur.RunAt.Valid {
		t := cur.RunAt.Time
		m.RunAt = &t
	}

	if req.Target != "" {
		m.Target = req.Target
	}
	if req.Timing != "" {
		m.Timing = req.Timing
	}
	if req.IssueIID != nil {
		m.IssueIID = req.IssueIID
	}
	if req.Labels != nil {
		m.Labels = req.Labels
	}
	if req.Prompt != "" {
		m.Prompt = req.Prompt
	}
	if req.CronExpr != "" {
		m.CronExpr = req.CronExpr
	}
	if req.RunAt != nil {
		m.RunAt = req.RunAt
	}
	if req.Timezone != "" {
		m.Timezone = req.Timezone
	}
	if req.AutoApprove != nil {
		m.AutoApprove = req.AutoApprove
	}
	if req.WaitOnLimit != nil {
		m.WaitOnLimit = req.WaitOnLimit
	}
	// max_issues (PRD #274 M2) takes the request value DIRECTLY — it is NOT seeded from
	// cur and kept-on-empty like the fields above. RATIONALE: a config PATCH rewrites the
	// whole row, and the only sparse PATCH (enabled-only) is short-circuited by onlyEnabled
	// before this merge, so the web always sends the full config. Replace-semantics is what
	// makes clear-to-unlimited work: a `max_issues: null` in the PATCH must reach the DB as
	// NULL. A seed-and-keep would make an explicit null indistinguishable from "omitted",
	// leaving unlimited unreachable once a sweep has any cap set. This is the deliberate
	// difference from the keep-on-empty fields — the *int tri-state carries "cleared".
	m.MaxIssues = req.MaxIssues
	// guidance (PRD #274 M3) takes the request value DIRECTLY for the same reason as
	// max_issues above: a config PATCH rewrites the whole row (the enabled-only sparse PATCH
	// is short-circuited by onlyEnabled), so the web sends the full config. Replace-semantics
	// is what makes clear-to-none work — a `guidance: null` (or a cleared textarea, which
	// validateScheduleConfig normalizes to nil) must reach the DB as NULL. A seed-and-keep
	// would make an explicit clear indistinguishable from "omitted".
	m.Guidance = req.Guidance
	// model (PRD #300) takes the request value DIRECTLY, same replace-semantics as
	// max_issues/guidance above: the config PATCH rewrites the whole row (enabled-only is
	// short-circuited by onlyEnabled), so a `model: null` (or a cleared control) must reach
	// the DB as NULL = inherit. A seed-and-keep would make an explicit clear indistinguishable
	// from omitted.
	m.Model = req.Model
	// output_mode (PRD #929 M1) takes the request value DIRECTLY, same replace-semantics as
	// model/guidance/max_issues above: a config PATCH rewrites the whole row (enabled-only is
	// short-circuited by onlyEnabled), so an `output_mode: null` (or a cleared control) must
	// reach the DB as NULL = inherit the catalog default. A seed-and-keep would make an
	// explicit clear indistinguishable from omitted.
	m.OutputMode = req.OutputMode
	// mr_rework (PRD #841 M2) takes the request value DIRECTLY, same replace-semantics as
	// model/guidance/max_issues above: a config PATCH rewrites the whole row (enabled-only
	// is short-circuited by onlyEnabled), so a `mr_rework_enabled: null` must reach the DB
	// as NULL = inherit (D5). A seed-and-keep would make an explicit clear indistinguishable
	// from omitted. It is the tri-state *bool, so nil ≠ false here (unlike wait_on_limit).
	m.MrReworkEnabled = req.MrReworkEnabled
	// override_subagent_model (PRD #305) takes the request value DIRECTLY, same
	// replace-semantics as model/guidance/max_issues above: the config PATCH rewrites the
	// whole row (enabled-only is short-circuited by onlyEnabled), so nil ≡ false (Decision 5)
	// means an omitted value turns it off — which the web avoids by sending the full config.
	m.OverrideSubagentModel = req.OverrideSubagentModel
	// repo_id (Feature A, PRD #344) is seeded from the CURRENT row and overridden only by
	// a non-empty request value — keep-on-empty, deliberately NOT the replace-semantics of
	// max_issues/guidance/model above. RATIONALE: run_schedules.repo_id is NOT NULL + FK, and
	// every config edit flows through UpdateRunSchedule. If a config PATCH that omits --repo
	// let repo_id fall to "" -> uuid.Nil, the UPDATE would SET repo_id = '00000000-...' and
	// violate the FK -> 500 on EVERY config edit, not just repoints (S1). So keep the stored id.
	m.RepoID = cur.RepoID.String()
	if req.RepoID != "" {
		m.RepoID = req.RepoID
	}
	return m
}

// scheduleRepoChange classifies the merged PATCH's repo_id against the current row's repo.
// It is PURE (no store) so it unit-tests the 400/422 grounds directly; the caller performs
// the store-backed ownership check (404) only when repoint is true. merged.RepoID is always
// non-empty (mergeSchedule seeds it from cur), so uuid.Parse fails only when the caller sent
// a malformed repo_id -> 400. A parsed id equal to the current repo is not a repoint. A repoint
// of an issue-target schedule is REJECTED 422 (PRD #344 D4): issue_iid is repo-relative, so the
// same IID in the new repo is a different, unrelated issue that auto_approve would run to an MR.
func scheduleRepoChange(cur store.RunSchedule, merged apitypes.ScheduleRequest) (repoID uuid.UUID, repoint bool, status int, msg string) {
	id, err := uuid.Parse(merged.RepoID)
	if err != nil {
		return uuid.Nil, false, http.StatusBadRequest, "invalid repo id"
	}
	if id == cur.RepoID {
		return id, false, 0, ""
	}
	if merged.Target == "issue" {
		return uuid.Nil, false, http.StatusUnprocessableEntity,
			"repointing an issue-target schedule is not supported; delete and recreate it against the new repo"
	}
	return id, true, 0, ""
}

// scheduleColumns maps a normalized request to the per-target nullable store columns.
// Only the columns the target/timing actually uses are set Valid; the rest stay SQL
// NULL, matching the DB's field-presence CHECK.
func scheduleColumns(m apitypes.ScheduleRequest) (issueIID pgtype.Int8, labels []byte, prompt pgtype.Text, cronExpr pgtype.Text, runAt pgtype.Timestamptz) {
	if m.Target == "issue" && m.IssueIID != nil {
		issueIID = pgtype.Int8{Int64: *m.IssueIID, Valid: true}
	}
	if m.Target == "sweep" {
		labels = marshalLabels(m.Labels)
	}
	if m.Target == "prompt" {
		prompt = pgtype.Text{String: m.Prompt, Valid: true}
	}
	if m.Timing == "recurring" {
		cronExpr = pgtype.Text{String: m.CronExpr, Valid: true}
	}
	if m.Timing == "once" && m.RunAt != nil {
		runAt = pgtype.Timestamptz{Time: m.RunAt.UTC(), Valid: true}
	}
	return
}

// maxIssuesColumn maps the request's *int max_issues to the nullable store column (PRD
// #274 M2). It is set Valid ONLY for the sweep target (mirroring labels): a non-sweep
// row must carry SQL NULL, so an issue/prompt schedule never persists a stray cap even
// if one slipped past validation. A nil pointer is SQL NULL = unlimited.
func maxIssuesColumn(m apitypes.ScheduleRequest) pgtype.Int4 {
	if m.Target == "sweep" && m.MaxIssues != nil {
		return pgtype.Int4{Int32: int32(*m.MaxIssues), Valid: true} //nolint:gosec // G115: MaxIssues is validated to [1, MaxSweepIssues] before persist
	}
	return pgtype.Int4{}
}

// guidanceColumn maps the request's *string guidance to the nullable store column (PRD
// #274 M3). It is set Valid ONLY for the issue/sweep targets and only for a non-blank
// value: the prompt target carries its own text (validateScheduleConfig rejects guidance
// there), and a blank was already normalized to nil, so a non-issue/sweep row never
// persists a stray guidance value. A nil pointer is SQL NULL = no guidance.
func guidanceColumn(m apitypes.ScheduleRequest) pgtype.Text {
	if (m.Target == "issue" || m.Target == "sweep") && m.Guidance != nil && strings.TrimSpace(*m.Guidance) != "" {
		return pgtype.Text{String: *m.Guidance, Valid: true}
	}
	return pgtype.Text{}
}

// modelColumn maps the request's *string model to the nullable store column (PRD #300).
// Unlike maxIssuesColumn/guidanceColumn it is NOT target-scoped — a per-schedule model
// applies to every target. validateScheduleConfig already normalized a blank to nil and
// rejected a malformed token, so a nil pointer here is SQL NULL = inherit.
func modelColumn(m apitypes.ScheduleRequest) pgtype.Text {
	if m.Model != nil && strings.TrimSpace(*m.Model) != "" {
		return pgtype.Text{String: *m.Model, Valid: true}
	}
	return pgtype.Text{}
}

// outputModeColumn maps the request's *string output_mode to the nullable store column
// (PRD #929 M1). It is set Valid ONLY for the prompt target (mirroring guidanceColumn's
// target scoping): a non-prompt row must carry SQL NULL, so an issue/sweep/self_improve
// schedule never persists a stray mode even if one slipped past validation. A nil pointer
// is SQL NULL = inherit the catalog default at fire time.
func outputModeColumn(m apitypes.ScheduleRequest) pgtype.Text {
	if m.Target == "prompt" && m.OutputMode != nil && strings.TrimSpace(*m.OutputMode) != "" {
		return pgtype.Text{String: *m.OutputMode, Valid: true}
	}
	return pgtype.Text{}
}

// overrideSubagentModelColumn maps the request's *bool to the NOT NULL DEFAULT false
// column (PRD #305): nil (absent) ≡ false. Unlike modelColumn there is no tri-state —
// the flag is off or on (Decision 5), not target-scoped (applies to every target).
func overrideSubagentModelColumn(m apitypes.ScheduleRequest) bool {
	return m.OverrideSubagentModel != nil && *m.OverrideSubagentModel
}

// parseSiblingGroupColumn validates the optional, create-only sibling_group_id (PRD #636
// Decision 4) into the nullable store column. A nil/blank value is SQL NULL (a standalone
// row, the common single-repo case); a malformed uuid writes a 400 and returns ok=false so
// the caller stops before the insert (a bad value is a client error, not a 500).
func parseSiblingGroupColumn(w http.ResponseWriter, s *string) (pgtype.UUID, bool) {
	if s == nil || strings.TrimSpace(*s) == "" {
		return pgtype.UUID{}, true
	}
	id, err := uuid.Parse(strings.TrimSpace(*s))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid sibling_group_id")
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: id, Valid: true}, true
}

// nextFireFor computes the durable next_fire_at from a validated request: the next cron
// fire for a recurring schedule, or the normalized run_at for a once schedule.
func nextFireFor(m apitypes.ScheduleRequest, now time.Time) (pgtype.Timestamptz, error) {
	switch m.Timing {
	case "recurring":
		t, err := schedsvc.NextFire(m.CronExpr, m.Timezone, now)
		if err != nil {
			return pgtype.Timestamptz{}, err
		}
		return pgtype.Timestamptz{Time: t, Valid: true}, nil
	case "once":
		if m.RunAt == nil {
			return pgtype.Timestamptz{}, fmt.Errorf("once schedule missing run_at")
		}
		t, err := schedsvc.OnceFire(*m.RunAt, now)
		if err != nil {
			return pgtype.Timestamptz{}, err
		}
		return pgtype.Timestamptz{Time: t, Valid: true}, nil
	default:
		return pgtype.Timestamptz{}, fmt.Errorf("unknown timing %q", m.Timing)
	}
}

// nonBlankLabels trims and drops blank entries from a label selector.
func nonBlankLabels(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
