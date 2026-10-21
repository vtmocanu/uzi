package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type judgeLaneResp struct {
	Settings struct {
		JudgeModel      *string                        `json:"judge_model"`
		CodexJudgeModel *string                        `json:"codex_judge_model"`
		JudgeModels     []apitypes.ModelSettingCellDTO `json:"judge_models"`
	} `json:"settings"`
}

func decodeJudgeLanes(t *testing.T, body []byte) judgeLaneResp {
	t.Helper()
	var resp judgeLaneResp
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return resp
}

func putJudgeLanes(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.PutMySettings(rec, authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader([]byte(body)))))
	return rec
}

func getJudgeLanes(h *Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.GetMySettings(rec, authed(httptest.NewRequest(http.MethodGet, "/api/me/settings", nil)))
	return rec
}

func instanceHandler(db *fakeSettingsDB, rows ...store.AppSetting) *Handler {
	h := &Handler{q: store.New(db)}
	if rows != nil {
		h.settings = settings.New(&settingsStore{rows: rows}, time.Minute)
	}
	return h
}

func TestPutMySettingsCodexJudgeModelRoundTripAndClear(t *testing.T) {
	db := &fakeSettingsDB{}
	h := instanceHandler(db)
	rec := putJudgeLanes(h, `{"codex_judge_model":"  gpt-6-sol  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !db.codexJudge.Valid || db.codexJudge.String != "gpt-6-sol" {
		t.Fatalf("stored = %+v, want trimmed gpt-6-sol", db.codexJudge)
	}
	if got := decodeJudgeLanes(t, rec.Body.Bytes()).Settings.CodexJudgeModel; got == nil || *got != "gpt-6-sol" {
		t.Fatalf("PUT response codex_judge_model = %v", got)
	}
	if got := decodeJudgeLanes(t, getJudgeLanes(h).Body.Bytes()).Settings.CodexJudgeModel; got == nil || *got != "gpt-6-sol" {
		t.Fatalf("GET codex_judge_model = %v", got)
	}

	rec = putJudgeLanes(h, `{"codex_judge_model":null}`)
	if rec.Code != http.StatusOK || db.codexJudge.Valid {
		t.Fatalf("null must clear: status=%d stored=%+v", rec.Code, db.codexJudge)
	}
	db.codexJudge = txt("gpt-6-sol")
	if rec = putJudgeLanes(h, `{"codex_judge_model":"   "}`); rec.Code != http.StatusOK || db.codexJudge.Valid {
		t.Fatalf("blank must clear: status=%d stored=%+v", rec.Code, db.codexJudge)
	}
}

func TestPutMySettingsRejectsNonCuratedCodexJudgeModel(t *testing.T) {
	for _, bad := range []string{"opus", "gpt-6-luna", "gpt-7-x", "two words"} {
		db := &fakeSettingsDB{codexJudge: txt("gpt-6-sol")}
		rec := putJudgeLanes(instanceHandler(db), `{"codex_judge_model":"`+bad+`"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "codex_judge_model") {
			t.Errorf("%q: status=%d body=%s, want 400 naming codex_judge_model", bad, rec.Code, rec.Body.String())
		}
		if db.codexJudge.String != "gpt-6-sol" {
			t.Errorf("%q: stored value changed to %+v", bad, db.codexJudge)
		}
	}
}

