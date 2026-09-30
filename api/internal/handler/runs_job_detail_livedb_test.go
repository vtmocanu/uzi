package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
)

// PRD #1908 D-D: GET /api/runs/{id} (the cookie run detail) carries a `job` block for a
// kind='job' run, under the existing run-read authorization (owner or admin), and only then.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestRunDetailJobBlockLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	owner, uzc := e.user()
	product := e.product(owner, "research")
	pTok := v1MintProductToken(t, e.h.q, owner, product, producttoken.Scopes, nil)
	var productName string
	if err := e.pool.QueryRow(t.Context(), `SELECT name FROM products WHERE id = $1`, product).Scan(&productName); err != nil {
		t.Fatal(err)
	}

	byProduct := e.create(pTok.token, `{"type":"research","title":"t","prompt":"p","requested_by_label":"end user",`+
		`"inputs":[{"name":"notes.md","content":"hello"},{"name":"big.txt","content":"`+strings.Repeat("z", 1000)+`"}]}`)
	byUser := e.create(uzc, v1MinimalJob)
	e.exec(`INSERT INTO job_results (run_id, status, report_md) VALUES ($1, 'completed', '# done')`, byProduct.ID)
	e.exec(`INSERT INTO job_findings (run_id, ordinal, severity, message_md, url) VALUES ($1, 0, 'warning', 'look', 'https://example.com/x')`, byProduct.ID)
	e.exec(`INSERT INTO job_findings (run_id, ordinal, severity, message_md, file, line) VALUES ($1, 1, 'error', 'here', 'a.go', 9)`, byProduct.ID)

	routes, _ := v1Routers(e.h)
	get := func(userID uuid.UUID, id string) (int, []byte) {
		rec := cookieReq(t, routes, http.MethodGet, "/api/runs/"+id, cliMintJWT(t, e.pool, userID), "")
		return rec.Code, rec.Body.Bytes()
	}

	t.Run("a product job carries type, inputs, origin and the result", func(t *testing.T) {
		code, body := get(owner, byProduct.ID)
		if code != http.StatusOK {
			t.Fatalf("status %d %s", code, body)
		}
		var wrapped struct {
			Run apitypes.RunDTO `json:"run"`
		}
		if err := json.Unmarshal(body, &wrapped); err != nil {
			t.Fatal(err)
		}
		j := wrapped.Run.Job
		if j == nil {
			t.Fatalf("no job block in %s", body)
		}
		if j.Type != "research" || j.Origin.ProductName == nil || *j.Origin.ProductName != productName ||
			j.Origin.RequestedByLabel == nil || *j.Origin.RequestedByLabel != "end user" {
			t.Errorf("job = %+v", j)
		}
		if len(j.Inputs) != 2 || j.Inputs[0].Name != "notes.md" || j.Inputs[0].SizeBytes != 5 || j.Inputs[1].SizeBytes != 1000 {
			t.Errorf("inputs = %+v, want names and sizes in order", j.Inputs)
		}
		if strings.Contains(string(body), "hello") {
			t.Error("the run detail carries input CONTENT; it must carry name and size only")
		}
		if j.Result == nil || j.Result.Status != "completed" || j.Result.ReportMd != "# done" || len(j.Result.Findings) != 2 {
			t.Fatalf("result = %+v", j.Result)
		}
		f0, f1 := j.Result.Findings[0], j.Result.Findings[1]
		if f0.Severity != "warning" || f0.URL == nil || f0.File != nil || f1.File == nil || *f1.File != "a.go" || f1.Line == nil || *f1.Line != 9 {
			t.Errorf("findings = %+v", j.Result.Findings)
		}
	})

	t.Run("a user-created job has no product and no result yet", func(t *testing.T) {
		code, body := get(owner, byUser.ID)
		if code != http.StatusOK {
			t.Fatalf("status %d %s", code, body)
		}
		var wrapped struct {
			Run apitypes.RunDTO `json:"run"`
		}
		if err := json.Unmarshal(body, &wrapped); err != nil {
			t.Fatal(err)
		}
		run := wrapped.Run
		if run.Job == nil || run.Job.Origin.ProductName != nil || run.Job.Origin.RequestedByLabel != nil || run.Job.Result != nil || len(run.Job.Inputs) != 0 {
			t.Fatalf("job = %+v, want no product, no label, no result and an empty (not null) inputs list", run.Job)
		}
		if !strings.Contains(string(body), `"inputs":[]`) || !strings.Contains(string(body), `"result":null`) {
			t.Errorf("wire form lacks inputs [] / result null: %s", body)
		}
	})

	t.Run("the run-read authorization is unchanged", func(t *testing.T) {
		other := cliSeedUser(t, e.pool, false)
		if code, _ := get(other, byProduct.ID); code != http.StatusNotFound {
			t.Errorf("another user's GET = %d, want 404", code)
		}
		admin := cliSeedUser(t, e.pool, true)
		code, body := get(admin, byProduct.ID)
		if code != http.StatusOK || !strings.Contains(string(body), `"job":{`) {
			t.Errorf("an admin's GET = %d, want 200 with the job block (the same visibility as the run): %s", code, body)
		}
	})

	t.Run("a run of another kind has no job key", func(t *testing.T) {
		chat := uuid.New()
		e.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status) VALUES ($1, $2, 'chat', 't', 'd', 'completed')`, chat, owner)
		code, body := get(owner, chat.String())
		if code != http.StatusOK {
			t.Fatalf("status %d %s", code, body)
		}
		if strings.Contains(string(body), `"job"`) {
			t.Errorf("a chat run's detail carries a job key: %s", body)
		}
	})
}
