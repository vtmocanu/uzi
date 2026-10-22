package main

import (
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// resetApprovalOptions validates consent before any read, credential switch or input.
func resetApprovalOptions(cmd *cobra.Command, sel *apitypes.AgentSelection) (*uzicli.RunInputOptions, error) {
	reset, _ := cmd.Flags().GetBool("reset-milestones")
	flags := []string{"reason", "expected-reset-state-digest", "expected-completion-revision"}
	if !reset {
		for _, flag := range flags {
			if cmd.Flags().Changed(flag) {
				return nil, uzicli.Exitf(uzicli.ExitUsage, "--%s requires --reset-milestones", flag)
			}
		}
		return nil, nil
	}
	if cmd.Flags().Changed("semantic-approval") {
		return nil, uzicli.Exitf(uzicli.ExitUsage, "semantic milestone reset is not supported")
	}
	if cmd.Flags().Changed("token") {
		return nil, uzicli.Exitf(uzicli.ExitUsage, "--reset-milestones cannot be combined with --token")
	}
	for _, flag := range append(flags, expectedGateRevisionFlag) {
		if !cmd.Flags().Changed(flag) {
			return nil, uzicli.Exitf(uzicli.ExitUsage, "--reset-milestones requires explicit --%s; review uzi run get first", flag)
		}
	}
	reason, _ := cmd.Flags().GetString("reason")
	if strings.TrimSpace(reason) == "" {
		return nil, uzicli.Exitf(uzicli.ExitUsage, "--reason must be nonblank")
	}
	gate, _ := cmd.Flags().GetInt64(expectedGateRevisionFlag)
	if gate <= 0 {
		return nil, uzicli.Exitf(uzicli.ExitUsage, "--expected-gate-revision must be positive for reset")
	}
	digest, _ := cmd.Flags().GetString("expected-reset-state-digest")
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return nil, uzicli.Exitf(uzicli.ExitUsage, "--expected-reset-state-digest must be a SHA-256 hex digest")
	}
	value, _ := cmd.Flags().GetString("expected-completion-revision")
	var revision *int
	if value != "null" {
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return nil, uzicli.Exitf(uzicli.ExitUsage, "--expected-completion-revision must be positive or literal null")
		}
		revision = &n
	}
	return &uzicli.RunInputOptions{
		Kind: kindApprovePlan, Selection: sel, ExpectedGateRevision: &gate,
		ExpectedResetStateDigest: digest,
		Reconciliation:           &apitypes.MilestoneReconciliation{Mode: "reset", Reason: reason, ExpectedCompletionRevision: revision},
	}, nil
}
