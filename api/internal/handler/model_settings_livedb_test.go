package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3264 M1: PUT /api/me/model-settings. The writes are exercised against a live database
// (the single-transaction guarantee, the row lock, and the shared validators' parity with
// PutMySettings are properties of real statements). Skipped unless UZI_TEST_DATABASE_URL is set;
// ./e2e/run-store-it.sh provides one.
//
// Write invariant the route's row lock relies on: every column it SETs (default_claude_model,
// default_codex_model, the legacy default_model mirror, default_effort, default_codex_effort,
// judge_model, codex_judge_model, judge_runs_on, summary_model, codex_summary_model, task_review_model,
// codex_task_review_model, task_review_effort, codex_task_review_effort) is a plain
// non-key column. users.id is never in a SET list, and the only unique or foreign-key-referenced
// columns of users are id, email, (oidc_issuer, oidc_subject) and the slack resolved id
// (migrations 00001, 00056, 00044), none of which is written here. That is why FOR NO KEY UPDATE,
// the mode a plain UPDATE of such columns takes anyway, is the right lock.

// modelSettingsColumns are the users columns the tests snapshot: every column the route may
// write, plus default_harness (never writable here) and the consent switches it must not touch.
var modelSettingsColumns = []string{
	"default_claude_model", "default_codex_model", "default_model", "default_harness",
	"default_effort", "default_codex_effort", "judge_model", "codex_judge_model", "judge_runs_on",
	"summary_model", "codex_summary_model", "task_review_model", "codex_task_review_model",
	"task_review_effort", "codex_task_review_effort",
	"judge_enabled", "autopilot_enabled", "plan_cross_check_enabled", "code_cross_check_enabled",
}

func (f *effFix) putModel(body string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.h.PutMyModelSettings(rec, userReq(http.MethodPut, "/api/me/model-settings", body, f.user, nil))
	return rec
}

// modelRow snapshots modelSettingsColumns as text ("<null>" for SQL NULL).
func (f *effFix) modelRow() map[string]string {
	f.t.Helper()
	exprs := make([]string, len(modelSettingsColumns))
	for i, c := range modelSettingsColumns {
		exprs[i] = fmt.Sprintf("COALESCE(%s::text, '<null>')", c)
	}
	dest := make([]string, len(modelSettingsColumns))
	ptrs := make([]any, len(dest))
	for i := range dest {
		ptrs[i] = &dest[i]
	}
	if err := f.pool.QueryRow(f.ctx, "SELECT "+strings.Join(exprs, ", ")+" FROM users WHERE id = $1", f.user).Scan(ptrs...); err != nil {
		f.t.Fatalf("snapshot users row: %v", err)
	}
	out := make(map[string]string, len(dest))
	for i, c := range modelSettingsColumns {
		out[c] = dest[i]
	}
	return out
}

func (f *effFix) clearModelColumns() {
	f.t.Helper()
	f.exec(`UPDATE users SET default_claude_model = NULL, default_codex_model = NULL, default_model = NULL,
		default_harness = NULL, default_effort = NULL, default_codex_effort = NULL, judge_model = NULL,
		codex_judge_model = NULL, judge_runs_on = NULL, summary_model = NULL, codex_summary_model = NULL,
		task_review_model = NULL, codex_task_review_model = NULL,
		task_review_effort = NULL, codex_task_review_effort = NULL
		WHERE id = $1`, f.user)
}

func modelErrBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return m
}

func (f *effFix) wireSettings() map[string]json.RawMessage {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.h.GetMySettings(rec, userReq(http.MethodGet, "/api/me/settings", "", f.user, nil))
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET settings = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		f.t.Fatalf("decode settings: %v", err)
	}
	return body.Settings
}

