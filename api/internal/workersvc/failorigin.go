package workersvc

// PRD #69 M7a Pass A (the SEAM): the server's authoritative copy of the fail_origin
// vocabulary, modelled EXACTLY on the rate_limit_type precedent above (see
// limitwait.go's rateLimitTypes / CoerceRateLimitType). The judge needs a TRUSTED,
// structured failure ORIGIN for each failed run, independent of free-text
// failure_reason, which is never parsed. This is the closed enum stamped at
// each terminal-failure write site (the latest runs_fail_origin_check migration).
//
// WHY THE ALLOWLIST IS THE WHOLE SANITIZER, and why it lives here rather than in the
// worker: identical to rate_limit_type's argument. The worker reports fail_origin as
// UNTRUSTED free text — whatever the report site attached — and CoerceFailOrigin maps
// everything outside the WORKER-REPORTABLE subset (see workerReportableFailOrigins) to
// nil before it can reach the DB, a DTO, the feed or the judge. An allowlist closes
// length, control-char and injection concerns in one move; scoping it to the subset the
// worker legitimately emits additionally stops a worker forging a server-authoritative
// class. No worker-controlled byte survives it.
//
// WHERE THE UNKNOWN MAPPING DIFFERS FROM rate_limit_type, ON PURPOSE. CoerceRateLimitType
// maps an unrecognised value to the literal "unknown" — a real member of its stored
// vocabulary, because "rejected by a window we could not name" and "never parked" are
// different facts a support query must distinguish. fail_origin has no such member: an
// unrecognised worker value coerces to nil so it never fabricates a bogus CLASS the
// judge would then trust. The `failed` arm defaults that nil to 'agent_failure'
// separately (SetState), because a worker-reported failure with no explicit origin IS
// the judgeable agent-failure case — but that default is a decision made at the write
// site, not smuggled in by the coercer.

