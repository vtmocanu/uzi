package workersvc

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodeCrossCheckInstructionCommitDigest(t *testing.T) {
	cc := store.CrossCheck{Stage: "code", Round: 1, LeadRunID: uuid.New(),
		BaseCommit:       pgtype.Text{String: strings.Repeat("a", 40), Valid: true},
		HeadCommit:       pgtype.Text{String: strings.Repeat("b", 40), Valid: true},
		CodeContext:      []byte(`{"issue_title":"Legacy title","issue_description":"Legacy body"}`),
		GuidanceSnapshot: pgtype.Text{Valid: true}, SizeClass: pgtype.Text{String: "m", Valid: true},
		RequiredCapabilities: []string{}, RequiredTools: []string{},
		DeadlineAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}}
	legacy, err := codeCandidateDigest(cc)
	if err != nil || hex.EncodeToString(legacy) != "562f0bae72b173177bdb55a6317c1cdc2896d70f6e9d2d200b774adfc5e664f1" {
		t.Fatalf("legacy digest: %x %v", legacy, err)
	}
	for _, enabled := range []bool{false, true} {
		cc.RepoInstructionsEnabled = enabled
		if enabled {
			cc.RepoInstructionsText = pgtype.Text{String: "fixed instructions", Valid: true}
		}
		cc.RepoInstructionsCommit = pgtype.Text{}
		absent, err := codeCandidateDigest(cc)
		if err != nil {
			t.Fatal(err)
		}
		cc.RepoInstructionsCommit = pgtype.Text{String: strings.Repeat("c", 40), Valid: true}
		first, err := codeCandidateDigest(cc)
		if err != nil {
			t.Fatal(err)
		}
		cc.CandidateDigest = first
		claim, err := crossCheckCandidateInput(cc)
		if err != nil {
			t.Fatal(err)
		}
		if claim.RepoInstructionsCommit == nil || *claim.RepoInstructionsCommit != cc.RepoInstructionsCommit.String {
			t.Fatal("claim lost pin")
		}
		payload := &ClaimPayload{Kind: "cross_check", RunID: uuid.New().String(), CrossCheck: claim}
		if err := ValidateCrossCheckClaim(payload); err != nil {
			t.Fatal(err)
		}
		cc.RepoInstructionsCommit.String = strings.Repeat("d", 40)
		second, err := codeCandidateDigest(cc)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(first, absent) || bytes.Equal(first, second) {
			t.Fatal("pin does not independently bind candidate")
		}
		*claim.RepoInstructionsCommit = cc.RepoInstructionsCommit.String
		if ValidateCrossCheckClaim(payload) == nil {
			t.Fatal("changed claim pin accepted")
		}
	}
}

func TestCodeCrossCheckInstructionCommitValidation(t *testing.T) {
	for _, pin := range []string{"", strings.Repeat("A", 40), strings.Repeat("a", 39), strings.Repeat("g", 40)} {
		if _, err := codeCrossCheckInstructions([]CodeCrossCheckInstructions{{RepoInstructionsCommit: &pin}}); err == nil {
			t.Fatalf("accepted %q", pin)
		}
	}
	pin := strings.Repeat("a", 40)
	input := CodeCrossCheckInstructions{RepoInstructionsCommit: &pin}
	cc := store.CrossCheck{RepoInstructionsCommit: pgtype.Text{String: pin, Valid: true}}
	if !input.matches(cc) {
		t.Fatal("identical retry refused")
	}
	cc.RepoInstructionsCommit.String = strings.Repeat("b", 40)
	if input.matches(cc) {
		t.Fatal("changed retry accepted")
	}
	cc.RepoInstructionsCommit = pgtype.Text{}
	if input.matches(cc) {
		t.Fatal("absent retry accepted")
	}
	plan := boundedCheckerFixture(t)
	plan.CrossCheck.RepoInstructionsCommit = &pin
	if ValidateCrossCheckClaim(plan) == nil {
		t.Fatal("plan pin accepted")
	}
}
