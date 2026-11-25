package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Unit tests of PUT /api/me/model-settings phase 1 (issue #3263 module A): error aggregation,
// pin cell identity and the first-error contract of decodeCrossCheckPins. No database.

func putModelNoStore(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	(&Handler{}).PutMyModelSettings(rec, userReq(http.MethodPut, "/api/me/model-settings", body, uuid.New(), nil))
	return rec
}

// Every invalid allowlisted field is reported, in PutMySettings' decode order, whatever order the
// body lists them in; the first one is the flat error and field. Nothing reaches the store (the
// Handler has no pool or queries).
func TestPutMyModelSettingsReportsEveryFieldErrorInDecodeOrder(t *testing.T) {
	// A JSON number is invalid for every field's decoder, so each message is "invalid <field>".
	// The list below is the expected order, written out: PutMySettings decodes the model, effort,
	// judge, summary and task-review fields first, then default_harness, judge_runs_on, the
	// planning fields, and the pins last.
	want := []string{
		"default_claude_model", "default_codex_model", "default_effort", "default_codex_effort",
		"judge_model", "codex_judge_model", "summary_model", "codex_summary_model",
		"task_review_model", "codex_task_review_model", "task_review_effort", "codex_task_review_effort",
		"default_harness", "judge_runs_on",
		"planning_claude_model", "planning_claude_effort", "planning_codex_model", "planning_codex_effort",
	}
	// List the keys in reverse so request order cannot be what produces the expected order.
	parts := make([]string, 0, len(want)+1)
	parts = append(parts, `"cross_check_pins":[{"stage":"plan","harness":"claude","effort":"bogus"}]`)
	for i := len(want) - 1; i >= 0; i-- {
		parts = append(parts, `"`+want[i]+`":7`)
	}
	rec := putModelNoStore(t, "{"+strings.Join(parts, ",")+"}")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d %s, want 400", rec.Code, rec.Body.String())
	}
	errs := modelErrList(t, rec)
	if len(errs) != len(want)+1 {
		t.Fatalf("got %d errors, want %d: %+v", len(errs), len(want)+1, errs)
	}
	for i, field := range want {
		if errs[i].Field != field || errs[i].Message != "invalid "+field {
			t.Fatalf("errors[%d] = %+v, want field %s message %q", i, errs[i], field, "invalid "+field)
		}
		if errs[i].Stage != "" || errs[i].Harness != "" || errs[i].Member != "" {
			t.Fatalf("errors[%d] = %+v: a scalar field must carry no cell identity", i, errs[i])
		}
	}
	last := errs[len(want)]
	if last.Field != "cross_check_pins" || last.Stage != "plan" || last.Harness != "claude" || last.Member != "effort" {
		t.Fatalf("pin error = %+v, want cross_check_pins plan/claude effort last", last)
	}
	flat := modelErrBody(t, rec)
	if flat["field"] != "default_claude_model" || flat["error"] != "invalid default_claude_model" {
		t.Fatalf("flat error/field = %v, want the first of the list", flat)
	}
}

