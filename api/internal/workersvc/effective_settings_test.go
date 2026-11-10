package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// predictFake scripts the facts PredictHarness and PredictLead read, over poolFakeStore's D11
// credential surface. The embedded Store is nil: a reader that reached for anything else would
// panic and fail the test.
type predictFake struct {
	*poolFakeStore
	defaultHarness string // users.default_harness; "" = NULL
	policy         store.GetHarnessCreationPolicyRow
	candidate      store.GetCodexDefaultSelectionCandidateRow
	pins           []store.ListUserCrossCheckPinsRow
	templates      []store.AgentTemplate
	pinsErr        error
	candidateReads int
}

func (f *predictFake) GetUserDefaultHarness(context.Context, uuid.UUID) (pgtype.Text, error) {
	return pgconv.TextOrNull(f.defaultHarness), nil
}

func (f *predictFake) GetHarnessCreationPolicy(context.Context, uuid.UUID) (store.GetHarnessCreationPolicyRow, error) {
	return f.policy, nil
}

func (f *predictFake) GetCodexDefaultSelectionCandidate(context.Context, store.GetCodexDefaultSelectionCandidateParams) (store.GetCodexDefaultSelectionCandidateRow, error) {
	f.candidateReads++
	return f.candidate, nil
}

func (f *predictFake) ListUserCrossCheckPins(context.Context, uuid.UUID) ([]store.ListUserCrossCheckPinsRow, error) {
	return f.pins, f.pinsErr
}

func (f *predictFake) ListClaimAgentTemplates(context.Context, pgtype.UUID) ([]store.AgentTemplate, error) {
	return f.templates, nil
}

// bothUsableFake is a user with an enabled Anthropic token and a linked default Codex subscription.
func bothUsableFake() *predictFake {
	return &predictFake{
		poolFakeStore: &poolFakeStore{hasAnthropic: true, totalCodex: 1, enabledCodex: 1, subDefault: subDefault(), subLinked: true},
		candidate:     store.GetCodexDefaultSelectionCandidateRow(eligiblePoolRow()),
	}
}

func predict(t *testing.T, f *predictFake, explicit *Harness, o PredictOrigin) (HarnessPrediction, error) {
	t.Helper()
	return (&Service{q: f}).PredictHarness(context.Background(), uuid.New(), explicit, o)
}

func hp(h Harness) *Harness { return &h }