// Without a user the route is a 401, and an unknown key is refused before any store access, so
// a Handler with no pool and no queries is enough.
func TestPutMyModelSettingsAuthAndUnknownKeyNeedNoStore(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.PutMyModelSettings(rec, httptest.NewRequest(http.MethodPut, "/api/me/model-settings", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no user = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.PutMyModelSettings(rec, userReq(http.MethodPut, "/api/me/model-settings", `{"theme":"mission","default_model":"x"}`, uuid.New(), nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown key = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	// Unknown keys are reported in sorted order: default_model sorts before theme.
	if got := modelErrBody(t, rec); got["field"] != "default_model" || got["error"] == "" {
		t.Fatalf("body = %v, want field default_model with a message", got)
	}

	for _, body := range []string{`[]`, `null`, `"x"`, `{"default_effort":"low"} {}`, `{`} {
		rec = httptest.NewRecorder()
		h.PutMyModelSettings(rec, userReq(http.MethodPut, "/api/me/model-settings", body, uuid.New(), nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, rec.Code)
		}
	}
}

// Every allowlisted field goes through both PUT handlers: same acceptance, same stored row, and
// the same error message. The new route adds the field name; only message text is compared.
func TestPutMyModelSettingsParityWithPutMySettingsLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	cases := []struct {
		field string
		raw   string
		ok    bool
	}{
		{"default_claude_model", `"sonnet"`, true},
		{"default_claude_model", `"  opus  "`, true},
		{"default_claude_model", `"claude-custom-9"`, true},
		{"default_claude_model", `null`, true},
		{"default_claude_model", `""`, true},
		{"default_claude_model", `"gpt-6-sol"`, false},
		{"default_claude_model", `"has space"`, false},
		{"default_claude_model", `7`, false},
		{"default_codex_model", `"gpt-6-sol"`, true},
		{"default_codex_model", `"custom-codex-1"`, true},
		{"default_codex_model", `null`, true},
		{"default_codex_model", `"sonnet"`, false},
		{"default_codex_model", `["x"]`, false},
		{"default_effort", `"low"`, true},
		{"default_effort", `"medium"`, true},
		{"default_effort", `"high"`, true},
		{"default_effort", `"xhigh"`, true},
		{"default_effort", `"max"`, true},
		{"default_effort", `"runtime"`, true},
		{"default_effort", `null`, true},
		{"default_effort", `"bogus"`, false},
		{"default_effort", `5`, false},
		{"default_codex_effort", `"high"`, true},
		{"default_codex_effort", `"runtime"`, true},
		{"default_codex_effort", `null`, true},
		{"default_codex_effort", `"bogus"`, false},
		{"judge_model", `"haiku"`, true},
		{"judge_model", `null`, true},
		{"judge_model", `"gpt-6-sol"`, false},
		{"judge_model", `{}`, false},
		{"codex_judge_model", `"gpt-6-sol"`, true},
		{"codex_judge_model", `null`, true},
		{"codex_judge_model", `"not-curated"`, false},
		{"judge_runs_on", `"claude"`, true},
		{"judge_runs_on", `"codex"`, true},
		{"judge_runs_on", `null`, true},
		{"judge_runs_on", `"other"`, false},
		{"judge_runs_on", `7`, false},
		{"summary_model", `"sonnet"`, true},
		{"summary_model", `null`, true},
		{"summary_model", `"gpt-6-sol"`, false},
		{"codex_summary_model", `"gpt-6-luna"`, true},
		{"codex_summary_model", `null`, true},
		{"codex_summary_model", `"nope"`, false},
		{"task_review_model", `"opus"`, true},
		{"task_review_model", `"  claude-custom-9  "`, true},
		{"task_review_model", `null`, true},
		{"task_review_model", `""`, true},
		{"task_review_model", `"gpt-6-astra"`, false},
		{"task_review_model", `"has space"`, false},
		{"task_review_model", `"bad\u0000id"`, false},
		{"task_review_model", `7`, false},
		{"codex_task_review_model", `"gpt-6.1-sol"`, true},
		{"codex_task_review_model", `"gpt-6-luna"`, true},
		{"codex_task_review_model", `null`, true},
		{"codex_task_review_model", `"custom-codex-1"`, false},
		{"codex_task_review_model", `"opus"`, false},
		{"codex_task_review_model", `["x"]`, false},
		{"task_review_effort", `"low"`, true},
		{"task_review_effort", `"max"`, true},
		{"task_review_effort", `"runtime"`, true},
		{"task_review_effort", `"  high  "`, true},
		{"task_review_effort", `null`, true},
		{"task_review_effort", `""`, true},
		{"task_review_effort", `"HIGH"`, false},
		{"task_review_effort", `"hi gh"`, false},
		{"task_review_effort", `"bogus"`, false},
		{"task_review_effort", `"hi\u0000gh"`, false},
		{"task_review_effort", `5`, false},
		{"codex_task_review_effort", `"xhigh"`, true},
		{"codex_task_review_effort", `"runtime"`, true},
		{"codex_task_review_effort", `null`, true},
		{"codex_task_review_effort", `"HIGH"`, false},
		{"codex_task_review_effort", `"hi gh"`, false},
		{"codex_task_review_effort", `"bogus"`, false},
		{"codex_task_review_effort", `"hi\u0007gh"`, false},
		{"codex_task_review_effort", `["high"]`, false},
		{"cross_check_pins", `[{"stage":"plan","harness":"claude","effort":"high"}]`, true},
		{"cross_check_pins", `[]`, true},
		{"cross_check_pins", `null`, false},
		{"cross_check_pins", `[{"stage":"plan","harness":"claude","effort":"runtime"}]`, false},
		{"cross_check_pins", `[{"stage":"x","harness":"claude"}]`, false},
	}
	covered := map[string]bool{}
	outer := f.t
	defer func() { f.t = outer }()
	for _, c := range cases {
		covered[c.field] = true
		body := fmt.Sprintf(`{%q:%s}`, c.field, c.raw)
		t.Run(c.field+"="+c.raw, func(t *testing.T) {
			f.t = t
			f.clearModelColumns()
			f.exec(`UPDATE users SET default_effort = 'low', summary_model = 'haiku' WHERE id = $1`, f.user)
			old := f.putSettings(body)
			oldRow := f.modelRow()

			f.clearModelColumns()
			f.exec(`UPDATE users SET default_effort = 'low', summary_model = 'haiku' WHERE id = $1`, f.user)
			cur := f.putModel(body)
			curRow := f.modelRow()

			wantStatus := http.StatusOK
			if !c.ok {
				wantStatus = http.StatusBadRequest
			}
			if old.Code != wantStatus || cur.Code != wantStatus {
				t.Fatalf("PUT /me/settings = %d, PUT /me/model-settings = %d, want both %d\n%s\n%s",
					old.Code, cur.Code, wantStatus, old.Body.String(), cur.Body.String())
			}
			if !reflect.DeepEqual(oldRow, curRow) {
				t.Fatalf("stored rows differ:\n PutMySettings:  %v\n PutMyModelSettings: %v", oldRow, curRow)
			}
			if c.ok {
				return
			}
			oldBody, curBody := modelErrBody(t, old), modelErrBody(t, cur)
			if oldBody["error"] != curBody["error"] {
				t.Fatalf("error text differs: %q vs %q", oldBody["error"], curBody["error"])
			}
			if curBody["field"] != c.field {
				t.Fatalf("field = %q, want %q", curBody["field"], c.field)
			}
			if _, has := oldBody["field"]; has {
				t.Fatalf("PutMySettings body gained a field key: %v", oldBody)
			}
		})
	}
	for k := range modelSettingsAllowlist {
		if !covered[k] {
			t.Errorf("allowlisted field %s has no parity case", k)
		}
	}
}

// With two invalid allowlisted fields in one body, both handlers report the same first error.
func TestPutMyModelSettingsParityFirstOfTwoErrorsLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	for _, body := range []string{
		`{"default_codex_effort":"bogus","judge_model":"gpt-6-sol"}`,
		`{"judge_model":"gpt-6-sol","default_codex_effort":"bogus"}`,
	} {
		old := f.putSettings(body)
		cur := f.putModel(body)
		if old.Code != http.StatusBadRequest || cur.Code != http.StatusBadRequest {
			t.Fatalf("%s: PUT /me/settings = %d, PUT /me/model-settings = %d, want both 400", body, old.Code, cur.Code)
		}
		if o, c := modelErrBody(t, old)["error"], modelErrBody(t, cur)["error"]; o == "" || o != c {
			t.Fatalf("%s: first error differs: %q vs %q", body, o, c)
		}
	}
}

// When the effective harness is Codex, the legacy default_model mirror follows the Codex lane,
// whichever lane the request writes.
func TestPutMyModelSettingsMirrorFollowsCodexLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.codexDefault()
	f.policy("codex", false)
	f.exec(`UPDATE users SET default_codex_model = 'gpt-6-sol' WHERE id = $1`, f.user)

	if rec := f.putModel(`{"default_claude_model":"opus"}`); rec.Code != http.StatusOK {
		t.Fatalf("set claude lane = %d %s", rec.Code, rec.Body.String())
	}
	row := f.modelRow()
	if row["default_claude_model"] != "opus" || row["default_model"] != "gpt-6-sol" {
		t.Fatalf("claude-lane write with Codex effective: %v, want default_model = stored codex lane gpt-6-sol", row)
	}

	if rec := f.putModel(`{"default_codex_model":"gpt-6-luna"}`); rec.Code != http.StatusOK {
		t.Fatalf("set codex lane = %d %s", rec.Code, rec.Body.String())
	}
	row = f.modelRow()
	if row["default_claude_model"] != "opus" || row["default_model"] != "gpt-6-luna" {
		t.Fatalf("codex-lane write: %v, want default_model gpt-6-luna", row)
	}
}

// Set, reset and the runtime/null distinction for every scalar field, both families.
func TestPutMyModelSettingsSetAndResetEveryFieldLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	body := `{"default_claude_model":"opus","default_codex_model":"gpt-6-sol","default_effort":"runtime","default_codex_effort":"xhigh",
		"judge_model":"haiku","codex_judge_model":"gpt-6-astra","judge_runs_on":"codex","summary_model":"sonnet","codex_summary_model":"gpt-6-luna",
		"task_review_model":"opus","codex_task_review_model":"gpt-6.1-sol",
		"task_review_effort":"high","codex_task_review_effort":"runtime"}`
	rec := f.putModel(body)
	if rec.Code != http.StatusOK {
		t.Fatalf("set all = %d %s", rec.Code, rec.Body.String())
	}
	want := map[string]string{
		"default_claude_model": "opus", "default_codex_model": "gpt-6-sol", "default_effort": "runtime",
		"default_codex_effort": "xhigh", "judge_model": "haiku", "codex_judge_model": "gpt-6-astra",
		"judge_runs_on": "codex", "summary_model": "sonnet", "codex_summary_model": "gpt-6-luna",
		"task_review_model": "opus", "codex_task_review_model": "gpt-6.1-sol",
		"task_review_effort": "high", "codex_task_review_effort": "runtime",
	}
	row := f.modelRow()
	for k, v := range want {
		if row[k] != v {
			t.Errorf("%s = %q, want %q", k, row[k], v)
		}
	}
	// The response is the GET /me/settings envelope: same keys as a fresh GET.
	var put struct {
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &put); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := f.wireSettings(); !reflect.DeepEqual(keysOfRaw(got), keysOfRaw(put.Settings)) {
		t.Fatalf("response keys differ from GET /me/settings:\n%v\n%v", keysOfRaw(put.Settings), keysOfRaw(got))
	}
	if string(put.Settings["default_effort"]) != `"runtime"` {
		t.Fatalf("response default_effort = %s, want \"runtime\"", put.Settings["default_effort"])
	}

	// Runtime (explicit) and null (cleared) are distinct stored states.
	if rec := f.putModel(`{"default_effort":null}`); rec.Code != http.StatusOK {
		t.Fatalf("clear effort = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.modelRow()["default_effort"]; got != "<null>" {
		t.Fatalf("cleared default_effort = %q, want NULL (not runtime)", got)
	}
	if got := f.modelRow()["default_codex_effort"]; got != "xhigh" {
		t.Fatalf("clearing default_effort touched default_codex_effort: %q", got)
	}

	// Reset everything with nulls.
	if rec := f.putModel(`{"default_claude_model":null,"default_codex_model":null,"default_effort":null,"default_codex_effort":null,
		"judge_model":null,"codex_judge_model":null,"judge_runs_on":null,"summary_model":null,"codex_summary_model":null,
		"task_review_model":null,"codex_task_review_model":null,"task_review_effort":null,"codex_task_review_effort":null}`); rec.Code != http.StatusOK {
		t.Fatalf("reset all = %d %s", rec.Code, rec.Body.String())
	}
	for k := range want {
		if got := f.modelRow()[k]; got != "<null>" {
			t.Errorf("after reset %s = %q, want NULL", k, got)
		}
	}

	// An empty object writes nothing and still answers with the envelope.
	f.exec(`UPDATE users SET default_effort = 'high' WHERE id = $1`, f.user)
	rec = f.putModel(`{}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"settings"`) {
		t.Fatalf("empty object = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.modelRow()["default_effort"]; got != "high" {
		t.Fatalf("empty object changed default_effort: %q", got)
	}
}

func keysOfRaw(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// An unknown key, or a bad value next to a good one, writes nothing.
func TestPutMyModelSettingsRejectsWithoutWritingLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`UPDATE users SET default_effort = 'low', summary_model = 'haiku', default_claude_model = 'opus' WHERE id = $1`, f.user)
	before := f.modelRow()

	for _, tc := range []struct{ body, field string }{
		{`{"default_effort":"high","zzz_unknown":1}`, "zzz_unknown"},
		{`{"default_effort":"high","default_harness":"codex"}`, "default_harness"},
		{`{"default_effort":"high","default_model":"opus"}`, "default_model"},
		{`{"default_effort":"high","cross_check_pins":null}`, "cross_check_pins"},
		{`{"default_effort":"high","theme":"mission"}`, "theme"},
		{`{"default_effort":"high","summary_model":"gpt-6-sol"}`, "summary_model"},
		{`{"default_claude_model":"sonnet","codex_summary_model":"nope"}`, "codex_summary_model"},
		{`{"judge_runs_on":"codex","default_codex_effort":"bogus"}`, "default_codex_effort"},
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

	// Unknown keys are reported in sorted order.
	rec := f.putModel(`{"zzz":1,"aaa":2}`)
	if got := modelErrBody(t, rec)["field"]; got != "aaa" {
		t.Fatalf("first unknown key = %q, want aaa", got)
	}
}

// Setting or resetting one lane preserves the other lane and default_harness, and the legacy
// default_model keeps mirroring the effective lane.
func TestPutMyModelSettingsLanesPreserveEachOtherAndHarnessLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`UPDATE users SET default_harness = 'codex', default_claude_model = 'opus' WHERE id = $1`, f.user)

	if rec := f.putModel(`{"default_codex_model":"gpt-6-sol"}`); rec.Code != http.StatusOK {
		t.Fatalf("set codex lane = %d %s", rec.Code, rec.Body.String())
	}
	row := f.modelRow()
	if row["default_claude_model"] != "opus" || row["default_codex_model"] != "gpt-6-sol" || row["default_harness"] != "codex" {
		t.Fatalf("after setting codex lane: %v", row)
	}

	if rec := f.putModel(`{"default_codex_model":null}`); rec.Code != http.StatusOK {
		t.Fatalf("reset codex lane = %d %s", rec.Code, rec.Body.String())
	}
	row = f.modelRow()
	if row["default_claude_model"] != "opus" || row["default_codex_model"] != "<null>" || row["default_harness"] != "codex" {
		t.Fatalf("after resetting codex lane: %v", row)
	}

	if rec := f.putModel(`{"default_claude_model":"sonnet"}`); rec.Code != http.StatusOK {
		t.Fatalf("set claude lane = %d %s", rec.Code, rec.Body.String())
	}
	row = f.modelRow()
	if row["default_claude_model"] != "sonnet" || row["default_codex_model"] != "<null>" || row["default_harness"] != "codex" {
		t.Fatalf("after setting claude lane: %v", row)
	}
	// With no credentials the settings harness resolves to Claude, so the legacy column mirrors it.
	if row["default_model"] != "sonnet" {
		t.Fatalf("legacy default_model = %q, want the effective (claude) lane sonnet", row["default_model"])
	}
	if rec := f.putModel(`{"default_claude_model":null}`); rec.Code != http.StatusOK {
		t.Fatalf("reset claude lane = %d %s", rec.Code, rec.Body.String())
	}
	row = f.modelRow()
	if row["default_claude_model"] != "<null>" || row["default_harness"] != "codex" || row["default_model"] != "<null>" {
		t.Fatalf("after resetting claude lane: %v", row)
	}
}

