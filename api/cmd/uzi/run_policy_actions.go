package main

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// policyContext includes live completion questions and budget holds with policy evidence.
func policyContext(run apitypes.RunDTO) bool {
	switch run.Status {
	case "completed", "failed", "cancelled":
		return false
	}
	return run.PolicyRefusal != nil || (run.HoldReason != nil && *run.HoldReason == holdProviderPolicyRefusal)
}

func policyActionToken(run apitypes.RunDTO) (string, error) {
	if !policyContext(run) {
		return "", nil
	}
	if run.PolicyRefusal == nil || run.PolicyRefusal.ActionIfMatch == "" {
		return "", uzicli.Exitf(uzicli.ExitConflict, "policy action binding unavailable; upgrade the server and review the run before retrying")
	}
	return run.PolicyRefusal.ActionIfMatch, nil
}

func validatePolicyGuidance(guidance string) error {
	if len(guidance) > 16000 || strings.ContainsRune(guidance, '\x00') || !utf8.ValidString(guidance) {
		return uzicli.Exitf(uzicli.ExitUsage, "--guidance must be valid UTF-8, at most 16000 bytes, without NUL")
	}
	return nil
}

func warnPolicyResume(env Env, run apitypes.RunDTO, fresh bool) {
	if !policyContext(run) {
		return
	}
	if fresh {
		_, _ = fmt.Fprintln(env.Stderr, "warning: fresh session discards conversation context; verified or recovered files and contract scope remain. Latest local edits are not guaranteed.")
	} else {
		_, _ = fmt.Fprintln(env.Stderr, "warning: preserving the session requests existing conversation context; provider resume is not guaranteed. Released or different workers may lack that context; recovery only preserves verified or recovered files.")
	}
	if run.CompletionBudgetExhausted || (run.HoldReason != nil && *run.HoldReason == "budget_exhausted") ||
		(run.BudgetUsedSeconds != nil && run.BudgetTotalSeconds != nil && *run.BudgetUsedSeconds >= *run.BudgetTotalSeconds) {
		_, _ = fmt.Fprintln(env.Stderr, "warning: the run's budget is spent; extend it before resuming.")
	}
}

// approvalPresentation retains the same read for the plan fence and credential action.
// A missing run keeps resolveExpectedGateRevision's authoritative write-time 404 behavior.
func approvalPresentation(cmd *cobra.Command, c uzicli.Client, id string) (apitypes.RunDTO, *int64, error) {
	var expected *int64
	if cmd.Flags().Changed(expectedGateRevisionFlag) {
		rev, _ := cmd.Flags().GetInt64(expectedGateRevisionFlag)
		if rev < 0 {
			return apitypes.RunDTO{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--%s must be a non-negative revision", expectedGateRevisionFlag)
		}
		expected = &rev
	}
	run, err := c.GetRun(cmd.Context(), id)
	if err != nil {
		var ee *uzicli.ExitError
		if errors.As(err, &ee) && ee.Code == uzicli.ExitNotFound {
			return run, expected, nil
		}
		return run, nil, err
	}
	if expected == nil && run.Status == "awaiting_approval" && run.GateRevision > 0 {
		rev := run.GateRevision
		expected = &rev
	}
	return run, expected, nil
}
