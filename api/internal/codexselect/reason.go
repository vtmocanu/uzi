// Package codexselect is the pure core of Codex account auto-selection (issue #3719): given
// a user's pooled Codex subscription accounts, their limit readings and their in-flight
// counts, it picks the account a new run should bind. It reads no database and no clock:
// every input, including "now", is a parameter, so the run-creation path owns all I/O and
// this package stays a table-testable function.
package codexselect

// Reason is WHY run creation bound the Codex alias it bound, persisted in
// runs.codex_select_reason and rendered by the CLI, TUI and web. It is a CLOSED vocabulary
// and this file is its ONE Go home, mirroring autoselect.Reason. The SQL CHECK
// (runs_codex_select_reason_check, migration 00387) lists the same values, and a guard test
// parses that migration and compares it with AllReasons: a value added in Go and forgotten
// in SQL is a constraint violation at create time, i.e. a refused run.
type Reason string

const (
	// ReasonDefault: the user's codex_account_mode is 'default', so the owner's default
	// Codex credential paid (today's behaviour, untouched).
	ReasonDefault Reason = "default"
	// ReasonPoolEmpty: mode 'auto' but the user pooled no Codex alias, so the owner's
	// default paid.
	ReasonPoolEmpty Reason = "pool_empty"
	// ReasonAuto: picked from the pool by headroom with at least MinHeadroom to spare.
	ReasonAuto Reason = "auto"
	// ReasonBestOfPool: every measurable account was below MinHeadroom, so the emptiest
	// of them was picked anyway.
	ReasonBestOfPool Reason = "best_of_pool"
	// ReasonPoolStale: accounts are pooled and usable but none has a measurable reading,
	// so the lowest account key was picked.
	ReasonPoolStale Reason = "pool_stale"
	// ReasonPoolExhausted: every usable account is at capacity (limit reached, in
	// cooldown or held by the weekly reserve), so the one releasing soonest was picked.
	ReasonPoolExhausted Reason = "pool_exhausted"
)

// AllReasons is the WHOLE reason vocabulary in a form a guard can enumerate. It returns a
// fresh slice: a package-level var would let one caller's append corrupt every other
// reader's view of a CLOSED set.
func AllReasons() []Reason {
	return []Reason{
		ReasonDefault, ReasonPoolEmpty, ReasonAuto, ReasonBestOfPool, ReasonPoolStale, ReasonPoolExhausted,
	}
}