// The route cannot flip a consent switch: those keys are unknown, and the flags stay as they were.
func TestPutMyModelSettingsCannotFlipConsentLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.exec(`UPDATE users SET judge_enabled = false, autopilot_enabled = false, plan_cross_check_enabled = false,
		code_cross_check_enabled = false WHERE id = $1`, f.user)
	before := f.modelRow()
	for _, key := range []string{"judge_enabled", "enabled", "autopilot_enabled", "plan_cross_check_enabled",
		"code_cross_check_enabled", "cross_check_enabled", "ci_autofix_enabled", "wait_on_limit", "mr_rework_enabled"} {
		rec := f.putModel(fmt.Sprintf(`{"default_effort":"high",%q:true}`, key))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s, want 400 unknown field", key, rec.Code, rec.Body.String())
		}
		if got := modelErrBody(t, rec)["field"]; got != key {
			t.Fatalf("%s field = %q", key, got)
		}
	}
	after := f.modelRow()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("consent keys changed the row:\n before %v\n after  %v", before, after)
	}
	for _, flag := range []string{"judge_enabled", "autopilot_enabled", "plan_cross_check_enabled", "code_cross_check_enabled"} {
		if after[flag] != "false" {
			t.Errorf("%s = %q, want false", flag, after[flag])
		}
	}
}

