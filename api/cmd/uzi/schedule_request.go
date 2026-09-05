package main

// ScheduleRequest builders for `schedule create` and `schedule edit`, split out of
// schedule.go (PRD #1009 M2). Declaration motion only.

import (
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// atLayouts are the timestamp forms `--at` accepts, tried in order. RFC3339 is the
// canonical one; the minute-precision variant (no seconds) is accepted because that
// is the shape a human writes and the mock's own example uses
// ("2026-08-08T09:00+03:00"). The parsed instant is forwarded as-is; the server is
// the authority on "must be in the future" (422).
var atLayouts = []string{time.RFC3339, "2006-01-02T15:04Z07:00"}

// buildScheduleRequest assembles the ScheduleRequest from the create flags, enforcing
// the one-of TARGET and one-of TIMING constraints client-side so a bad invocation is a
// clean exit-2 usage error before any request is sent (the server also enforces them).
// It returns the request and the (one or more) repo ids to fan the create out across.
func buildScheduleRequest(cmd *cobra.Command) (apitypes.ScheduleRequest, []string, error) {
	repos, _ := cmd.Flags().GetStringArray("repo")
	repos = nonBlankTrimmed(repos)
	if len(repos) == 0 {
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--repo is required (a repo id from `uzi repo list`)")
	}

	issue, _ := cmd.Flags().GetInt64("issue")
	sweep, _ := cmd.Flags().GetBool("sweep")
	prompt, _ := cmd.Flags().GetString("prompt")
	labels, _ := cmd.Flags().GetStringArray("label")

	// Exactly one target. Use Changed() for --issue/--prompt so an explicit --issue 0
	// is still rejected below (positive-IID check) rather than read as "unset".
	issueSet := cmd.Flags().Changed("issue")
	promptSet := cmd.Flags().Changed("prompt")
	targets := 0
	for _, on := range []bool{issueSet, sweep, promptSet} {
		if on {
			targets++
		}
	}
	if targets != 1 {
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage,
			"specify exactly one target: --issue <iid>, --sweep, or --prompt <text>")
	}
	if len(labels) > 0 && !sweep {
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--label is only valid with --sweep")
	}
	// --max-issues is sweep-only; reject an EXPLICIT set on a non-sweep target (an
	// unchanged default is silently ignored, mirroring the --label rule above).
	if cmd.Flags().Changed("max-issues") && !sweep {
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--max-issues is only valid with --sweep")
	}
	// --guidance is issue/sweep-only; reject an EXPLICIT set on the prompt target (a prompt
	// carries its own text). --guidance is distinct from the --prompt target selector.
	guidanceSet := cmd.Flags().Changed("guidance")
	if guidanceSet && !issueSet && !sweep {
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--guidance is only valid with --issue or --sweep")
	}
	// --output is prompt-target-only (PRD #929 M1): it selects a proposal run's output shape,
	// which is meaningless for issue/sweep (they run on an existing issue, not a proposal).
	// Reject an EXPLICIT set on a non-prompt target, mirroring the --guidance guard above.
	if cmd.Flags().Changed("output") && !promptSet {
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--output is only valid with --prompt")
	}

	req := apitypes.ScheduleRequest{}
	switch {
	case issueSet:
		if issue <= 0 {
			return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--issue must be a positive issue IID")
		}
		req.Target = schedTargetIssue
		req.IssueIID = &issue
	case sweep:
		req.Target = schedTargetSweep
		req.Labels = labels
		// Only a sweep sends max_issues. The flag default (10) matches the server default,
		// so a plain `--sweep` naturally requests a bounded fan-out; --max-issues overrides.
		maxIssues, _ := cmd.Flags().GetInt("max-issues")
		req.MaxIssues = &maxIssues
	case promptSet:
		if strings.TrimSpace(prompt) == "" {
			return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--prompt needs task text")
		}
		req.Target = schedTargetPrompt
		req.Prompt = prompt
	}

	// Guidance rides only on issue/sweep targets (guarded above). Send it only when set so
	// an unset flag stays absent (nil) rather than clearing on a PATCH-shaped payload.
	if guidanceSet && (issueSet || sweep) {
		guidance, _ := cmd.Flags().GetString("guidance")
		req.Guidance = &guidance
	}

	// --model is valid on every target (a run's model is orthogonal to what it works on),
	// so unlike --guidance/--max-issues it carries no target guard. Send it only when set so
	// an unset flag stays absent (nil) rather than clearing.
	if cmd.Flags().Changed("model") {
		model, _ := cmd.Flags().GetString("model")
		req.Model = &model
	}

	// --output rides ONLY the prompt target (guarded above): a proposal run's output shape
	// (PRD #929 M1). Send it only when set so an unset flag stays absent (nil), matching
	// --model's replace-semantics; the server validates the "mr"/"issues"/"" enum.
	if cmd.Flags().Changed("output") {
		v, _ := cmd.Flags().GetString("output")
		req.OutputMode = &v
	}

	// PRD #305: opt-in to override every subagent's model with the run model. Only set
	// when the caller passed the flag, so an omitted flag stays nil (server default false).
	if cmd.Flags().Changed("apply-model-to-agents") {
		v, _ := cmd.Flags().GetBool("apply-model-to-agents")
		req.OverrideSubagentModel = &v
	}

	// Exactly one timing.
	atStr, _ := cmd.Flags().GetString("at")
	cron, _ := cmd.Flags().GetString("cron")
	atSet := cmd.Flags().Changed("at")
	cronSet := cmd.Flags().Changed("cron")
	if atSet == cronSet {
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage,
			"specify exactly one timing: --at <RFC3339> (once) or --cron <expr> (recurring)")
	}
	tz, _ := cmd.Flags().GetString("tz")
	req.Timezone = strings.TrimSpace(tz)
	if atSet {
		runAt, err := parseAt(atStr)
		if err != nil {
			return apitypes.ScheduleRequest{}, nil, err
		}
		req.Timing = schedTimingOnce
		req.RunAt = &runAt
	} else {
		req.Timing = schedTimingRecurring
		req.CronExpr = cron
	}

	// auto_approve and wait_on_limit carry a client-side default (on / off), so they are
	// always sent as pointers rather than omitted — the flag value IS the statement.
	autoApprove, _ := cmd.Flags().GetBool("auto-approve")
	waitOnLimit, _ := cmd.Flags().GetBool("wait-on-limit")
	req.AutoApprove = &autoApprove
	req.WaitOnLimit = &waitOnLimit

	// mr_rework is tri-state and its schedule default is INHERIT (nil), not on (PRD #841
	// D5) — so it is Changed()-gated via mrReworkFlag rather than always-sent like
	// wait_on_limit above: an omitted flag stays nil so the fired jobs follow the owner's
	// global setting, and only an explicit --mr-rework[=false] stamps the schedule.
	req.MrReworkEnabled = mrReworkFlag(cmd)

	// --enabled is only sent when the caller passed it, so an omitted flag stays nil and
	// the server's create default (enabled=true) applies. Use Changed() rather than the
	// always-send pointer pattern of --auto-approve so today's default behavior is byte-identical.
	if cmd.Flags().Changed("enabled") {
		enabled, _ := cmd.Flags().GetBool("enabled")
		req.Enabled = &enabled
	}
	return req, repos, nil
}

