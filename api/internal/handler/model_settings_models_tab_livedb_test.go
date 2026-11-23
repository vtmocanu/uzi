package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Issue #3263 module A: PUT /api/me/model-settings carries default_harness and the four planning
// fields, reports every defect of a rejected request, and returns the effective settings with the
// saved ones. LiveDB only (skipped without UZI_TEST_DATABASE_URL; ./e2e/run-store-it.sh provides
// one). Stored state is asserted from direct SQL reads, never from the response of the write.

// seedModelsTab stores a distinct value in every column the Models tab writes and in every pin
// cell, so a request that fails or is rejected has something to leave unchanged.
func (f *effFix) seedModelsTab() (map[string]string, map[string]string) {
	f.t.Helper()
	f.clearModelColumns()
	f.exec(`UPDATE users SET default_claude_model = 'sonnet', default_codex_model = 'gpt-6-sol', default_model = 'sonnet',
		default_harness = 'claude', default_effort = 'low', default_codex_effort = 'medium', judge_model = 'haiku',
		judge_runs_on = 'claude', summary_model = 'haiku', task_review_model = 'sonnet', task_review_effort = 'low',
		planning_claude_model = 'sonnet', planning_claude_effort = 'low',
		planning_codex_model = 'gpt-6-sol', planning_codex_effort = 'medium' WHERE id = $1`, f.user)
	pins := f.seedAllPins()
	return f.modelRow(), pins
}

// failNthPinExecTx fails the nth statement that writes user_cross_check_pins (counting from 1)
// and otherwise delegates, so a LiveDB test can fail the last pin write of a request.
type failNthPinExecTx struct {
	pgx.Tx
	n     int
	count *int
}

func (t failNthPinExecTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "user_cross_check_pins") {
		*t.count++
		if *t.count == t.n {
			return pgconn.CommandTag{}, errors.New("forced pin write failure")
		}
	}
	return t.Tx.Exec(ctx, sql, args...)
}

func (f *effFix) failNthPinWrite(n int) (stop func()) {
	count := 0
	f.h.modelSettingsBeginTx = func(ctx context.Context) (pgx.Tx, error) {
		tx, err := f.pool.Begin(ctx)
		return failNthPinExecTx{Tx: tx, n: n, count: &count}, err
	}
	return func() { f.h.modelSettingsBeginTx = nil }
}

