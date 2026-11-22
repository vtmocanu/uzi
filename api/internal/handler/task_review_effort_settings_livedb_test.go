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

// PRD #1570 M2: the task-review effort columns through PUT /api/me/model-settings and the read
// surface. Skipped unless UZI_TEST_DATABASE_URL is set; ./e2e/run-store-it.sh provides one.

func (f *effFix) taskReviewEffortCells() []apitypes.EffortSettingCellDTO {
	f.t.Helper()
	var cells []apitypes.EffortSettingCellDTO
	if err := json.Unmarshal(f.wireSettings()["task_review_efforts"], &cells); err != nil {
		f.t.Fatalf("decode task_review_efforts: %v", err)
	}
	return cells
}

func TestPutMyModelSettingsTaskReviewEffortTriStateLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()

	if rec := f.putModel(`{"task_review_effort":"  high  ","codex_task_review_effort":"runtime"}`); rec.Code != http.StatusOK {
		t.Fatalf("set both = %d %s", rec.Code, rec.Body.String())
	}
	row := f.modelRow()
	if row["task_review_effort"] != "high" || row["codex_task_review_effort"] != "runtime" {
		t.Fatalf("stored = %v, want trimmed high and runtime (runtime is stored as runtime)", row)
	}
	raw := f.wireSettings()
	if string(raw["task_review_effort"]) != `"high"` || string(raw["codex_task_review_effort"]) != `"runtime"` {
		t.Fatalf("raw fields = %s / %s", raw["task_review_effort"], raw["codex_task_review_effort"])
	}
	// Omitted leaves both alone.
	if rec := f.putModel(`{"default_effort":"low"}`); rec.Code != http.StatusOK {
		t.Fatalf("unrelated write = %d %s", rec.Code, rec.Body.String())
	}
	if row = f.modelRow(); row["task_review_effort"] != "high" || row["codex_task_review_effort"] != "runtime" {
		t.Fatalf("an omitted field changed: %v", row)
	}
	// null clears one lane; blank clears the other.
	if rec := f.putModel(`{"task_review_effort":null}`); rec.Code != http.StatusOK {
		t.Fatalf("clear claude = %d %s", rec.Code, rec.Body.String())
	}
	if row = f.modelRow(); row["task_review_effort"] != "<null>" || row["codex_task_review_effort"] != "runtime" {
		t.Fatalf("after clearing claude: %v", row)
	}
	if rec := f.putModel(`{"codex_task_review_effort":"   "}`); rec.Code != http.StatusOK {
		t.Fatalf("blank codex = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.modelRow()["codex_task_review_effort"]; got != "<null>" {
		t.Fatalf("blank must clear, got %q", got)
	}
}

func TestPutMyModelSettingsTaskReviewEffortRefusalsWriteNothingLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`UPDATE users SET task_review_effort = 'low', codex_task_review_effort = 'max', default_effort = 'low' WHERE id = $1`, f.user)
	before := f.modelRow()

	for _, field := range []string{"task_review_effort", "codex_task_review_effort"} {
		for _, bad := range []string{`"HIGH"`, `"hi gh"`, `"bogus"`, `"hi\u0000gh"`, `"hi\u0007gh"`, `7`} {
			body := `{"default_effort":"max","summary_model":"haiku","` + field + `":` + bad + `}`
			rec := f.putModel(body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s = %d %s, want 400", body, rec.Code, rec.Body.String())
				continue
			}
			if got := modelErrBody(t, rec)["field"]; got != field {
				t.Errorf("%s field = %q, want %q", body, got, field)
			}
			if after := f.modelRow(); !reflect.DeepEqual(before, after) {
				t.Fatalf("%s wrote something:\n before %v\n after  %v", body, before, after)
			}
		}
	}
}