func TestPredictHarnessCreationPolicies(t *testing.T) {
	manual := IssueStartOrigin()
	cases := []struct {
		name       string
		fake       func() *predictFake
		explicit   *Harness
		origin     PredictOrigin
		wantHarn   Harness // "" = chosen at create
		wantPolicy string
		wantRule   string
		wantSkip   bool
	}{
		{
			name: "unset default, both usable, eligible origin: automatic policy decides at create",
			fake: bothUsableFake, origin: manual, wantPolicy: PredictPolicyAutomaticWeeklyReset, wantRule: PredictRuleAutomatic,
		},
		{
			name:   "unset default with overflow on stays the automatic policy",
			fake:   func() *predictFake { f := bothUsableFake(); f.policy.HarnessOverflowEnabled = true; return f },
			origin: manual, wantPolicy: PredictPolicyAutomaticWeeklyReset, wantRule: PredictRuleAutomatic,
		},
		{
			name: "an explicit harness is exact even with overflow on",
			fake: func() *predictFake {
				f := bothUsableFake()
				f.defaultHarness = "claude"
				f.policy.HarnessOverflowEnabled = true
				return f
			},
			explicit: hp(HarnessCodex), origin: manual, wantHarn: HarnessCodex, wantRule: PredictRuleExplicit,
		},
		{
			name:     "an explicit harness is exact under the automatic policy",
			fake:     bothUsableFake,
			explicit: hp(HarnessClaude), origin: manual, wantHarn: HarnessClaude, wantRule: PredictRuleExplicit,
		},
		{
			name:   "stored default with overflow off is exact",
			fake:   func() *predictFake { f := bothUsableFake(); f.defaultHarness = "codex"; return f },
			origin: manual, wantHarn: HarnessCodex, wantRule: PredictRuleUserDefault,
		},
		{
			name: "stored default with overflow on is chosen at create",
			fake: func() *predictFake {
				f := bothUsableFake()
				f.defaultHarness = "codex"
				f.policy.HarnessOverflowEnabled = true
				return f
			},
			origin: manual, wantPolicy: PredictPolicyOverflow, wantRule: PredictRuleUserDefault,
		},
		{
			name: "overflow with Claude preferred and a structurally available Codex default is chosen at create",
			fake: func() *predictFake {
				f := bothUsableFake()
				f.defaultHarness = "claude"
				f.policy.HarnessOverflowEnabled = true
				return f
			},
			origin: manual, wantPolicy: PredictPolicyOverflow, wantRule: PredictRuleUserDefault,
		},
		{
			// Deviation 7: creation excludes overflow when a Claude-preferred run's Codex default is
			// structurally unavailable, so the preview must return the exact harness.
			name: "overflow with Claude preferred and a structurally unavailable Codex default is exact",
			fake: func() *predictFake {
				f := bothUsableFake()
				f.defaultHarness = "claude"
				f.policy.HarnessOverflowEnabled = true
				f.candidate = store.GetCodexDefaultSelectionCandidateRow(rowWith(func(r *store.ListCodexPoolAliasesRow) { r.ReauthRequired = pgtype.Bool{Bool: true, Valid: true} }))
				return f
			},
			origin: manual, wantHarn: HarnessClaude, wantRule: PredictRuleUserDefault,
		},
		{
			name: "overflow with Codex preferred keeps the overflow even when its own default is not structural",
			fake: func() *predictFake {
				f := bothUsableFake()
				f.defaultHarness = "codex"
				f.policy.HarnessOverflowEnabled = true
				f.candidate = store.GetCodexDefaultSelectionCandidateRow(rowWith(func(r *store.ListCodexPoolAliasesRow) { r.ReauthRequired = pgtype.Bool{Bool: true, Valid: true} }))
				return f
			},
			origin: manual, wantPolicy: PredictPolicyOverflow, wantRule: PredictRuleUserDefault,
		},
		{
			name: "overflow with the other harness unusable is exact",
			fake: func() *predictFake {
				f := bothUsableFake()
				f.defaultHarness = "codex"
				f.policy.HarnessOverflowEnabled = true
				f.hasAnthropic = false
				return f
			},
			origin: manual, wantHarn: HarnessCodex, wantRule: PredictRuleUserDefault,
		},
		{
			name: "default harness unusable falls through with the note",
			fake: func() *predictFake {
				f := bothUsableFake()
				f.defaultHarness = "codex"
				f.totalCodex, f.enabledCodex = 0, 0
				return f
			},
			origin: manual, wantHarn: HarnessClaude, wantRule: PredictRuleAutomatic, wantSkip: true,
		},
		{
			name: "a non-inherit Anthropic override makes the policies ineligible",
			fake: bothUsableFake, origin: PredictOrigin{Kind: "issue", TriggerSource: "manual", AnthropicOverride: true},
			wantHarn: HarnessClaude, wantRule: PredictRuleAutomatic,
		},
		{
			name: "a run model makes the policies ineligible",
			fake: bothUsableFake, origin: PredictOrigin{Kind: "issue", TriggerSource: "manual", Model: strp("sonnet")},
			wantHarn: HarnessClaude, wantRule: PredictRuleAutomatic,
		},
		{
			name:   "plan cross-check makes the policies ineligible",
			fake:   func() *predictFake { f := bothUsableFake(); f.policy.PlanCrossCheckEnabled = true; return f },
			origin: manual, wantHarn: HarnessClaude, wantRule: PredictRuleAutomatic,
		},
		{
			name: "a non-issue kind is never eligible",
			fake: bothUsableFake, origin: PredictOrigin{Kind: "task", TriggerSource: "manual"},
			wantHarn: HarnessClaude, wantRule: PredictRuleAutomatic,
		},
		{
			name: "a chat trigger is never eligible",
			fake: bothUsableFake, origin: PredictOrigin{Kind: "issue", TriggerSource: "chat"},
			wantHarn: HarnessClaude, wantRule: PredictRuleAutomatic,
		},
		{
			name:   "sole usable harness is automatic and exact",
			fake:   func() *predictFake { f := bothUsableFake(); f.hasAnthropic = false; return f },
			origin: manual, wantHarn: HarnessCodex, wantRule: PredictRuleAutomatic,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := predict(t, c.fake(), c.explicit, c.origin)
			if err != nil {
				t.Fatal(err)
			}
			if got.Harness != c.wantHarn || got.ChosenAtCreate != (c.wantHarn == "") || got.Policy != c.wantPolicy {
				t.Fatalf("harness=%q chosenAtCreate=%v policy=%q, want harness=%q policy=%q", got.Harness, got.ChosenAtCreate, got.Policy, c.wantHarn, c.wantPolicy)
			}
			if got.Rule != c.wantRule || got.DefaultSkipped != c.wantSkip {
				t.Fatalf("rule=%q skipped=%v, want %q %v", got.Rule, got.DefaultSkipped, c.wantRule, c.wantSkip)
			}
			if (got.Credential == nil) != got.ChosenAtCreate {
				t.Fatalf("credential = %+v with chosenAtCreate=%v", got.Credential, got.ChosenAtCreate)
			}
		})
	}
}