// failOrigins is the closed set the latest runs_fail_origin_check migration enforces.
//
// Adding or removing a member requires an additive migration, checked
// by TestFailOriginVocabularyMatchesCheck, which parses the latest Up CHECK
// and compares — so a drift reddens at `go test` rather than raising 23514 on a user's
// failed run.
var failOrigins = []string{
	"provisioning_failed",
	"credential_unavailable",
	"guardrail_blocked",
	"rate_limited",
	"run_timeout",
	"worker_lost",
	"agent_failure",
	"plan_rejected",
	"auto_stopped",
	// PRD #377 M1: a GitHub run whose branch touches .github/workflows/** cannot be
	// pushed by the bot's repo-only PAT, so the worker fails early with this origin and
	// preserves the diff (worker-reportable — see workerReportableFailOrigins).
	"workflow_scope_missing",
	// PRD #456 M2: a run behind on .github/workflows/** aligns its branch with the
	// current default before the finalize push; if BOTH the merge and the rebase
	// fallback conflict, the worker aborts, fails with this origin and preserves the
	// pre-align diff (worker-reportable — see workerReportableFailOrigins).
	"finalize_base_align_conflict",
	// issue #974: a GitHub run whose push carries a secret is rejected by GitHub Push
	// Protection / GH013. The worker PREDICTS it with a pre-push gitleaks scan and also
	// PARSES the remote reject, failing typed WITHOUT a preserved diff (it may carry the
	// detected secret; the committed work stays recoverable from the run branch/PVC) instead
	// of a raw `remote rejected` (worker-reportable — see workerReportableFailOrigins).
	"push_secret_blocked",
	// PRD #1392 M1: a run whose forge stayed unreachable at CLONE past
	// RUN_FORGE_UNREACHABLE_MAX_PARKS parks. SERVER-DERIVED, NOT worker-reportable (it is
	// stamped directly inside SetState's forge-park transaction, so it is deliberately absent
	// from workerReportableFailOrigins) — a worker reporting it is a forgery CoerceFailOrigin
	// drops. Excluded from the judge (neverJudgeFailOrigins, judge_enqueue.go) REGARDLESS of
	// iteration_count: a forge cap-fail is server-derived, never a real agent defect, and on a
	// resumed run it carries iteration_count > 0, so the skip cannot be gated on == 0 (SC3).
	"forge_unreachable",
	// PRD #1416 M4: a run on a PUBLISHED branch whose history was rewritten below the
	// published floor P, where the ancestry bridge B (tree == H, P and H both ancestors)
	// could not be built or validated, so finalize cannot fast-forward and fails typed with
	// preserved_patch on both push paths instead of finalize_base_align_conflict or the
	// generic catch (SC3). WORKER-REPORTABLE (see workerReportableFailOrigins): the worker
	// detects the un-bridgeable divergence at finalize. It is an AGENT DEFECT (the agent
	// rewrote published history against the steer), so it is JUDGE-ELIGIBLE — deliberately
	// absent from BOTH preStartInfraFailOrigins and neverJudgeFailOrigins (judge_enqueue.go).
	"history_rewritten",
	// issue #1367: a kind='task' (handoff) run created status='queued' with dispatched_at NULL
	// that the CLI never dispatched (push/dispatch never landed) is terminalized by the
	// undispatched-handoff sweep (SweepTaskNeverDispatched) past its dispatch grace window.
	// SERVER-DERIVED, NOT worker-reportable (stamped inside the sweep's conditional UPDATE, so
	// it is deliberately absent from workerReportableFailOrigins — a worker reporting it is a
	// forgery CoerceFailOrigin drops). Excluded from the judge (neverJudgeFailOrigins,
	// judge_enqueue.go): an undispatched run was never claimed, so there is no agent attempt or
	// trace to retrospect.
	"task_undispatched",
	// issue #1593: a gated plan (or plan-revision) turn that ended with prose only (no
	// submit_plan, no ask_user), was nudged once, and still produced neither on an unattended
	// run (or after owner guidance), so the worker fails the run typed with a fixed
	// failure_reason instead of the generic agent_failure. WORKER-REPORTABLE (see
	// workerReportableFailOrigins). It is an AGENT DEFECT (model noncompliance with the plan
	// contract), so it is JUDGE-ELIGIBLE — deliberately absent from preStartInfraFailOrigins,
	// neverJudgeFailOrigins and envPublishFailOrigins (judge_enqueue.go) — and it is not
	// human-landable (no finalize ran, so there is no committed work to land).
	"plan_missing",
	// issue #1783: the worker could not prove the run's execution stopped (surviving or
	// unverifiable run-owned processes, e.g. a tool shell or Docker invocation that outlived a
	// park) or could not clear or quarantine residue at the run's clone path, so it refused to
	// push, capture or reseed and failed the run typed instead of guessing (the generic
	// agent_failure it surfaced as before). WORKER-REPORTABLE (see workerReportableFailOrigins).
	// It is WORKER INFRASTRUCTURE, not an agent defect, so it is NEVER JUDGED: a member of
	// neverJudgeFailOrigins (judge_enqueue.go), which skips regardless of iteration_count, since
	// it can fire pre-start at reseed or at finalize on a resumed run. It is not human-landable:
	// the worker refused to publish from an unproven state.
	"worker_residue_blocked",
	// PRD #1795 M1: a run whose plan gate could not be re-presented — a historical presentation
	// id, a changed payload on the current id, or a stale adoption refused — across more than
	// RUN_GATE_REFUSAL_MAX claims. SERVER-DERIVED, NOT worker-reportable (stamped inside
	// SetState's awaiting_approval transaction, so it is absent from workerReportableFailOrigins).
	// Excluded from the judge (neverJudgeFailOrigins): a refusal is a protocol state, not an
	// agent defect.
	"gate_presentation_refused",
	// PRD #1809 M5 (D6): a run whose worker's data volume stayed full across more than
	// UZI_RUN_DISK_PARK_MAX counted 'data_volume_full' parks. SERVER-DERIVED, NOT
	// worker-reportable (stamped directly inside SetState's disk-park transaction,
	// parkDataVolumeFull, so it is deliberately absent from workerReportableFailOrigins — a
	// worker reporting it is a forgery CoerceFailOrigin drops). Excluded from the judge
	// (neverJudgeFailOrigins, judge_enqueue.go) REGARDLESS of iteration_count: a full worker
	// volume is the environment's failure, not an agent defect, and it lands mid-run.
	"data_volume_full",
	// issue #1888: a run with selected skills whose Claude SDK skills plugin failed to load at
	// session start (the SDK reported plugin load errors, or an error report the worker could not
	// parse), so the worker fails the run typed with a bounded, redacted failure_reason instead of
	// letting it proceed without the skills its owner selected. WORKER-REPORTABLE (see
	// workerReportableFailOrigins). It is a WORKER ENVIRONMENT failure, not an agent defect, so it
	// is NEVER JUDGED: a member of neverJudgeFailOrigins (judge_enqueue.go), which skips
	// regardless of iteration_count, since a resumed run's session start carries
	// iteration_count > 0. It is not human-landable (not a publish failure), but it can fire
	// on a resumed run that already has commits, so earlier work may still need recovery.
	"skills_plugin_load_failed",
	// PRD #1908: the three fail_origins of the repo-less `job` run kind. All SERVER-DERIVED, NOT
	// worker-reportable (absent from workerReportableFailOrigins, so CoerceFailOrigin drops a
	// worker forging one): no_job_capable_worker and ephemeral_worker_never_registered are
	// stamped by the sweeper's unservable-ephemeral-worker pass, job_no_result by the job-result
	// ingest invariant (a job reported completed with no result row). A job is never judged
	// (runkind.JudgeEligible is false for it), so none of them joins a judge skip set.
	"no_job_capable_worker",
	"ephemeral_worker_never_registered",
	"job_no_result",
	// #2321: a worker-reported provider safety refusal. Judge eligibility matches
	// agent_failure, including resumed runs; no judge skip set includes this origin.
	"provider_policy_refusal",
}

