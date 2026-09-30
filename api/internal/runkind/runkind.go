// Package runkind is the single Go source of truth for the nine values of
// runs.kind. It is a leaf package (stdlib imports only, nothing from internal/)
// so every consumer in api/ can depend on it without creating a cycle.
//
// The authoritative set lives in the database: the runs_kind_check CHECK
// constraint, redefined by the highest-numbered migration under
// api/internal/store/migrations/ that touches it (today
// 00275_run_job_kind.sql). The constants and All() below mirror that
// constraint in DB CHECK order; runkind_migration_test.go reads the live
// migration and fails if the two ever drift, in either direction and in order.
//
// The constants are untyped strings (not a `type Kind string`), so they are a
// drop-in replacement for the bare string literals and the per-package RunKind*
// constants the rest of api/ compares against — every existing comparison
// compiles unchanged after repointing at runkind.
//
// There is deliberately no Valid() function: nothing in api/ validates a kind
// string today (every create path assigns a SQL literal, no handler accepts a
// kind from the wire), so a validator would have no caller — deadcode:api would
// flag it — and adding a call site would be a new rejection path, i.e. a
// behaviour change (PRD #983 Decision D1). It arrives with its first real
// validator, not before.
package runkind

// The nine run kinds, in DB runs_kind_check order. See package doc for the
// source of truth.
const (
	Issue       = "issue"
	CIFix       = "ci_fix"
	Chat        = "chat"
	Judge       = "judge"
	SelfImprove = "self_improve"
	Prompt      = "prompt"
	Task        = "task"
	MRRework    = "mr_rework"
	// Job is the repo-less, API-created run of PRD #1908: no repo, clone, git, MR or plan
	// gate; it returns a structured result. It carries runs.job_type (see JobTypes).
	Job = "job"
)

// All returns the nine run kinds in DB runs_kind_check order.
func All() []string {
	return []string{Issue, CIFix, Chat, Judge, SelfImprove, Prompt, Task, MRRework, Job}
}

// JudgeEligible reports whether a run of this kind may be reviewed by the judge
// (PRD #46 allowlist: issue, ci_fix). Consolidates the former per-package
// judge-eligibility allowlist that workersvc held.
func JudgeEligible(kind string) bool { return kind == Issue || kind == CIFix }

// Listed reports whether a run of this kind appears on the general Runs list — every kind
// except the repo-less meta-runs chat and judge. A job IS listed (it is a user-visible run).
// Mirrors the `kind NOT IN ('chat','judge')` filter of CountInProgressRunsForUser,
// ListRunsForUser and ListActiveRunsAll in store/queries/runtime.sql.
func Listed(kind string) bool { return kind != Chat && kind != Judge }

// PlanningCapable reports whether a run of this kind can have a planning turn and a plan gate:
// every kind except the repo-less chat and judge meta-runs and the job, which is created
// approved-by-construction and never plans. Backs handler.isPlanningPhase.
func PlanningCapable(kind string) bool { return kind != Chat && kind != Judge && kind != Job }

// WallTimed reports whether the sweeper's wall-clock passes (RequestWallParks / ParkRunsAtWall)
// park a run of this kind at its wall. A job is NOT wall-timed: it never parks, so a wall breach
// fails it instead (PRD #1908 D-E). Mirrors the `kind NOT IN (...)` filter of those two blocks in
// store/queries/runtime.sql once the job-never-parks milestone lands (see runkind_sql_test.go).
func WallTimed(kind string) bool { return kind != Chat && kind != Judge && kind != Job }

// JobTypeResearch is the only job type today. JobTypes mirrors the runs_job_type_check and
// products_allowed_job_types_check CHECK constraints (pinned by a live-DB test).
const JobTypeResearch = "research"

// JobTypes returns the known job types, in DB CHECK order.
func JobTypes() []string { return []string{JobTypeResearch} }