// TestPredictHarnessStructuralExclusionReadsTheDefaultCandidate pins deviation 7's mechanism: the
// exclusion is the structural check on the Codex default candidate, read only for a Claude-preferred
// overflow.
func TestPredictHarnessStructuralExclusionReadsTheDefaultCandidate(t *testing.T) {
	f := bothUsableFake()
	f.defaultHarness = "claude"
	f.policy.HarnessOverflowEnabled = true
	if _, err := predict(t, f, nil, IssueStartOrigin()); err != nil {
		t.Fatal(err)
	}
	if f.candidateReads != 1 {
		t.Fatalf("candidate reads = %d, want 1", f.candidateReads)
	}
}

func TestPredictHarnessCredentials(t *testing.T) {
	t.Run("claude is chosen at claim", func(t *testing.T) {
		got, err := predict(t, bothUsableFake(), hp(HarnessClaude), IssueStartOrigin())
		if err != nil {
			t.Fatal(err)
		}
		want := CredentialPrediction{Kind: CredentialKindAnthropic, Source: CredentialSourceClaimSelection, FixedAt: FixedAtClaim}
		if *got.Credential != want {
			t.Fatalf("credential = %+v, want %+v", *got.Credential, want)
		}
	})
	t.Run("codex default credential carries its auth mode", func(t *testing.T) {
		got, err := predict(t, bothUsableFake(), hp(HarnessCodex), IssueStartOrigin())
		if err != nil {
			t.Fatal(err)
		}
		want := CredentialPrediction{Kind: CredentialKindCodex, Source: CredentialSourceCodexDefault, FixedAt: FixedAtCreate, AuthMode: "subscription"}
		if *got.Credential != want {
			t.Fatalf("credential = %+v, want %+v", *got.Credential, want)
		}
	})
	t.Run("codex account mode auto with a pool is chosen at create", func(t *testing.T) {
		f := bothUsableFake()
		f.mode = "auto"
		f.pool = []store.ListCodexPoolAliasesRow{eligiblePoolRow()}
		got, err := predict(t, f, hp(HarnessCodex), IssueStartOrigin())
		if err != nil {
			t.Fatal(err)
		}
		if got.Credential.Source != CredentialSourceChosenAtCreate || got.Credential.Kind != CredentialKindCodex || got.Credential.FixedAt != FixedAtCreate {
			t.Fatalf("credential = %+v", *got.Credential)
		}
	})
}

