package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPredictRunOverrideLead pins the start-time model and effort through resolveLeadSettings
// (PRD #3250 section 8) without a database: the run choice wins over the owner's lane and effort
// with source run_override, an incompatible model is reported and never replaced, and PredictLead
// keeps its schedule-pin behaviour for the same model.
func TestPredictRunOverrideLead(t *testing.T) {
	lanes := func(r *store.ListUserCrossCheckPinsRow) {
		r.DefaultClaudeModel = pgconv.TextOrNull("haiku")
		r.TemplateModel = pgconv.TextOrNull("opus")
		r.DefaultEffort = pgconv.TextOrNull("low")
		r.DefaultCodexEffort = pgconv.TextOrNull("runtime")
	}
	predict := func(h Harness, model, effort *string) LeadPrediction {
		t.Helper()
		f := &predictFake{poolFakeStore: &poolFakeStore{}, pins: pinRow(lanes)}
		got, err := (&Service{q: f}).PredictRunOverrideLead(context.Background(), uuid.New(), h, model, effort, false)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := predict(HarnessClaude, strp("sonnet"), strp("max"))
	if got.Unhonorable != nil || got.Model != "sonnet" || got.ModelSource != "run_override" ||
		derefOr(got.ClaimModel, "") != "sonnet" || got.ClaimModelSource != "run_override" ||
		derefOr(got.Effort, "") != "max" || got.EffortSource != "run_override" {
		t.Errorf("claude model and effort = %+v", got)
	}

	got = predict(HarnessClaude, nil, strp("high"))
	if got.Model != "haiku" || got.ModelSource != "user_default" || derefOr(got.Effort, "") != "high" || got.EffortSource != "run_override" {
		t.Errorf("claude effort only keeps the lane model: %+v", got)
	}

	got = predict(HarnessCodex, nil, strp("xhigh"))
	if derefOr(got.Effort, "") != "xhigh" || got.EffortSource != "run_override" {
		t.Errorf("an effort beats the owner's runtime-default choice: %+v", got)
	}

	for _, c := range []struct {
		h     Harness
		model string
	}{{HarnessClaude, "gpt-6-sol"}, {HarnessCodex, "opus"}, {HarnessCodex, "gpt-foo"}} {
		got := predict(c.h, strp(c.model), strp("max"))
		if got.Unhonorable == nil || got.Unhonorable.Setting != "model" || got.Unhonorable.Value != c.model || got.Unhonorable.Harness != c.h {
			t.Errorf("%s %q = %+v, want an unhonorable model", c.h, c.model, got)
		}
		if got.Model != "" || got.ClaimModel != nil || got.Effort != nil {
			t.Errorf("%s %q: a refused choice leaked a value: %+v", c.h, c.model, got)
		}
	}

	// The same model passed as a schedule's frozen model keeps its schedule_pin source.
	pin := predictLead(t, &predictFake{poolFakeStore: &poolFakeStore{}, pins: pinRow(lanes)}, HarnessClaude, strp("sonnet"), false, false)
	if pin.ModelSource != "schedule_pin" {
		t.Errorf("PredictLead frozen model source = %q, want schedule_pin", pin.ModelSource)
	}
}
