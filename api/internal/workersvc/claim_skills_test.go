package workersvc

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

func skillRow(tmpl, name, scope string, id uuid.UUID, body string) store.ListRunSkillAllocationsRow {
	return store.ListRunSkillAllocationsRow{
		TemplateName: tmpl,
		SkillID:      id,
		SkillName:    name,
		Description:  name + " description.",
		Body:         body,
		Scope:        scope,
	}
}

func unionNames(u []ClaimSkill) []string {
	out := make([]string, len(u))
	for i, s := range u {
		out[i] = s.Name
	}
	return out
}

func TestAssembleRunSkills_UnionAndPerTemplate(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	rows := []store.ListRunSkillAllocationsRow{
		skillRow("coder", "alpha", "global", a, "A"),
		skillRow("reviewer", "beta", "builtin", b, "B"),
		skillRow("coder", "beta", "builtin", b, "B"), // same skill on two templates
	}
	got := assembleRunSkills(rows, 32)

	if names := unionNames(got.union); !reflect.DeepEqual(names, []string{"alpha", "beta"}) {
		t.Errorf("union names = %v, want [alpha beta] (deduped, sorted)", names)
	}
	if !reflect.DeepEqual(got.perTemplate["coder"], []string{"alpha", "beta"}) {
		t.Errorf("coder skills = %v", got.perTemplate["coder"])
	}
	if !reflect.DeepEqual(got.perTemplate["reviewer"], []string{"beta"}) {
		t.Errorf("reviewer skills = %v", got.perTemplate["reviewer"])
	}
	if len(got.dropped) != 0 {
		t.Errorf("expected no drops, got %v", got.dropped)
	}
}

func TestAssembleRunSkills_DedupesSharedAndOverlay(t *testing.T) {
	id := uuid.New()
	// The same skill allocated to one template both as a shared row and as the
	// owner's overlay must yield exactly one union entry and one per-template name.
	rows := []store.ListRunSkillAllocationsRow{
		skillRow("coder", "kb", "global", id, "K"),
		skillRow("coder", "kb", "global", id, "K"),
	}
	got := assembleRunSkills(rows, 32)
	if len(got.union) != 1 || got.union[0].Name != "kb" {
		t.Fatalf("union = %v, want single kb", got.union)
	}
	if !reflect.DeepEqual(got.perTemplate["coder"], []string{"kb"}) {
		t.Errorf("coder skills = %v, want [kb]", got.perTemplate["coder"])
	}
}

func TestAssembleRunSkills_PrecedenceShadowsByName(t *testing.T) {
	builtinID, userID := uuid.New(), uuid.New()
	// A user skill named "x" shadows the builtin "x": the union carries the user
	// body, the builtin is a shadowed drop, and the template that allocated the
	// builtin still lists "x" (the name survives).
	rows := []store.ListRunSkillAllocationsRow{
		skillRow("coder", "x", "builtin", builtinID, "BUILTIN-BODY"),
		skillRow("coder", "x", "user", userID, "USER-BODY"),
	}
	got := assembleRunSkills(rows, 32)
	if len(got.union) != 1 || got.union[0].Body != "USER-BODY" {
		t.Fatalf("union should carry the user body, got %+v", got.union)
	}
	if !reflect.DeepEqual(got.perTemplate["coder"], []string{"x"}) {
		t.Errorf("coder skills = %v, want [x] (name survives)", got.perTemplate["coder"])
	}
	if len(got.dropped) != 1 || got.dropped[0].Name != "x" || got.dropped[0].Reason != DropShadowed {
		t.Errorf("expected one shadowed drop of x, got %v", got.dropped)
	}
}

