package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCrossCheckFourIndependentPinCellsLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	save := func(body string, want int) []crossCheckPinDTO {
		t.Helper()
		rec := httptest.NewRecorder()
		h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", body, user, nil))
		if rec.Code != want {
			t.Fatalf("settings: %d %s", rec.Code, rec.Body.String())
		}
		if want != 200 {
			return nil
		}
		var got struct {
			Settings struct {
				Pins []crossCheckPinDTO `json:"cross_check_pins"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.Settings.Pins
	}
	pins := save(`{"cross_check_pins":[{"stage":"plan","harness":"claude","model":"opus","effort":"high"},{"stage":"plan","harness":"codex","model":"gpt-6-astra","effort":"xhigh"},{"stage":"code","harness":"claude","model":"haiku","effort":"low"},{"stage":"code","harness":"codex","model":"gpt-6-sol","effort":"medium"}]}`, 200)
	if len(pins) != 4 {
		t.Fatalf("four cells missing: %+v", pins)
	}
	for i, want := range []struct{ stage, harness, model, effort string }{
		{"plan", "claude", "opus", "high"}, {"plan", "codex", "gpt-6-astra", "xhigh"},
		{"code", "claude", "haiku", "low"}, {"code", "codex", "gpt-6-sol", "medium"},
	} {
		if pins[i].Stage != want.stage || pins[i].Harness != want.harness || pins[i].Model == nil || *pins[i].Model != want.model || pins[i].ResolvedEffort != want.effort {
			t.Fatalf("cell %d: %+v", i, pins[i])
		}
	}
	save(`{"cross_check_pins":[{"stage":"code","harness":"claude","model":"sonnet"},{"stage":"code","harness":"claude","effort":"high"}]}`, 400)
	pins = save(`{"cross_check_pins":[{"stage":"code","harness":"claude","model":null}]}`, 200)
	if pins[2].Model != nil || pins[2].Effort == nil || *pins[2].Effort != "low" || *pins[0].Model != "opus" || *pins[3].Model != "gpt-6-sol" {
		t.Fatal("reset crossed a stage or field")
	}
	// Legacy omission and a plan-only patch continue to preserve code cells.
	pins = save(`{"cross_check_pins":[{"stage":"plan","harness":"claude","effort":"medium"}]}`, 200)
	if len(pins) != 4 || pins[0].ResolvedEffort != "medium" || pins[2].Model != nil || pins[2].ResolvedEffort != "low" || *pins[3].Model != "gpt-6-sol" {
		t.Fatal("plan-only patch lost independent code pins")
	}
	pins = save("{}", 200)
	if *pins[3].Model != "gpt-6-sol" || pins[3].ResolvedEffort != "medium" || pins[0].ResolvedEffort != "medium" {
		t.Fatal("stage-local patch/omission lost code pins")
	}
}
