package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// PRD #1570 M1: the task-review model columns through PUT /api/me/model-settings and the read
// surface. Skipped unless UZI_TEST_DATABASE_URL is set; ./e2e/run-store-it.sh provides one.

func (f *effFix) taskReviewCells() []apitypes.ModelSettingCellDTO {
	f.t.Helper()
	var cells []apitypes.ModelSettingCellDTO
	if err := json.Unmarshal(f.wireSettings()["task_review_models"], &cells); err != nil {
		f.t.Fatalf("decode task_review_models: %v", err)
	}
	return cells
}

func TestPutMyModelSettingsTaskReviewTriStateLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()

	if rec := f.putModel(`{"task_review_model":"  opus  ","codex_task_review_model":"gpt-6.1-sol"}`); rec.Code != http.StatusOK {
		t.Fatalf("set both = %d %s", rec.Code, rec.Body.String())
	}
	row := f.modelRow()
	if row["task_review_model"] != "opus" || row["codex_task_review_model"] != "gpt-6.1-sol" {
		t.Fatalf("stored = %v, want trimmed opus and gpt-6.1-sol", row)
	}
	raw := f.wireSettings()
	if string(raw["task_review_model"]) != `"opus"` || string(raw["codex_task_review_model"]) != `"gpt-6.1-sol"` {
		t.Fatalf("raw fields = %s / %s", raw["task_review_model"], raw["codex_task_review_model"])
	}

	// Omitted leaves both columns alone.
	if rec := f.putModel(`{"default_effort":"high"}`); rec.Code != http.StatusOK {
		t.Fatalf("unrelated write = %d %s", rec.Code, rec.Body.String())
	}
	row = f.modelRow()
	if row["task_review_model"] != "opus" || row["codex_task_review_model"] != "gpt-6.1-sol" {
		t.Fatalf("an omitted field changed: %v", row)
	}

	// null clears one lane and leaves the other.
	if rec := f.putModel(`{"task_review_model":null}`); rec.Code != http.StatusOK {
		t.Fatalf("clear claude = %d %s", rec.Code, rec.Body.String())
	}
	row = f.modelRow()
	if row["task_review_model"] != "<null>" || row["codex_task_review_model"] != "gpt-6.1-sol" {
		t.Fatalf("after clearing claude: %v", row)
	}
	// Blank clears too.
	if rec := f.putModel(`{"codex_task_review_model":"   "}`); rec.Code != http.StatusOK {
		t.Fatalf("blank codex = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.modelRow()["codex_task_review_model"]; got != "<null>" {
		t.Fatalf("blank must clear, got %q", got)
	}
}

func TestPutMyModelSettingsTaskReviewRefusalsWriteNothingLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`UPDATE users SET task_review_model = 'opus', codex_task_review_model = 'gpt-6-sol', default_effort = 'low' WHERE id = $1`, f.user)
	before := f.modelRow()

	for _, tc := range []struct{ body, field string }{
		{`{"task_review_model":"gpt-6-astra"}`, "task_review_model"},
		{`{"task_review_model":"bad\u0000id"}`, "task_review_model"},
		{`{"task_review_model":"two words"}`, "task_review_model"},
		{`{"codex_task_review_model":"custom-codex-1"}`, "codex_task_review_model"},
		{`{"codex_task_review_model":"opus"}`, "codex_task_review_model"},
		{`{"codex_task_review_model":"bad\u0007id"}`, "codex_task_review_model"},
		// A valid key next to an invalid one writes nothing: the valid one must not land.
		{`{"default_effort":"max","task_review_model":"sonnet","codex_task_review_model":"nope"}`, "codex_task_review_model"},
		{`{"codex_task_review_model":"gpt-6.1-sol","task_review_model":"gpt-6-astra"}`, "task_review_model"},
	} {
		rec := f.putModel(tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", tc.body, rec.Code, rec.Body.String())
			continue
		}
		if got := modelErrBody(t, rec)["field"]; got != tc.field {
			t.Errorf("%s field = %q, want %q", tc.body, got, tc.field)
		}
		if after := f.modelRow(); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s wrote something:\n before %v\n after  %v", tc.body, before, after)
		}
	}
	// The Claude refusal is the existing Claude-lane message, not a new one.
	rec := f.putModel(`{"task_review_model":"gpt-6-astra"}`)
	if got := modelErrBody(t, rec)["error"]; !strings.Contains(got, "task_review_model") || !strings.Contains(got, "Codex model id") {
		t.Fatalf("error = %q, want the Claude-lane Codex-id refusal naming the field", got)
	}
}

