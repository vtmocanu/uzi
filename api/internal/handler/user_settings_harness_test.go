package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// user_settings_harness_test.go covers the PRD #1551 M1 grouped harness/model write on
// PUT /api/me/settings: the two per-harness lanes, the deprecated default_model legacy
// bridge (D3), the closed-list cross-vocabulary guard, and the effective-harness
// projection in the response. Every test runs with a nil wsvc, so ResolveSettingsHarness
// is not consulted and the effective harness is Claude — the zero-credential projection.
// A Codex target is reached deterministically through an explicit default_harness:codex,
// which takes no credential check (D3), so these need no live database.

type settingsLanes struct {
	DefaultModel       *string `json:"default_model"`
	DefaultClaudeModel *string `json:"default_claude_model"`
	DefaultCodexModel  *string `json:"default_codex_model"`
	DefaultHarness     *string `json:"default_harness"`
}

func decodeLanes(t *testing.T, body []byte) settingsLanes {
	t.Helper()
	var resp struct {
		Settings settingsLanes `json:"settings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode settings response %s: %v", body, err)
	}
	return resp.Settings
}

func putUserSettings(t *testing.T, db *fakeSettingsDB, bodyJSON string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(bodyJSON))))
	h.PutMySettings(rec, req)
	return rec
}

// A single PUT saves the harness and both lanes; RETURNING/GET reflect all three, and the
// legacy default_model mirrors the Claude lane (nil wsvc ⇒ Claude effective harness),
// even when the request pins Codex as the legacy input target.
func TestPutMySettingsHarnessModelsGroupedWrite(t *testing.T) {
	db := &fakeSettingsDB{}
	rec := putUserSettings(t, db, `{"default_harness":"codex","default_claude_model":"opus","default_codex_model":"gpt-6-sol"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.claudeModel.Valid || db.claudeModel.String != "opus" {
		t.Errorf("stored claude lane = %+v, want opus", db.claudeModel)
	}
	if !db.codexModel.Valid || db.codexModel.String != "gpt-6-sol" {
		t.Errorf("stored codex lane = %+v, want gpt-6-sol", db.codexModel)
	}
	if !db.defaultHarness.Valid || db.defaultHarness.String != "codex" {
		t.Errorf("stored default_harness = %+v, want codex", db.defaultHarness)
	}
	// default_harness=codex is the legacy input target, but a nil worker service has no
	// usable Codex credential, so the compatibility column mirrors the effective Claude lane.
	if !db.model.Valid || db.model.String != "opus" {
		t.Errorf("legacy default_model column = %+v, want the effective Claude lane opus", db.model)
	}
	got := decodeLanes(t, rec.Body.Bytes())
	if got.DefaultClaudeModel == nil || *got.DefaultClaudeModel != "opus" {
		t.Errorf("response default_claude_model = %v, want opus", got.DefaultClaudeModel)
	}
	if got.DefaultCodexModel == nil || *got.DefaultCodexModel != "gpt-6-sol" {
		t.Errorf("response default_codex_model = %v, want gpt-6-sol", got.DefaultCodexModel)
	}
	// nil wsvc ⇒ effective harness Claude ⇒ default_model projects the Claude lane.
	if got.DefaultModel == nil || *got.DefaultModel != "opus" {
		t.Errorf("response default_model = %v, want the Claude-lane projection opus", got.DefaultModel)
	}
}

// Setting only one lane leaves the other lane untouched (PATCH retention).
func TestPutMySettingsHarnessModelsLaneRetention(t *testing.T) {
	db := &fakeSettingsDB{
		claudeModel: pgtype.Text{String: "sonnet", Valid: true},
		codexModel:  pgtype.Text{String: "gpt-6-astra", Valid: true},
	}
	rec := putUserSettings(t, db, `{"default_codex_model":"gpt-6-sol"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.codexModel.Valid || db.codexModel.String != "gpt-6-sol" {
		t.Errorf("codex lane = %+v, want gpt-6-sol", db.codexModel)
	}
	if !db.claudeModel.Valid || db.claudeModel.String != "sonnet" {
		t.Errorf("claude lane must be retained, got %+v, want sonnet", db.claudeModel)
	}
}

// A present-null lane clears it to inherit while retaining the other lane.
func TestPutMySettingsHarnessModelsClearLane(t *testing.T) {
	db := &fakeSettingsDB{
		claudeModel: pgtype.Text{String: "sonnet", Valid: true},
		codexModel:  pgtype.Text{String: "gpt-6-sol", Valid: true},
	}
	rec := putUserSettings(t, db, `{"default_claude_model":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if db.claudeModel.Valid {
		t.Errorf("claude lane should clear to NULL, got %+v", db.claudeModel)
	}
	if !db.codexModel.Valid || db.codexModel.String != "gpt-6-sol" {
		t.Errorf("codex lane must be retained, got %+v", db.codexModel)
	}
}

// Zero-credential (nil wsvc): a legacy-only default_model routes to the Claude lane and the
// legacy column mirrors it — settings never fail for want of a usable credential (D3).
func TestPutMySettingsLegacyOnlyZeroCredentialRoutesToClaude(t *testing.T) {
	db := &fakeSettingsDB{}
	rec := putUserSettings(t, db, `{"default_model":"opus"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.claudeModel.Valid || db.claudeModel.String != "opus" {
		t.Errorf("legacy write must land in the Claude lane, got %+v", db.claudeModel)
	}
	if db.codexModel.Valid {
		t.Errorf("legacy write must not touch the Codex lane, got %+v", db.codexModel)
	}
	if !db.model.Valid || db.model.String != "opus" {
		t.Errorf("legacy default_model column = %+v, want opus", db.model)
	}
}

// Rule 1: an explicit default_harness:codex is the legacy write target with no credential
// check, so default_model routes into the Codex lane.
func TestPutMySettingsLegacyBridgeExplicitCodexTarget(t *testing.T) {
	db := &fakeSettingsDB{}
	rec := putUserSettings(t, db, `{"default_model":"gpt-6-sol","default_harness":"codex"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.codexModel.Valid || db.codexModel.String != "gpt-6-sol" {
		t.Errorf("legacy write must land in the Codex lane, got %+v", db.codexModel)
	}
	if db.claudeModel.Valid {
		t.Errorf("legacy write must not touch the Claude lane, got %+v", db.claudeModel)
	}
}

// Rule 2: an explicit default_harness:null makes the target Claude regardless of credentials.
func TestPutMySettingsLegacyBridgeExplicitNullHarnessTarget(t *testing.T) {
	db := &fakeSettingsDB{}
	rec := putUserSettings(t, db, `{"default_model":"opus","default_harness":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.claudeModel.Valid || db.claudeModel.String != "opus" {
		t.Errorf("null harness ⇒ Claude target, got claude lane %+v", db.claudeModel)
	}
	if db.defaultHarness.Valid {
		t.Errorf("default_harness should be cleared to NULL, got %+v", db.defaultHarness)
	}
}

// A legacy default_model that matches its target lane's explicit value is accepted (no conflict).
func TestPutMySettingsLegacyMatchingExplicitLaneOK(t *testing.T) {
	db := &fakeSettingsDB{}
	rec := putUserSettings(t, db, `{"default_model":"opus","default_claude_model":"opus"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.claudeModel.Valid || db.claudeModel.String != "opus" {
		t.Errorf("claude lane = %+v, want opus", db.claudeModel)
	}
}

// A legacy default_model that DISAGREES with its target lane's explicit value is a 400 that
// writes nothing (D3 conflict).
func TestPutMySettingsLegacyConflictIsRejectedWithNoWrite(t *testing.T) {
	db := &fakeSettingsDB{}
	rec := putUserSettings(t, db, `{"default_model":"opus","default_claude_model":"sonnet"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if db.claudeModel.Valid || db.codexModel.Valid || db.model.Valid {
		t.Errorf("a conflict must write nothing, got claude=%+v codex=%+v legacy=%+v", db.claudeModel, db.codexModel, db.model)
	}
}

// The closed-list cross-vocabulary guard: a curated Codex id in the Claude lane, and a known
// Claude alias in the Codex lane, each 400 with no write.
func TestPutMySettingsCrossVocabularyRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"codex-id-in-claude-lane", `{"default_claude_model":"gpt-6-sol"}`},
		{"claude-alias-in-codex-lane", `{"default_codex_model":"opus"}`},
		{"legacy-codex-id-claude-target", `{"default_model":"gpt-6-astra"}`},
		{"legacy-claude-alias-codex-target", `{"default_model":"sonnet","default_harness":"codex"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeSettingsDB{}
			rec := putUserSettings(t, db, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if db.claudeModel.Valid || db.codexModel.Valid || db.model.Valid {
				t.Errorf("a cross-vocabulary reject must write nothing, got claude=%+v codex=%+v legacy=%+v", db.claudeModel, db.codexModel, db.model)
			}
		})
	}
}

// A validated CUSTOM Codex id (not a curated one, not a Claude alias) is accepted in the
// Codex lane — the escape hatch (D5), gated only by the closed cross-vocabulary list.
func TestPutMySettingsCustomCodexIDAccepted(t *testing.T) {
	db := &fakeSettingsDB{}
	rec := putUserSettings(t, db, `{"default_codex_model":"gpt-6-custom-x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.codexModel.Valid || db.codexModel.String != "gpt-6-custom-x" {
		t.Errorf("codex lane = %+v, want the validated custom id", db.codexModel)
	}
}

// GET returns both lanes and the effective-harness default_model projection.
func TestGetMySettingsHarnessModelShape(t *testing.T) {
	db := &fakeSettingsDB{
		claudeModel: pgtype.Text{String: "sonnet", Valid: true},
		codexModel:  pgtype.Text{String: "gpt-6-sol", Valid: true},
	}
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeLanes(t, rec.Body.Bytes())
	if got.DefaultClaudeModel == nil || *got.DefaultClaudeModel != "sonnet" {
		t.Errorf("default_claude_model = %v, want sonnet", got.DefaultClaudeModel)
	}
	if got.DefaultCodexModel == nil || *got.DefaultCodexModel != "gpt-6-sol" {
		t.Errorf("default_codex_model = %v, want gpt-6-sol", got.DefaultCodexModel)
	}
	// nil wsvc ⇒ Claude effective ⇒ projection is the Claude lane, not the Codex lane.
	if got.DefaultModel == nil || *got.DefaultModel != "sonnet" {
		t.Errorf("default_model projection = %v, want sonnet (Claude lane)", got.DefaultModel)
	}
}
