// Package runkind is the single Go source of truth for the eight values of
// runs.kind. It is a leaf package (stdlib imports only, nothing from internal/)
// so every consumer in api/ can depend on it without creating a cycle.
//
// The authoritative set lives in the database: the runs_kind_check CHECK
// constraint, redefined by the highest-numbered migration under
// api/internal/store/migrations/ that touches it (today
// 00167_run_mr_rework_kind.sql). The constants and All() below mirror that
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

// The eight run kinds, in DB runs_kind_check order. See package doc for the
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
)

// All returns the eight run kinds in DB runs_kind_check order.
func All() []string {
	return []string{Issue, CIFix, Chat, Judge, SelfImprove, Prompt, Task, MRRework}
}

// JudgeEligible reports whether a run of this kind may be reviewed by the judge
// (PRD #46 allowlist: issue, ci_fix). Consolidates the former per-package
// judge-eligibility allowlist that workersvc held.
func JudgeEligible(kind string) bool { return kind == Issue || kind == CIFix }

// Listed reports whether a run of this kind appears on the general Runs list and is
// planning-capable — every kind except the repo-less meta-runs chat and judge. Mirrors
// the `kind NOT IN ('chat','judge')` filter in store/queries/runtime.sql.
func Listed(kind string) bool { return kind != Chat && kind != Judge }