func TestPredictHarnessRefusalsAreTheCreateSentinels(t *testing.T) {
	t.Run("explicit unusable", func(t *testing.T) {
		f := bothUsableFake()
		f.totalCodex, f.enabledCodex = 0, 0
		if _, err := predict(t, f, hp(HarnessCodex), IssueStartOrigin()); !errors.Is(err, ErrNoCredentialForHarness) || errors.Is(err, ErrHarnessCredentialDisabled) {
			t.Fatalf("err = %v, want the plain no_credential_for_harness sentinel", err)
		}
	})
	t.Run("explicit with every credential disabled", func(t *testing.T) {
		f := bothUsableFake()
		f.totalCodex, f.enabledCodex = 2, 0
		if _, err := predict(t, f, hp(HarnessCodex), IssueStartOrigin()); !errors.Is(err, ErrHarnessCredentialDisabled) {
			t.Fatalf("err = %v, want ErrHarnessCredentialDisabled", err)
		}
	})
	t.Run("explicit codex with an unavailable pool", func(t *testing.T) {
		f := bothUsableFake()
		f.totalCodex, f.enabledCodex, f.subDefault = 1, 1, nil
		f.mode = "auto"
		f.pool = []store.ListCodexPoolAliasesRow{rowWith(func(r *store.ListCodexPoolAliasesRow) { r.Enabled = false })}
		if _, err := predict(t, f, hp(HarnessCodex), IssueStartOrigin()); !errors.Is(err, ErrCodexPoolUnavailable) {
			t.Fatalf("err = %v, want ErrCodexPoolUnavailable", err)
		}
	})
	t.Run("neither usable", func(t *testing.T) {
		f := bothUsableFake()
		f.hasAnthropic = false
		f.totalCodex, f.enabledCodex = 0, 0
		if _, err := predict(t, f, nil, IssueStartOrigin()); !errors.Is(err, ErrNoUsableCredential) {
			t.Fatalf("err = %v, want ErrNoUsableCredential", err)
		}
	})
}

func TestPredictHarnessIsReadOnly(t *testing.T) {
	// The fake embeds a nil Store and implements only reads; a write through it would panic. The
	// automatic and overflow paths are the ones that could reach the limit evidence or the lock.
	f := bothUsableFake()
	f.defaultHarness = "codex"
	f.policy.HarnessOverflowEnabled = true
	if _, err := predict(t, f, nil, IssueStartOrigin()); err != nil {
		t.Fatal(err)
	}
	if _, err := predict(t, bothUsableFake(), nil, IssueStartOrigin()); err != nil {
		t.Fatal(err)
	}
}

func pinRow(mut func(*store.ListUserCrossCheckPinsRow)) []store.ListUserCrossCheckPinsRow {
	var r store.ListUserCrossCheckPinsRow
	if mut != nil {
		mut(&r)
	}
	return []store.ListUserCrossCheckPinsRow{r}
}

