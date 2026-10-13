package workersvc

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func guidanceWireFixture(t *testing.T) *ClaimPayload {
	t.Helper()
	cc := store.CrossCheck{Stage: "code", Round: 1, LeadRunID: uuid.New(),
		BaseCommit:              pgtype.Text{String: strings.Repeat("a", 40), Valid: true},
		HeadCommit:              pgtype.Text{String: strings.Repeat("b", 40), Valid: true},
		CodeContext:             json.RawMessage(`{"issue_title":"title","issue_description":"body"}`),
		GuidanceSnapshot:        pgtype.Text{String: "Check compatibility.", Valid: true},
		GuidanceText:            pgtype.Text{String: "Check compatibility.", Valid: true},
		RepoInstructionsEnabled: true,
		RepoInstructionsText:    pgtype.Text{String: "Follow base conventions.", Valid: true},
		DeadlineAt:              pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}}
	var err error
	cc.CandidateDigest, err = codeCandidateDigest(cc)
	if err != nil {
		t.Fatal(err)
	}
	input, err := crossCheckCandidateInput(cc)
	if err != nil {
		t.Fatal(err)
	}
	return &ClaimPayload{Kind: "cross_check", RunID: uuid.NewString(), CrossCheck: input}
}

func TestCodeCrossCheckFrozenWireValidation(t *testing.T) {
	p := guidanceWireFixture(t)
	if _, err := MarshalCrossCheckClaim(p); err != nil {
		t.Fatalf("frozen claim refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ClaimPlanCrossCheck)
	}{
		{"consent", func(c *ClaimPlanCrossCheck) { off := false; c.RepoInstructionsEnabled = &off }},
		{"text", func(c *ClaimPlanCrossCheck) { c.RepoInstructionsText = "Changed" }},
		{"guidance hash", func(c *ClaimPlanCrossCheck) { c.GuidanceDigest = strings.Repeat("0", 64) }},
		{"instruction hash", func(c *ClaimPlanCrossCheck) { c.RepoInstructionsDigest = strings.Repeat("0", 64) }},
		{"malformed hash", func(c *ClaimPlanCrossCheck) { c.GuidanceDigest = "not-hex" }},
		{"short hash", func(c *ClaimPlanCrossCheck) { c.RepoInstructionsDigest = hex.EncodeToString([]byte{1}) }},
		{"custom applied", func(c *ClaimPlanCrossCheck) { off := false; c.CustomGuidanceApplied = &off }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := guidanceWireFixture(t)
			tc.mutate(p.CrossCheck)
			if _, err := MarshalCrossCheckClaim(p); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("changed frozen claim admitted: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ClaimPlanCrossCheck)
	}{
		{"false consent", func(c *ClaimPlanCrossCheck) { off := false; c.RepoInstructionsEnabled = &off }},
		{"false custom", func(c *ClaimPlanCrossCheck) { off := false; c.CustomGuidanceApplied = &off }},
		{"text", func(c *ClaimPlanCrossCheck) { c.RepoInstructionsText = "text" }},
		{"hash", func(c *ClaimPlanCrossCheck) { c.GuidanceDigest = strings.Repeat("0", 64) }},
	} {
		t.Run("plan "+tc.name, func(t *testing.T) {
			p := boundedCheckerFixture(t)
			tc.mutate(p.CrossCheck)
			if _, err := MarshalCrossCheckClaim(p); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("code fields admitted on plan: %v", err)
			}
		})
	}
}