// failOriginSet is the lookup form. Built once; failOrigins stays the declaration so
// the vocabulary reads as a list in source order (same shape as rateLimitTypeSet).
var failOriginSet = func() map[string]bool {
	m := make(map[string]bool, len(failOrigins))
	for _, o := range failOrigins {
		m[o] = true
	}
	return m
}()

// AllFailOrigins returns the whole vocabulary, in a form a guard can enumerate.
//
// Returns a fresh slice for the same reason AllRateLimitTypes does: a package-level
// var would let one caller's append corrupt every other reader's view of a CLOSED set.
func AllFailOrigins() []string {
	out := make([]string, len(failOrigins))
	copy(out, failOrigins)
	return out
}

// workerReportableFailOrigins is the SUBSET of the vocabulary a worker may report on its
// own `failed` state. Server-only origins (including worker_lost, run_timeout,
// plan_rejected, auto_stopped and guardrail_blocked) are stamped by server sweeper/queries
// and claim-assembly recovery, never by a worker, so a worker value naming one of them is
// a forgery: it would corrupt the TRUSTED classification the judge and any consumer key
// on, and (guardrail_blocked being a Gate 4b member) let an untrusted report steer whether
// a run is judged. CoerceFailOrigin gates on THIS set, not the full failOrigins vocabulary.
// The worker legitimately emits only provisioning_failed / credential_unavailable
// (failOriginForReason), rate_limited (the limit opt-out path, runner.ts),
// workflow_scope_missing (PRD #377: the finalize detection that the branch touches
// .github/workflows/** the bot PAT cannot push), finalize_base_align_conflict
// (PRD #456: the finalize base-align merge AND rebase both conflict, so the worker
// aborts and preserves the diff), and push_secret_blocked (issue #974: the finalize
// pre-push gitleaks range scan finds a secret, or the push is rejected by GitHub Push
// Protection / GH013, so the worker fails typed WITHOUT a preserved diff — it may carry
// the detected secret), and history_rewritten (PRD #1416 M4: the worker cannot build or
// validate the ancestry bridge for a published branch whose history was rewritten below the
// published floor, so finalize fails typed with the preserved diff instead of the generic
// catch), and plan_missing (issue #1593: a gated plan turn ended prose-only, with no
// submit_plan and no ask_user, after one corrective nudge, so the worker fails the run typed
// with a fixed failure_reason), and worker_residue_blocked (issue #1783: the worker could not
// prove the run's execution stopped or could not clear or quarantine residue at the run's clone
// path, so it refused to push, capture or reseed), and skills_plugin_load_failed (issue #1888:
// the Claude SDK reported load errors for the run's selected-skills plugin at session start, so
// the worker failed the run rather than run it without its skills), and provider_policy_refusal
// (#2321: a provider safety refusal); agent_failure is included because it is the judgeable
// default the `failed` arm applies anyway, so an explicit worker agent_failure is
// harmless and semantically correct. The partition (worker-reportable + server-only ==
// vocabulary) is pinned by TestCoerceFailOrigin.
var workerReportableFailOrigins = map[string]bool{
	"provisioning_failed":          true,
	"credential_unavailable":       true,
	"rate_limited":                 true,
	"agent_failure":                true,
	"workflow_scope_missing":       true,
	"finalize_base_align_conflict": true,
	"push_secret_blocked":          true,
	// PRD #1416 M4: the worker detects an un-bridgeable published-history rewrite at
	// finalize and reports it typed with preserved_patch (judge-eligible; see failOrigins).
	"history_rewritten": true,
	// issue #1593: the worker fails a gated plan turn that ended prose-only after one
	// corrective nudge (judge-eligible; see failOrigins).
	"plan_missing": true,
	// issue #1783: the worker refuses to push, capture or reseed when it cannot prove the run's
	// execution stopped or cannot clear the run's clone-path residue (never judged; see
	// failOrigins and neverJudgeFailOrigins).
	"worker_residue_blocked": true,
	// issue #1888: the worker fails a run with selected skills whose Claude SDK skills plugin
	// failed to load at session start (never judged; see failOrigins and neverJudgeFailOrigins).
	"skills_plugin_load_failed": true,
	"provider_policy_refusal":   true,
}