func predictLead(t *testing.T, f *predictFake, h Harness, frozen *string, override, review bool) LeadPrediction {
	t.Helper()
	got, err := (&Service{q: f}).PredictLead(context.Background(), uuid.New(), h, frozen, override, review)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPredictLeadReviewBranchIsDecidedFirst(t *testing.T) {
	// A review never reads the lanes: a store whose lane read errors still predicts the review.
	f := &predictFake{poolFakeStore: &poolFakeStore{}, pinsErr: errors.New("lane read must not happen")}
	for _, c := range []struct {
		harness Harness
		model   string
	}{{HarnessClaude, ""}, {HarnessCodex, "gpt-6-sol"}} {
		got := predictLead(t, f, c.harness, nil, false, true)
		if !got.Review || got.Model != c.model || got.ModelSource != "builtin" || got.Effort != nil || got.EffortSource != "builtin" || got.Subagents != nil {
			t.Fatalf("%s review = %+v", c.harness, got)
		}
	}
	if agenttmpl.CodexTaskReviewModel != "gpt-6-sol" || agenttmpl.ClaudeTaskReviewModel != "" {
		t.Fatal("task review built-ins moved; the review branch reads them")
	}
}

func TestPredictLeadModelAndEffort(t *testing.T) {
	cases := []struct {
		name               string
		harness            Harness
		mut                func(*store.ListUserCrossCheckPinsRow)
		frozen             *string
		wantModel, wantSrc string
		wantClaim          *string // the default_model the claim ships
		wantDefault        string
		wantEffort         *string
		wantEffortSrc      string
	}{
		{
			name: "claude nothing configured is the SDK default", harness: HarnessClaude,
			wantModel: "", wantSrc: "builtin", wantDefault: "", wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
		{
			name: "claude lead template supplies the worker default", harness: HarnessClaude,
			mut:       func(r *store.ListUserCrossCheckPinsRow) { r.TemplateModel = pgconv.TextOrNull("opus") },
			wantModel: "opus", wantSrc: "template", wantDefault: "opus", wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
		{
			name: "claude lane wins and ships", harness: HarnessClaude,
			mut: func(r *store.ListUserCrossCheckPinsRow) {
				r.DefaultClaudeModel = pgconv.TextOrNull("sonnet")
				r.TemplateModel = pgconv.TextOrNull("opus")
			},
			wantModel: "sonnet", wantSrc: "user_default", wantClaim: strp("sonnet"), wantDefault: "opus", wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
		{
			name: "claude lane holding a Codex role falls to the template", harness: HarnessClaude,
			mut: func(r *store.ListUserCrossCheckPinsRow) {
				r.DefaultClaudeModel = pgconv.TextOrNull("gpt-6-luna")
				r.TemplateModel = pgconv.TextOrNull("opus")
			},
			wantModel: "opus", wantSrc: "template", wantDefault: "opus", wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
		{
			name: "legacy luna template on Claude is the SDK default", harness: HarnessClaude,
			mut:       func(r *store.ListUserCrossCheckPinsRow) { r.TemplateModel = pgconv.TextOrNull("gpt-6-luna") },
			wantModel: "", wantSrc: "builtin", wantDefault: "", wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
		{
			name: "claude empty lane runs the template, as the worker's || does", harness: HarnessClaude,
			mut: func(r *store.ListUserCrossCheckPinsRow) {
				r.DefaultClaudeModel = pgtype.Text{String: "", Valid: true}
				r.TemplateModel = pgconv.TextOrNull("opus")
			},
			wantModel: "opus", wantSrc: "template", wantClaim: strp(""), wantDefault: "opus", wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
		{
			name: "claude empty lane with no template is the SDK default", harness: HarnessClaude,
			mut:       func(r *store.ListUserCrossCheckPinsRow) { r.DefaultClaudeModel = pgtype.Text{String: "", Valid: true} },
			wantModel: "", wantSrc: "builtin", wantClaim: strp(""), wantDefault: "", wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
		{
			name: "codex empty lane stays empty, as the worker's ?? does", harness: HarnessCodex,
			mut:       func(r *store.ListUserCrossCheckPinsRow) { r.DefaultCodexModel = pgtype.Text{String: "", Valid: true} },
			wantModel: "", wantSrc: "user_default", wantClaim: strp(""), wantDefault: agenttmpl.DefaultCodexModel, wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
		{
			name: "codex nothing configured is the built-in", harness: HarnessCodex,
			wantModel: agenttmpl.DefaultCodexModel, wantSrc: "builtin", wantDefault: agenttmpl.DefaultCodexModel, wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
		{
			name: "codex lead pin is the worker default", harness: HarnessCodex,
			mut:       func(r *store.ListUserCrossCheckPinsRow) { r.TemplateCodexModel = pgconv.TextOrNull("gpt-6-sol") },
			wantModel: "gpt-6-sol", wantSrc: "template", wantDefault: "gpt-6-sol", wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
		{
			name: "codex lane ships", harness: HarnessCodex,
			mut:       func(r *store.ListUserCrossCheckPinsRow) { r.DefaultCodexModel = pgconv.TextOrNull("gpt-6-sol") },
			wantModel: "gpt-6-sol", wantSrc: "user_default", wantClaim: strp("gpt-6-sol"), wantDefault: agenttmpl.DefaultCodexModel, wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
		{
			name: "codex runtime effort sends nothing", harness: HarnessCodex,
			mut:       func(r *store.ListUserCrossCheckPinsRow) { r.DefaultCodexEffort = pgconv.TextOrNull("runtime") },
			wantModel: agenttmpl.DefaultCodexModel, wantSrc: "builtin", wantDefault: agenttmpl.DefaultCodexModel, wantEffort: nil, wantEffortSrc: "user_default",
		},
		{
			name: "claude effort is its own column", harness: HarnessClaude,
			mut: func(r *store.ListUserCrossCheckPinsRow) {
				r.DefaultEffort = pgconv.TextOrNull("high")
				r.DefaultCodexEffort = pgconv.TextOrNull("low")
			},
			wantModel: "", wantSrc: "builtin", wantDefault: "", wantEffort: strp("high"), wantEffortSrc: "user_default",
		},
		{
			name: "compatible schedule pin wins", harness: HarnessClaude, frozen: strp("haiku"),
			mut:       func(r *store.ListUserCrossCheckPinsRow) { r.DefaultClaudeModel = pgconv.TextOrNull("opus") },
			wantModel: "haiku", wantSrc: "schedule_pin", wantClaim: strp("haiku"), wantDefault: "", wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
		{
			name: "incompatible schedule pin falls to the lane", harness: HarnessCodex, frozen: strp("sonnet"),
			mut:       func(r *store.ListUserCrossCheckPinsRow) { r.DefaultCodexModel = pgconv.TextOrNull("gpt-6-sol") },
			wantModel: "gpt-6-sol", wantSrc: "user_default", wantClaim: strp("gpt-6-sol"), wantDefault: agenttmpl.DefaultCodexModel, wantEffort: strp("medium"), wantEffortSrc: "builtin",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &predictFake{poolFakeStore: &poolFakeStore{}, pins: pinRow(c.mut)}
			got := predictLead(t, f, c.harness, c.frozen, false, false)
			if got.Model != c.wantModel || got.ModelSource != c.wantSrc {
				t.Errorf("model = %q (%s), want %q (%s)", got.Model, got.ModelSource, c.wantModel, c.wantSrc)
			}
			if got.DefaultModel != c.wantDefault {
				t.Errorf("default model = %q, want %q", got.DefaultModel, c.wantDefault)
			}
			if derefOrNil(got.ClaimModel) != derefOrNil(c.wantClaim) {
				t.Errorf("claim model = %v, want %v", got.ClaimModel, c.wantClaim)
			}
			if derefOrNil(got.Effort) != derefOrNil(c.wantEffort) || got.EffortSource != c.wantEffortSrc {
				t.Errorf("effort = %v (%s), want %v (%s)", got.Effort, got.EffortSource, c.wantEffort, c.wantEffortSrc)
			}
		})
	}
}

func derefOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func template(name string, claudePin, codexPin string) store.AgentTemplate {
	return store.AgentTemplate{Name: name, Model: pgconv.TextOrNull(claudePin), CodexModel: pgconv.TextOrNull(codexPin)}
}

func TestPredictLeadSubagents(t *testing.T) {
	templates := []store.AgentTemplate{
		template("coder", "sonnet", "gpt-6-sol"),
		template("lead", "opus", ""),
		template("tester", "", ""),
		template("reviewer", "gpt-6-luna", ""), // a legacy Codex pin on the Claude column is filtered out
	}
	base := func() *predictFake {
		return &predictFake{poolFakeStore: &poolFakeStore{}, pins: pinRow(func(r *store.ListUserCrossCheckPinsRow) {
			r.TemplateModel = pgconv.TextOrNull("opus")
			r.TemplateCodexModel = pgconv.TextOrNull("gpt-6-sol")
		}), templates: templates}
	}
	t.Run("override off on Claude lists the own pins, minus the lead", func(t *testing.T) {
		got := predictLead(t, base(), HarnessClaude, nil, false, false).Subagents
		want := []SubagentPin{{Name: "coder", Model: "sonnet"}}
		if got.Mode != SubagentModeOwnPins || got.LeadValue != "opus" || fmt.Sprint(got.Pinned) != fmt.Sprint(want) || got.PinnedTruncated {
			t.Fatalf("subagents = %+v", got)
		}
	})
	t.Run("Codex reads the Codex column", func(t *testing.T) {
		got := predictLead(t, base(), HarnessCodex, nil, false, false).Subagents
		if got.Mode != SubagentModeOwnPins || fmt.Sprint(got.Pinned) != fmt.Sprint([]SubagentPin{{Name: "coder", Model: "gpt-6-sol"}}) {
			t.Fatalf("subagents = %+v", got)
		}
	})
	t.Run("override on follows the lead on both harnesses", func(t *testing.T) {
		for _, h := range []Harness{HarnessClaude, HarnessCodex} {
			got := predictLead(t, base(), h, nil, true, false)
			if got.Subagents.Mode != SubagentModeFollowLead || got.Subagents.LeadValue != got.Model || got.Subagents.Pinned != nil {
				t.Fatalf("%s subagents = %+v (lead %q)", h, got.Subagents, got.Model)
			}
		}
	})
	t.Run("Claude with an empty lead keeps its own pins even with the override on", func(t *testing.T) {
		f := &predictFake{poolFakeStore: &poolFakeStore{}, pins: pinRow(nil), templates: []store.AgentTemplate{template("coder", "sonnet", "")}}
		got := predictLead(t, f, HarnessClaude, nil, true, false).Subagents
		if got.Mode != SubagentModeOwnPins || got.LeadValue != "" || len(got.Pinned) != 1 {
			t.Fatalf("subagents = %+v", got)
		}
	})
	t.Run("the pinned list is sorted and capped at 32", func(t *testing.T) {
		var many []store.AgentTemplate
		for i := 40; i > 0; i-- {
			many = append(many, template(fmt.Sprintf("agent-%02d", i), "sonnet", ""))
		}
		f := &predictFake{poolFakeStore: &poolFakeStore{}, pins: pinRow(nil), templates: many}
		got := predictLead(t, f, HarnessClaude, nil, false, false).Subagents
		if len(got.Pinned) != 32 || !got.PinnedTruncated || got.Pinned[0].Name != "agent-01" || got.Pinned[31].Name != "agent-32" {
			t.Fatalf("pinned = %d truncated=%v first=%v last=%v", len(got.Pinned), got.PinnedTruncated, got.Pinned[0], got.Pinned[len(got.Pinned)-1])
		}
	})
}

func TestPredictLeadStoreErrorFailsTheWholePrediction(t *testing.T) {
	f := &predictFake{poolFakeStore: &poolFakeStore{}, pinsErr: errors.New("boom")}
	if _, err := (&Service{q: f}).PredictLead(context.Background(), uuid.New(), HarnessClaude, nil, false, false); err == nil {
		t.Fatal("PredictLead swallowed a store error")
	}
}