// A write failing at the LAST statement rolls back every earlier write of the request.
// The trigger raises only for this fixture's user, so other tests sharing the database are not
// affected, and is dropped on cleanup.
func TestPutMyModelSettingsRollsBackOnLateFailureLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	f.exec(`UPDATE users SET default_effort = 'low' WHERE id = $1`, f.user)
	fn := "uzi_t3264_fail_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	f.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = '%s'::uuid AND NEW.codex_summary_model IS DISTINCT FROM OLD.codex_summary_model THEN
				RAISE EXCEPTION 'forced codex_summary_model failure';
			END IF;
			RETURN NEW;
		END $$`, fn, f.user))
	f.exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn))
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON users`, fn))
		_, _ = f.pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
	before := f.modelRow()

	rec := f.putModel(`{"default_effort":"max","default_claude_model":"opus","judge_model":"haiku","codex_summary_model":"gpt-6-luna"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("forced failure = %d %s, want 500", rec.Code, rec.Body.String())
	}
	if got := modelErrBody(t, rec); got["error"] != "internal error" || got["reason"] != "" {
		t.Fatalf("body = %v, want a plain internal error", got)
	}
	if after := f.modelRow(); !reflect.DeepEqual(before, after) {
		t.Fatalf("a failed save left a partial write:\n before %v\n after  %v", before, after)
	}
	if got := f.modelRow()["default_effort"]; got != "low" {
		t.Fatalf("default_effort = %q, want unchanged low", got)
	}

	// The same request without the failing field succeeds, proving the trigger is the cause.
	if rec := f.putModel(`{"default_effort":"max","default_claude_model":"opus","judge_model":"haiku"}`); rec.Code != http.StatusOK {
		t.Fatalf("without the failing field = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.modelRow()["default_effort"]; got != "max" {
		t.Fatalf("default_effort = %q, want max", got)
	}
}

type commitFailTx struct {
	pgx.Tx
	apply bool
}

func (c commitFailTx) Commit(ctx context.Context) error {
	if c.apply {
		_ = c.Tx.Commit(ctx)
	} else {
		_ = c.Rollback(ctx)
	}
	return errors.New("connection reset by peer")
}

// A commit error is 500 settings_commit_unknown whether the write landed or not, and a failed
// post-commit readback is 500 settings_saved_readback_failed with the write durable.
func TestPutMyModelSettingsCommitAndReadbackFailuresLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()

	for _, apply := range []bool{false, true} {
		f.clearModelColumns()
		f.h.modelSettingsBeginTx = func(ctx context.Context) (pgx.Tx, error) {
			tx, err := f.pool.Begin(ctx)
			return commitFailTx{Tx: tx, apply: apply}, err
		}
		rec := f.putModel(`{"default_effort":"max"}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("apply=%v commit error = %d %s, want 500", apply, rec.Code, rec.Body.String())
		}
		got := modelErrBody(t, rec)
		if got["reason"] != "settings_commit_unknown" ||
			got["error"] != "the save may or may not have been applied; check with `uzi settings get`" {
			t.Fatalf("apply=%v body = %v", apply, got)
		}
		wantStored := "<null>"
		if apply {
			wantStored = "max"
		}
		if v := f.modelRow()["default_effort"]; v != wantStored {
			t.Fatalf("apply=%v stored default_effort = %q, want %q", apply, v, wantStored)
		}
	}
	f.h.modelSettingsBeginTx = nil

	f.clearModelColumns()
	f.h.modelSettingsReadback = func(*http.Request, uuid.UUID) (userSettingsDTO, error) {
		return userSettingsDTO{}, errors.New("readback boom")
	}
	rec := f.putModel(`{"default_effort":"high"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("readback failure = %d %s, want 500", rec.Code, rec.Body.String())
	}
	got := modelErrBody(t, rec)
	if got["reason"] != "settings_saved_readback_failed" || got["error"] != "settings saved; reading them back failed" {
		t.Fatalf("body = %v", got)
	}
	if v := f.modelRow()["default_effort"]; v != "high" {
		t.Fatalf("a failed readback must leave the write durable: default_effort = %q", v)
	}
	f.h.modelSettingsReadback = nil
}

// A handler without a pool refuses to write rather than fall back to non-transactional writes.
func TestPutMyModelSettingsNoPoolIsInternalErrorLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	pool := f.h.pool
	f.h.pool = nil
	defer func() { f.h.pool = pool }()
	rec := f.putModel(`{"default_effort":"max"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("no pool = %d %s, want 500", rec.Code, rec.Body.String())
	}
	if v := f.modelRow()["default_effort"]; v != "<null>" {
		t.Fatalf("default_effort = %q, want NULL", v)
	}
}