// CoerceFailOrigin maps a worker-reported fail_origin onto the WORKER-REPORTABLE subset.
//
// Absent (nil) stays absent. A value in workerReportableFailOrigins passes through
// verbatim. Everything else — an unrecognised value OR a server-authoritative class a
// worker has no authority to claim (worker_lost/run_timeout/plan_rejected/auto_stopped/
// guardrail_blocked) — becomes nil, NOT a placeholder member, so the coercer never invents
// or forges a class; the caller decides what a classless failure means (the `failed` arm
// defaults it to 'agent_failure'). Never an error: a terminal report is never failed on a
// technicality, mirroring CoerceRateLimitType's stated principle.
func CoerceFailOrigin(reported *string) *string {
	if reported == nil {
		return nil
	}
	if workerReportableFailOrigins[*reported] {
		return reported
	}
	return nil
}

// Landing-state values (issue #1418). Server-derived, read-only presentation bucket for a
// failed run whose committed work is human-landable; a worker cannot report it.
const (
	LandingStateNone          = "none"
	LandingStateNeedsLanding  = "needs_landing"
	LandingStateUnrecoverable = "unrecoverable"
)

// humanLandableFailOrigins: the fail_origins whose committed work a human can land — the
// finalize-time publish failures (PRD #377/#456/#974/#1416). A STRICT SUBSET of failOrigins
// (pinned by TestHumanLandableFailOriginsExact).
var humanLandableFailOrigins = map[string]bool{
	"finalize_base_align_conflict": true,
	"workflow_scope_missing":       true,
	"push_secret_blocked":          true,
	"history_rewritten":            true,
}

// IsHumanLandableFailOrigin reports whether a fail_origin belongs to the human-landable set.
func IsHumanLandableFailOrigin(o string) bool { return humanLandableFailOrigins[o] }

// AllHumanLandableFailOrigins returns the set as a fresh slice in failOrigins source order,
// for use as a SQL bind array. Fresh copy for the same reason AllFailOrigins is.
func AllHumanLandableFailOrigins() []string {
	out := make([]string, 0, len(humanLandableFailOrigins))
	for _, o := range failOrigins {
		if humanLandableFailOrigins[o] {
			out = append(out, o)
		}
	}
	return out
}

// DeriveLandingState is the ONE landing_state derivation (issue #1418). Pure function of the
// run's fail_origin plus two availability facts. needs_landing when the origin is human-landable
// AND the work is recoverable (a preserved_patch, or an available recovery capture);
// unrecoverable when landable but neither exists; none otherwise.
func DeriveLandingState(failOrigin *string, hasPreservedPatch, hasAvailableCapture bool) string {
	if failOrigin == nil || !humanLandableFailOrigins[*failOrigin] {
		return LandingStateNone
	}
	if hasPreservedPatch || hasAvailableCapture {
		return LandingStateNeedsLanding
	}
	return LandingStateUnrecoverable
}
