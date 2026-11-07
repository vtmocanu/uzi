package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestStartRefusalMapsTheCreateSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want refusal
	}{
		{"plain explicit refusal", workersvc.ErrNoCredentialForHarness, refusal{Token: "no_credential_for_harness"}},
		{"all credentials disabled", workersvc.ErrHarnessCredentialDisabled, refusal{"no_credential_for_harness", "credential_disabled"}},
		{"codex pool unavailable", workersvc.ErrCodexPoolUnavailable, refusal{"no_credential_for_harness", "codex_pool_unavailable"}},
		{"neither usable", workersvc.ErrNoUsableCredential, refusal{Token: "no_usable_credential"}},
		{"disabled pinned token", workersvc.ErrCredentialDisabled, refusal{Token: "credential_disabled"}},
		{"missing, deleted or foreign token", workersvc.ErrCredentialOverrideSecretNotFound, refusal{Token: "credential_not_found"}},
		{"codex harness", workersvc.ErrCredentialOverrideHarnessUnsupported, refusal{Token: "credential_override_harness_unsupported"}},
		{"pinned without a token", workersvc.ErrCredentialOverridePinnedNeedsSecret, refusal{Token: "credential_override_invalid"}},
		{"wrapped sentinel", fmt.Errorf("create: %w", workersvc.ErrHarnessCredentialDisabled), refusal{"no_credential_for_harness", "credential_disabled"}},
	}
	for _, c := range cases {
		got, ok := startRefusal(c.err)
		if !ok || got != c.want {
			t.Errorf("%s: got %+v ok=%v, want %+v", c.name, got, ok, c.want)
		}
	}
	if _, ok := startRefusal(errors.New("store down")); ok {
		t.Error("an arbitrary error must not read as a refusal")
	}
}

func alt(harness string, ref *refusal) apitypes.StartPreviewAlternativeDTO {
	a := apitypes.StartPreviewAlternativeDTO{Harness: harness}
	if ref != nil {
		a.Refusal, a.RefusalDetail = ref.ptrs()
	} else {
		a.Credential = &apitypes.CredentialDTO{Kind: "anthropic", Source: "claim_selection", FixedAt: "claim"}
	}
	return a
}

