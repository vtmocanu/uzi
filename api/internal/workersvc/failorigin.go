package workersvc

// PRD #69 M7a Pass A (the SEAM): the server's authoritative copy of the fail_origin
// vocabulary, modelled EXACTLY on the rate_limit_type precedent above (see
// limitwait.go's rateLimitTypes / CoerceRateLimitType). The judge needs a TRUSTED,
// structured failure ORIGIN for each failed run; today that origin lives only in
// free-text failure_reason, which is never parsed. This is the closed enum stamped at
// each terminal-failure write site (migration 00126's CHECK).
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

// failOrigins is the closed set migration 00126's CHECK enforces, in source order.
//
// Adding or removing a member here without moving 00126 in the same commit is caught
// by TestFailOriginVocabularyMatchesCheck, which parses the CHECK out of the migration
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
	// PARSES the remote reject, failing typed with the diff preserved instead of a raw
	// `remote rejected` (worker-reportable — see workerReportableFailOrigins).
	"push_secret_blocked",
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
// own `failed` state. The other five — worker_lost, run_timeout, plan_rejected,
// auto_stopped, guardrail_blocked — are stamped ONLY by the server's own sweeper/queries
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
// Protection / GH013, so the worker fails typed and preserves the diff); agent_failure
// is included because it is the judgeable
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
