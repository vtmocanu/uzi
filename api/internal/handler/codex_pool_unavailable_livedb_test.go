package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// codex_pool_unavailable_livedb_test.go pins issue #3719 on the two derived-run handlers that
// inherit the source run's harness: a Codex source run whose owner runs codex_account_mode
// 'auto' with a configured pool in which no alias is linked is refused with 422, the stable
// no_credential_for_harness code and the typed codex_pool_unavailable reason. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// codexPoolUnavailableRig turns the rework rig's run into a Codex run whose owner pooled one
// UNLINKED codex_auth alias (so the pool is configured but nothing can be bound).
func codexPoolUnavailableRig(t *testing.T) *reworkRig {
	t.Helper()
	rig := newReworkRig(t, &settingsStore{}, 0)
	cliMustExec(t, rig.pool, `UPDATE runs SET harness = 'codex' WHERE id = $1`, rig.runID)
	cliMustExec(t, rig.pool, `UPDATE users SET codex_account_mode = 'auto' WHERE id = $1`, rig.user.ID)
	cliMustExec(t, rig.pool, `INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with, auto_eligible)
		VALUES ($1, 'codex_auth', 'pooled-unlinked', false, $2, 'master', true)`, rig.user.ID, []byte{3})
	return rig
}

func assertPoolUnavailable422(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if body["code"] != "no_credential_for_harness" || body["reason"] != "codex_pool_unavailable" {
		t.Fatalf("body = %v, want code no_credential_for_harness and reason codex_pool_unavailable", body)
	}
}

// MUTATION: delete the ErrCodexPoolUnavailable arm in StartRunRework; the error then falls to
// the generic no_credential_for_harness arm and the reason assertion fails.
func TestStartRunReworkCodexPoolUnavailableLiveDB(t *testing.T) {
	rig := codexPoolUnavailableRig(t)
	rig.f.answers[11] = forge.AuthorEligible
	rig.f.comments = []forge.MRComment{rcomment(121, 11, "please rename the helper")}

	assertPoolUnavailable422(t, rig.post(t, `{}`))
	if n := rig.reworkRuns(t); n != 0 {
		t.Fatalf("rework runs = %d, want 0", n)
	}
}

// MUTATION: delete the ErrCodexPoolUnavailable arm in RerunJudge; same failure.
func TestRerunJudgeCodexPoolUnavailableLiveDB(t *testing.T) {
	rig := codexPoolUnavailableRig(t)
	rig.h.wsvc.SetSettings(judgeSwitch{enabled: true})

	rec := httptest.NewRecorder()
	rig.h.RerunJudge(rec, rerunReq(rig.user, rig.runID))
	assertPoolUnavailable422(t, rec)
	if n := rig.count(t, `SELECT count(*) FROM runs WHERE kind = 'judge' AND target_run_id = $1`, rig.runID); n != 0 {
		t.Fatalf("judge runs = %d, want 0", n)
	}
}
