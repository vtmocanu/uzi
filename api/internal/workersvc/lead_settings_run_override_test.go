package workersvc

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// PRD #3250: the lead resolver's run_override branches. Values that need a control character are
// assembled at runtime so no raw bidi byte sits in source.
func TestResolveLeadSettingsRunOverrideModel(t *testing.T) {
	bidi := "op" + string(rune(0x202e)) + "us"
	long := strings.Repeat("a", 101)
	src := func(s string) pgtype.Text { return txt(s) }
	type want struct {
		model, source string
		unhonorable   string // "" = honorable, else the refused setting
		value         string
		droppedRun    string
	}
	cases := []struct {
		name string
		in   LeadSettingsInput
		want want
	}{
		{"claude override wins over lane", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus"), RunModel: txt("sonnet"), RunModelSource: src("run_override")},
			want{model: "sonnet", source: "run_override"}},
		{"claude override with no lane", LeadSettingsInput{Harness: HarnessClaude, RunModel: txt("sonnet"), RunModelSource: src("run_override")},
			want{model: "sonnet", source: "run_override"}},
		{"codex curated override", LeadSettingsInput{Harness: HarnessCodex, LaneModel: txt("gpt-6-luna"), RunModel: txt("gpt-6-sol"), RunModelSource: src("run_override")},
			want{model: "gpt-6-sol", source: "run_override"}},
		{"codex non-curated override is unhonorable", LeadSettingsInput{Harness: HarnessCodex, LaneModel: txt("gpt-6-sol"), RunModel: txt("custom-codex-preview"), RunModelSource: src("run_override")},
			want{unhonorable: "model", value: "custom-codex-preview"}},
		{"claude role id override is unhonorable, never a dropped fallback", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus"), RunModel: txt("gpt-6-sol"), RunModelSource: src("run_override")},
			want{unhonorable: "model", value: "gpt-6-sol"}},
		{"claude blank override is unhonorable", LeadSettingsInput{Harness: HarnessClaude, RunModel: txt("   "), RunModelSource: src("run_override")},
			want{unhonorable: "model", value: "   "}},
		{"claude 101-byte override is unhonorable", LeadSettingsInput{Harness: HarnessClaude, RunModel: txt(long), RunModelSource: src("run_override")},
			want{unhonorable: "model", value: long}},
		{"claude bidi-control override is unhonorable", LeadSettingsInput{Harness: HarnessClaude, RunModel: txt(bidi), RunModelSource: src("run_override")},
			want{unhonorable: "model", value: bidi}},
		{"codex claude alias override is unhonorable", LeadSettingsInput{Harness: HarnessCodex, RunModel: txt("opus"), RunModelSource: src("run_override")},
			want{unhonorable: "model", value: "opus"}},
		{"schedule pin keeps the soft screen", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus"), RunModel: txt("sonnet"), RunModelSource: src("schedule_pin")},
			want{model: "sonnet", source: "schedule_pin"}},
		{"incompatible schedule pin falls back to the lane source", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus"), RunModel: txt("gpt-6-sol"), RunModelSource: src("schedule_pin")},
			want{model: "opus", source: "user_default", droppedRun: "gpt-6-sol"}},
		{"NULL source model keeps the existing branch", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus"), RunModel: txt("sonnet")},
			want{model: "sonnet", source: "schedule_pin"}},
		{"override source without a model changes nothing", LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus"), RunModelSource: src("run_override")},
			want{model: "opus", source: "user_default"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveLeadSettings(c.in)
			if c.want.unhonorable != "" {
				u := got.Unhonorable
				if u == nil || u.Setting != c.want.unhonorable || u.Value != c.want.value || u.Harness != c.in.Harness {
					t.Fatalf("Unhonorable = %+v, want %s %q on %s", u, c.want.unhonorable, c.want.value, c.in.Harness)
				}
				if got.DroppedRunModel.Valid {
					t.Fatalf("an unhonorable run_override must never be a dropped fallback: %+v", got.DroppedRunModel)
				}
				return
			}
			if got.Unhonorable != nil {
				t.Fatalf("Unhonorable = %+v, want none", got.Unhonorable)
			}
			if got.Model.String != c.want.model || got.Model.Valid != (c.want.model != "") || string(got.ModelSource) != c.want.source {
				t.Fatalf("model = %+v/%q, want %q/%q", got.Model, got.ModelSource, c.want.model, c.want.source)
			}
			if got.DroppedRunModel.String != c.want.droppedRun {
				t.Fatalf("DroppedRunModel = %q, want %q", got.DroppedRunModel.String, c.want.droppedRun)
			}
		})
	}
}