func TestAssembleRunSkills_CapDropsLowestPrecedenceFirst(t *testing.T) {
	userID, builtinID := uuid.New(), uuid.New()
	rows := []store.ListRunSkillAllocationsRow{
		skillRow("coder", "keep", "user", userID, "U"),
		skillRow("coder", "drop", "builtin", builtinID, "B"),
	}
	got := assembleRunSkills(rows, 1)
	if names := unionNames(got.union); !reflect.DeepEqual(names, []string{"keep"}) {
		t.Fatalf("union = %v, want [keep] (builtin dropped over cap)", names)
	}
	// The over-limit name is removed from the template's list too — never deliver a
	// name whose body isn't in the union.
	if !reflect.DeepEqual(got.perTemplate["coder"], []string{"keep"}) {
		t.Errorf("coder skills = %v, want [keep]", got.perTemplate["coder"])
	}
	if len(got.dropped) != 1 || got.dropped[0].Name != "drop" || got.dropped[0].Reason != DropOverLimit {
		t.Errorf("expected one over_limit drop of 'drop', got %v", got.dropped)
	}
}

func TestAssembleRunSkills_Empty(t *testing.T) {
	got := assembleRunSkills(nil, 32)
	if len(got.union) != 0 || len(got.dropped) != 0 || len(got.perTemplate) != 0 {
		t.Errorf("empty input should yield empty output, got %+v", got)
	}
	// dropped is a non-nil empty slice so the payload serializes `[]`, not null.
	if got.dropped == nil {
		t.Error("dropped should be a non-nil empty slice")
	}
}

// TestClaimDeliversSkills exercises the whole assembly through Claim: the fake
// store returns skill allocations, and the payload must carry the union, the
// per-template ClaimAgent.skills, the repo opt-in flag, and the config caps.
func TestClaimDeliversSkills(t *testing.T) {
	box := newBox(t)
	sealedPAT, _ := box.Seal([]byte("bot-pat-SKILLTEST-abcdef1234567890"))
	sealedTok, _ := box.Seal([]byte("anthropic-SKILLTEST-abcdef1234567890"))
	kbID := uuid.New()

	fs := &fakeStore{
		claimRun: store.Run{ID: uuid.New(), IssueIid: pgtype.Int8{Int64: 5, Valid: true}, Status: "claimed"},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p",
			ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
			RepoSkillsEnabled: true,
		},
		anthropic: sealedTok,
		templates: []store.AgentTemplate{
			{Name: "coder", Description: "writes code", PromptBody: "you code"},
			{Name: "reviewer", Description: "reviews", PromptBody: "you review"},
		},
		skillAllocations: []store.ListRunSkillAllocationsRow{
			skillRow("coder", "ci-cd-norms", "builtin", kbID, "CICD BODY"),
		},
	}

	payload, err := New(fs, box, testParams()).Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if names := unionNames(payload.Skills); !reflect.DeepEqual(names, []string{"ci-cd-norms"}) {
		t.Fatalf("payload.Skills = %v, want [ci-cd-norms]", names)
	}
	if payload.Skills[0].Body != "CICD BODY" {
		t.Errorf("skill body not carried: %q", payload.Skills[0].Body)
	}
	if !payload.Repo.SkillsEnabled {
		t.Error("repo.skills_enabled should ride the claim when the repo opted in")
	}
	if payload.Config.SkillMaxBytes != 65536 || payload.Config.SkillsMaxPerRun != 32 {
		t.Errorf("config skill caps wrong: %+v", payload.Config)
	}
	// Per-template scoping: coder got the skill, reviewer got an explicit empty list.
	var coder, reviewer *ClaimAgent
	for i := range payload.Agents {
		switch payload.Agents[i].Name {
		case "coder":
			coder = &payload.Agents[i]
		case "reviewer":
			reviewer = &payload.Agents[i]
		}
	}
	if coder == nil || !reflect.DeepEqual(coder.Skills, []string{"ci-cd-norms"}) {
		t.Errorf("coder.Skills = %v, want [ci-cd-norms]", coder)
	}
	if reviewer == nil || reviewer.Skills == nil || len(reviewer.Skills) != 0 {
		t.Errorf("reviewer.Skills should be an explicit empty list, got %v", reviewer)
	}
	if payload.SkillsDropped == nil {
		t.Error("SkillsDropped must be a non-nil slice (serialized as [])")
	}
}
