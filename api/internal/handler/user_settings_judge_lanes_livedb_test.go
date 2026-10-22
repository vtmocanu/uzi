package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUserJudgeLanesRoundTripLiveDB drives the user settings PUT/GET against the real
// users columns (judge_model, codex_judge_model) and the real GetUserSettings column
// list, with the instance tier supplied through the settings cache (PRD #3245).
// Skipped unless UZI_TEST_DATABASE_URL is set (./e2e/run-store-it.sh).
func TestUserJudgeLanesRoundTripLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	t.Cleanup(func() { cliMustExec(t, pool, `DELETE FROM users WHERE id=$1`, owner) })
	h.settings = settings.New(&settingsStore{rows: []store.AppSetting{
		{Key: settings.KeyCodexJudgeModel, Value: "gpt-6-sol"},
	}}, time.Minute)
	user := store.User{ID: owner, Email: "x@e2e", IsActive: true}
	do := func(method, body string) *httptest.ResponseRecorder {
		var rd *bytes.Reader
		if body != "" {
			rd = bytes.NewReader([]byte(body))
		} else {
			rd = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, "/api/me/settings", rd)
		req = req.WithContext(mw.ContextWithUser(req.Context(), user))
		rec := httptest.NewRecorder()
		if method == http.MethodPut {
			h.PutMySettings(rec, req)
		} else {
			h.GetMySettings(rec, req)
		}
		return rec
	}

	if rec := do(http.MethodPut, `{"judge_model":"sonnet","codex_judge_model":" gpt-6-astra "}`); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	var stored, storedCodex *string
	if err := pool.QueryRow(t.Context(), `SELECT judge_model, codex_judge_model FROM users WHERE id=$1`, owner).Scan(&stored, &storedCodex); err != nil {
		t.Fatal(err)
	}
	if stored == nil || *stored != "sonnet" || storedCodex == nil || *storedCodex != "gpt-6-astra" {
		t.Fatalf("stored = %v / %v", stored, storedCodex)
	}
	got := decodeJudgeLanes(t, do(http.MethodGet, "").Body.Bytes()).Settings.JudgeModels
	if len(got) != 2 || got[0].Harness != "claude" || got[0].ResolvedModel != "sonnet" || got[0].Source != "user_default" ||
		got[0].DefaultModel != "opus" || got[0].DefaultSource != "builtin" ||
		got[1].Harness != "codex" || got[1].ResolvedModel != "gpt-6-astra" || got[1].Source != "user_default" ||
		got[1].DefaultModel != "gpt-6-sol" || got[1].DefaultSource != "instance_default" {
		t.Fatalf("cells = %+v", got)
	}

	// Absent leaves the other lane; null clears only its own.
	if rec := do(http.MethodPut, `{"codex_judge_model":null}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	if err := pool.QueryRow(t.Context(), `SELECT judge_model, codex_judge_model FROM users WHERE id=$1`, owner).Scan(&stored, &storedCodex); err != nil {
		t.Fatal(err)
	}
	if stored == nil || *stored != "sonnet" || storedCodex != nil {
		t.Fatalf("after clear stored = %v / %v", stored, storedCodex)
	}
	if rec := do(http.MethodPut, `{"codex_judge_model":"opus"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-curated Codex id: %d", rec.Code)
	}
}
