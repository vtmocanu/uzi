package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func txt(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

func TestResolveLeadSettingsModel(t *testing.T) {
	type want struct {
		model, source, droppedLane, droppedRun string // "" = invalid / unset
	}
	cases := []struct {
		name string
		in   LeadSettingsInput
		want want
	}{
		{"claude lane preserved", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus")},
			want{model: "opus", source: "user_default"}},
		{"claude custom id preserved", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("claude-custom-preview")},
			want{model: "claude-custom-preview", source: "user_default"}},
		{"claude empty lane ships nothing", LeadSettingsInput{Harness: HarnessClaude},
			want{}},
		{"codex lane untouched by the guard", LeadSettingsInput{Harness: HarnessCodex, LaneModel: txt("gpt-6-luna")},
			want{model: "gpt-6-luna", source: "user_default"}},
		{"codex custom lane untouched", LeadSettingsInput{Harness: HarnessCodex, LaneModel: txt("custom-codex-preview")},
			want{model: "custom-codex-preview", source: "user_default"}},
		{"schedule pin beats the lane", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus"), RunModel: txt("sonnet")},
			want{model: "sonnet", source: "schedule_pin"}},
		{"schedule pin with no lane", LeadSettingsInput{Harness: HarnessClaude, RunModel: txt("sonnet")},
			want{model: "sonnet", source: "schedule_pin"}},
		{"valid but blank pin still wins", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus"), RunModel: txt("  ")},
			want{model: "  ", source: "schedule_pin"}},
		{"incompatible pin dropped, lane kept", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus"), RunModel: txt(" gpt-6-sol ")},
			want{model: "opus", source: "user_default", droppedRun: " gpt-6-sol "}},
		{"incompatible pin dropped, lane empty", LeadSettingsInput{Harness: HarnessClaude, RunModel: txt("gpt-6-sol")},
			want{droppedRun: "gpt-6-sol"}},
		{"codex incompatible pin dropped", LeadSettingsInput{Harness: HarnessCodex, LaneModel: txt("gpt-6-sol"), RunModel: txt("opus")},
			want{model: "gpt-6-sol", source: "user_default", droppedRun: "opus"}},
		{"both dropped", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("gpt-6-luna"), RunModel: txt("gpt-6-sol")},
			want{droppedLane: "gpt-6-luna", droppedRun: "gpt-6-sol"}},
		{"lane dropped, pin wins", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("gpt-6-luna"), RunModel: txt("sonnet")},
			want{model: "sonnet", source: "schedule_pin", droppedLane: "gpt-6-luna"}},
	}
	// Every Codex role id in the Claude lane is discarded, with a trimmed note value.
	for id := range agenttmpl.CodexRoleModels {
		cases = append(cases, struct {
			name string
			in   LeadSettingsInput
			want want
		}{"claude lane drops role " + id, LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("  " + id + " ")},
			want{droppedLane: id}})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveLeadSettings(c.in)
			str := func(v pgtype.Text) string {
				if !v.Valid {
					return ""
				}
				return v.String
			}
			if str(got.Model) != c.want.model {
				t.Errorf("Model = %q, want %q", str(got.Model), c.want.model)
			}
			if string(got.ModelSource) != c.want.source {
				t.Errorf("ModelSource = %q, want %q", got.ModelSource, c.want.source)
			}
			if got.Model.Valid != (got.ModelSource != "") {
				t.Errorf("ModelSource %q must be empty exactly when Model is invalid (valid=%v)", got.ModelSource, got.Model.Valid)
			}
			if str(got.DroppedLaneModel) != c.want.droppedLane || got.DroppedLaneModel.Valid != (c.want.droppedLane != "") {
				t.Errorf("DroppedLaneModel = %+v, want %q", got.DroppedLaneModel, c.want.droppedLane)
			}
			if str(got.DroppedRunModel) != c.want.droppedRun || got.DroppedRunModel.Valid != (c.want.droppedRun != "") {
				t.Errorf("DroppedRunModel = %+v, want %q", got.DroppedRunModel, c.want.droppedRun)
			}
		})
	}
}