// A reset follows upstream: the stored value goes back to NULL (never a copy of the instance
// default), and the effective view then tracks the instance default with a non-user source.
func TestPutMyModelSettingsResetFollowsInstanceDefaultLiveDB(t *testing.T) {
	inst := &settingsStore{rows: []store.AppSetting{{Key: settings.KeyJudgeModel, Value: "haiku"}}}
	f := newEffFix(t, inst)
	f.clearModelColumns()

	if rec := f.putModel(`{"judge_model":"opus"}`); rec.Code != http.StatusOK {
		t.Fatalf("set judge model = %d %s", rec.Code, rec.Body.String())
	}
	dto, _ := f.get()
	cell := modelCell(t, dto.Models, "judge_model", "claude")
	if cell.Source != "user_default" || cell.ResolvedModel != "opus" {
		t.Fatalf("after set: %+v, want user_default opus", cell)
	}

	if rec := f.putModel(`{"judge_model":null}`); rec.Code != http.StatusOK {
		t.Fatalf("reset judge model = %d %s", rec.Code, rec.Body.String())
	}
	dto, _ = f.get()
	cell = modelCell(t, dto.Models, "judge_model", "claude")
	if cell.Source != "instance_default" || cell.ResolvedModel != "haiku" {
		t.Fatalf("after reset: %+v, want instance_default haiku", cell)
	}

	// The admin changes the instance default: the reset user follows it, and nothing was copied.
	inst.rows = []store.AppSetting{{Key: settings.KeyJudgeModel, Value: "sonnet"}}
	f.h.settings.Invalidate()
	dto, _ = f.get()
	cell = modelCell(t, dto.Models, "judge_model", "claude")
	if cell.Source != "instance_default" || cell.ResolvedModel != "sonnet" {
		t.Fatalf("after the instance default changed: %+v, want instance_default sonnet", cell)
	}
	if got := f.modelRow()["judge_model"]; got != "<null>" {
		t.Fatalf("stored judge_model = %q, want NULL (reset stores no copy of the default)", got)
	}
}
