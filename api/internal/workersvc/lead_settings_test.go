package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
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

func TestLeadEffortSourceAndResolution(t *testing.T) {
	for _, h := range []Harness{HarnessClaude, HarnessCodex} {
		for _, c := range []struct {
			name   string
			effort pgtype.Text
			source leadcfg.EffortSource
		}{
			{"unset", pgtype.Text{}, leadcfg.EffortSourceBuiltin},
			{"empty", txt(""), leadcfg.EffortSourceBuiltin},
			{"whitespace only", txt("  \t"), leadcfg.EffortSourceBuiltin},
			{"chosen", txt("high"), leadcfg.EffortSourceUserDefault},
			{"chosen padded", txt(" xhigh "), leadcfg.EffortSourceUserDefault},
		} {
			t.Run(string(h)+"/"+c.name, func(t *testing.T) {
				if got := leadEffortSource(h, c.effort); got != c.source {
					t.Errorf("leadEffortSource = %q, want %q", got, c.source)
				}
				got := resolveLeadSettings(LeadSettingsInput{Harness: h, UserEffort: c.effort})
				want := resolveHarnessEffortPtr(h, c.effort)
				if got.Effort == nil || *got.Effort != *want {
					t.Errorf("Effort = %v, want %v", got.Effort, *want)
				}
				if got.EffortSource != c.source {
					t.Errorf("EffortSource = %q, want %q", got.EffortSource, c.source)
				}
			})
		}
	}
}