// Unknown keys are refused before anything is decoded: all of them are listed, sorted, the first
// as the flat error, and a bad value on an allowlisted key is not looked at.
func TestPutMyModelSettingsListsEveryUnknownKeySorted(t *testing.T) {
	rec := putModelNoStore(t, `{"zzz":1,"default_effort":"bogus","theme":"mission","aaa":2}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d %s, want 400", rec.Code, rec.Body.String())
	}
	got := modelErrList(t, rec)
	want := []modelSettingsError{
		{Field: "aaa", Message: "unknown field aaa"},
		{Field: "theme", Message: "unknown field theme"},
		{Field: "zzz", Message: "unknown field zzz"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("errors = %+v, want %+v", got, want)
	}
	if flat := modelErrBody(t, rec); flat["field"] != "aaa" || flat["error"] != "unknown field aaa" {
		t.Fatalf("flat = %v, want the first unknown key", flat)
	}
	// default_model stays refused: only the legacy mirror may write it.
	rec = putModelNoStore(t, `{"default_model":"opus"}`)
	if got := modelErrBody(t, rec)["field"]; rec.Code != http.StatusBadRequest || got != "default_model" {
		t.Fatalf("default_model = %d field %q, want 400 default_model", rec.Code, got)
	}
}

// Each pin defect names its cell and member; list-shape defects name none.
func TestPutMyModelSettingsPinErrorsCarryCellIdentity(t *testing.T) {
	body := `{"cross_check_pins":[
		{"stage":"plan","harness":"claude","model":"gpt-6-sol","effort":"bogus"},
		{"stage":"x","harness":"codex"},
		{"stage":"code","harness":"zzz"},
		{"stage":"plan","harness":"claude","effort":"low"}]}`
	rec := putModelNoStore(t, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d %s, want 400", rec.Code, rec.Body.String())
	}
	errs := modelErrList(t, rec)
	if len(errs) != 5 {
		t.Fatalf("got %d errors, want 5: %+v", len(errs), errs)
	}
	type id struct{ stage, harness, member string }
	wantID := []id{
		{"plan", "claude", "model"}, {"plan", "claude", "effort"},
		{"x", "codex", ""}, {"code", "zzz", ""}, {"plan", "claude", ""},
	}
	for i, w := range wantID {
		e := errs[i]
		if e.Field != "cross_check_pins" || e.Stage != w.stage || e.Harness != w.harness || e.Member != w.member {
			t.Fatalf("errors[%d] = %+v, want cross_check_pins %v", i, e, w)
		}
	}
	for i, prefix := range []string{
		"cross_check_pins plan/claude model: ", "cross_check_pins plan/claude effort: ",
		"cross_check_pins x/codex stage: unknown stage", "cross_check_pins code/zzz harness: unknown harness",
		"cross_check_pins plan/claude: duplicate cell",
	} {
		if !strings.HasPrefix(errs[i].Message, prefix) {
			t.Fatalf("errors[%d].Message = %q, want prefix %q", i, errs[i].Message, prefix)
		}
	}

	// A wrongly typed member is a member error too.
	rec = putModelNoStore(t, `{"cross_check_pins":[{"stage":"code","harness":"codex","model":5}]}`)
	errs = modelErrList(t, rec)
	if len(errs) != 1 || errs[0].Stage != "code" || errs[0].Harness != "codex" || errs[0].Member != "model" ||
		errs[0].Message != "cross_check_pins code/codex model: expected string or null" {
		t.Fatalf("typed member error = %+v", errs)
	}

	// List-shape defects have no cell to name: the JSON carries no stage, harness or member keys.
	for body, msg := range map[string]string{
		`{"cross_check_pins":null}`: "cross_check_pins: expected at most four keyed cells",
		`{"cross_check_pins":[{"stage":"plan","harness":"claude"},{"stage":"plan","harness":"codex"},{"stage":"code","harness":"claude"},{"stage":"code","harness":"codex"},{"stage":"code","harness":"codex"}]}`: "cross_check_pins: expected at most four keyed cells",
		`{"cross_check_pins":"x"}`: "cross_check_pins: expected at most four keyed cells",
	} {
		rec := putModelNoStore(t, body)
		var raw struct {
			Errors []map[string]any `json:"errors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusBadRequest || len(raw.Errors) != 1 {
			t.Fatalf("%s = %d %s", body, rec.Code, rec.Body.String())
		}
		if !reflect.DeepEqual(raw.Errors[0], map[string]any{"field": "cross_check_pins", "message": msg}) {
			t.Fatalf("%s error = %v, want only field and message %q", body, raw.Errors[0], msg)
		}
	}
}

// decodeCrossCheckPins (PutMySettings) keeps returning the FIRST defect
// with the text it has always had (for stage/harness values of at most 64 bytes; longer ones are now clipped), even though the collector now sees every defect.
func TestDecodeCrossCheckPinsFirstErrorTextUnchanged(t *testing.T) {
	user := uuid.New()
	for _, tc := range []struct{ raw, want string }{
		{`null`, "cross_check_pins: expected at most four keyed cells"},
		{`{"a":1}`, "cross_check_pins: expected at most four keyed cells"},
		{`[{"stage":"plan","harness":"claude","bogus":1}]`, "cross_check_pins: expected at most four keyed cells"},
		{`[{"stage":"x","harness":"claude"},{"stage":"plan","harness":"zzz"}]`, "cross_check_pins x/claude stage: unknown stage"},
		{`[{"stage":"plan","harness":"zzz"},{"stage":"x","harness":"claude"}]`, "cross_check_pins plan/zzz harness: unknown harness"},
		{`[{"stage":"plan","harness":"claude"},{"stage":"plan","harness":"claude"}]`, "cross_check_pins plan/claude: duplicate cell"},
		{`[{"stage":"plan","harness":"codex","effort":5,"model":"gpt-6-sol"}]`, "cross_check_pins plan/codex effort: expected string or null"},
		{`[{"stage":"plan","harness":"codex","effort":5,"model":"sonnet"}]`, "cross_check_pins plan/codex model: model is a Claude model alias"},
		{`[{"stage":"plan","harness":"claude","model":"gpt-6-sol","effort":"bogus"}]`, "cross_check_pins plan/claude model: model is a Codex model id"},
	} {
		patches, err := decodeCrossCheckPins(json.RawMessage(tc.raw), user)
		if err == nil || err.Error() != tc.want || patches != nil {
			t.Errorf("%s: got (%v, %v), want error %q and no patches", tc.raw, patches, err, tc.want)
		}
	}
	if err := func() error { _, e := decodeCrossCheckPins(json.RawMessage(`[] 1`), user); return e }(); err == nil ||
		err.Error() != "cross_check_pins: invalid list" {
		t.Errorf("trailing value: %v, want cross_check_pins: invalid list", err)
	}
	// Absent and valid input still decode as before.
	if patches, err := decodeCrossCheckPins(nil, user); patches != nil || err != nil {
		t.Errorf("absent = (%v, %v), want (nil, nil)", patches, err)
	}
	patches, err := decodeCrossCheckPins(json.RawMessage(`[{"stage":"code","harness":"codex","effort":"high"}]`), user)
	if err != nil || len(patches) != 1 || patches[0].Stage != "code" || patches[0].Harness != "codex" ||
		!patches[0].SetEffort || patches[0].SetModel || patches[0].Effort.String != "high" {
		t.Errorf("valid = (%+v, %v)", patches, err)
	}
}
