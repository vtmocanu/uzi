package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Resolve once before credential selection; never re-read or rebind after it.
func resolveSemanticApproval(env Env, cmd *cobra.Command, c uzicli.Client, id string) (*int64, *apitypes.SemanticGateApproval, error) {
	if cmd.Flags().Changed("semantic-approval") {
		raw, _ := cmd.Flags().GetString("semantic-approval")
		var approval apitypes.SemanticGateApproval
		dec := json.NewDecoder(bytes.NewBufferString(raw))
		dec.DisallowUnknownFields()
		err := dec.Decode(&approval)
		if err == nil {
			var extra any
			if dec.Decode(&extra) != io.EOF {
				err = errors.New("trailing JSON")
			}
		}
		if err != nil || approval.PresentationRevision <= 0 || approval.PresentationDigest == "" || approval.CandidateDigest == "" {
			return nil, nil, uzicli.Exitf(uzicli.ExitUsage, "invalid semantic approval snapshot")
		}
		if cmd.Flags().Changed(expectedGateRevisionFlag) {
			rev, _ := cmd.Flags().GetInt64(expectedGateRevisionFlag)
			if rev != approval.PresentationRevision {
				return nil, nil, uzicli.Exitf(uzicli.ExitUsage, "semantic approval and expected gate revision differ")
			}
		}
		return &approval.PresentationRevision, &approval, nil
	}
	// Explicit structural revisions retain their existing no-read behavior.
	// Semantic scripts supply the reviewed snapshot through --semantic-approval.
	if cmd.Flags().Changed(expectedGateRevisionFlag) {
		rev, err := resolveExpectedGateRevision(cmd, c, id)
		return rev, nil, err
	}
	run, err := c.GetRun(cmd.Context(), id)
	if err != nil {
		var ee *uzicli.ExitError
		if errors.As(err, &ee) && ee.Code == uzicli.ExitNotFound {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if run.Status != "awaiting_approval" || run.GateRevision <= 0 {
		return nil, nil, nil
	}
	rev := run.GateRevision
	if run.SemanticGate == nil {
		return &rev, nil, nil
	}
	// Copy the exact requested snapshot before any subsequent await.
	raw, err := json.Marshal(run.SemanticGate.Approval)
	if err != nil {
		return nil, nil, err
	}
	var approval apitypes.SemanticGateApproval
	if err = json.Unmarshal(raw, &approval); err != nil {
		return nil, nil, err
	}
	if approval.PresentationRevision != rev {
		return nil, nil, uzicli.Exitf(uzicli.ExitConflict, "semantic presentation revision changed; review the gate again")
	}
	for _, row := range semanticGateRows(run) {
		_, _ = fmt.Fprintf(env.Stderr, "%s: %s\n", row[0], row[1])
	}
	return &rev, &approval, nil
}
