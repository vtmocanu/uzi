package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1906 M3: a profile-bound run is refused, before anything is opened, on the lanes
// that cannot isolate it, and never assembled without a transaction to mint with.
func TestAssembleClaimRefusesUnisolatableBoundRuns(t *testing.T) {
	bound := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	s := &Service{} // no store: the refusal must come before any read
	for name, run := range map[string]store.Run{
		"judge": {ID: uuid.New(), Kind: runkind.Judge, Harness: "claude", EgressProfileID: bound},
		"codex": {ID: uuid.New(), Kind: runkind.Issue, Harness: harnessCodex, EgressProfileID: bound},
	} {
		p, err := s.assembleClaim(context.Background(), store.Worker{}, run)
		if p != nil || !errors.Is(err, errIsolatedClaimRefused) || !errors.Is(err, errCredentialUnavailable) {
			t.Errorf("%s: assembleClaim = %v, %v; want the terminal isolated refusal", name, p, err)
		}
	}
	if err := s.isolateClaim(context.Background(), store.Run{ID: uuid.New(), EgressProfileID: bound}, &ClaimPayload{}); !errors.Is(err, errIsolatedClaimRefused) {
		t.Errorf("isolateClaim without a transaction source = %v, want a refusal", err)
	}
}

func TestStripForIsolation(t *testing.T) {
	iid := int64(1)
	branch, base := "agent/issue-1-feature", "release-base"
	p := &ClaimPayload{
		IssueIID: &iid,
		Repo:     ClaimRepo{ID: "r", URL: "u", CloneURL: "c", ForgeType: "github", SkillsEnabled: true},
		Secrets:  ClaimSecrets{ForgeUsername: "bot", ForgePAT: "secret-pat", AnthropicOAuthToken: "model", Codex: &ClaimCodexSecrets{}},
		Agents:   []ClaimAgent{{Name: "a"}}, Skills: []ClaimSkill{{Name: "s"}}, SkillsDropped: []ClaimSkillDrop{{Name: "d"}},
		Config:                 ClaimConfig{ToolPackages: []string{"jq"}, RepoDevboxOptIn: true},
		InflightTargets:        []string{"x"},
		SelfImproveOpenMRs:     []string{"y"},
		KnownImproveUziTargets: []string{"z"},
		SelfImproveDogfood:     true,
		Pipeline:               &ClaimPipeline{},
		ReviewComments:         &ReviewCommentsSnapshot{},
		IssueComments:          &IssueCommentsSnapshot{Truncated: true},
		Branch:                 &branch,
		BaseBranch:             &base,
		PrDescription:          &apitypes.PrDescriptionState{MrIid: 42},
	}
	stripForIsolation(p)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"secret-pat", `"bot"`, `"jq"`, `"clone_url":"c"`, "inflight_targets", "self_improve_open_mrs", "known_improve_uzi_targets", `"pipeline"`, `"review_comments"`, `"codex"`,
		`"issue_comments"`, "agent/issue-1-feature", "release-base", `"pr_description"`} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("stripped claim still carries %s: %s", leak, raw)
		}
	}
	for _, keep := range []string{`"anthropic_oauth_token":"model"`, `"agents":[]`, `"skills":[]`, `"skills_dropped":[]`} {
		if !strings.Contains(string(raw), keep) {
			t.Errorf("stripped claim lacks %s: %s", keep, raw)
		}
	}
}