func TestResolveLeadSettingsBlankPinKeepsScheduleSource(t *testing.T) {
	got := resolveLeadSettings(LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus"), RunModel: txt("   ")})
	if !got.Model.Valid || got.ModelSource != leadcfg.ModelSourceSchedulePin || got.DroppedRunModel.Valid {
		t.Fatalf("blank valid pin: %+v", got)
	}
}

// TestResolveLeadEffort pins the lead resolver to agenttmpl.ResolveHarnessEffort (#3247) for both
// harnesses: NULL, blank and a corrupt stored value are the explicit built-in level with source
// builtin; an exact level is user_default; `runtime` ships no effort (nil) with source user_default.
func TestResolveLeadEffort(t *testing.T) {
	cases := []struct {
		name   string
		effort pgtype.Text
		value  string // "" = nil (runtime default)
		source leadcfg.EffortSource
	}{
		{"null", pgtype.Text{}, "medium", leadcfg.EffortSourceBuiltin},
		{"empty", txt(""), "medium", leadcfg.EffortSourceBuiltin},
		{"whitespace", txt(" \t "), "medium", leadcfg.EffortSourceBuiltin},
		{"explicit level", txt("high"), "high", leadcfg.EffortSourceUserDefault},
		{"explicit padded", txt(" xhigh "), "xhigh", leadcfg.EffortSourceUserDefault},
		{"runtime", txt("runtime"), "", leadcfg.EffortSourceUserDefault},
		{"runtime padded", txt(" runtime "), "", leadcfg.EffortSourceUserDefault},
		{"corrupt", txt("HI gh"), "medium", leadcfg.EffortSourceBuiltin},
		{"corrupt unknown level", txt("ultra"), "medium", leadcfg.EffortSourceBuiltin},
	}
	for _, h := range []Harness{HarnessClaude, HarnessCodex} {
		for _, c := range cases {
			t.Run(string(h)+"/"+c.name, func(t *testing.T) {
				got, src := resolveLeadEffort(h, c.effort)
				if c.value == "" {
					if got != nil {
						t.Errorf("effort = %q, want nil (runtime default)", *got)
					}
				} else if got == nil || *got != c.value {
					t.Errorf("effort = %v, want %q", got, c.value)
				}
				if src != c.source {
					t.Errorf("source = %q, want %q", src, c.source)
				}
				// resolveLeadSettings and the ordinary-claim pointer agree with the lead resolver.
				settings := resolveLeadSettings(LeadSettingsInput{Harness: h, UserEffort: c.effort})
				if settings.EffortSource != c.source || (settings.Effort == nil) != (got == nil) {
					t.Errorf("resolveLeadSettings = (%v, %q), want (%v, %q)", settings.Effort, settings.EffortSource, got, c.source)
				}
				if ptr := resolveHarnessEffortPtr(h, c.effort); (ptr == nil) != (got == nil) || (ptr != nil && *ptr != *got) {
					t.Errorf("resolveHarnessEffortPtr = %v, lead effort = %v", ptr, got)
				}
			})
		}
	}
}

func TestLeadEffortTemplateTier(t *testing.T) {
	override := func(level string) (pgtype.Text, pgtype.Text) {
		return txt(level), txt(string(leadcfg.EffortSourceRunOverride))
	}
	runEffort, runSource := override("max")
	cases := []struct {
		name       string
		harness    Harness
		in         LeadSettingsInput
		wantEffort string // "" = nothing sent
		wantSource leadcfg.EffortSource
	}{
		{"NULL preference takes the template", HarnessClaude,
			LeadSettingsInput{Harness: HarnessClaude, TemplateEffort: txt("high")}, "high", leadcfg.EffortSourceTemplate},
		{"blank preference takes the template", HarnessCodex,
			LeadSettingsInput{Harness: HarnessCodex, UserEffort: txt("  \t"), TemplateEffort: txt("low")}, "low", leadcfg.EffortSourceTemplate},
		{"user level wins over the template", HarnessClaude,
			LeadSettingsInput{Harness: HarnessClaude, UserEffort: txt("low"), TemplateEffort: txt("high")}, "low", leadcfg.EffortSourceUserDefault},
		{"runtime preference wins and sends nothing", HarnessCodex,
			LeadSettingsInput{Harness: HarnessCodex, UserEffort: txt("runtime"), TemplateEffort: txt("high")}, "", leadcfg.EffortSourceUserDefault},
		{"corrupt preference keeps the builtin, not the template", HarnessClaude,
			LeadSettingsInput{Harness: HarnessClaude, UserEffort: txt("ultra"), TemplateEffort: txt("high")}, "medium", leadcfg.EffortSourceBuiltin},
		{"run override wins over the template and the preference", HarnessClaude,
			LeadSettingsInput{Harness: HarnessClaude, UserEffort: txt("low"), TemplateEffort: txt("high"), RunEffort: runEffort, RunEffortSource: runSource},
			"max", leadcfg.EffortSourceRunOverride},
		{"run override wins over the template with no preference", HarnessCodex,
			LeadSettingsInput{Harness: HarnessCodex, TemplateEffort: txt("high"), RunEffort: runEffort, RunEffortSource: runSource},
			"max", leadcfg.EffortSourceRunOverride},
		{"no template is the builtin", HarnessClaude,
			LeadSettingsInput{Harness: HarnessClaude}, "medium", leadcfg.EffortSourceBuiltin},
		{"hostile template value is ignored", HarnessCodex,
			LeadSettingsInput{Harness: HarnessCodex, TemplateEffort: txt("High")}, "medium", leadcfg.EffortSourceBuiltin},
		{"runtime is not a template level", HarnessCodex,
			LeadSettingsInput{Harness: HarnessCodex, TemplateEffort: txt("runtime")}, "medium", leadcfg.EffortSourceBuiltin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveLeadSettings(tc.in)
			if got.Unhonorable != nil {
				t.Fatalf("unexpected unhonorable %+v", got.Unhonorable)
			}
			if gotEffort := derefOr(got.Effort, ""); gotEffort != tc.wantEffort {
				t.Errorf("effort = %q, want %q", gotEffort, tc.wantEffort)
			}
			if (got.Effort == nil) != (tc.wantEffort == "") {
				t.Errorf("effort nil-ness = %v, want nothing sent = %v", got.Effort == nil, tc.wantEffort == "")
			}
			if got.EffortSource != tc.wantSource {
				t.Errorf("source = %q, want %q", got.EffortSource, tc.wantSource)
			}
		})
	}
}