func TestLeadEffortFor(t *testing.T) {
	cases := []struct {
		name            string
		harness         Harness
		runEffort, srcE pgtype.Text
		user            pgtype.Text
		want            string // "" = nil effort (runtime default)
		wantSource      string
		unhonorable     bool
	}{
		{"override beats a lower user level", HarnessClaude, txt("xhigh"), txt("run_override"), txt("low"), "xhigh", "run_override", false},
		{"override beats NULL", HarnessClaude, txt("max"), txt("run_override"), pgtype.Text{}, "max", "run_override", false},
		{"override beats the runtime choice", HarnessCodex, txt("low"), txt("run_override"), txt("runtime"), "low", "run_override", false},
		{"every level is honorable", HarnessCodex, txt("medium"), txt("run_override"), txt("high"), "medium", "run_override", false},
		{"override outside the set is unhonorable", HarnessClaude, txt("ultra"), txt("run_override"), txt("low"), "", "", true},
		{"override with surrounding space is unhonorable", HarnessClaude, txt(" high"), txt("run_override"), txt("low"), "", "", true},
		{"override runtime token is unhonorable", HarnessClaude, txt("runtime"), txt("run_override"), txt("low"), "", "", true},
		{"override empty is unhonorable", HarnessCodex, txt(""), txt("run_override"), txt("low"), "", "", true},
		{"no override follows the user level", HarnessClaude, pgtype.Text{}, pgtype.Text{}, txt("low"), "low", "user_default", false},
		{"no override NULL user is the builtin", HarnessClaude, pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, "medium", "builtin", false},
		{"no override runtime user ships nothing", HarnessCodex, pgtype.Text{}, pgtype.Text{}, txt("runtime"), "", "user_default", false},
		{"effort without the override source is ignored", HarnessClaude, txt("max"), pgtype.Text{}, txt("low"), "low", "user_default", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eff, src, bad := leadEffortFor(c.harness, c.runEffort, c.srcE, c.user)
			if bad != c.unhonorable {
				t.Fatalf("unhonorable = %v, want %v", bad, c.unhonorable)
			}
			if bad {
				return
			}
			if (eff == nil) != (c.want == "") || (eff != nil && *eff != c.want) || string(src) != c.wantSource {
				t.Fatalf("leadEffortFor = (%v, %q), want (%q, %q)", eff, src, c.want, c.wantSource)
			}
		})
	}
}

func TestResolveLeadSettingsRunOverrideEffort(t *testing.T) {
	got := resolveLeadSettings(LeadSettingsInput{Harness: HarnessClaude, RunEffort: txt("xhigh"), RunEffortSource: txt("run_override"), UserEffort: txt("low")})
	if got.Unhonorable != nil || got.Effort == nil || *got.Effort != "xhigh" || got.EffortSource != "run_override" {
		t.Fatalf("settings = %+v", got)
	}
	got = resolveLeadSettings(LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus"), RunEffort: txt("ultra"), RunEffortSource: txt("run_override")})
	if got.Unhonorable == nil || got.Unhonorable.Setting != "effort" || got.Unhonorable.Value != "ultra" {
		t.Fatalf("out-of-set effort: %+v", got.Unhonorable)
	}
	// Model is checked first when both are unhonorable.
	got = resolveLeadSettings(LeadSettingsInput{Harness: HarnessClaude, RunModel: txt("gpt-6-sol"), RunModelSource: txt("run_override"), RunEffort: txt("ultra"), RunEffortSource: txt("run_override")})
	if got.Unhonorable == nil || got.Unhonorable.Setting != "model" {
		t.Fatalf("both unhonorable: %+v", got.Unhonorable)
	}
}

func TestRunOverrideUnavailableErrorIsSentinel(t *testing.T) {
	var err error = &runOverrideUnavailableError{Setting: "model", Value: "x", Harness: HarnessClaude}
	if !errors.Is(err, errRunOverrideUnavailable) || errors.Is(err, errCredentialDisabled) {
		t.Fatal("run_override_unavailable must match only its own sentinel")
	}
	raw, perr := runOverrideUnavailablePayload(err.(*runOverrideUnavailableError))
	if perr != nil || !strings.Contains(string(raw), `"setting":"model"`) || !strings.Contains(string(raw), `"harness":"claude"`) ||
		!strings.Contains(string(raw), `"note"`) || !strings.Contains(string(raw), `"value":"x"`) {
		t.Fatalf("payload = %s (%v)", raw, perr)
	}
}
