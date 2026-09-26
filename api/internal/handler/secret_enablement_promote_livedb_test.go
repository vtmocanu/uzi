package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// secret_enablement_promote_livedb_test.go pins the handler half of PRD #1732 D14: every
// committed secret mutation requests a credential_disabled promoter pass AFTER its commit,
// and enabling a pinned credential resumes the run that waited on it with no manual action.

// promotingSecretsHandler is secretsCRUDHandler with a real workersvc wired as main.go wires it,
// whose background dispatcher runs each requested pass synchronously and counts the requests.
func promotingSecretsHandler(t *testing.T) (*Handler, *pgxpool.Pool, *int) {
	t.Helper()
	h, pool := secretsCRUDHandler(t)
	wsvc := workersvc.New(store.New(pool), h.box, workersvc.Params{RunTimeout: 2 * time.Hour})
	wsvc.SetTxBeginner(pool)
	requests := new(int)
	wsvc.SetBackground(func(fn func()) { *requests++; fn() })
	h.wsvc = wsvc
	return h, pool, requests
}

// TestSecretMutationsRequestPromotionLiveDB: create (first credential and a second one),
// make-default, disable, enable, a default hand-off and delete each request exactly one pass;
// a refused mutation requests none.
func TestSecretMutationsRequestPromotionLiveDB(t *testing.T) {
	h, pool, requests := promotingSecretsHandler(t)
	user := mkSecretUser(t, pool)
	step := func(name string, want int, do func() int, wantCode int) {
		t.Helper()
		before := *requests
		if code := do(); code != wantCode {
			t.Fatalf("%s: code %d, want %d", name, code, wantCode)
		}
		if got := *requests - before; got != want {
			t.Fatalf("%s: promotion requests = %d, want %d", name, got, want)
		}
	}
	var a, b string
	step("create first", 1, func() int {
		rec := h.createToken(t, user, "a", "tok-a-value", false)
		a = decodeSecret(t, rec).Secret.ID
		return rec.Code
	}, http.StatusCreated)
	step("create second", 1, func() int {
		rec := h.createToken(t, user, "b", "tok-b-value", false)
		b = decodeSecret(t, rec).Secret.ID
		return rec.Code
	}, http.StatusCreated)
	step("make default", 1, func() int {
		rec := httptest.NewRecorder()
		h.PatchAnthropicToken(rec, userReq(http.MethodPatch, "/api/me/secrets/anthropic_token/"+b,
			`{"default":true}`, user, map[string]string{"id": b}))
		return rec.Code
	}, http.StatusOK)
	step("disable", 1, func() int {
		code, _ := enablementRequest(t, h, user, "anthropic_token", uuid.MustParse(a), `{"enabled":false}`)
		return code
	}, http.StatusOK)
	step("enable", 1, func() int {
		code, _ := enablementRequest(t, h, user, "anthropic_token", uuid.MustParse(a), `{"enabled":true}`)
		return code
	}, http.StatusOK)
	step("refused disable of the default", 0, func() int {
		code, _ := enablementRequest(t, h, user, "anthropic_token", uuid.MustParse(b), `{"enabled":false}`)
		return code
	}, http.StatusConflict)
	step("default hand-off", 1, func() int {
		code, _ := enablementRequest(t, h, user, "anthropic_token", uuid.MustParse(b), `{"enabled":false,"new_default_id":"`+a+`"}`)
		return code
	}, http.StatusOK)
	step("delete", 1, func() int {
		rec := httptest.NewRecorder()
		h.DeleteAnthropicTokenByID(rec, userReq(http.MethodDelete, "/api/me/secrets/anthropic_token/"+b, "", user, map[string]string{"id": b}))
		return rec.Code
	}, http.StatusNoContent)
}

// TestSecretEnablePromotesParkedRunLiveDB: a run held on credential_disabled by a worker pinned
// to a disabled token resumes when the owner enables that token, without any other action,
// and the held interval is banked rather than charged to its wall-clock budget.
func TestSecretEnablePromotesParkedRunLiveDB(t *testing.T) {
	h, pool, _ := promotingSecretsHandler(t)
	ctx := t.Context()
	user := mkSecretUser(t, pool)
	def, pin := uuid.New(), uuid.New()
	for i, id := range []uuid.UUID{def, pin} {
		if _, err := pool.Exec(ctx, `INSERT INTO user_secrets (id,user_id,kind,label,is_default,ciphertext,sealed_with)
		    VALUES ($1,$2,'anthropic_token',$3,$4,'x','master')`, id, user, []string{"def", "pin"}[i], i == 0); err != nil {
			t.Fatal(err)
		}
	}
	connID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		  VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, 'x')`, []any{connID, user}},
		{`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		  VALUES ($1, $2, 1, $3, $4, 'main', true)`, []any{repoID, connID, "g/" + repoID.String(), "https://forge.e2e/g/" + repoID.String()}},
		{`INSERT INTO workers (id, user_id, name, token_hash, status, anthropic_bind_mode, anthropic_secret_id)
		  VALUES ($1, $2, $3, $4, 'online', 'pinned', $5)`, []any{workerID, user, "w-" + workerID.String(), workerID[:], pin}},
		{`UPDATE user_secrets SET disabled_at = now(), enablement_rev = 1 WHERE id = $1`, []any{pin}},
		{`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, status_since,
		      hold_reason, worker_id, claim_released_at, credential_disable_released_worker_id, started_at, budget_wall_seconds)
		  VALUES ($1, $2, $3, 'issue', 1, 't', 'd', 'paused', now() - interval '10 minutes', 'credential_disabled', $4,
		      now() - interval '10 minutes', $4, now() - interval '15 minutes', 3600)`, []any{runID, user, repoID, workerID}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed %q: %v", stmt.sql, err)
		}
	}
	var status string
	var paused int32
	read := func() {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status, budget_paused_seconds FROM runs WHERE id = $1`, runID).Scan(&status, &paused); err != nil {
			t.Fatal(err)
		}
	}
	// Re-enabling a different credential leaves the pinned run held.
	if code, _ := enablementRequest(t, h, user, "anthropic_token", def, `{"enabled":true}`); code != http.StatusOK {
		t.Fatalf("no-op enable: %d", code)
	}
	if read(); status != "paused" {
		t.Fatalf("status = %s after an unrelated enable, want paused", status)
	}
	if code, _ := enablementRequest(t, h, user, "anthropic_token", pin, `{"enabled":true}`); code != http.StatusOK {
		t.Fatalf("enable pin: %d", code)
	}
	if read(); status != "queued" || paused < 600 || paused > 660 {
		t.Fatalf("after enable: status=%s paused=%d, want queued with the ~600s hold banked", status, paused)
	}
}