// A write failing at the review-effort statement rolls back every earlier write of the request.
func TestPutMyModelSettingsTaskReviewEffortRollsBackOnLateFailureLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`UPDATE users SET default_effort = 'low' WHERE id = $1`, f.user)
	fn := "uzi_t1570e_fail_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	f.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '%s'::uuid AND NEW.codex_task_review_effort IS DISTINCT FROM OLD.codex_task_review_effort THEN
				RAISE EXCEPTION 'forced codex_task_review_effort failure';
			END IF;
			RETURN NEW;
		END $$`, fn, f.user))
	f.exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn))
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON users`, fn))
		_, _ = f.pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
	before := f.modelRow()

	rec := f.putModel(`{"default_effort":"max","task_review_effort":"high","codex_task_review_effort":"low"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("forced failure = %d %s, want 500", rec.Code, rec.Body.String())
	}
	if after := f.modelRow(); !reflect.DeepEqual(before, after) {
		t.Fatalf("a failed save left a partial write:\n before %v\n after  %v", before, after)
	}
	if rec := f.putModel(`{"default_effort":"max","task_review_effort":"high"}`); rec.Code != http.StatusOK {
		t.Fatalf("without the failing field = %d %s", rec.Code, rec.Body.String())
	}
	if row := f.modelRow(); row["default_effort"] != "max" || row["task_review_effort"] != "high" {
		t.Fatalf("row = %v", row)
	}
}

// PUT /api/me/settings accepts the same fields with the same refusals.
func TestPutMySettingsTaskReviewEffortsLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	if rec := f.putSettings(`{"task_review_effort":"max","codex_task_review_effort":"runtime"}`); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	row := f.modelRow()
	if row["task_review_effort"] != "max" || row["codex_task_review_effort"] != "runtime" {
		t.Fatalf("stored = %v", row)
	}
	for _, bad := range []string{`"HIGH"`, `"hi gh"`, `"bogus"`} {
		if rec := f.putSettings(`{"codex_task_review_effort":` + bad + `}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", bad, rec.Code)
		}
	}
	if got := f.modelRow()["codex_task_review_effort"]; got != "runtime" {
		t.Fatalf("a refused PUT changed the column: %q", got)
	}
}

func TestSettingsTaskReviewEffortCellsLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()

	// Unset review effort and unset worker efforts: inherits the built-in medium.
	cells := f.taskReviewEffortCells()
	if len(cells) != 2 || cells[0].Harness != "claude" || cells[1].Harness != "codex" {
		t.Fatalf("cells = %+v, want claude then codex", cells)
	}
	for _, c := range cells {
		if c.Effort != nil || c.DefaultEffort != "medium" || c.DefaultSource != "builtin" || c.ResolvedEffort == nil || *c.ResolvedEffort != "medium" ||
			c.Source != "builtin" || c.Mode != "explicit" {
			t.Errorf("unset %s cell = %+v", c.Harness, c)
		}
	}

	// Worker effort set: an unset review effort inherits it, with the worker's source.
	f.exec(`UPDATE users SET default_effort = 'high', default_codex_effort = 'runtime' WHERE id = $1`, f.user)
	cells = f.taskReviewEffortCells()
	if c := cells[0]; c.Effort != nil || c.DefaultEffort != "high" || c.DefaultSource != "user_default" || c.ResolvedEffort == nil || *c.ResolvedEffort != "high" || c.Source != "user_default" {
		t.Errorf("inheriting claude cell = %+v", c)
	}
	if c := cells[1]; c.Effort != nil || c.DefaultEffort != "runtime" || c.ResolvedEffort != nil || c.Mode != "runtime_default" || c.Source != "user_default" {
		t.Errorf("inheriting codex cell (Runtime default worker) = %+v", c)
	}

	// Explicit values override; runtime sends no level even with a worker level.
	if rec := f.putModel(`{"task_review_effort":"runtime","codex_task_review_effort":"max"}`); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	cells = f.taskReviewEffortCells()
	if c := cells[0]; c.Effort == nil || *c.Effort != "runtime" || c.ResolvedEffort != nil || c.Mode != "runtime_default" || c.DefaultEffort != "high" {
		t.Errorf("explicit runtime claude cell = %+v", c)
	}
	if c := cells[1]; c.Effort == nil || *c.Effort != "max" || c.ResolvedEffort == nil || *c.ResolvedEffort != "max" || c.Source != "user_default" || c.DefaultEffort != "runtime" {
		t.Errorf("explicit max codex cell = %+v", c)
	}

	// A corrupt stored value shows as saved but resolves to the built-in, as the claim does.
	f.exec(`UPDATE users SET task_review_effort = 'HI gh' WHERE id = $1`, f.user)
	if c := f.taskReviewEffortCells()[0]; c.Effort == nil || *c.Effort != "HI gh" || c.ResolvedEffort == nil || *c.ResolvedEffort != "medium" || c.Source != "builtin" {
		t.Errorf("corrupt claude cell = %+v", c)
	}
}
