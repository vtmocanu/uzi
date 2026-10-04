package workersvc

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func boundedCheckerFixture(t *testing.T) *ClaimPayload {
	t.Helper()
	c := PlanCrossCheckCandidate{PlanMd: "plan", Milestones: json.RawMessage("[]"),
		RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40)}
	digest, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return &ClaimPayload{Kind: "cross_check", RunID: uuid.NewString(),
		CrossCheck: &ClaimPlanCrossCheck{Stage: "plan", Round: 1, LeadRunID: uuid.NewString(),
			CandidateDigest: hex.EncodeToString(digest), DeadlineAt: time.Now().Add(time.Minute),
			PlanCrossCheckCandidate: c}}
}

func TestCheckerContextPayloadBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cap   int
		title bool
	}{
		{"title", 4096, true}, {"body", 262144, false},
	} {
		for _, extra := range []int{0, 1} {
			t.Run(tc.name+strconv.Itoa(extra), func(t *testing.T) {
				p := boundedCheckerFixture(t)
				value := strings.Repeat("é", tc.cap/2) + strings.Repeat("x", extra)
				if tc.title {
					p.IssueTitle = value
				} else {
					p.IssueDescription = value
				}
				raw, err := MarshalCrossCheckClaim(p)
				if extra == 0 {
					if err != nil {
						t.Fatal(err)
					}
					var decoded ClaimPayload
					if err := json.Unmarshal(raw, &decoded); err != nil {
						t.Fatal(err)
					}
					if decoded.IssueTitle != p.IssueTitle || decoded.IssueDescription != p.IssueDescription {
						t.Fatal("context truncated")
					}
				} else if !errors.Is(err, ErrCrossCheckRefused) || raw != nil {
					t.Fatal("oversized context delivered")
				}
			})
		}
	}
}

func TestCheckerEnvelopeBoundsWithoutMarker(t *testing.T) {
	p := boundedCheckerFixture(t)
	// HTML escaping contributes six encoded bytes per '<'. Pad an ordinary
	// claim field to hit the full envelope bound without an unbounded fixture.
	p.IssueDescription = strings.Repeat("<", 262144)
	p.Branch = strPtr("")
	base, err := MarshalCrossCheckClaim(p)
	if err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat("x", 2097152-len(base))
	p.Branch = &padding
	raw, err := MarshalCrossCheckClaim(p)
	if err != nil || len(raw) != 2097152 || raw[len(raw)-1] != '\n' {
		t.Fatal("exact envelope refused or newline missing")
	}
	padding += "x"
	if raw, err := MarshalCrossCheckClaim(p); !errors.Is(err, ErrCrossCheckRefused) || raw != nil {
		t.Fatal("plus-one envelope delivered")
	}
	p.Kind = "issue"
	p.CrossCheck = nil
	p.IssueTitle = strings.Repeat("x", 4097)
	if _, err := MarshalCrossCheckClaim(p); err != nil {
		t.Fatal("ordinary claim changed")
	}
	p.Kind = "cross_check"
	if _, err := MarshalCrossCheckClaim(p); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatal("decoded checker without dedicated input accepted")
	}
	p = boundedCheckerFixture(t)
	p.CrossCheck.CandidateDigest = strings.Repeat("0", 64)
	if _, err := MarshalCrossCheckClaim(p); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatal("incoherent checker accepted")
	}
}