func TestPutMySettingsRejectsCuratedIDsInClaudeJudgeLane(t *testing.T) {
	for _, bad := range []string{"gpt-6-sol", "gpt-6.1-sol", "gpt-6-luna"} {
		db := &fakeSettingsDB{}
		rec := putJudgeLanes(instanceHandler(db), `{"judge_model":"`+bad+`"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "judge_model") {
			t.Errorf("%q: status=%d body=%s, want 400 naming judge_model", bad, rec.Code, rec.Body.String())
		}
		if db.judge.Valid {
			t.Errorf("%q: stored %+v", bad, db.judge)
		}
	}
}

func TestPutMySettingsAbsentJudgeLaneLeavesOtherLane(t *testing.T) {
	db := &fakeSettingsDB{judge: txt("sonnet"), codexJudge: txt("gpt-6-sol")}
	h := instanceHandler(db)
	if rec := putJudgeLanes(h, `{"judge_model":"opus"}`); rec.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
	}
	if db.codexJudge.String != "gpt-6-sol" || db.judge.String != "opus" {
		t.Fatalf("after claude-only PUT: judge=%+v codex=%+v", db.judge, db.codexJudge)
	}
	if rec := putJudgeLanes(h, `{"codex_judge_model":"gpt-6.1-sol"}`); rec.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
	}
	if db.judge.String != "opus" || db.codexJudge.String != "gpt-6.1-sol" {
		t.Fatalf("after codex-only PUT: judge=%+v codex=%+v", db.judge, db.codexJudge)
	}
}

func TestPutMySettingsRejectsJudgeModelsInBody(t *testing.T) {
	db := &fakeSettingsDB{}
	rec := putJudgeLanes(instanceHandler(db), `{"judge_models":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for the read-only judge_models; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUserSettingsJudgeModelsCells(t *testing.T) {
	inst := func(claude, codex string) []store.AppSetting {
		return []store.AppSetting{
			{Key: settings.KeyJudgeModel, Value: claude},
			{Key: settings.KeyCodexJudgeModel, Value: codex},
		}
	}
	str := func(s string) *string { return &s }
	cases := []struct {
		name string
		db   *fakeSettingsDB
		rows []store.AppSetting
		want []apitypes.ModelSettingCellDTO
	}{
		{
			name: "no pins",
			db:   &fakeSettingsDB{},
			want: []apitypes.ModelSettingCellDTO{
				{Harness: "claude", DefaultModel: "opus", DefaultSource: "builtin", ResolvedModel: "opus", Source: "builtin"},
				{Harness: "codex", DefaultModel: "gpt-6.1-sol", DefaultSource: "builtin", ResolvedModel: "gpt-6.1-sol", Source: "builtin"},
			},
		},
		{
			name: "instance pins",
			db:   &fakeSettingsDB{},
			rows: inst("sonnet", "gpt-6-sol"),
			want: []apitypes.ModelSettingCellDTO{
				{Harness: "claude", DefaultModel: "sonnet", DefaultSource: "instance_default", ResolvedModel: "sonnet", Source: "instance_default"},
				{Harness: "codex", DefaultModel: "gpt-6-sol", DefaultSource: "instance_default", ResolvedModel: "gpt-6-sol", Source: "instance_default"},
			},
		},
		{
			name: "user pins",
			db:   &fakeSettingsDB{judge: txt("haiku"), codexJudge: txt("gpt-6-astra")},
			rows: inst("sonnet", "gpt-6-sol"),
			want: []apitypes.ModelSettingCellDTO{
				{Harness: "claude", Model: str("haiku"), DefaultModel: "sonnet", DefaultSource: "instance_default", ResolvedModel: "haiku", Source: "user_default"},
				{Harness: "codex", Model: str("gpt-6-astra"), DefaultModel: "gpt-6-sol", DefaultSource: "instance_default", ResolvedModel: "gpt-6-astra", Source: "user_default"},
			},
		},
		{
			// A legacy Codex id stored in the user Claude lane falls through to the next tier.
			name: "legacy codex id in the user claude lane",
			db:   &fakeSettingsDB{judge: txt("gpt-6-sol")},
			rows: inst("sonnet", ""),
			want: []apitypes.ModelSettingCellDTO{
				{Harness: "claude", Model: str("gpt-6-sol"), DefaultModel: "sonnet", DefaultSource: "instance_default", ResolvedModel: "sonnet", Source: "instance_default"},
				{Harness: "codex", DefaultModel: "gpt-6.1-sol", DefaultSource: "builtin", ResolvedModel: "gpt-6.1-sol", Source: "builtin"},
			},
		},
		{
			name: "legacy codex id, no instance pin",
			db:   &fakeSettingsDB{judge: txt("gpt-6-luna")},
			want: []apitypes.ModelSettingCellDTO{
				{Harness: "claude", Model: str("gpt-6-luna"), DefaultModel: "opus", DefaultSource: "builtin", ResolvedModel: "opus", Source: "builtin"},
				{Harness: "codex", DefaultModel: "gpt-6.1-sol", DefaultSource: "builtin", ResolvedModel: "gpt-6.1-sol", Source: "builtin"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := instanceHandler(c.db, c.rows...)
			for _, rec := range []*httptest.ResponseRecorder{getJudgeLanes(h), putJudgeLanes(h, `{}`)} {
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
				}
				got := decodeJudgeLanes(t, rec.Body.Bytes()).Settings.JudgeModels
				if len(got) != len(c.want) {
					t.Fatalf("cells = %+v, want %d", got, len(c.want))
				}
				for i := range c.want {
					if got[i].Harness != c.want[i].Harness || got[i].DefaultModel != c.want[i].DefaultModel ||
						got[i].DefaultSource != c.want[i].DefaultSource || got[i].ResolvedModel != c.want[i].ResolvedModel ||
						got[i].Source != c.want[i].Source || (got[i].Model == nil) != (c.want[i].Model == nil) ||
						(got[i].Model != nil && *got[i].Model != *c.want[i].Model) {
						t.Errorf("cell %d = %+v, want %+v", i, got[i], c.want[i])
					}
				}
			}
		})
	}
}
