package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Issue #3263 M2.5: every Models-tab helper field and every pin cell, saved together, rolled back
// together, attributed per field when invalid, and independent per cell. LiveDB only (skipped
// without UZI_TEST_DATABASE_URL; ./e2e/run-store-it.sh provides one). Stored state is asserted from
// direct SQL reads, never from the response of the write.

// seedHelpers stores a distinct value in every helper column on top of seedModelsTab.
func (f *effFix) seedHelpers() (map[string]string, map[string]string) {
	f.t.Helper()
	f.seedModelsTab()
	f.exec(`UPDATE users SET codex_judge_model = 'gpt-6-sol', judge_runs_on = 'claude', summary_model = 'haiku',
		codex_summary_model = 'gpt-6-luna', task_review_model = 'sonnet', codex_task_review_model = 'gpt-6-sol',
		task_review_effort = 'low', codex_task_review_effort = 'medium' WHERE id = $1`, f.user)
	return f.modelRow(), f.pinRows()
}

// helperSaveBody changes every helper field, a worker field, a planning field, default_harness and
// all four pin cells (model and effort independently).
const helperSaveBody = `{"judge_runs_on":"codex","judge_model":"opus","codex_judge_model":"gpt-6.1-sol",
	"summary_model":"sonnet","codex_summary_model":"gpt-5.6-sol",
	"task_review_model":"opus","codex_task_review_model":"gpt-6-luna",
	"task_review_effort":"high","codex_task_review_effort":"xhigh",
	"default_claude_model":"opus","planning_claude_effort":"max","default_harness":"codex",
	"cross_check_pins":[
		{"stage":"plan","harness":"claude","model":"haiku","effort":"high"},
		{"stage":"plan","harness":"codex","model":"gpt-6.1-sol","effort":"low"},
		{"stage":"code","harness":"claude","model":"sonnet","effort":"max"},
		{"stage":"code","harness":"codex","model":"gpt-5.6-sol","effort":"xhigh"}]}`

var helperSaveWantRow = map[string]string{
	"judge_runs_on": "codex", "judge_model": "opus", "codex_judge_model": "gpt-6.1-sol",
	"summary_model": "sonnet", "codex_summary_model": "gpt-5.6-sol",
	"task_review_model": "opus", "codex_task_review_model": "gpt-6-luna",
	"task_review_effort": "high", "codex_task_review_effort": "xhigh",
	"default_claude_model": "opus", "planning_claude_effort": "max", "default_harness": "codex",
}

func TestPutMyModelSettingsHelpersWorkerPlanningAndAllPinsCommitTogetherLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	rowBefore, _ := f.seedHelpers()

	rec := f.putModel(helperSaveBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("helper save = %d %s", rec.Code, rec.Body.String())
	}
	row := f.modelRow()
	for k, want := range helperSaveWantRow {
		if row[k] != want {
			t.Errorf("%s = %q, want %q", k, row[k], want)
		}
	}
	// Columns the request did not name keep their seeded value.
	for _, k := range []string{"default_codex_model", "default_effort", "default_codex_effort", "planning_claude_model",
		"planning_codex_model", "planning_codex_effort", "judge_enabled", "autopilot_enabled"} {
		if row[k] != rowBefore[k] {
			t.Errorf("untouched %s changed: %q, want %q", k, row[k], rowBefore[k])
		}
	}
	wantPins := map[string]string{
		"plan/claude": "haiku|high", "plan/codex": "gpt-6.1-sol|low",
		"code/claude": "sonnet|max", "code/codex": "gpt-5.6-sol|xhigh",
	}
	if got := f.pinRows(); !reflect.DeepEqual(got, wantPins) {
		t.Fatalf("pins = %v, want %v", got, wantPins)
	}
}

// Failing the last (fourth) pin write rolls back every helper column, the worker, planning and
// harness columns, and the three earlier pin writes.
func TestPutMyModelSettingsLastPinFailureRollsBackEveryHelperLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	rowBefore, pinsBefore := f.seedHelpers()

	stop := f.failNthPinWrite(4)
	rec := f.putModel(helperSaveBody)
	stop()
	if rec.Code != http.StatusInternalServerError || modelErrBody(t, rec)["error"] != "internal error" {
		t.Fatalf("forced failure = %d %s, want a plain 500", rec.Code, rec.Body.String())
	}
	after := f.modelRow()
	for k := range helperSaveWantRow {
		if after[k] != rowBefore[k] {
			t.Errorf("helper column %s = %q after a failed save, want %q", k, after[k], rowBefore[k])
		}
	}
	if !reflect.DeepEqual(rowBefore, after) {
		t.Fatalf("a failed save left the user row changed:\n before %v\n after  %v", rowBefore, after)
	}
	if got := f.pinRows(); !reflect.DeepEqual(pinsBefore, got) {
		t.Fatalf("a failed save left pins changed: %v", got)
	}
	// Control: the same request without the seam commits.
	if rec := f.putModel(helperSaveBody); rec.Code != http.StatusOK {
		t.Fatalf("control = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.modelRow()["codex_task_review_effort"]; got != "xhigh" {
		t.Fatalf("control codex_task_review_effort = %q", got)
	}
}

