package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type publicationContextRouteForge struct {
	settleFakeForge
	barrier func()
}

func (f *publicationContextRouteForge) GetMergeRequestSummary(context.Context, int64, int64) (forge.MergeRequestSummary, error) {
	if f.barrier != nil {
		f.barrier()
	}
	return forge.MergeRequestSummary{IID: 17, State: forge.MRStateOpened, SourceBranch: "agent/issue-7", HeadSHA: strings.Repeat("b", 40)}, nil
}

func TestPublicationContextHandlerLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	h.wsvc.SetBackground(func(func()) {})
	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 2507, true)
	source := rmSeedRun(t, pool, owner, repo, "running")
	worker := uuid.New()
	token := "publication-worker-" + uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'publication',$3,'online')", worker, owner, sum[:])
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,claim_generation=1,issue_iid=7 WHERE id=$1", source, worker)
	call := func(path, auth, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+auth)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec
	}
	head := strings.Repeat("a", 40)
	body := `{"status":"completed","claim_generation":1,"branch":"agent/issue-7","mr_iid":17,"source_publication":{"version":1,"published_head":"` + head + `","non_closing":false}}`
	if rec := call(source.String()+"/state", token, body); rec.Code != 200 {
		t.Fatalf("terminal handler: %d %s", rec.Code, rec.Body.String())
	}
	var stamp []byte
	if err := pool.QueryRow(context.Background(), "SELECT source_publication_intent FROM runs WHERE id=$1", source).Scan(&stamp); err != nil || len(stamp) == 0 {
		t.Fatalf("handler stamp missing: %s %v", stamp, err)
	}
	child := uuid.New()
	cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,status,worker_id,claim_generation,pipeline_ref,mr_iid,target_run_id,issue_title,issue_description)
 VALUES($1,$2,$3,'mr_rework','running',$4,1,'agent/issue-7',17,$5,'rework','')`, child, owner, repo, worker, source)
	f := &publicationContextRouteForge{}
	h.wsvc.SetForges(settleForgeBuilder{f: f})
	path := child.String() + "/publication-context"
	request := `{"claim_generation":1,"branch":"agent/issue-7","candidate_head":"` + strings.Repeat("c", 40) + `"}`
	for _, tc := range []struct {
		name, auth, body string
		code             int
	}{
		{"auth", "wrong", request, 401},
		{"unknown_field", token, strings.TrimSuffix(request, "}") + `,"extra":1}`, 400},
		{"trailing", token, request + " {}", 400},
		{"oversize", token, request + strings.Repeat(" ", 4096), 413},
		{"unsafe_generation", token, strings.Replace(request, `"claim_generation":1`, `"claim_generation":9007199254740992`, 1), 400},
		{"valid", token, request, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := call(path, tc.auth, tc.body)
			if rec.Code != tc.code {
				t.Fatalf("HTTP %d want %d: %s", rec.Code, tc.code, rec.Body.String())
			}
			if tc.name == "valid" {
				var result workersvc.PublicationContextResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Status != "resolved" || result.Binding == nil ||
					result.Binding.Intent != "closing" || result.Binding.SourceRunID != source || result.Binding.SourcePublication == nil ||
					result.Binding.CandidateHead != strings.Repeat("c", 40) {
					t.Fatalf("handler/query binding: %s %v", rec.Body.String(), err)
				}
			}
		})
	}
	f.barrier = func() {
		cliMustExec(t, pool, "UPDATE runs SET completion_non_closing_reason='partial' WHERE id=$1", source)
	}
	rec := call(path, token, request)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"reason":"authority_changed"`) {
		t.Fatalf("forge barrier: %d %s", rec.Code, rec.Body.String())
	}
}