// parseAt parses a --at timestamp, tolerating both RFC3339 and its minute-precision
// form. A malformed value is a usage error before any request (the server is the
// authority on whether a well-formed time is in the future).
func parseAt(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range atLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, uzicli.Exitf(uzicli.ExitUsage,
		"--at must be an RFC3339 timestamp (e.g. 2026-08-08T09:00:00Z)")
}

// buildScheduleEditRequest builds a full ScheduleRequest for `schedule edit` from the
// FETCHED schedule, then overlays only the flags the caller explicitly Changed().
//
// A typed rebuild from the DTO's config fields (rather than re-posting the raw DTO) is
// deliberate: the PATCH endpoint decodes with DisallowUnknownFields, so the DTO's
// response-only fields (id, status, created_at, updated_at, next_fire_at, last_fired_at,
// next_fires, repo_id, repo_path) would be rejected as unknown → 400. It also compensates
// for mergeSchedule, which is keep-on-empty for most fields but takes max_issues,
// guidance, model and override_subagent_model STRAIGHT from the request (nil clears them)
// — so those MUST be re-sent from the fetched row, or a --cron-only edit would silently
// wipe them. Enabled is left nil so a config edit never touches the pause flag
// (enable/disable is pause/resume's job).
func buildScheduleEditRequest(cmd *cobra.Command, s apitypes.ScheduleDTO) (apitypes.ScheduleRequest, error) {
	if s.Origin == schedOriginDefault {
		return buildDefaultScheduleEditRequest(cmd, s)
	}
	req := apitypes.ScheduleRequest{
		Target:    s.Target,
		IssueIID:  s.IssueIID,
		Labels:    s.Labels,
		Prompt:    s.Prompt,
		Timing:    s.Timing,
		CronExpr:  s.CronExpr,
		RunAt:     s.RunAt,
		Timezone:  s.Timezone,
		MaxIssues: s.MaxIssues,
		Guidance:  s.Guidance,
		// PRD #300 replace-semantics: restate or a partial edit (e.g. --cron only) wipes
		// the stored model, since mergeSchedule does m.Model = req.Model (pre-existing bug).
		Model: s.Model,
		// PRD #929 M1 replace-semantics: same class as Model — restate the stored output mode
		// (a direct pointer copy) or a partial edit wipes it. nil for a non-prompt row, so
		// restating it is a no-op there; an explicit --output overrides below.
		OutputMode: s.OutputMode,
		// PRD #305 replace-semantics: same class — restate the subagent override or a
		// partial edit wipes it. The DTO always sets this bool non-nil.
		OverrideSubagentModel: s.OverrideSubagentModel,
	}
	// DTO carries these as plain bool; re-send as pointer copies (the config PATCH path
	// always restates them). Enabled stays nil — see the doc comment.
	autoApprove := s.AutoApprove
	waitOnLimit := s.WaitOnLimit
	req.AutoApprove = &autoApprove
	req.WaitOnLimit = &waitOnLimit
	// mr_rework is a tri-state *bool (PRD #841): RESTATE the fetched value so a partial edit
	// (e.g. --cron only) does not wipe the stored override under the server's replace-semantics
	// — mirroring model/wait_on_limit. Its inherit state is nil, and restating nil re-sends
	// nil, so an inherit schedule stays inherit. An explicit --mr-rework overrides below.
	req.MrReworkEnabled = s.MrReworkEnabled

	f := cmd.Flags()
	cronSet := f.Changed("cron")
	atSet := f.Changed("at")
	tzSet := f.Changed("tz")
	promptSet := f.Changed("prompt")
	labelSet := f.Changed("label")
	autoSet := f.Changed("auto-approve")
	waitSet := f.Changed("wait-on-limit")
	guidanceSet := f.Changed("guidance")
	maxIssuesSet := f.Changed("max-issues")
	clearGuidance := f.Changed("clear-guidance")
	clearMaxIssues := f.Changed("clear-max-issues")
	clearMrRework := f.Changed("clear-mr-rework")
	repoSet := f.Changed("repo")

	// --cron and --at both restate TIMING; at most one may win.
	if cronSet && atSet {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage,
			"specify at most one timing: --cron <expr> (recurring) or --at <RFC3339> (once)")
	}
	// Target-scoped flags: reject an EXPLICIT set on the wrong target (mirrors create).
	if promptSet && s.Target != schedTargetPrompt {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--prompt is only valid on a prompt-target schedule")
	}
	if f.Changed("output") && s.Target != schedTargetPrompt {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--output is only valid on a prompt-target schedule")
	}
	if labelSet && s.Target != schedTargetSweep {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--label is only valid on a sweep-target schedule")
	}
	if (guidanceSet || clearGuidance) && s.Target != schedTargetIssue && s.Target != schedTargetSweep {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--guidance/--clear-guidance are only valid on an issue or sweep target")
	}
	if (maxIssuesSet || clearMaxIssues) && s.Target != schedTargetSweep {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--max-issues/--clear-max-issues are only valid on a sweep target")
	}
	// Set-vs-clear conflicts: a field cannot be both changed and cleared in one edit.
	if guidanceSet && clearGuidance {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--guidance and --clear-guidance are mutually exclusive")
	}
	if maxIssuesSet && clearMaxIssues {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--max-issues and --clear-max-issues are mutually exclusive")
	}
	if f.Changed("mr-rework") && clearMrRework {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--mr-rework and --clear-mr-rework are mutually exclusive")
	}

	changed := false
	if cronSet {
		cron, _ := f.GetString("cron")
		req.Timing = schedTimingRecurring
		req.CronExpr = cron
		req.RunAt = nil
		changed = true
	}
	if atSet {
		atStr, _ := f.GetString("at")
		runAt, err := parseAt(atStr)
		if err != nil {
			return apitypes.ScheduleRequest{}, err
		}
		req.Timing = schedTimingOnce
		req.RunAt = &runAt
		req.CronExpr = ""
		changed = true
	}
	if tzSet {
		tz, _ := f.GetString("tz")
		req.Timezone = strings.TrimSpace(tz)
		changed = true
	}
	if promptSet {
		req.Prompt, _ = f.GetString("prompt")
		changed = true
	}
	if labelSet {
		req.Labels, _ = f.GetStringArray("label")
		changed = true
	}
	if autoSet {
		v, _ := f.GetBool("auto-approve")
		req.AutoApprove = &v
		changed = true
	}
	if waitSet {
		v, _ := f.GetBool("wait-on-limit")
		req.WaitOnLimit = &v
		changed = true
	}
	if f.Changed("mr-rework") {
		v, _ := f.GetBool("mr-rework")
		req.MrReworkEnabled = &v
		changed = true
	}
	if clearMrRework {
		// Explicit nil reaches the server as `mr_rework_enabled: null` (the field is not
		// omitempty) and mergeSchedule's replace-semantics clears the stored override back
		// to inherit. Restated auto_approve/wait_on_limit keep this off the onlyEnabled path.
		req.MrReworkEnabled = nil
		changed = true
	}
	if guidanceSet {
		v, _ := f.GetString("guidance")
		req.Guidance = &v
		changed = true
	}
	if clearGuidance {
		req.Guidance = nil
		changed = true
	}
	if maxIssuesSet {
		v, _ := f.GetInt("max-issues")
		req.MaxIssues = &v
		changed = true
	}
	if clearMaxIssues {
		req.MaxIssues = nil
		changed = true
	}
	if f.Changed("model") {
		v, _ := f.GetString("model")
		req.Model = &v
		changed = true
	}
	if f.Changed("output") {
		v, _ := f.GetString("output")
		req.OutputMode = &v
		changed = true
	}
	if f.Changed("apply-model-to-agents") {
		v, _ := f.GetBool("apply-model-to-agents")
		req.OverrideSubagentModel = &v
		changed = true
	}
	if repoSet {
		v, _ := f.GetString("repo")
		// Keep-on-empty in the server merge: only a non-empty value repoints. Trim so a
		// stray space does not reach the server's uuid.Parse as a malformed id.
		req.RepoID = strings.TrimSpace(v)
		changed = true
	}
	if !changed {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "nothing to edit (pass at least one field to change)")
	}
	return req, nil
}