// Patching only plan/claude effort leaves plan/claude model and the other three cells untouched,
// and the reverse (model only) leaves that cell's effort.
func TestPutMyModelSettingsPinCellsAreIndependentLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	_, pinsBefore := f.seedHelpers()

	if rec := f.putModel(`{"cross_check_pins":[{"stage":"plan","harness":"claude","effort":"high"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("effort-only patch = %d %s", rec.Code, rec.Body.String())
	}
	want := map[string]string{}
	for k, v := range pinsBefore {
		want[k] = v
	}
	want["plan/claude"] = "sonnet|high"
	if got := f.pinRows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pins after plan/claude effort patch = %v, want %v", got, want)
	}

	if rec := f.putModel(`{"cross_check_pins":[{"stage":"code","harness":"codex","model":"gpt-6-sol"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("model-only patch = %d %s", rec.Code, rec.Body.String())
	}
	want["code/codex"] = "gpt-6-sol|max"
	if got := f.pinRows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pins after code/codex model patch = %v, want %v", got, want)
	}
}

// The browser path through the real router for helper and pin cells: save then GET shows them, a
// rejected request names each offending field (and stage/harness/member for pins) and changes nothing.
func TestModelsTabHelpersAndPinsEndToEndThroughRouterLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user) })
	jwt := cliMintJWT(t, pool, user)

	getSettings := func() map[string]json.RawMessage {
		t.Helper()
		rec := cookieReq(t, router, http.MethodGet, "/api/me/settings", jwt, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/me/settings = %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Settings map[string]json.RawMessage `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Settings
	}

	rec := cookieReq(t, router, http.MethodPut, "/api/me/model-settings", jwt, helperSaveBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	after := getSettings()
	for k, want := range helperSaveWantRow {
		if got := string(after[k]); got != `"`+want+`"` {
			t.Errorf("GET %s = %s, want %q", k, got, want)
		}
	}
	var cells []apitypes.CrossCheckPinDTO
	if err := json.Unmarshal(after["cross_check_pins"], &cells); err != nil {
		t.Fatal(err)
	}
	pinned := map[string]string{}
	for _, c := range cells {
		if c.Model != nil && c.Effort != nil {
			pinned[c.Stage+"/"+c.Harness] = *c.Model + "|" + *c.Effort
		}
	}
	wantPins := map[string]string{
		"plan/claude": "haiku|high", "plan/codex": "gpt-6.1-sol|low",
		"code/claude": "sonnet|max", "code/codex": "gpt-5.6-sol|xhigh",
	}
	if !reflect.DeepEqual(pinned, wantPins) {
		t.Fatalf("pins after save = %v, want %v", pinned, wantPins)
	}

	// A Codex custom worker and planning ID is accepted (only a Claude alias is refused there).
	rec = cookieReq(t, router, http.MethodPut, "/api/me/model-settings", jwt,
		`{"default_codex_model":"gpt-custom-9","planning_codex_model":"gpt-custom-9"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("codex custom ids = %d %s, want 200", rec.Code, rec.Body.String())
	}
	after = getSettings()
	if string(after["default_codex_model"]) != `"gpt-custom-9"` || string(after["planning_codex_model"]) != `"gpt-custom-9"` {
		t.Fatalf("custom ids not stored: %s %s", after["default_codex_model"], after["planning_codex_model"])
	}

	rejections := []struct {
		name, body                    string
		field, stage, harness, member string
	}{
		{"codex judge outside curated set", `{"codex_judge_model":"gpt-6-luna"}`, "codex_judge_model", "", "", ""},
		{"codex summary outside role models", `{"codex_summary_model":"gpt-custom-9"}`, "codex_summary_model", "", "", ""},
		{"codex task review outside role models", `{"codex_task_review_model":"opus"}`, "codex_task_review_model", "", "", ""},
		{"claude judge refuses a codex id", `{"judge_model":"gpt-6-sol"}`, "judge_model", "", "", ""},
		{"helper effort invalid", `{"task_review_effort":"bogus"}`, "task_review_effort", "", "", ""},
		{"codex helper effort invalid", `{"codex_task_review_effort":"bogus"}`, "codex_task_review_effort", "", "", ""},
		{"judge_runs_on invalid", `{"judge_runs_on":"other"}`, "judge_runs_on", "", "", ""},
		{"claude pin refuses a codex model", `{"cross_check_pins":[{"stage":"code","harness":"claude","model":"gpt-6-sol"}]}`,
			"cross_check_pins", "code", "claude", "model"},
		{"codex pin refuses a claude alias", `{"cross_check_pins":[{"stage":"plan","harness":"codex","model":"opus"}]}`,
			"cross_check_pins", "plan", "codex", "model"},
		{"pin effort invalid", `{"cross_check_pins":[{"stage":"plan","harness":"claude","effort":"bogus"}]}`,
			"cross_check_pins", "plan", "claude", "effort"},
	}
	for _, tc := range rejections {
		rec := cookieReq(t, router, http.MethodPut, "/api/me/model-settings", jwt, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: = %d %s, want 400", tc.name, rec.Code, rec.Body.String())
			continue
		}
		errs := modelErrList(t, rec)
		if len(errs) != 1 || errs[0].Field != tc.field || errs[0].Stage != tc.stage || errs[0].Harness != tc.harness || errs[0].Member != tc.member {
			t.Errorf("%s: errors = %+v, want one error on %s %s/%s/%s", tc.name, errs, tc.field, tc.stage, tc.harness, tc.member)
		}
	}
	// One request mixing a valid helper change with an invalid one writes neither.
	rec = cookieReq(t, router, http.MethodPut, "/api/me/model-settings", jwt,
		`{"summary_model":"haiku","codex_judge_model":"gpt-6-luna"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mixed valid+invalid = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if again := getSettings(); !reflect.DeepEqual(after, again) {
		t.Fatalf("rejected saves changed what GET returns:\n before %s\n after  %s", rawJSON(after), rawJSON(again))
	}
}
