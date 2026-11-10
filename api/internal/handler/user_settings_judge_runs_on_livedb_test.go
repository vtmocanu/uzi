package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// putSettings drives PutMySettings for the fixture user and returns the recorder.
func (f *effFix) putSettings(body string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", body, f.user, nil))
	return rec
}

func (f *effFix) storedJudgeRunsOn() *string {
	f.t.Helper()
	var v *string
	if err := f.pool.QueryRow(f.ctx, `SELECT judge_runs_on FROM users WHERE id = $1`, f.user).Scan(&v); err != nil {
		f.t.Fatalf("read judge_runs_on: %v", err)
	}
	return v
}

func (f *effFix) wireJudgeRunsOn() (*string, bool) {
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
		f.t.Fatalf("decode: %v", err)
	}
	raw, present := body.Settings["judge_runs_on"]
	if !present {
		return nil, false
	}
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil {
		f.t.Fatalf("decode judge_runs_on %s: %v", raw, err)
	}
	return v, true
}

// TestUserJudgeRunsOnRoundTripLiveDB covers the tri-state of judge_runs_on on PUT/GET
// /api/me/settings (PRD #3257): absent leaves it, a value sets it, null clears it, and an
// unknown value is a 400 that writes nothing. Skipped unless UZI_TEST_DATABASE_URL is set.
func TestUserJudgeRunsOnRoundTripLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	if v, present := f.wireJudgeRunsOn(); !present || v != nil {
		t.Fatalf("fresh user judge_runs_on = %v present=%v, want present null", v, present)
	}

	if rec := f.putSettings(`{"judge_runs_on":"codex"}`); rec.Code != http.StatusOK {
		t.Fatalf("set codex: %d %s", rec.Code, rec.Body.String())
	}
	if v := f.storedJudgeRunsOn(); v == nil || *v != "codex" {
		t.Fatalf("stored = %v, want codex", v)
	}
	if v, _ := f.wireJudgeRunsOn(); v == nil || *v != "codex" {
		t.Fatalf("wire = %v, want codex", v)
	}

	// Absent leaves it unchanged (another field written alone).
	if rec := f.putSettings(`{"mr_rework_enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("unrelated put: %d %s", rec.Code, rec.Body.String())
	}
	if v := f.storedJudgeRunsOn(); v == nil || *v != "codex" {
		t.Fatalf("absent changed the column: %v", v)
	}

	// An unknown value is refused before any write, even next to a valid field.
	for _, body := range []string{`{"judge_runs_on":"other"}`, `{"judge_runs_on":"","mr_rework_enabled":true}`, `{"judge_runs_on":7}`} {
		rec := f.putSettings(body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %s = %d %s, want 400", body, rec.Code, rec.Body.String())
		}
		var e map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e["error"] != "invalid judge_runs_on" {
			t.Fatalf("PUT %s body = %s, want error invalid judge_runs_on", body, rec.Body.String())
		}
	}
	if v := f.storedJudgeRunsOn(); v == nil || *v != "codex" {
		t.Fatalf("a refused PUT changed the column: %v", v)
	}

	if rec := f.putSettings(`{"judge_runs_on":"claude"}`); rec.Code != http.StatusOK {
		t.Fatalf("set claude: %d %s", rec.Code, rec.Body.String())
	}
	if v := f.storedJudgeRunsOn(); v == nil || *v != "claude" {
		t.Fatalf("stored = %v, want claude", v)
	}
	if rec := f.putSettings(`{"judge_runs_on":null}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	if v := f.storedJudgeRunsOn(); v != nil {
		t.Fatalf("stored after null = %v, want NULL", *v)
	}
	if v, present := f.wireJudgeRunsOn(); !present || v != nil {
		t.Fatalf("wire after null = %v present=%v, want present null", v, present)
	}
}

// TestEffectiveSettingsJudgeHarnessLiveDB pins the full judge_harness wire cell and the
// not_used note on the judge-model lane that will not run (PRD #3257): unset reads value null
// with source same_as_run and no note; an explicit choice reads that value with source
// user_default and marks only the other harness's judge-model cell.
func TestEffectiveSettingsJudgeHarnessLiveDB(t *testing.T) {
	f := newEffFix(t, nil)
	f.bothUsable(10, 10)

	dto, _ := f.get()
	if want := (apitypes.HarnessValueDTO{Source: "same_as_run", FixedAt: "create", Notes: []string{}}); !reflect.DeepEqual(dto.JudgeHarness, want) {
		t.Fatalf("unset judge_harness = %+v, want %+v", dto.JudgeHarness, want)
	}
	for _, hn := range []string{"claude", "codex"} {
		if c := modelCell(t, dto.Models, "judge_model", hn); len(c.Notes) != 0 {
			t.Errorf("unset: %s judge_model notes = %v, want none", hn, c.Notes)
		}
	}

	codex := "codex"
	f.set("judge_runs_on", codex)
	dto, raw := f.get()
	if want := (apitypes.HarnessValueDTO{Value: &codex, Source: "user_default", FixedAt: "create", Notes: []string{}}); !reflect.DeepEqual(dto.JudgeHarness, want) {
		t.Fatalf("explicit judge_harness = %+v, want %+v", dto.JudgeHarness, want)
	}
	var wire struct {
		JudgeHarness json.RawMessage `json:"judge_harness"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if got, want := string(wire.JudgeHarness), `{"value":"codex","source":"user_default","fixed_at":"create","notes":[]}`; got != want {
		t.Fatalf("explicit wire cell = %s, want %s", got, want)
	}
	if c := modelCell(t, dto.Models, "judge_model", "claude"); !reflect.DeepEqual(c.Notes, []string{"not_used"}) {
		t.Errorf("claude judge_model notes = %v, want [not_used]", c.Notes)
	}
	if c := modelCell(t, dto.Models, "judge_model", "codex"); len(c.Notes) != 0 {
		t.Errorf("codex judge_model notes = %v, want none", c.Notes)
	}
	// Only the judge lane is dimmed; the summary lanes are untouched.
	if c := modelCell(t, dto.Models, "summary_model", "claude"); len(c.Notes) != 0 {
		t.Errorf("claude summary_model notes = %v, want none", c.Notes)
	}

	f.set("judge_runs_on", "claude")
	dto, _ = f.get()
	if c := modelCell(t, dto.Models, "judge_model", "codex"); !reflect.DeepEqual(c.Notes, []string{"not_used"}) {
		t.Errorf("explicit claude: codex judge_model notes = %v, want [not_used]", c.Notes)
	}
	if c := modelCell(t, dto.Models, "judge_model", "claude"); len(c.Notes) != 0 {
		t.Errorf("explicit claude: claude judge_model notes = %v, want none", c.Notes)
	}
}
