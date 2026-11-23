package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func textOf(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func putPlanning(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.PutMySettings(rec, authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewBufferString(body))))
	return rec
}

func decodePlanningSettings(t *testing.T, rec *httptest.ResponseRecorder) apitypes.UserSettingsDTO {
	t.Helper()
	var resp struct {
		Settings apitypes.UserSettingsDTO `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body)
	}
	return resp.Settings
}

func ptr(s string) *string { return &s }

type planningFixtureCase struct {
	ID      string `json:"id"`
	Harness string `json:"harness"`
	Seed    struct {
		Model              *string `json:"planning_claude_model"`
		Effort             *string `json:"planning_claude_effort"`
		Shared             *string `json:"default_effort"`
		CodexModel         *string `json:"planning_codex_model"`
		CodexEffort        *string `json:"planning_codex_effort"`
		CodexDefaultEffort *string `json:"default_codex_effort"`
	} `json:"seed"`
	Body   json.RawMessage `json:"body"`
	Expect struct {
		Status             int     `json:"status"`
		Model              *string `json:"planning_claude_model"`
		Effort             *string `json:"planning_claude_effort"`
		DefaultEffort      *string `json:"default_effort"`
		ErrorContains      string  `json:"error_contains"`
		CodexModel         *string `json:"planning_codex_model"`
		CodexEffort        *string `json:"planning_codex_effort"`
		CodexDefaultEffort *string `json:"default_codex_effort"`
	} `json:"expect"`
}

// TestPlanningDefaultsFixture runs every case of fixtures/settings/planning-defaults-cases.json
// through PutMySettings and asserts each was exercised. fixtures/ sits above the api module, so
// run with -count=1 after editing the fixture.
func TestPlanningDefaultsFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/settings/planning-defaults-cases.json")
	if err != nil {
		t.Fatalf("fixture unreadable: %v", err)
	}
	var f struct {
		Cases []planningFixtureCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	seen := map[string]bool{}
	var ids []string
	for _, c := range f.Cases {
		ids = append(ids, c.ID)
		db := &fakeSettingsDB{planningModel: textOf(c.Seed.Model), planningEffort: textOf(c.Seed.Effort), effort: textOf(c.Seed.Shared),
			planningCodexModel: textOf(c.Seed.CodexModel), planningCodexEffort: textOf(c.Seed.CodexEffort), codexEffort: textOf(c.Seed.CodexDefaultEffort)}
		if c.Harness == "codex" {
			c.Expect.Model, c.Expect.Effort, c.Expect.DefaultEffort = c.Expect.CodexModel, c.Expect.CodexEffort, c.Expect.CodexDefaultEffort
		}
		h := &Handler{q: store.New(db)}
		rec := putPlanning(t, h, string(c.Body))
		if rec.Code != c.Expect.Status {
			t.Errorf("%s: status = %d, want %d: %s", c.ID, rec.Code, c.Expect.Status, rec.Body)
		}
		if c.Expect.ErrorContains != "" && !strings.Contains(rec.Body.String(), c.Expect.ErrorContains) {
			t.Errorf("%s: body %s does not name %q", c.ID, rec.Body, c.Expect.ErrorContains)
		}
		model, effort, ordinary := db.planningModel, db.planningEffort, db.effort
		if c.Harness == "codex" {
			model, effort, ordinary = db.planningCodexModel, db.planningCodexEffort, db.codexEffort
		}
		if model != textOf(c.Expect.Model) || effort != textOf(c.Expect.Effort) {
			t.Errorf("%s: stored pair = %+v/%+v, want %v/%v", c.ID, model, effort, c.Expect.Model, c.Expect.Effort)
		}
		if c.Expect.DefaultEffort != nil && ordinary != textOf(c.Expect.DefaultEffort) {
			t.Errorf("%s: default_effort = %+v, want %s", c.ID, db.effort, *c.Expect.DefaultEffort)
		}
		if c.Expect.Status == 200 {
			got := decodePlanningSettings(t, rec)
			if c.Harness == "codex" {
				got.PlanningClaudeModel, got.PlanningClaudeEffort = got.PlanningCodexModel, got.PlanningCodexEffort
			}
			if (got.PlanningClaudeModel == nil) != (c.Expect.Model == nil) || (got.PlanningClaudeModel != nil && *got.PlanningClaudeModel != *c.Expect.Model) {
				t.Errorf("%s: response model = %v, want %v", c.ID, got.PlanningClaudeModel, c.Expect.Model)
			}
			if (got.PlanningClaudeEffort == nil) != (c.Expect.Effort == nil) || (got.PlanningClaudeEffort != nil && *got.PlanningClaudeEffort != *c.Expect.Effort) {
				t.Errorf("%s: response effort = %v, want %v", c.ID, got.PlanningClaudeEffort, c.Expect.Effort)
			}
		}
		markCase(t, seen, c.ID)
	}
	assertExercised(t, ids, seen)
}

func TestPutMySettingsRejectsPlanningCellsAsUnknownFields(t *testing.T) {
	for _, body := range []string{`{"planning_models":[]}`, `{"planning_efforts":[]}`} {
		db := &fakeSettingsDB{planningModel: textOf(ptr("opus"))}
		h := &Handler{q: store.New(db)}
		if rec := putPlanning(t, h, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
		if db.planningModel.String != "opus" {
			t.Errorf("%s: rejected request changed the stored pair", body)
		}
	}
}

func TestPutMySettingsPlanningNullClearsBothAndGetReturnsNull(t *testing.T) {
	db := &fakeSettingsDB{planningModel: textOf(ptr("opus")), planningEffort: textOf(ptr("high"))}
	h := &Handler{q: store.New(db)}
	if rec := putPlanning(t, h, `{"planning_claude_model":null,"planning_claude_effort":null}`); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
	var got struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"planning_claude_model", "planning_claude_effort"} {
		if v, present := got.Settings[k]; !present || v != nil {
			t.Errorf("%s = %v (present=%v), want explicit null", k, v, present)
		}
	}
}

// claudePinSettingsDB gives the settings fake a lead template Claude pin (template_model) and
// Claude effort (template_effort).
type claudePinSettingsDB struct {
	*fakeSettingsDB
	pin         pgtype.Text
	effort      pgtype.Text
	codexEffort pgtype.Text
}

func (f *claudePinSettingsDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := f.fakeSettingsDB.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	if nr, ok := rows.(*fakeNotifRows); ok {
		for i, scan := range nr.scans {
			nr.scans[i] = func(dest ...any) error {
				if err := scan(dest...); err != nil {
					return err
				}
				*dest[8].(*pgtype.Text) = f.pin
				*dest[10].(*pgtype.Text) = f.effort
				*dest[11].(*pgtype.Text) = f.codexEffort
				return nil
			}
		}
	}
	return rows, nil
}

func TestPlanningCellsResolution(t *testing.T) {
	tests := []struct {
		name                   string
		lane, pin              pgtype.Text
		tmplEffort             pgtype.Text
		sharedEffort           pgtype.Text
		planModel, planEffort  pgtype.Text
		wantDefault            string
		wantDefaultSrc         string
		wantResolved, wantSrc  string
		wantEffDefault         string
		wantEffDefSrc          string
		wantEffResolved        *string
		wantEffSrc, wantEffMod string
	}{
		{name: "nothing set: builtin, built-in effort", wantDefault: "", wantDefaultSrc: "builtin", wantResolved: "", wantSrc: "builtin",
			wantEffDefault: "medium", wantEffDefSrc: "builtin", wantEffResolved: ptr("medium"), wantEffSrc: "builtin", wantEffMod: "explicit"},
		{name: "lane wins default", lane: textOf(ptr("sonnet")), pin: textOf(ptr("haiku")), wantDefault: "sonnet", wantDefaultSrc: "user_default", wantResolved: "sonnet", wantSrc: "user_default",
			wantEffDefault: "medium", wantEffDefSrc: "builtin", wantEffResolved: ptr("medium"), wantEffSrc: "builtin", wantEffMod: "explicit"},
		{name: "template when lane empty", pin: textOf(ptr("haiku")), wantDefault: "haiku", wantDefaultSrc: "template", wantResolved: "haiku", wantSrc: "template",
			wantEffDefault: "medium", wantEffDefSrc: "builtin", wantEffResolved: ptr("medium"), wantEffSrc: "builtin", wantEffMod: "explicit"},
		{name: "planning values win", lane: textOf(ptr("sonnet")), sharedEffort: textOf(ptr("low")), planModel: textOf(ptr("opus")), planEffort: textOf(ptr("max")),
			wantDefault: "sonnet", wantDefaultSrc: "user_default", wantResolved: "opus", wantSrc: "user_planning_default",
			wantEffDefault: "low", wantEffDefSrc: "user_default", wantEffResolved: ptr("max"), wantEffSrc: "user_planning_default", wantEffMod: "explicit"},
		{name: "stored Codex role planning model is unset", planModel: textOf(ptr("gpt-6-sol")), lane: textOf(ptr("sonnet")),
			wantDefault: "sonnet", wantDefaultSrc: "user_default", wantResolved: "sonnet", wantSrc: "user_default",
			wantEffDefault: "medium", wantEffDefSrc: "builtin", wantEffResolved: ptr("medium"), wantEffSrc: "builtin", wantEffMod: "explicit"},
		{name: "Codex role id in the Claude lane is dropped", lane: textOf(ptr("gpt-6-sol")), pin: textOf(ptr("haiku")),
			wantDefault: "haiku", wantDefaultSrc: "template", wantResolved: "haiku", wantSrc: "template",
			wantEffDefault: "medium", wantEffDefSrc: "builtin", wantEffResolved: ptr("medium"), wantEffSrc: "builtin", wantEffMod: "explicit"},
		{name: "Codex role id in the Claude lane, no pin: builtin", lane: textOf(ptr("gpt-6-sol")),
			wantDefault: "", wantDefaultSrc: "builtin", wantResolved: "", wantSrc: "builtin",
			wantEffDefault: "medium", wantEffDefSrc: "builtin", wantEffResolved: ptr("medium"), wantEffSrc: "builtin", wantEffMod: "explicit"},
		{name: "runtime default effort with planning effort", sharedEffort: textOf(ptr("runtime")), planEffort: textOf(ptr("high")),
			wantDefault: "", wantDefaultSrc: "builtin", wantResolved: "", wantSrc: "builtin",
			wantEffDefault: "runtime", wantEffDefSrc: "user_default", wantEffResolved: ptr("high"), wantEffSrc: "user_planning_default", wantEffMod: "explicit"},
		{name: "runtime default effort, no planning effort", sharedEffort: textOf(ptr("runtime")),
			wantDefault: "", wantDefaultSrc: "builtin", wantResolved: "", wantSrc: "builtin",
			wantEffDefault: "runtime", wantEffDefSrc: "user_default", wantEffResolved: nil, wantEffSrc: "user_default", wantEffMod: "runtime_default"},
		{name: "lead template effort, no preference", tmplEffort: textOf(ptr("high")),
			wantDefault: "", wantDefaultSrc: "builtin", wantResolved: "", wantSrc: "builtin",
			wantEffDefault: "high", wantEffDefSrc: "template", wantEffResolved: ptr("high"), wantEffSrc: "template", wantEffMod: "explicit"},
		{name: "preference beats lead template effort", tmplEffort: textOf(ptr("high")), sharedEffort: textOf(ptr("low")),
			wantDefault: "", wantDefaultSrc: "builtin", wantResolved: "", wantSrc: "builtin",
			wantEffDefault: "low", wantEffDefSrc: "user_default", wantEffResolved: ptr("low"), wantEffSrc: "user_default", wantEffMod: "explicit"},
		{name: "runtime preference beats lead template effort", tmplEffort: textOf(ptr("high")), sharedEffort: textOf(ptr("runtime")),
			wantDefault: "", wantDefaultSrc: "builtin", wantResolved: "", wantSrc: "builtin",
			wantEffDefault: "runtime", wantEffDefSrc: "user_default", wantEffResolved: nil, wantEffSrc: "user_default", wantEffMod: "runtime_default"},
		{name: "planning level beats lead template effort", tmplEffort: textOf(ptr("high")), planEffort: textOf(ptr("max")),
			wantDefault: "", wantDefaultSrc: "builtin", wantResolved: "", wantSrc: "builtin",
			wantEffDefault: "high", wantEffDefSrc: "template", wantEffResolved: ptr("max"), wantEffSrc: "user_planning_default", wantEffMod: "explicit"},
		{name: "invalid lead template effort is ignored", tmplEffort: textOf(ptr("bogus")),
			wantDefault: "", wantDefaultSrc: "builtin", wantResolved: "", wantSrc: "builtin",
			wantEffDefault: "medium", wantEffDefSrc: "builtin", wantEffResolved: ptr("medium"), wantEffSrc: "builtin", wantEffMod: "explicit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &claudePinSettingsDB{fakeSettingsDB: &fakeSettingsDB{claudeModel: tc.lane, effort: tc.sharedEffort, planningModel: tc.planModel, planningEffort: tc.planEffort}, pin: tc.pin, effort: tc.tmplEffort}
			h := &Handler{q: store.New(db)}
			rec := httptest.NewRecorder()
			h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
			got := decodePlanningSettings(t, rec)
			if len(got.PlanningModels) != 2 || len(got.PlanningEfforts) != 2 {
				t.Fatalf("cells = %d models, %d efforts, want two each: %s", len(got.PlanningModels), len(got.PlanningEfforts), rec.Body)
			}
			m, e := got.PlanningModels[0], got.PlanningEfforts[0]
			if m.Harness != "claude" || e.Harness != "claude" {
				t.Errorf("harness = %q/%q, want claude", m.Harness, e.Harness)
			}
			if m.DefaultModel != tc.wantDefault || m.DefaultSource != tc.wantDefaultSrc || m.ResolvedModel != tc.wantResolved || m.Source != tc.wantSrc {
				t.Errorf("model cell = %+v", m)
			}
			if (m.Model == nil) != !tc.planModel.Valid || (m.Model != nil && *m.Model != tc.planModel.String) {
				t.Errorf("model cell stored = %v, want %+v", m.Model, tc.planModel)
			}
			if e.DefaultEffort != tc.wantEffDefault || e.DefaultSource != tc.wantEffDefSrc || e.Source != tc.wantEffSrc || e.Mode != tc.wantEffMod {
				t.Errorf("effort cell = %+v", e)
			}
			if (e.ResolvedEffort == nil) != (tc.wantEffResolved == nil) || (e.ResolvedEffort != nil && *e.ResolvedEffort != *tc.wantEffResolved) {
				t.Errorf("effort resolved = %v, want %v", e.ResolvedEffort, tc.wantEffResolved)
			}
		})
	}
}

// TestCodexPlanningRuntimeSaveOrders exercises real successive PUTs, including the
// ordinary Codex effort setter, rather than relying only on seeded fixture state.
func TestCodexPlanningRuntimeSaveOrders(t *testing.T) {
	for _, planningFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "planning first", false: "runtime first"}[planningFirst], func(t *testing.T) {
			h := &Handler{q: store.New(&fakeSettingsDB{})}
			bodies := []string{`{"planning_codex_model":"custom-plan","planning_codex_effort":"high"}`, `{"default_codex_effort":"runtime"}`}
			if !planningFirst {
				bodies[0], bodies[1] = bodies[1], bodies[0]
			}
			var got apitypes.UserSettingsDTO
			for _, body := range bodies {
				rec := putPlanning(t, h, body)
				if rec.Code != http.StatusOK {
					t.Fatalf("PUT %s: %d %s", body, rec.Code, rec.Body)
				}
				got = decodePlanningSettings(t, rec)
			}
			if got.PlanningCodexModel == nil || *got.PlanningCodexModel != "custom-plan" || got.PlanningCodexEffort == nil || *got.PlanningCodexEffort != "high" || got.DefaultCodexEffort == nil || *got.DefaultCodexEffort != "runtime" {
				t.Fatalf("saved pair/default = %+v", got)
			}
			m, e := got.PlanningModels[1], got.PlanningEfforts[1]
			if m.Harness != "codex" || m.ResolvedModel != "custom-plan" || m.Source != "user_planning_default" || e.Harness != "codex" || e.DefaultEffort != "runtime" || e.ResolvedEffort == nil || *e.ResolvedEffort != "high" || e.Source != "user_planning_default" {
				t.Fatalf("Codex cells = %+v / %+v", m, e)
			}
		})
	}
}

// A Codex planning Default must use the same template-aware tier as a claim, just as Claude does.
func TestCodexPlanningDefaultFollowsLeadTemplate(t *testing.T) {
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
	for _, tc := range []struct {
		name, worker, planning, template, wantDefault, wantDefaultSource, wantResolved, wantSource, wantMode string
	}{
		{"template", "", "", "high", "high", "template", "high", "template", "explicit"},
		{"blank worker", "  ", "", "high", "high", "template", "high", "template", "explicit"},
		{"worker preference", "low", "", "high", "low", "user_default", "low", "user_default", "explicit"},
		{"runtime preference", "runtime", "", "high", "runtime", "user_default", "", "user_default", "runtime_default"},
		{"planning pin", "", "max", "high", "high", "template", "max", "user_planning_default", "explicit"},
		{"invalid template", "", "", "future", "medium", "builtin", "medium", "builtin", "explicit"},
		{"invalid preference", "future", "", "high", "medium", "builtin", "medium", "builtin", "explicit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &claudePinSettingsDB{fakeSettingsDB: &fakeSettingsDB{codexEffort: text(tc.worker), planningCodexEffort: text(tc.planning)},
				effort: text("low"), codexEffort: text(tc.template)}
			h := &Handler{q: store.New(db)}
			rec := httptest.NewRecorder()
			h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
			got := decodePlanningSettings(t, rec)
			if len(got.PlanningEfforts) != 2 {
				t.Fatalf("planning cells: %+v", got.PlanningEfforts)
			}
			e := got.PlanningEfforts[1]
			resolved := ""
			if e.ResolvedEffort != nil {
				resolved = *e.ResolvedEffort
			}
			if e.Harness != "codex" || e.DefaultEffort != tc.wantDefault || e.DefaultSource != tc.wantDefaultSource || resolved != tc.wantResolved || e.Source != tc.wantSource || e.Mode != tc.wantMode {
				t.Fatalf("Codex planning cell = %+v; want default %s/%s resolved %s/%s/%s", e, tc.wantDefault, tc.wantDefaultSource, tc.wantResolved, tc.wantSource, tc.wantMode)
			}
			if (e.Effort == nil) != !db.planningCodexEffort.Valid || (e.Effort != nil && *e.Effort != tc.planning) {
				t.Fatalf("stored planning effort = %v", e.Effort)
			}
		})
	}
}
