package handler

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

func TestPatchRepoCodeCrossCheckGuidanceLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	stranger := cliSeedUser(t, pool, false)
	admin := cliSeedUser(t, pool, true)
	repo := cliSeedOwnedRepo(t, pool, owner)
	path := "/api/repos/" + repo.String()
	call := func(user uuid.UUID, body string, csrf bool, length int64) *httptest.ResponseRecorder {
		jwt := cliMintJWT(t, pool, user)
		req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
		req.ContentLength = length
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // Test request cookie.
		if csrf {
			req.Header.Set("X-CSRF-Token", cliCSRFHeader(t, jwt))
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(`{"cross_check_repo_instructions_enabled":true}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("bearer consent write: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(owner, `{"cross_check_repo_instructions_enabled":true}`, false, -1); rec.Code != 403 {
		t.Fatalf("missing CSRF: %d", rec.Code)
	}
	if rec := call(stranger, `{"code_cross_check_guidance":"foreign","cross_check_repo_instructions_enabled":true}`, true, -1); rec.Code != 404 {
		t.Fatalf("foreign write: %d", rec.Code)
	}
	assertStored := func(guidance *string, enabled bool) {
		t.Helper()
		var got *string
		var on bool
		if err := pool.QueryRow(t.Context(), "SELECT code_cross_check_guidance,cross_check_repo_instructions_enabled FROM repos WHERE id=$1", repo).Scan(&got, &on); err != nil {
			t.Fatal(err)
		}
		if on != enabled || (got == nil) != (guidance == nil) || (got != nil && *got != *guidance) {
			t.Fatalf("stored guidance=%v enabled=%t", got, on)
		}
	}
	guidance := strings.Repeat("é", 4096)
	body, _ := json.Marshal(map[string]any{"code_cross_check_guidance": guidance, "cross_check_repo_instructions_enabled": true})
	rec = call(owner, string(body), true, 1)
	if rec.Code != 200 {
		t.Fatalf("8192 bytes with inaccurate ContentLength: %d %s", rec.Code, rec.Body.String())
	}
	dto := decodeRepoDTO(t, rec)
	if dto.CodeCrossCheckGuidance == nil || *dto.CodeCrossCheckGuidance != guidance || !dto.CrossCheckRepoInstructionsEnabled {
		t.Fatalf("repo DTO: %+v", dto)
	}
	assertStored(&guidance, true)
	for _, other := range []string{`"repo_skills_enabled":true`, `"repo_claudemd_enabled":false`, `"repo_devbox_opt_in":false`, `"required_capabilities":[]`, `"repo_fold_improve_uzi_backlog":false`} {
		rec = call(owner, `{"code_cross_check_guidance":null,"cross_check_repo_instructions_enabled":false,`+other+`}`, true, -1)
		if rec.Code != 400 {
			t.Fatalf("mixed group: %d %s", rec.Code, rec.Body.String())
		}
		assertStored(&guidance, true)
	}
	tooLarge, _ := json.Marshal(map[string]any{"code_cross_check_guidance": guidance + "a", "cross_check_repo_instructions_enabled": false})
	for _, invalid := range []string{string(tooLarge), `{"code_cross_check_guidance":"` + string([]byte{255}) + `","cross_check_repo_instructions_enabled":false}`} {
		rec = call(owner, invalid, true, -1)
		if rec.Code != 400 {
			t.Fatalf("invalid bounds/UTF8: %d %s", rec.Code, rec.Body.String())
		}
		assertStored(&guidance, true)
	}
	rec = call(admin, `{"cross_check_repo_instructions_enabled":false}`, true, -1)
	if rec.Code != 200 {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body.String())
	}
	assertStored(&guidance, false)
	rec = call(owner, `{"code_cross_check_guidance":null,"cross_check_repo_instructions_enabled":true}`, true, -1)
	if rec.Code != 200 {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	assertStored(nil, true)
}

func TestCodeCrossCheckInstructionStagePayloadLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	repo := cliSeedOwnedRepo(t, pool, owner)
	lead := rmSeedRun(t, pool, owner, repo, "running")
	worker := uuid.New()
	token := "guidance-worker-" + uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,max_cross_check_slots,protocol_capabilities) VALUES($1,$2,'guidance',$3,'online',1,ARRAY['cross_check_code_v1','cross_check_lane_v1','codex_harness_v1'])", worker, owner, hash[:])
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,harness='claude',code_cross_check_required=true,claim_generation=1 WHERE id=$1", lead, worker)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	root := "/api/worker/runs/" + lead.String()
	for _, field := range []string{`"repo_instructions_enabled":false`, `"repo_instructions_text":""`} {
		for _, stage := range []string{`"stage":"plan","plan_md":"plan","base_commit":"` + strings.Repeat("a", 40) + `","size_class":"s","milestones":[]`, `"stage":"code","reason_class":"snapshot_failed"`} {
			rec := call(http.MethodPost, root+"/cross-checks", `{"claim_generation":1,`+stage+`,`+field+`}`)
			if rec.Code != 400 {
				t.Fatalf("stage payload: %d %s", rec.Code, rec.Body.String())
			}
		}
	}
	cliMustExec(t, pool, "UPDATE repos SET cross_check_repo_instructions_enabled=true WHERE id=$1", repo)
	rec := call(http.MethodGet, root+"/cross-checks/code/latest?claim_generation=1", "")
	if rec.Code != 200 {
		t.Fatalf("no-row settings: %d %s", rec.Code, rec.Body.String())
	}
	var noRow struct {
		Result  string `json:"result"`
		Enabled *bool  `json:"repo_instructions_enabled"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &noRow) != nil || noRow.Result != "no_row" || noRow.Enabled == nil || !*noRow.Enabled {
		t.Fatalf("settings body: %s", rec.Body.String())
	}
	rec = call(http.MethodGet, root+"/cross-checks/code/latest?claim_generation=2", "")
	if rec.Code != 409 {
		t.Fatalf("stale settings: %d %s", rec.Code, rec.Body.String())
	}
	rec = call(http.MethodPost, root+"/cross-checks", `{"stage":"code","claim_generation":1,"reason_class":"snapshot_failed"}`)
	if rec.Code != 200 {
		t.Fatalf("failure: %d %s", rec.Code, rec.Body.String())
	}
	rec = call(http.MethodGet, root+"/cross-checks/code/latest?claim_generation=1", "")
	var summary map[string]json.RawMessage
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &summary) != nil {
		t.Fatalf("summary: %d %s", rec.Code, rec.Body.String())
	}
	if string(summary["repo_instructions_enabled"]) != "false" || len(summary["repo_instructions_text"]) != 0 || len(summary["guidance_text"]) != 0 {
		t.Fatalf("failure authority/raw text: %s", rec.Body.String())
	}
}
