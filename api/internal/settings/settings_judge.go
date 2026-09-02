package settings

// This file holds the run-judge accessors, their bounds const and their
// write-time validators (PRD #1021 M3, split verbatim from settings.go).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// maxJudgeDailyBudget bounds the per-user judge daily budget (PRD #69 M5 Decision
// 9). 0 means unlimited (the guard is off); a positive count caps judge runs per
// rolling 24h. The upper bound only catches a fat-fingered value — no real user
// runs thousands of judges a day — so an admin meaning 50 and typing 50000 gets a
// rejected write instead of an effectively-unlimited guard.
const maxJudgeDailyBudget = 10000

// JudgeEnabled reports whether the run-judge feature is enabled instance-wide
// (PRD #46 Decision 7): the global kill-switch. Stored as the text "true"/"false";
// any other value falls back to the compiled-in default (false) — the same strict
// junk-tolerance as SlackEnabled, so a malformed value never silently turns token
// spend on.
func (c *Cache) JudgeEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyJudgeEnabled)
}

// JudgeEnforceAll reports whether the judge is enforced for every run (PRD #69),
// bypassing the per-user judge_enabled opt-in gate. Stored as the text
// "true"/"false"; any other value falls back to the compiled-in default (false) —
// the same strict junk-tolerance as JudgeEnabled, so a malformed row never
// silently turns forced token spend on.
func (c *Cache) JudgeEnforceAll(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyJudgeEnforceAll)
}

// JudgeModel returns the model alias the judge runs on (PRD #46 Decision 7). Falls
// back to the strong DefaultJudgeModel ("opus", PRD #69 Decision 1).
func (c *Cache) JudgeModel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyJudgeModel)
}

// SummaryModel returns the model alias the inline run-summary generator runs on
// (PRD #362 Decision 8). Falls back to DefaultSummaryModel ("haiku"). The per-user
// override (users.summary_model) is resolved user-value-wins at issue-run claim
// assembly, mirroring JudgeModel but on the issue-run claim rather than the judge.
func (c *Cache) SummaryModel(ctx context.Context) (string, error) {
	return c.get(ctx, KeySummaryModel)
}

// JudgeCooldownSeconds returns the per-user judge cooldown in seconds (PRD #69 M5
// Decision 9); 0 disables the cooldown guard. Best-effort at the enqueue gate — the
// caller proceeds (fails open) on a read error, since the guard is a soft cost
// backstop, not a correctness control.
func (c *Cache) JudgeCooldownSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyJudgeCooldownSeconds)
}

// JudgeDailyBudget returns the per-user judge daily budget as a count (PRD #69 M5
// Decision 9); 0 means unlimited (the guard is off). Best-effort at the enqueue
// gate, like JudgeCooldownSeconds.
func (c *Cache) JudgeDailyBudget(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyJudgeDailyBudget)
}

// validateModelAlias is the format gate for the judge model setting (PRD #46): a
// non-empty model alias / id, checked with the shared PRD #17 rules (single token,
// no control chars, length-capped). Blank is rejected here (unlike the per-user
// inherit case) — the judge always needs a concrete model.
func validateModelAlias(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be empty")
	}
	_, err := agenttmpl.ValidateModel(value)
	return err
}

// validateJudgeDailyBudget is the write-time gate for the per-user judge daily
// budget (PRD #69 M5 Decision 9): a base-10 integer in {0} ∪ [1, maxJudgeDailyBudget],
// where 0 is the documented "unlimited / guard off" value rather than a rejection.
// Negatives, non-integers, and values above the cap are refused.
//
// Like validateHostedWorkerQuota, the explicit Validate case this backs is
// load-bearing: Validate's default branch falls through to ValidateLabel, which
// accepts any non-empty ≤64-char string — so without this case "abc" would save and
// intSetting would silently fall back to the compiled-in default on every read, an
// admin's typed cap silently ignored.
func validateJudgeDailyBudget(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of judge runs")
	}
	if n == 0 {
		return nil
	}
	if n < 0 || n > maxJudgeDailyBudget {
		return fmt.Errorf("must be 0 (unlimited) or between 1 and %d judge runs", maxJudgeDailyBudget)
	}
	return nil
}
