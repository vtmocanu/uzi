package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type summaryLaneResp struct {
	Settings struct {
		SummaryModel      *string                        `json:"summary_model"`
		CodexSummaryModel *string                        `json:"codex_summary_model"`
		SummaryModels     []apitypes.ModelSettingCellDTO `json:"summary_models"`
	} `json:"settings"`
}

func decodeSummaryLanes(t *testing.T, body []byte) summaryLaneResp {
	t.Helper()
	var resp summaryLaneResp
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return resp
}

func TestPutMySettingsCodexSummaryModelRoundTripAndClear(t *testing.T) {
	db := &fakeSettingsDB{}
	h := instanceHandler(db)
	rec := putJudgeLanes(h, `{"codex_summary_model":"  gpt-6-astra  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !db.codexSummary.Valid || db.codexSummary.String != "gpt-6-astra" {
		t.Fatalf("stored = %+v, want trimmed gpt-6-astra", db.codexSummary)
	}
	if got := decodeSummaryLanes(t, rec.Body.Bytes()).Settings.CodexSummaryModel; got == nil || *got != "gpt-6-astra" {
		t.Fatalf("PUT response codex_summary_model = %v", got)
	}
	if got := decodeSummaryLanes(t, getJudgeLanes(h).Body.Bytes()).Settings.CodexSummaryModel; got == nil || *got != "gpt-6-astra" {
		t.Fatalf("GET codex_summary_model = %v", got)
	}
	// The built-in luna is a role id, so it is a valid user value too.
	if rec = putJudgeLanes(h, `{"codex_summary_model":"gpt-6-luna"}`); rec.Code != http.StatusOK || db.codexSummary.String != "gpt-6-luna" {
		t.Fatalf("gpt-6-luna: status=%d stored=%+v", rec.Code, db.codexSummary)
	}

	for _, clear := range []string{`null`, `"   "`, `""`} {
		db.codexSummary = txt("gpt-6-astra")
		if rec = putJudgeLanes(h, `{"codex_summary_model":`+clear+`}`); rec.Code != http.StatusOK || db.codexSummary.Valid {
			t.Fatalf("%s must clear: status=%d stored=%+v", clear, rec.Code, db.codexSummary)
		}
	}
}

func TestPutMySettingsClaudeSummaryModelBlankInherits(t *testing.T) {
	for _, blank := range []string{`null`, `"  "`} {
		db := &fakeSettingsDB{summary: txt("opus")}
		rec := putJudgeLanes(instanceHandler(db), `{"summary_model":`+blank+`}`)
		if rec.Code != http.StatusOK || db.summary.Valid {
			t.Errorf("%s: status=%d stored=%+v, want 200 and NULL", blank, rec.Code, db.summary)
		}
	}
}

func TestPutMySettingsRejectsBadCodexSummaryModel(t *testing.T) {
	for _, bad := range []string{"opus", "gpt-7-custom", "two words"} {
		db := &fakeSettingsDB{codexSummary: txt("gpt-6-sol")}
		rec := putJudgeLanes(instanceHandler(db), `{"codex_summary_model":"`+bad+`"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "codex_summary_model") {
			t.Errorf("%q: status=%d body=%s, want 400 naming codex_summary_model", bad, rec.Code, rec.Body.String())
		}
		if db.codexSummary.String != "gpt-6-sol" {
			t.Errorf("%q: stored value changed to %+v", bad, db.codexSummary)
		}
	}
}