// TestCommonAlternativeRefusal pins the owner-bound top-level rule: promoted only when every
// alternative is refused for the same reason; differing reasons or any valid branch leave it null.
func TestCommonAlternativeRefusal(t *testing.T) {
	unsupported := refusal{Token: "credential_override_harness_unsupported"}
	noCred := refusal{"no_credential_for_harness", "credential_disabled"}
	cases := []struct {
		name string
		alts []apitypes.StartPreviewAlternativeDTO
		want refusal
	}{
		{"both refused for the same reason", []apitypes.StartPreviewAlternativeDTO{alt("claude", &unsupported), alt("codex", &unsupported)}, unsupported},
		{"both refused for the same token and detail", []apitypes.StartPreviewAlternativeDTO{alt("claude", &noCred), alt("codex", &noCred)}, noCred},
		{"both refused for differing reasons stays null", []apitypes.StartPreviewAlternativeDTO{alt("claude", &noCred), alt("codex", &unsupported)}, refusal{}},
		{"same token with a differing detail stays null", []apitypes.StartPreviewAlternativeDTO{
			alt("claude", &refusal{Token: "no_credential_for_harness"}), alt("codex", &noCred)}, refusal{}},
		{"a valid branch leaves it null", []apitypes.StartPreviewAlternativeDTO{alt("claude", nil), alt("codex", &unsupported)}, refusal{}},
		{"two valid branches", []apitypes.StartPreviewAlternativeDTO{alt("claude", nil), alt("codex", nil)}, refusal{}},
	}
	for _, c := range cases {
		if got := commonAlternativeRefusal(c.alts); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func pinCell(stage, harness string, pin, effort *string, resolvedModel *string, resolvedEffort, modelSource, effortSource string) crossCheckPinDTO {
	def := "gpt-6.1-sol"
	return crossCheckPinDTO{Stage: stage, Harness: harness, Model: pin, Effort: effort, WorkerDefaultModel: &def,
		ResolvedModel: resolvedModel, ResolvedEffort: resolvedEffort, ModelSource: modelSource, EffortSource: effortSource, Active: true}
}

func fourPins(mut func(*crossCheckPinDTO)) []crossCheckPinDTO {
	var out []crossCheckPinDTO
	for _, s := range []string{"plan", "code"} {
		for _, h := range []string{"claude", "codex"} {
			m := "gpt-6.1-sol"
			c := pinCell(s, h, nil, nil, &m, "high", "worker default", "cross-check default")
			if mut != nil {
				mut(&c)
			}
			out = append(out, c)
		}
	}
	return out
}

func TestCrossCheckEffectiveCellsMapTheTokens(t *testing.T) {
	pin, eff := "gpt-6-sol", "low"
	pins := fourPins(func(c *crossCheckPinDTO) {
		if c.Stage == "plan" && c.Harness == "codex" {
			c.Model, c.Effort = &pin, &eff
			c.ResolvedModel, c.ResolvedEffort = &pin, eff
			c.ModelSource, c.EffortSource = "pin", "pin"
		}
		if c.Stage == "code" && c.Harness == "claude" {
			c.ResolvedModel = nil // the Claude SDK decides
		}
	})
	// Delivered out of order: the view orders plan before code and Claude before Codex.
	pins[0], pins[3] = pins[3], pins[0]
	models, efforts, err := crossCheckEffectiveCells(pins)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 4 || len(efforts) != 4 {
		t.Fatalf("cells = %d/%d, want 4/4", len(models), len(efforts))
	}
	order := []string{"plan/claude", "plan/codex", "code/claude", "code/codex"}
	for i, key := range order {
		m, e := models[i], efforts[i]
		if got := strings.TrimPrefix(m.Setting, "cross_check_") + "/" + m.Harness; got != key {
			t.Fatalf("model cell %d = %s, want %s", i, got, key)
		}
		if got := strings.TrimPrefix(e.Setting, "cross_check_") + "/" + e.Harness; got != key {
			t.Fatalf("effort cell %d = %s, want %s", i, got, key)
		}
	}
	unpinned := efforts[0]
	if unpinned.Effort != nil || unpinned.DefaultEffort != "high" || unpinned.DefaultSource != "cross_check_default" ||
		unpinned.ResolvedEffort == nil || *unpinned.ResolvedEffort != "high" || unpinned.Source != "cross_check_default" || unpinned.Mode != "explicit" {
		t.Errorf("unpinned effort cell = %+v", unpinned)
	}
	pinned := efforts[1]
	if pinned.Effort == nil || *pinned.Effort != "low" || *pinned.ResolvedEffort != "low" || pinned.Source != "cross_check_pin" || pinned.DefaultSource != "cross_check_default" {
		t.Errorf("pinned effort cell = %+v", pinned)
	}
	if m := models[1]; m.Model == nil || *m.Model != pin || m.ResolvedModel != pin || m.Source != "cross_check_pin" || m.DefaultSource != "cross_check_worker_default" {
		t.Errorf("pinned model cell = %+v", m)
	}
	if m := models[0]; m.Source != "cross_check_worker_default" || m.ResolvedModel != "gpt-6.1-sol" || m.FixedAt != "claim" {
		t.Errorf("worker-default model cell = %+v", m)
	}
	if m := models[2]; m.ResolvedModel != "" || len(m.Notes) != 1 || m.Notes[0] != "sdk_default" {
		t.Errorf("SDK-default model cell = %+v", m)
	}
}

func TestCrossCheckEffectiveCellsRefuseUnknownTokens(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(*crossCheckPinDTO)
	}{
		{"model source", func(c *crossCheckPinDTO) { c.ModelSource = "template" }},
		{"effort source", func(c *crossCheckPinDTO) { c.EffortSource = "worker default" }},
		{"empty effort source", func(c *crossCheckPinDTO) { c.EffortSource = "" }},
	} {
		if _, _, err := crossCheckEffectiveCells(fourPins(c.mut)); err == nil {
			t.Errorf("%s: an unknown token must be an error, not a guessed label", c.name)
		}
	}
	if _, _, err := crossCheckEffectiveCells(fourPins(nil)[:3]); err == nil {
		t.Error("a missing cell must be an error")
	}
}

// ---- instance cache posture: three fake-cache cases ----

type flakyInstanceStore struct {
	rows []store.AppSetting
	err  error
}

func (s *flakyInstanceStore) ListAppSettings(context.Context) ([]store.AppSetting, error) {
	return s.rows, s.err
}

func TestEffectiveInstanceModelsCachePosture(t *testing.T) {
	row := store.AppSetting{Key: settings.KeyJudgeModel, Value: "haiku"}
	ctx := context.Background()
	t.Run("warm cache serves the instance value", func(t *testing.T) {
		h := &Handler{settings: settings.New(&flakyInstanceStore{rows: []store.AppSetting{row}}, time.Minute)}
		vals, notes := h.effectiveInstanceModels(ctx)
		if vals[settings.KeyJudgeModel] != "haiku" || len(notes) != 0 {
			t.Fatalf("vals=%v notes=%v", vals, notes)
		}
	})
	t.Run("warm then a refresh error serves the last map", func(t *testing.T) {
		st := &flakyInstanceStore{rows: []store.AppSetting{row}}
		h := &Handler{settings: settings.New(st, time.Millisecond)}
		if _, notes := h.effectiveInstanceModels(ctx); len(notes) != 0 {
			t.Fatalf("warm read noted %v", notes)
		}
		time.Sleep(20 * time.Millisecond) // past the TTL
		st.err = errors.New("db down")
		vals, notes := h.effectiveInstanceModels(ctx)
		if vals[settings.KeyJudgeModel] != "haiku" || len(notes) != 0 {
			t.Fatalf("a valid-but-stale cache must serve its last map: vals=%v notes=%v", vals, notes)
		}
	})
	t.Run("cold cache error is the built-in with the note", func(t *testing.T) {
		h := &Handler{settings: settings.New(&flakyInstanceStore{err: errors.New("db down")}, time.Minute)}
		vals, notes := h.effectiveInstanceModels(ctx)
		if len(vals) != 0 || len(notes) != 1 || notes[0] != "instance_settings_unavailable" {
			t.Fatalf("vals=%v notes=%v", vals, notes)
		}
	})
	t.Run("an error right after an admin write is the built-in with the note", func(t *testing.T) {
		st := &flakyInstanceStore{rows: []store.AppSetting{row}}
		cache := settings.New(st, time.Minute)
		h := &Handler{settings: cache}
		if _, notes := h.effectiveInstanceModels(ctx); len(notes) != 0 {
			t.Fatalf("warm read noted %v", notes)
		}
		cache.Invalidate()
		st.err = errors.New("db down")
		vals, notes := h.effectiveInstanceModels(ctx)
		if len(vals) != 0 || len(notes) != 1 {
			t.Fatalf("vals=%v notes=%v", vals, notes)
		}
	})
}

func TestClaimedModelCellCarriesTheInstanceNote(t *testing.T) {
	cell := claimedModelCell("judge_model", judgeModelCells(nil, nil, map[string]string{})[0], []string{"instance_settings_unavailable"})
	if cell.Setting != "judge_model" || cell.FixedAt != "claim" || len(cell.Notes) != 1 || cell.Notes[0] != "instance_settings_unavailable" {
		t.Fatalf("cell = %+v", cell)
	}
	if cell.Source != "builtin" || cell.ResolvedModel == "" {
		t.Fatalf("with no instance tier the cell resolves the built-in: %+v", cell)
	}
}

// ---- request decoding ----

func previewRequest(body string) *http.Request {
	return userReq(http.MethodPost, "/api/me/effective-settings/start-preview", body, uuid.New(), nil)
}

func TestStartPreviewRejectsMalformedRequests(t *testing.T) {
	huge := `{"harness":null,"credential_override":{"mode":"pinned","secret_id":"` + strings.Repeat("a", startPreviewBodyLimit) + `"}}`
	cases := map[string]string{
		"unknown field":            `{"harness":null,"credential_override":null,"model":"opus"}`,
		"unknown nested field":     `{"credential_override":{"mode":"auto","label":"x"}}`,
		"trailing value":           `{"harness":null} {"harness":null}`,
		"oversize body":            huge,
		"bad harness":              `{"harness":"gemini"}`,
		"empty harness":            `{"harness":""}`,
		"bad mode":                 `{"credential_override":{"mode":"sometimes"}}`,
		"empty mode":               `{"credential_override":{}}`,
		"malformed secret id":      `{"credential_override":{"mode":"pinned","secret_id":"not-a-uuid"}}`,
		"not json":                 `harness`,
		"wrong type":               `{"harness":7}`,
		"empty body":               ``,
		"harness in the wrong key": `{"Harness":"claude","credential":{}}`,
	}
	h := &Handler{} // every one of these is refused before any service is consulted
	for name, body := range cases {
		rec := httptest.NewRecorder()
		h.PreviewStartSettings(rec, previewRequest(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
}

func TestEffectiveSettingsRoutesRequireAUser(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.GetEffectiveSettings(rec, httptest.NewRequest(http.MethodGet, "/api/me/effective-settings", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET without a user = %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.PreviewStartSettings(rec, httptest.NewRequest(http.MethodPost, "/api/me/effective-settings/start-preview", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST without a user = %d, want 401", rec.Code)
	}
}

// ---- store errors: 500 and no partial body ----

type failingDB struct{}

var errFailingDB = errors.New("failingDB: store is down")

func (failingDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errFailingDB
}
func (failingDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errFailingDB }
func (failingDB) QueryRow(context.Context, string, ...any) pgx.Row        { return failingRow{} }

type failingRow struct{}

func (failingRow) Scan(...any) error { return errFailingDB }

func TestEffectiveSettingsStoreErrorIs500WithNoPartialBody(t *testing.T) {
	q := store.New(failingDB{})
	h := &Handler{q: q, wsvc: workersvc.New(q, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()}),
		settings: settings.New(&flakyInstanceStore{}, time.Minute)}
	user := uuid.New()
	for name, call := range map[string]func(*httptest.ResponseRecorder){
		"GET": func(rec *httptest.ResponseRecorder) {
			h.GetEffectiveSettings(rec, userReq(http.MethodGet, "/api/me/effective-settings", "", user, nil))
		},
		"start-preview": func(rec *httptest.ResponseRecorder) {
			h.PreviewStartSettings(rec, userReq(http.MethodPost, "/api/me/effective-settings/start-preview", `{"harness":null,"credential_override":null}`, user, nil))
		},
	} {
		rec := httptest.NewRecorder()
		call(rec)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500", name, rec.Code)
		}
		if body := strings.TrimSpace(rec.Body.String()); body != `{"error":"internal error"}` {
			t.Errorf("%s: body %q, want only the generic error envelope", name, body)
		}
	}
}