func TestLeadEffortTemplateTierUnhonorableOverrideStillParks(t *testing.T) {
	got := resolveLeadSettings(LeadSettingsInput{
		Harness: HarnessClaude, TemplateEffort: txt("high"),
		RunEffort: txt("runtime"), RunEffortSource: txt(string(leadcfg.EffortSourceRunOverride)),
	})
	if got.Unhonorable == nil || got.Unhonorable.Setting != "effort" {
		t.Fatalf("an out-of-set run_override effort must stay unhonorable, got %+v", got.Unhonorable)
	}
}

// Lane control: the non-lead consumers resolve through leadEffortFor, which never reads a template
// tier, so the lead template's effort cannot reach them.
func TestLeadEffortForIgnoresTemplateTier(t *testing.T) {
	for _, h := range []Harness{HarnessClaude, HarnessCodex} {
		effort, source, bad := leadEffortFor(h, pgtype.Text{}, pgtype.Text{}, pgtype.Text{})
		if bad || derefOr(effort, "") != "medium" || source != leadcfg.EffortSourceBuiltin {
			t.Errorf("%s: leadEffortFor with no preference = %v %q %v, want builtin medium", h, effort, source, bad)
		}
	}
}

func TestLeadTemplateEffortPicksTheLeadTemplateByFamily(t *testing.T) {
	rows := []store.AgentTemplate{
		{Name: "coder", Effort: txt("low"), CodexEffort: txt("low")},
		{Name: "orchestrator", Effort: txt("high"), CodexEffort: txt("xhigh")},
		{Name: "lead", Effort: txt("max")},
	}
	if got := leadTemplateEffort(rows, HarnessClaude); got.String != "high" || !got.Valid {
		t.Errorf("claude = %+v, want the first lead-shaped template's claude effort", got)
	}
	if got := leadTemplateEffort(rows, HarnessCodex); got.String != "xhigh" || !got.Valid {
		t.Errorf("codex = %+v, want codex_effort", got)
	}
	if got := leadTemplateEffort(rows[:1], HarnessClaude); got.Valid {
		t.Errorf("no lead template = %+v, want unset", got)
	}
	if got := leadTemplateEffort([]store.AgentTemplate{{Name: "lead", Effort: txt("bogus")}}, HarnessClaude); got.Valid {
		t.Errorf("hostile lead effort = %+v, want unset", got)
	}
}