// One request mixing scalar fields, default_harness, the planning fields, a task-review helper
// and two pin cells commits all of them together.
func TestPutMyModelSettingsMixedSaveCommitsTogetherLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	_, pinsBefore := f.seedModelsTab()

	rec := f.putModel(`{"default_effort":"high","default_claude_model":"opus","default_harness":"codex",
		"task_review_model":"opus","judge_runs_on":"codex",
		"planning_claude_model":"opus","planning_claude_effort":"xhigh",
		"planning_codex_model":"gpt-6-luna","planning_codex_effort":"high",
		"cross_check_pins":[{"stage":"plan","harness":"claude","model":"haiku"},{"stage":"code","harness":"codex","effort":"low"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("mixed save = %d %s", rec.Code, rec.Body.String())
	}
	row := f.modelRow()
	for k, want := range map[string]string{
		"default_effort": "high", "default_claude_model": "opus", "default_harness": "codex",
		"task_review_model": "opus", "judge_runs_on": "codex",
		"planning_claude_model": "opus", "planning_claude_effort": "xhigh",
		"planning_codex_model": "gpt-6-luna", "planning_codex_effort": "high",
		// Untouched columns keep their seeded value.
		"default_codex_model": "gpt-6-sol", "default_codex_effort": "medium", "judge_model": "haiku",
		// The fixture user has no Codex credential, so a codex preference still resolves to
		// Claude and the legacy mirror follows the Claude lane written in the same request.
		"default_model": "opus",
	} {
		if row[k] != want {
			t.Errorf("%s = %q, want %q", k, row[k], want)
		}
	}
	pins := f.pinRows()
	wantPins := map[string]string{}
	for k, v := range pinsBefore {
		wantPins[k] = v
	}
	wantPins["plan/claude"] = "haiku|low"
	wantPins["code/codex"] = "gpt-6-astra|low"
	if !reflect.DeepEqual(pins, wantPins) {
		t.Fatalf("pins = %v, want %v", pins, wantPins)
	}
}

// An explicit null (and a blank string for the planning fields) resets each new key to NULL.
func TestPutMyModelSettingsExplicitNullResetsNewKeysLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.seedModelsTab()
	f.exec(`UPDATE users SET default_harness = 'codex' WHERE id = $1`, f.user)

	if rec := f.putModel(`{"default_harness":null,"planning_claude_model":null,"planning_claude_effort":"",
		"planning_codex_model":null,"planning_codex_effort":null}`); rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
	}
	row := f.modelRow()
	for _, k := range []string{"default_harness", "planning_claude_model", "planning_claude_effort", "planning_codex_model", "planning_codex_effort"} {
		if row[k] != "<null>" {
			t.Errorf("after reset %s = %q, want NULL", k, row[k])
		}
	}
	// The reset touched nothing else.
	for k, want := range map[string]string{"default_claude_model": "sonnet", "default_codex_model": "gpt-6-sol", "default_effort": "low", "judge_model": "haiku"} {
		if row[k] != want {
			t.Errorf("reset changed %s: %q, want %q", k, row[k], want)
		}
	}
}

// A request with many defects answers with every one of them, names the offending pin cell and
// member, and writes nothing.
func TestPutMyModelSettingsMultiInvalidReportsEverythingWritesNothingLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	rowBefore, pinsBefore := f.seedModelsTab()

	rec := f.putModel(`{"default_effort":"high","default_claude_model":"opus","default_harness":"gemini",
		"planning_claude_effort":"runtime","planning_codex_model":"sonnet","judge_runs_on":"other",
		"cross_check_pins":[{"stage":"plan","harness":"claude","model":"haiku"},
			{"stage":"code","harness":"codex","effort":"bogus"},{"stage":"code","harness":"codex"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("multi-invalid = %d %s, want 400", rec.Code, rec.Body.String())
	}
	var got []string
	for _, e := range modelErrList(t, rec) {
		got = append(got, e.Field)
	}
	want := []string{"default_harness", "judge_runs_on", "planning_claude_effort", "planning_codex_model", "cross_check_pins", "cross_check_pins"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("error fields = %v, want %v", got, want)
	}
	errs := modelErrList(t, rec)
	if e := errs[4]; e.Stage != "code" || e.Harness != "codex" || e.Member != "effort" {
		t.Fatalf("pin error = %+v, want code/codex effort", e)
	}
	if e := errs[5]; e.Stage != "code" || e.Harness != "codex" || e.Member != "" || !strings.Contains(e.Message, "duplicate cell") {
		t.Fatalf("duplicate error = %+v, want code/codex duplicate cell", e)
	}
	if flat := modelErrBody(t, rec); flat["field"] != "default_harness" {
		t.Fatalf("flat field = %q, want the first error default_harness", flat["field"])
	}
	if after := f.modelRow(); !reflect.DeepEqual(rowBefore, after) {
		t.Fatalf("a rejected request wrote the user row:\n before %v\n after  %v", rowBefore, after)
	}
	if after := f.pinRows(); !reflect.DeepEqual(pinsBefore, after) {
		t.Fatalf("a rejected request wrote pins: %v", after)
	}
}

// Failing the LAST pin write rolls back default_harness, the legacy mirror, the planning
// columns, the helper columns and the earlier pin write of the same request.
func TestPutMyModelSettingsLastPinFailureRollsBackEverythingLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	rowBefore, pinsBefore := f.seedModelsTab()
	body := `{"default_harness":"codex","default_claude_model":"opus","default_effort":"max",
		"task_review_model":"opus","summary_model":"opus",
		"planning_claude_model":"opus","planning_claude_effort":"max","planning_codex_model":"gpt-6-luna","planning_codex_effort":"max",
		"cross_check_pins":[{"stage":"plan","harness":"claude","model":"haiku"},{"stage":"code","harness":"codex","effort":"low"}]}`

	stop := f.failNthPinWrite(2)
	rec := f.putModel(body)
	stop()
	if rec.Code != http.StatusInternalServerError || modelErrBody(t, rec)["error"] != "internal error" {
		t.Fatalf("forced failure = %d %s, want a plain 500", rec.Code, rec.Body.String())
	}
	if after := f.modelRow(); !reflect.DeepEqual(rowBefore, after) {
		t.Fatalf("a failed save left the user row changed:\n before %v\n after  %v", rowBefore, after)
	}
	if after := f.pinRows(); !reflect.DeepEqual(pinsBefore, after) {
		t.Fatalf("a failed save left pins changed (the first pin write must roll back): %v", after)
	}

	// Without the forced failure the same request commits, so the rollback above was the seam's doing.
	if rec := f.putModel(body); rec.Code != http.StatusOK {
		t.Fatalf("control = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.modelRow(); got["default_harness"] != "codex" || got["planning_codex_effort"] != "max" || got["default_model"] != "opus" {
		t.Fatalf("control row = %v", got)
	}
}

// default_harness is written by the route, recomputes the legacy default_model mirror through the
// same D11 projection PutMySettings uses, and leaves both model lanes alone.
func TestPutMyModelSettingsDefaultHarnessWritesLiveDB(t *testing.T) {
	seed := func(f *effFix, harness string) {
		f.clearModelColumns()
		f.exec(`UPDATE users SET default_claude_model = 'opus', default_codex_model = 'gpt-6-sol', default_model = 'stale',
			default_harness = NULLIF($2, '') WHERE id = $1`, f.user, harness)
	}

	t.Run("harness only recomputes the mirror and preserves both lanes", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.codexDefault()
		seed(f, "")
		if rec := f.putModel(`{"default_harness":"codex"}`); rec.Code != http.StatusOK {
			t.Fatalf("harness only = %d %s", rec.Code, rec.Body.String())
		}
		row := f.modelRow()
		if row["default_harness"] != "codex" || row["default_model"] != "gpt-6-sol" ||
			row["default_claude_model"] != "opus" || row["default_codex_model"] != "gpt-6-sol" {
			t.Fatalf("row = %v, want harness codex, mirror = codex lane gpt-6-sol, both lanes kept", row)
		}
		// Switching back moves the mirror back to the Claude lane. A Claude preference needs a
		// usable Claude credential to be honoured (with only Codex usable, D11 falls back to Codex).
		f.anthropic()
		if rec := f.putModel(`{"default_harness":"claude"}`); rec.Code != http.StatusOK {
			t.Fatalf("harness back = %d %s", rec.Code, rec.Body.String())
		}
		row = f.modelRow()
		if row["default_harness"] != "claude" || row["default_model"] != "opus" ||
			row["default_claude_model"] != "opus" || row["default_codex_model"] != "gpt-6-sol" {
			t.Fatalf("row = %v, want harness claude, mirror = claude lane opus", row)
		}
	})

	t.Run("harness with a lane", func(t *testing.T) {
		f := newEffFix(t, nil)
		f.codexDefault()
		seed(f, "claude")
		if rec := f.putModel(`{"default_harness":"codex","default_claude_model":"sonnet"}`); rec.Code != http.StatusOK {
			t.Fatalf("harness + lane = %d %s", rec.Code, rec.Body.String())
		}
		row := f.modelRow()
		if row["default_harness"] != "codex" || row["default_claude_model"] != "sonnet" ||
			row["default_codex_model"] != "gpt-6-sol" || row["default_model"] != "gpt-6-sol" {
			t.Fatalf("row = %v, want harness codex, claude lane sonnet, codex lane kept, mirror gpt-6-sol", row)
		}
	})

	t.Run("null harness clears it and the mirror follows the fallback", func(t *testing.T) {
		f := newEffFix(t, nil)
		seed(f, "codex")
		if rec := f.putModel(`{"default_harness":null}`); rec.Code != http.StatusOK {
			t.Fatalf("null harness = %d %s", rec.Code, rec.Body.String())
		}
		row := f.modelRow()
		// No credentials at all: no preference resolves to Claude.
		if row["default_harness"] != "<null>" || row["default_model"] != "opus" ||
			row["default_claude_model"] != "opus" || row["default_codex_model"] != "gpt-6-sol" {
			t.Fatalf("row = %v, want harness NULL, mirror = claude lane opus, lanes kept", row)
		}
	})

	t.Run("unusable codex preference stores the preference but mirrors Claude", func(t *testing.T) {
		f := newEffFix(t, nil) // no Codex credential
		seed(f, "")
		if rec := f.putModel(`{"default_harness":"codex"}`); rec.Code != http.StatusOK {
			t.Fatalf("unusable codex = %d %s", rec.Code, rec.Body.String())
		}
		row := f.modelRow()
		if row["default_harness"] != "codex" || row["default_model"] != "opus" ||
			row["default_claude_model"] != "opus" || row["default_codex_model"] != "gpt-6-sol" {
			t.Fatalf("row = %v, want harness codex stored, mirror = claude lane opus", row)
		}
	})
}

// The effective-settings prediction failing after the commit leaves the save a success: 200, the
// settings, effective null and a reason, and the write durable.
func TestPutMyModelSettingsEffectiveReadbackFailureStillSavedLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()

	check := func(t *testing.T, body []byte) {
		t.Helper()
		var resp map[string]json.RawMessage
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if string(resp["effective"]) != "null" {
			t.Fatalf("effective = %s, want JSON null", resp["effective"])
		}
		if string(resp["effective_error"]) != `"settings_effective_readback_failed"` {
			t.Fatalf("effective_error = %s", resp["effective_error"])
		}
		if len(resp["settings"]) == 0 || string(resp["settings"]) == "null" {
			t.Fatalf("settings missing from %s", body)
		}
	}

	f.h.modelSettingsEffectiveReadback = func(context.Context, uuid.UUID) (apitypes.EffectiveSettingsDTO, error) {
		return apitypes.EffectiveSettingsDTO{}, errors.New("prediction boom")
	}
	rec := f.putModel(`{"default_effort":"high","planning_claude_model":"opus"}`)
	f.h.modelSettingsEffectiveReadback = nil
	if rec.Code != http.StatusOK {
		t.Fatalf("effective failure = %d %s, want 200", rec.Code, rec.Body.String())
	}
	check(t, rec.Body.Bytes())
	if row := f.modelRow(); row["default_effort"] != "high" || row["planning_claude_model"] != "opus" {
		t.Fatalf("the save is not durable: %v", row)
	}
	if got := f.wireSettings()["planning_claude_model"]; string(got) != `"opus"` {
		t.Fatalf("GET settings planning_claude_model = %s, want opus", got)
	}

	// No worker service: the prediction cannot be built, which is the same soft failure.
	wsvc := f.h.wsvc
	f.h.wsvc = nil
	rec = f.putModel(`{"default_effort":"low"}`)
	f.h.wsvc = wsvc
	if rec.Code != http.StatusOK {
		t.Fatalf("no wsvc = %d %s, want 200", rec.Code, rec.Body.String())
	}
	check(t, rec.Body.Bytes())
	if row := f.modelRow(); row["default_effort"] != "low" {
		t.Fatalf("no-wsvc save not durable: %v", row)
	}
}

