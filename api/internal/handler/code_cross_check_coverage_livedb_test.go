package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestCodeCrossCheckCoverageRouteLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	worker := uuid.New()
	token := "coverage-worker-" + uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,max_cross_check_slots,protocol_capabilities) VALUES($1,$2,'coverage',$3,'online',1,ARRAY['cross_check_code_v1','cross_check_lane_v1','codex_harness_v1'])", worker, owner, hash[:])
	conn := rmSeedConn(t, pool, owner)
	project := int64(35800)
	newPair := func(stage string) (uuid.UUID, uuid.UUID) {
		project++
		repo := rmSeedRepo(t, pool, conn, project, true)
		lead := rmSeedRun(t, pool, owner, repo, "running")
		cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,harness='claude',auto_approve=false,code_cross_check_required=true,claim_generation=1 WHERE id=$1", lead, worker)
		child := uuid.New()
		cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,target_run_id,harness,status,claim_generation,report_only,budget_wall_seconds,issue_title,issue_description)
 VALUES($1,$2,$3,$4,'cross_check',$5,'codex','running',1,true,1800,'coverage','candidate')`, child, owner, repo, worker, lead)
		if stage == "code" {
			cliMustExec(t, pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,checker_harness,stage,round,lead_claim_generation,base_commit,head_commit,candidate_digest,verdict,outcome,code_context,guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,deadline_at)
 VALUES($1,$2,'codex','code',1,1,$3,$4,$5,'failed','pending','{}','','',sha256(convert_to('','UTF8')),'',sha256(convert_to('','UTF8')),now()+interval '30 minutes')`, lead, child, strings.Repeat("a", 40), strings.Repeat("b", 40), make([]byte, 32))
		} else {
			cliMustExec(t, pool, `INSERT INTO cross_checks(lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,planning_diff,candidate_digest,deadline_at)
 VALUES($1,$2,'plan',1,1,'plan','[]','s',$3,'',$4,now()+interval '30 minutes')`, lead, child, strings.Repeat("a", 40), make([]byte, 32))
		}
		return lead, child
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	valid := `{"version":1,"entries":[{"path":"a.go","status":"examined","reason":"checked","recorded_read":true}],"inventory_incomplete":false,"omitted_paths":0,"omitted_count_exact":true,"unknown_judgments":0,"invalid_judgments":0,"omitted_judgments":0,"activity_complete":true,"reasons":[]}`
	for _, tc := range []struct {
		name, coverage string
		incomplete     bool
	}{
		{"absent", "", false}, {"null", "null", false}, {"partial", `{"version":1}`, true},
		{"scalar", `"malformed account"`, true}, {"invalid entries", strings.Replace(valid, `"examined"`, `"invented"`, 1), true},
		{"over coverage cap", `"` + strings.Repeat("x", 128<<10) + `"`, true},
		{"nul path", strings.Replace(valid, `"a.go"`, `"a\u0000.go"`, 1), true},
		{"empty examined reason", strings.Replace(valid, `"checked"`, `""`, 1), true},
		{"unassessed recorded read", strings.Replace(valid, `"examined"`, `"unassessed"`, 1), true},
		{"excluded recorded read", strings.Replace(valid, `"examined"`, `"excluded"`, 1), true},
		{"valid", valid, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lead, child := newPair("code")
			body := `{"claim_generation":1,"outcome":"completed","findings":[{"id":"retain","severity":"major","title":"guard"}]`
			if tc.coverage != "" {
				body += `,"coverage":` + tc.coverage
			}
			body += "}"
			// Code envelopes larger than the old plan bound must retain findings.
			if tc.name == "valid" {
				body += strings.Repeat(" ", 64<<10)
			}
			rec := call(http.MethodPost, "/api/worker/runs/"+child.String()+"/cross-check-verdict", body)
			if rec.Code != 200 {
				t.Fatalf("verdict: %d %s", rec.Code, rec.Body.String())
			}
			stored, err := h.q.GetCodeCrossCheck(t.Context(), lead)
			if err != nil || !bytes.Contains(stored.Findings, []byte("retain")) {
				t.Fatalf("coverage lost findings: %v", err)
			}
			if tc.coverage == "" || tc.coverage == "null" {
				if len(stored.Coverage) != 0 {
					t.Fatal("legacy coverage invented")
				}
			} else {
				var got apitypes.CodeCrossCheckCoverage
				if json.Unmarshal(stored.Coverage, &got) != nil || got.InventoryIncomplete != tc.incomplete {
					t.Fatalf("account: %s", stored.Coverage)
				}
				if tc.name == "valid" {
					var expected apitypes.CodeCrossCheckCoverage
					if json.Unmarshal([]byte(valid), &expected) != nil || !reflect.DeepEqual(got, expected) {
						t.Fatal("persisted account differs from independently specified input")
					}
				}
			}
			rec = call(http.MethodGet, "/api/worker/runs/"+lead.String()+"/cross-checks/code/latest?claim_generation=1", "")
			var got apitypes.CodeCrossCheck
			if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
				t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
			}
			if len(stored.Coverage) == 0 {
				if string(got.Coverage) != "null" {
					t.Fatal("legacy status not null")
				}
			} else {
				var compact bytes.Buffer
				if json.Compact(&compact, stored.Coverage) != nil || !bytes.Equal(got.Coverage, compact.Bytes()) {
					t.Fatal("status lost coverage")
				}
			}
		})
	}
	t.Run("strict failures", func(t *testing.T) {
		_, child := newPair("code")
		path := "/api/worker/runs/" + child.String() + "/cross-check-verdict"
		for _, body := range []string{
			`{"claim_generation":1,"outcome":"completed","findings":[],"coverage":{`,
			`{"claim_generation":1,"outcome":"completed","findings":{},"coverage":null}`,
			`{"claim_generation":1,"outcome":"completed","findings":[{"id":"bad id","severity":"major"}],"coverage":null}`,
			`{"claim_generation":1,"outcome":"completed","findings":[],"Coverage":null}`,
			`{"claim_generation":1,"outcome":"completed","findings":[],"coverage":null}` + strings.Repeat(" ", 256<<10),
		} {
			rec := call(http.MethodPost, path, body)
			if rec.Code != 400 && rec.Code != 409 {
				t.Fatalf("invalid envelope admitted: %d", rec.Code)
			}
		}
	})
	t.Run("plan isolation and body cap", func(t *testing.T) {
		lead, child := newPair("plan")
		path := "/api/worker/runs/" + child.String() + "/cross-check-verdict"
		for _, body := range []string{
			`{"claim_generation":1,"verdict":"approve","reason_class":"approve","items":[],"coverage":null}`,
			`{"claim_generation":1,"coverage":null}`,
			`{"claim_generation":1,"verdict":"approve","reason_class":"approve","items":[]}` + strings.Repeat(" ", 64<<10),
		} {
			rec := call(http.MethodPost, path, body)
			if rec.Code != 400 {
				t.Fatalf("plan accepted code field or oversized body: %d %s", rec.Code, rec.Body.String())
			}
		}
		var isNull bool
		if err := pool.QueryRow(t.Context(), "SELECT coverage IS NULL FROM cross_checks WHERE lead_run_id=$1", lead).Scan(&isNull); err != nil || !isNull {
			t.Fatalf("plan coverage: %v", err)
		}
	})
}