func TestPutMySettingsRejectsCodexIDsInClaudeSummaryLane(t *testing.T) {
	for _, bad := range []string{"gpt-6-luna", "gpt-6-sol", "gpt-6.1-sol", "gpt-6-astra"} {
		db := &fakeSettingsDB{}
		rec := putJudgeLanes(instanceHandler(db), `{"summary_model":"`+bad+`"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "summary_model") {
			t.Errorf("%q: status=%d body=%s, want 400 naming summary_model", bad, rec.Code, rec.Body.String())
		}
		if db.summary.Valid {
			t.Errorf("%q: stored %+v", bad, db.summary)
		}
	}
}

func TestPutMySettingsAbsentSummaryLaneLeavesOtherLane(t *testing.T) {
	db := &fakeSettingsDB{summary: txt("sonnet"), codexSummary: txt("gpt-6-sol")}
	h := instanceHandler(db)
	if rec := putJudgeLanes(h, `{"summary_model":"opus"}`); rec.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
	}
	if db.codexSummary.String != "gpt-6-sol" || db.summary.String != "opus" {
		t.Fatalf("after claude-only PUT: summary=%+v codex=%+v", db.summary, db.codexSummary)
	}
	if rec := putJudgeLanes(h, `{"codex_summary_model":"gpt-6.1-sol"}`); rec.Code != http.StatusOK {
		t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
	}
	if db.summary.String != "opus" || db.codexSummary.String != "gpt-6.1-sol" {
		t.Fatalf("after codex-only PUT: summary=%+v codex=%+v", db.summary, db.codexSummary)
	}
}

func TestPutMySettingsRejectsSummaryModelsInBody(t *testing.T) {
	rec := putJudgeLanes(instanceHandler(&fakeSettingsDB{}), `{"summary_models":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for the read-only summary_models; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUserSettingsSummaryModelsCells(t *testing.T) {
	inst := func(claude, codex string) []store.AppSetting {
		return []store.AppSetting{
			{Key: settings.KeySummaryModel, Value: claude},
			{Key: settings.KeyCodexSummaryModel, Value: codex},
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
			name: "builtin",
			db:   &fakeSettingsDB{},
			want: []apitypes.ModelSettingCellDTO{
				{Harness: "claude", DefaultModel: "haiku", DefaultSource: "builtin", ResolvedModel: "haiku", Source: "builtin"},
				{Harness: "codex", DefaultModel: "gpt-6-luna", DefaultSource: "builtin", ResolvedModel: "gpt-6-luna", Source: "builtin"},
			},
		},
		{
			name: "instance default",
			db:   &fakeSettingsDB{},
			rows: inst("sonnet", "gpt-6.1-sol"),
			want: []apitypes.ModelSettingCellDTO{
				{Harness: "claude", DefaultModel: "sonnet", DefaultSource: "instance_default", ResolvedModel: "sonnet", Source: "instance_default"},
				{Harness: "codex", DefaultModel: "gpt-6.1-sol", DefaultSource: "instance_default", ResolvedModel: "gpt-6.1-sol", Source: "instance_default"},
			},
		},
		{
			name: "user default",
			db:   &fakeSettingsDB{summary: txt("opus"), codexSummary: txt("gpt-6-astra")},
			rows: inst("sonnet", "gpt-6.1-sol"),
			want: []apitypes.ModelSettingCellDTO{
				{Harness: "claude", Model: str("opus"), DefaultModel: "sonnet", DefaultSource: "instance_default", ResolvedModel: "opus", Source: "user_default"},
				{Harness: "codex", Model: str("gpt-6-astra"), DefaultModel: "gpt-6.1-sol", DefaultSource: "instance_default", ResolvedModel: "gpt-6-astra", Source: "user_default"},
			},
		},
		{
			// A legacy Codex id stored in the user Claude lane falls through to the next tier.
			name: "legacy codex id in the user claude lane",
			db:   &fakeSettingsDB{summary: txt("gpt-6-sol")},
			rows: inst("sonnet", ""),
			want: []apitypes.ModelSettingCellDTO{
				{Harness: "claude", Model: str("gpt-6-sol"), DefaultModel: "sonnet", DefaultSource: "instance_default", ResolvedModel: "sonnet", Source: "instance_default"},
				{Harness: "codex", DefaultModel: "gpt-6-luna", DefaultSource: "builtin", ResolvedModel: "gpt-6-luna", Source: "builtin"},
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
				got := decodeSummaryLanes(t, rec.Body.Bytes()).Settings.SummaryModels
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