// buildDefaultScheduleEditRequest builds the PATCH body for a `schedule edit` on a
// DEFAULT-origin schedule (PRD #589). The server routes a default row to
// patchDefaultScheduleConfig, whose guard rejects any request carrying a catalog-owned
// field (prompt/labels/guidance/target/repo/issue/timing/run_at). So — unlike the user
// path's full rebuild — this constructs a FRESH minimal request with ONLY the
// catalog-editable fields set, leaving the catalog-owned ones at their zero value so the
// guard passes. A copy-then-zero approach would NOT work: a prompt-target default's DTO
// surfaces Guidance as a non-nil &"" (scheduleDTO), which trips the guard's
// `req.Guidance != nil` check even when the user passed no --guidance.
//
// User-editable on a default: cron, timezone, auto_approve, wait_on_limit, and — for a
// sweep — max_issues. For a sweep, max_issues is RESTATED from the fetched row because
// patchDefaultScheduleConfig uses replace-semantics on it (an omitted value clears the cap
// to unlimited). model and override_subagent_model ARE now owner-editable on a default
// (issue #691): --model sets the run model (empty string clears it back to the Worker-model
// default) and --apply-model-to-agents sets override_subagent_model, since
// patchDefaultScheduleConfig reads both req.Model and req.OverrideSubagentModel. Both are
// restated from the fetched row (replace-semantics) so a partial edit does not drop them,
// and an explicit flag overrides the restated value.
//
// Guidance is owner-editable on a PROMPT-target default (PRD #662 M1) and a SWEEP-target
// default (issue #675, where it is an overlay composed onto the baked catalog guidance at
// fire time): --guidance sets it and --clear-guidance blanks it, and the fetched value is
// RESTATED so a partial edit does not wipe it under the server's replace-semantics (for a
// sweep default the restated value is the OVERLAY, never the baked catalog value). On an
// issue/self_improve default guidance stays catalog-owned, so those flags are rejected
// client-side. The remaining catalog-owned flags (--prompt/--label/--repo/--at) are likewise
// rejected client-side with a usage error pointing at `schedule clone`.
func buildDefaultScheduleEditRequest(cmd *cobra.Command, s apitypes.ScheduleDTO) (apitypes.ScheduleRequest, error) {
	f := cmd.Flags()

	// Catalog-owned fields cannot be edited on a default; fail fast client-side with a
	// message naming clone, rather than forwarding to a server 400. Guidance is the one
	// exception: on a PROMPT-target default (PRD #662 M1) or a SWEEP-target default (issue
	// #675) the owner may edit it, so it is handled below rather than in this blanket reject.
	for _, flag := range []string{"prompt", "label", "repo", "at"} {
		if f.Changed(flag) {
			return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage,
				"--%s is catalog-owned on a default schedule; clone it first with `uzi schedule clone`", flag)
		}
	}
	// Guidance is owner-editable on a prompt-target default (issue #662) and on a sweep-target
	// default (issue #675: an owner overlay composed onto the baked catalog guidance at fire
	// time). On an issue/self_improve default it stays catalog-owned (the server still 400s a
	// guidance edit), so reject it here.
	guidanceSet := f.Changed("guidance")
	clearGuidance := f.Changed("clear-guidance")
	if (guidanceSet || clearGuidance) && s.Target != schedTargetPrompt && s.Target != schedTargetSweep {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage,
			"--guidance/--clear-guidance are catalog-owned on this default schedule; clone it first with `uzi schedule clone`")
	}
	if guidanceSet && clearGuidance {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--guidance and --clear-guidance are mutually exclusive")
	}
	// --output is owner-editable on a PROMPT-target default (PRD #929 M1) but prompt-only:
	// on a sweep/issue/self_improve default it is meaningless, so reject it client-side.
	if f.Changed("output") && s.Target != schedTargetPrompt {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage,
			"--output is only valid on a prompt-target schedule")
	}
	maxIssuesSet := f.Changed("max-issues")
	clearMaxIssues := f.Changed("clear-max-issues")
	clearMrRework := f.Changed("clear-mr-rework")
	if (maxIssuesSet || clearMaxIssues) && s.Target != schedTargetSweep {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--max-issues/--clear-max-issues are only valid on a sweep target")
	}
	if maxIssuesSet && clearMaxIssues {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--max-issues and --clear-max-issues are mutually exclusive")
	}
	if f.Changed("mr-rework") && clearMrRework {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--mr-rework and --clear-mr-rework are mutually exclusive")
	}

	// FRESH minimal request: only catalog-editable fields. Restate model/override from the
	// fetched row so a partial edit keeps them; an explicit --model/--apply-model-to-agents
	// overrides below (issue #691 — see the doc comment). Leave
	// Target/Labels/Prompt/Guidance/IssueIID/Timing/RunAt/RepoID at zero so the guard passes.
	req := apitypes.ScheduleRequest{
		CronExpr:              s.CronExpr,
		Timezone:              s.Timezone,
		Model:                 s.Model,
		OverrideSubagentModel: s.OverrideSubagentModel,
		// PRD #929 M1: output_mode is owner-editable on a prompt-target default and uses
		// replace-semantics server-side, so RESTATE the fetched value (nil for a non-prompt
		// default, so restating is a no-op there) or a partial edit wipes it. An explicit
		// --output overrides below.
		OutputMode: s.OutputMode,
	}
	autoApprove := s.AutoApprove
	waitOnLimit := s.WaitOnLimit
	req.AutoApprove = &autoApprove
	req.WaitOnLimit = &waitOnLimit
	// mr_rework (PRD #841): patchDefaultScheduleConfig uses replace-semantics on it, so RESTATE
	// the fetched tri-state value or a partial edit (e.g. --cron alone) sends nil and wipes the
	// stored override to inherit. Restating nil re-sends inherit; an explicit --mr-rework overrides.
	req.MrReworkEnabled = s.MrReworkEnabled
	// max_issues is meaningful only for a sweep default; restate it so a partial edit keeps
	// the stored cap (the server clears it to unlimited on an omitted value).
	if s.Target == schedTargetSweep {
		req.MaxIssues = s.MaxIssues
	}
	// Guidance on a PROMPT-target (issue #662) or SWEEP-target (issue #675) default is
	// owner-editable and uses replace-semantics on the server, so RESTATE the fetched value —
	// otherwise a partial edit (e.g. --cron alone) would send a nil guidance and wipe the
	// stored value. For a sweep default s.Guidance is the OVERLAY (nil when no overlay is set;
	// the baked catalog value is in s.BakedGuidance and is NEVER restated), so this never
	// echoes the baked value back into the column. The server guard accepts a nil guidance
	// (keeps NULL, no wipe). Issue/self_improve defaults never reach here with a guidance flag,
	// so they still send no guidance.
	if s.Target == schedTargetPrompt || s.Target == schedTargetSweep {
		req.Guidance = s.Guidance
	}

	changed := false
	if f.Changed("cron") {
		cron, _ := f.GetString("cron")
		// Leave Timing empty: patchDefaultScheduleConfig always writes recurring.
		req.CronExpr = cron
		changed = true
	}
	if f.Changed("tz") {
		tz, _ := f.GetString("tz")
		req.Timezone = strings.TrimSpace(tz)
		changed = true
	}
	if f.Changed("auto-approve") {
		v, _ := f.GetBool("auto-approve")
		req.AutoApprove = &v
		changed = true
	}
	if f.Changed("wait-on-limit") {
		v, _ := f.GetBool("wait-on-limit")
		req.WaitOnLimit = &v
		changed = true
	}
	if f.Changed("mr-rework") {
		v, _ := f.GetBool("mr-rework")
		req.MrReworkEnabled = &v
		changed = true
	}
	if clearMrRework {
		// nil reaches patchDefaultScheduleConfig's replace-semantics as a cleared override
		// (inherit); see the user-schedule builder for the wire-shape rationale.
		req.MrReworkEnabled = nil
		changed = true
	}
	if maxIssuesSet {
		v, _ := f.GetInt("max-issues")
		req.MaxIssues = &v
		changed = true
	}
	if clearMaxIssues {
		req.MaxIssues = nil
		changed = true
	}
	// Guidance overlay (prompt- or sweep-target default; guarded above). Restating the fetched
	// value does NOT by itself count as a change — only an explicit flag flips `changed`.
	if guidanceSet {
		v, _ := f.GetString("guidance")
		req.Guidance = &v
		changed = true
	}
	if clearGuidance {
		// Empty string, not nil: the server treats a blank guidance as NULL, and a non-nil
		// pointer keeps the request acceptable to the prompt-default guard.
		empty := ""
		req.Guidance = &empty
		changed = true
	}
	// --model is owner-editable on a default (issue #691): patchDefaultScheduleConfig reads
	// req.Model. Restated from the fetched row above; an explicit flag overrides (empty clears).
	if f.Changed("model") {
		v, _ := f.GetString("model")
		req.Model = &v
		changed = true
	}
	// --output is owner-editable on a prompt-target default (PRD #929 M1): restated from the
	// fetched row above; an explicit flag overrides (empty string clears back to inherit).
	if f.Changed("output") {
		v, _ := f.GetString("output")
		req.OutputMode = &v
		changed = true
	}
	// --apply-model-to-agents is owner-editable on a default (issue #691):
	// patchDefaultScheduleConfig reads req.OverrideSubagentModel. Restated from the fetched
	// row above; an explicit flag overrides.
	if f.Changed("apply-model-to-agents") {
		v, _ := f.GetBool("apply-model-to-agents")
		req.OverrideSubagentModel = &v
		changed = true
	}
	if !changed {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage,
			"nothing to edit (pass at least one editable field: --cron, --tz, --auto-approve, --wait-on-limit, --mr-rework, --max-issues, --guidance, --model, --output, --apply-model-to-agents)")
	}
	return req, nil
}