// A successful save answers with the effective settings of the saved state: the same body
// GET /api/me/effective-settings returns right afterwards.
func TestPutMyModelSettingsReturnsEffectiveSettingsLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.clearModelColumns()
	rec := f.putModel(`{"judge_model":"opus","default_harness":"claude","planning_claude_model":"haiku"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Settings  json.RawMessage `json:"settings"`
		Effective json.RawMessage `json:"effective"`
		EffErr    *string         `json:"effective_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.EffErr != nil || len(resp.Effective) == 0 || string(resp.Effective) == "null" {
		t.Fatalf("effective = %s, effective_error = %v, want a body and no error", resp.Effective, resp.EffErr)
	}
	var put apitypes.EffectiveSettingsDTO
	strictDecode(t, resp.Effective, &put)
	got, _ := f.get()
	if !reflect.DeepEqual(put, got) {
		t.Fatalf("PUT effective differs from GET /api/me/effective-settings:\n put %+v\n get %+v", put, got)
	}
	if cell := modelCell(t, put.Models, "judge_model", "claude"); cell.ResolvedModel != "opus" || cell.Source != "user_default" {
		t.Fatalf("effective judge cell = %+v, want the value just saved (user_default opus)", cell)
	}
}

// The browser path end to end, through the real router with a session cookie and the CSRF header:
// read, save many fields, read again, then a rejected request and a failed request each leave the
// next read identical to the last.
func TestModelsTabSaveEndToEndThroughRouterLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
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
	before := getSettings()

	rec := cookieReq(t, router, http.MethodPut, "/api/me/model-settings", jwt, `{"default_claude_model":"opus","default_effort":"high",
		"default_harness":"codex","judge_model":"haiku","planning_claude_model":"sonnet","planning_claude_effort":"low",
		"planning_codex_model":"gpt-6-sol","planning_codex_effort":"xhigh","task_review_effort":"max",
		"cross_check_pins":[{"stage":"plan","harness":"claude","model":"haiku"},{"stage":"code","harness":"codex","effort":"high"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	var saved struct {
		Effective json.RawMessage `json:"effective"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil || len(saved.Effective) == 0 || string(saved.Effective) == "null" {
		t.Fatalf("save response has no effective body: %s (%v)", rec.Body.String(), err)
	}

	after := getSettings()
	for k, want := range map[string]string{
		"default_claude_model": `"opus"`, "default_effort": `"high"`, "default_harness": `"codex"`, "judge_model": `"haiku"`,
		"planning_claude_model": `"sonnet"`, "planning_claude_effort": `"low"`,
		"planning_codex_model": `"gpt-6-sol"`, "planning_codex_effort": `"xhigh"`, "task_review_effort": `"max"`,
	} {
		if string(after[k]) != want {
			t.Errorf("GET %s = %s, want %s (was %s)", k, after[k], want, before[k])
		}
	}
	var cells []apitypes.CrossCheckPinDTO
	if err := json.Unmarshal(after["cross_check_pins"], &cells); err != nil {
		t.Fatal(err)
	}
	pinned := map[string]string{}
	for _, c := range cells {
		if c.Model != nil {
			pinned[c.Stage+"/"+c.Harness+" model"] = *c.Model
		}
		if c.Effort != nil {
			pinned[c.Stage+"/"+c.Harness+" effort"] = *c.Effort
		}
	}
	if !reflect.DeepEqual(pinned, map[string]string{"plan/claude model": "haiku", "code/codex effort": "high"}) {
		t.Fatalf("pins after save = %v", pinned)
	}

	// A request with a bad cell is a 400 and the next read is identical.
	rec = cookieReq(t, router, http.MethodPut, "/api/me/model-settings", jwt, `{"default_effort":"low","planning_claude_model":"opus",
		"cross_check_pins":[{"stage":"plan","harness":"codex","model":"sonnet"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid cell = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if errs := modelErrList(t, rec); len(errs) != 1 || errs[0].Stage != "plan" || errs[0].Harness != "codex" || errs[0].Member != "model" {
		t.Fatalf("errors = %+v, want one plan/codex model error", errs)
	}
	if again := getSettings(); !reflect.DeepEqual(after, again) {
		t.Fatalf("a rejected save changed what GET returns:\n before %s\n after  %s", rawJSON(after), rawJSON(again))
	}

	// A failure while writing the last pin is a 500 and the next read is identical.
	count := 0
	h.modelSettingsBeginTx = func(ctx context.Context) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx)
		return failNthPinExecTx{Tx: tx, n: 2, count: &count}, err
	}
	rec = cookieReq(t, router, http.MethodPut, "/api/me/model-settings", jwt, `{"default_effort":"low","planning_claude_model":"opus","default_harness":"claude",
		"cross_check_pins":[{"stage":"plan","harness":"claude","model":"opus"},{"stage":"code","harness":"codex","effort":"low"}]}`)
	h.modelSettingsBeginTx = nil
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("forced failure = %d %s, want 500", rec.Code, rec.Body.String())
	}
	if count != 2 {
		t.Fatalf("pin writes before the forced failure = %d, want 2 (the seam must reach the last pin write)", count)
	}
	if again := getSettings(); !reflect.DeepEqual(after, again) {
		t.Fatalf("a failed save changed what GET returns:\n before %s\n after  %s", rawJSON(after), rawJSON(again))
	}
}

func rawJSON(m map[string]json.RawMessage) string {
	b, _ := json.Marshal(m)
	return string(b)
}
