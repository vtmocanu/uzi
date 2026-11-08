package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// defaultHarnessFake is poolFakeStore with a scripted users.default_harness.
type defaultHarnessFake struct {
	*poolFakeStore
	def pgtype.Text
}

func (f defaultHarnessFake) GetUserDefaultHarness(context.Context, uuid.UUID) (pgtype.Text, error) {
	return f.def, nil
}

// TestResolveRunHarnessReportsRuleAndSkippedDefault pins the Rule and DefaultSkipped fields the
// store-backed resolver adds for read-only previews.
func TestResolveRunHarnessReportsRuleAndSkippedDefault(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	linked := func(claude bool) *poolFakeStore {
		return &poolFakeStore{mode: "default", hasAnthropic: claude, totalCodex: 1, enabledCodex: 1, subDefault: subDefault(), subLinked: true}
	}
	for _, c := range []struct {
		name        string
		fake        *poolFakeStore
		def         pgtype.Text
		explicit    *Harness
		want        Harness
		wantRule    harnessRule
		wantSkipped bool
	}{
		{"explicit", linked(true), pgtype.Text{}, h(HarnessCodex), HarnessCodex, harnessRuleExplicit, false},
		{"usable default", linked(true), txt("codex"), nil, HarnessCodex, harnessRuleUserDefault, false},
		{"unusable default codex, sole claude", &poolFakeStore{mode: "default", hasAnthropic: true}, txt("codex"), nil, HarnessClaude, harnessRuleSoleUsable, true},
		{"unusable default claude, sole codex", linked(false), txt("claude"), nil, HarnessCodex, harnessRuleSoleUsable, true},
		{"no default, sole codex", linked(false), pgtype.Text{}, nil, HarnessCodex, harnessRuleSoleUsable, false},
		{"no default, both usable", linked(true), pgtype.Text{}, nil, HarnessClaude, harnessRuleBothClaude, false},
		{"explicit ignores an unusable default", linked(true), txt("claude"), h(HarnessCodex), HarnessCodex, harnessRuleExplicit, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := defaultHarnessFake{poolFakeStore: c.fake, def: c.def}
			svc := &Service{q: f}
			res, err := svc.resolveRunHarnessQ(ctx, userID, c.explicit, f)
			if err != nil {
				t.Fatal(err)
			}
			if res.Harness != c.want || res.Rule != c.wantRule || res.DefaultSkipped != c.wantSkipped {
				t.Fatalf("got harness=%q rule=%q skipped=%t, want %q %q %t", res.Harness, res.Rule, res.DefaultSkipped, c.want, c.wantRule, c.wantSkipped)
			}
		})
	}
}
