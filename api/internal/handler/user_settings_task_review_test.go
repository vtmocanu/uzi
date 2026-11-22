package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

type taskReviewResp struct {
	Settings struct {
		TaskReviewModel      *string                        `json:"task_review_model"`
		CodexTaskReviewModel *string                        `json:"codex_task_review_model"`
		TaskReviewModels     []apitypes.ModelSettingCellDTO `json:"task_review_models"`
	} `json:"settings"`
}

func decodeTaskReview(t *testing.T, body []byte) taskReviewResp {
	t.Helper()
	var resp taskReviewResp
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return resp
}

func TestPutMySettingsTaskReviewModelsRoundTripAndClear(t *testing.T) {
	db := &fakeSettingsDB{}
	h := instanceHandler(db)
	rec := putJudgeLanes(h, `{"task_review_model":"  opus  ","codex_task_review_model":"gpt-6.1-sol"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if db.taskReview.String != "opus" || db.codexTaskReview.String != "gpt-6.1-sol" {
		t.Fatalf("stored = %+v / %+v, want trimmed values", db.taskReview, db.codexTaskReview)
	}
	got := decodeTaskReview(t, getJudgeLanes(h).Body.Bytes()).Settings
	if got.TaskReviewModel == nil || *got.TaskReviewModel != "opus" || got.CodexTaskReviewModel == nil || *got.CodexTaskReviewModel != "gpt-6.1-sol" {
		t.Fatalf("GET raw fields = %v / %v", got.TaskReviewModel, got.CodexTaskReviewModel)
	}
	if len(got.TaskReviewModels) != 2 || got.TaskReviewModels[0].Harness != "claude" || got.TaskReviewModels[0].Source != "user_default" ||
		got.TaskReviewModels[1].Harness != "codex" || got.TaskReviewModels[1].ResolvedModel != "gpt-6.1-sol" || got.TaskReviewModels[1].DefaultModel != "gpt-6-sol" {
		t.Fatalf("cells = %+v", got.TaskReviewModels)
	}

	// An omitted field is unchanged.
	if rec = putJudgeLanes(h, `{"summary_model":"haiku"}`); rec.Code != http.StatusOK || db.taskReview.String != "opus" || db.codexTaskReview.String != "gpt-6.1-sol" {
		t.Fatalf("unrelated write: status=%d stored=%+v / %+v", rec.Code, db.taskReview, db.codexTaskReview)
	}
	for _, clear := range []string{`null`, `"   "`, `""`} {
		db.taskReview, db.codexTaskReview = txt("opus"), txt("gpt-6.1-sol")
		if rec = putJudgeLanes(h, `{"task_review_model":`+clear+`,"codex_task_review_model":`+clear+`}`); rec.Code != http.StatusOK || db.taskReview.Valid || db.codexTaskReview.Valid {
			t.Fatalf("%s must clear: status=%d stored=%+v / %+v", clear, rec.Code, db.taskReview, db.codexTaskReview)
		}
	}
}

func TestPutMySettingsRejectsBadTaskReviewModels(t *testing.T) {
	for _, tc := range []struct{ body, field string }{
		{`{"task_review_model":"gpt-6-astra"}`, "task_review_model"},
		{`{"task_review_model":"bad\u0000id"}`, "task_review_model"},
		{`{"codex_task_review_model":"opus"}`, "codex_task_review_model"},
		{`{"codex_task_review_model":"custom-codex-1"}`, "codex_task_review_model"},
		{`{"codex_task_review_model":"bad\u0007id"}`, "codex_task_review_model"},
	} {
		db := &fakeSettingsDB{taskReview: txt("opus"), codexTaskReview: txt("gpt-6-sol")}
		rec := putJudgeLanes(instanceHandler(db), tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.field) {
			t.Errorf("%s: status=%d body=%s, want 400 naming %s", tc.body, rec.Code, rec.Body.String(), tc.field)
		}
		if db.taskReview.String != "opus" || db.codexTaskReview.String != "gpt-6-sol" {
			t.Errorf("%s: stored value changed: %+v / %+v", tc.body, db.taskReview, db.codexTaskReview)
		}
	}
}