// A write failing at the task-review statement rolls back every earlier write of the request, and
// a failure at the Claude review statement rolls back the Codex one that follows it.
func TestPutMyModelSettingsTaskReviewRollsBackOnLateFailureLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`UPDATE users SET default_effort = 'low' WHERE id = $1`, f.user)
	fn := "uzi_t1570_fail_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	f.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '%s'::uuid AND NEW.codex_task_review_model IS DISTINCT FROM OLD.codex_task_review_model THEN
				RAISE EXCEPTION 'forced codex_task_review_model failure';
			END IF;
			RETURN NEW;
		END $$`, fn, f.user))
	f.exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn))
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON users`, fn))
		_, _ = f.pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
	before := f.modelRow()

	rec := f.putModel(`{"default_effort":"max","summary_model":"haiku","task_review_model":"opus","codex_task_review_model":"gpt-6.1-sol"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("forced failure = %d %s, want 500", rec.Code, rec.Body.String())
	}
	if after := f.modelRow(); !reflect.DeepEqual(before, after) {
		t.Fatalf("a failed save left a partial write:\n before %v\n after  %v", before, after)
	}

	// Without the failing field the same request lands, proving the trigger is the cause.
	if rec := f.putModel(`{"default_effort":"max","summary_model":"haiku","task_review_model":"opus"}`); rec.Code != http.StatusOK {
		t.Fatalf("without the failing field = %d %s", rec.Code, rec.Body.String())
	}
	if row := f.modelRow(); row["default_effort"] != "max" || row["task_review_model"] != "opus" {
		t.Fatalf("row = %v", row)
	}
}

// PUT /api/me/settings accepts the same fields with the same refusals.
func TestPutMySettingsTaskReviewModelsLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	if rec := f.putSettings(`{"task_review_model":"opus","codex_task_review_model":"gpt-6-luna"}`); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	row := f.modelRow()
	if row["task_review_model"] != "opus" || row["codex_task_review_model"] != "gpt-6-luna" {
		t.Fatalf("stored = %v", row)
	}
	if rec := f.putSettings(`{"task_review_model":"gpt-6-astra"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("Codex id in the Claude field = %d, want 400", rec.Code)
	}
	if got := f.modelRow()["task_review_model"]; got != "opus" {
		t.Fatalf("a refused PUT changed the column: %q", got)
	}
}

func TestSettingsTaskReviewModelCellsLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()

	cells := f.taskReviewCells()
	if len(cells) != 2 || cells[0].Harness != "claude" || cells[1].Harness != "codex" {
		t.Fatalf("cells = %+v, want claude then codex", cells)
	}
	if c := cells[0]; c.Model != nil || c.DefaultModel != "" || c.ResolvedModel != "" || c.Source != "builtin" || c.DefaultSource != "builtin" {
		t.Fatalf("unset claude cell = %+v", c)
	}
	if c := cells[1]; c.Model != nil || c.DefaultModel != "gpt-6-sol" || c.ResolvedModel != "gpt-6-sol" || c.Source != "builtin" {
		t.Fatalf("unset codex cell = %+v", c)
	}

	if rec := f.putModel(`{"task_review_model":"opus","codex_task_review_model":"gpt-6.1-sol"}`); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	cells = f.taskReviewCells()
	if c := cells[0]; c.Model == nil || *c.Model != "opus" || c.ResolvedModel != "opus" || c.Source != "user_default" || c.DefaultModel != "" {
		t.Fatalf("set claude cell = %+v", c)
	}
	if c := cells[1]; c.Model == nil || *c.Model != "gpt-6.1-sol" || c.ResolvedModel != "gpt-6.1-sol" || c.Source != "user_default" || c.DefaultModel != "gpt-6-sol" {
		t.Fatalf("set codex cell = %+v", c)
	}

	// A stored Codex model that left the curated set is shown as saved but resolves to the built-in,
	// the same rule the claim applies.
	f.exec(`UPDATE users SET codex_task_review_model = 'gpt-4-retired' WHERE id = $1`, f.user)
	if c := f.taskReviewCells()[1]; c.Model == nil || *c.Model != "gpt-4-retired" || c.ResolvedModel != "gpt-6-sol" || c.Source != "builtin" {
		t.Fatalf("retired codex cell = %+v", c)
	}
}
