package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// v1_jobs_provider_livedb_test.go is PRD #4322 M1b through the PRODUCTION /api/v1 router: a job whose
// owner selected a usable Claude provider source is created provider-bound (201), every other
// provider-source outcome is a typed 409 with its exact reason and no row, and a provider job's output
// refusals and clean sibling files read back through GET /files and /result. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// v1ProviderBase is the gateway URL the usable-provider helper seeds and v1LiveDBMax allowlists.
const v1ProviderBase = "https://gw.example.com/anthropic"

const (
	v1ProviderMap         = `{"opus":{"model":"corp-opus"},"sonnet":{"model":"corp-sonnet"},"haiku":{"model":"corp-haiku"}}`
	v1ProviderMapNoSonnet = `{"opus":{"model":"corp-opus"},"haiku":{"model":"corp-haiku"}}`
)

// selectUsableProvider seeds a personal provider (revision 1 at v1ProviderBase with the given model
// map) and an enabled key, and selects it as the user's Claude source.
func (e *v1JobsEnv) selectUsableProvider(user uuid.UUID, modelMap string) (provider, key uuid.UUID) {
	e.t.Helper()
	return e.selectProviderAt(user, v1ProviderBase, modelMap)
}

func (e *v1JobsEnv) selectProviderAt(user uuid.UUID, base, modelMap string) (provider, key uuid.UUID) {
	e.t.Helper()
	ctx := context.Background()
	p, err := e.h.q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: user, Name: "p-" + uuid.NewString()[:8], Engine: "claude"})
	if err != nil {
		e.t.Fatalf("insert provider: %v", err)
	}
	if err := e.h.q.InsertModelProviderRevision(ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "anthropic_messages", BaseUrl: base, AuthStyle: "bearer", ModelMap: []byte(modelMap),
	}); err != nil {
		e.t.Fatalf("insert revision: %v", err)
	}
	k, err := e.h.q.InsertProviderKeySecret(ctx, store.InsertProviderKeySecretParams{
		UserID: user, Label: p.ID.String(), Ciphertext: []byte("ct"), SealedWith: "master", ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true},
	})
	if err != nil {
		e.t.Fatalf("insert key: %v", err)
	}
	e.exec(`UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, user, p.ID)
	// Registered after the user's cleanup, so it runs first: a selected provider cannot be deleted
	// (RESTRICT).
	e.t.Cleanup(func() {
		if _, err := e.pool.Exec(ctx, `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, user); err != nil {
			e.t.Errorf("clear the Claude source in cleanup: %v", err)
		}
	})
	return p.ID, k.ID
}

func (e *v1JobsEnv) jobOriginCount(user uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM job_origins o JOIN runs r ON r.id = o.run_id WHERE r.user_id = $1`, user).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// TestV1JobsProviderSourceLiveDB: every reachable provider-source outcome of POST /api/v1/jobs.
func TestV1JobsProviderSourceLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.enableLane(true) // the site-list case needs the isolated lane enabled (issue #1965)

	t.Run("a usable source: 201 and the job is frozen to the source with no vendor credential", func(t *testing.T) {
		user, tok := e.user()
		provider, key := e.selectUsableProvider(user, v1ProviderMap)
		job := e.create(tok, v1MinimalJob)
		var gotProvider, gotKey, anthropic pgtype.UUID
		var mode pgtype.Text
		var harness string
		if err := e.pool.QueryRow(context.Background(),
			`SELECT model_provider_id, model_provider_key_secret_id, model_provider_key_mode, anthropic_secret_id, harness FROM runs WHERE id = $1`,
			job.ID).Scan(&gotProvider, &gotKey, &mode, &anthropic, &harness); err != nil {
			t.Fatal(err)
		}
		if uuid.UUID(gotProvider.Bytes) != provider || uuid.UUID(gotKey.Bytes) != key || mode.String != "user_key" || anthropic.Valid || harness != "claude" {
			t.Fatalf("job binding = provider %v key %v mode %v anthropic %v harness %s, want provider %s key %s user_key and no vendor credential",
				gotProvider, gotKey, mode, anthropic, harness, provider, key)
		}
	})

	refusals := []struct {
		name   string
		setup  func(user uuid.UUID)
		body   func() string
		reason string
		msg    string
	}{
		{"a site-list job under a usable source", func(u uuid.UUID) { e.selectUsableProvider(u, v1ProviderMap) },
			func() string {
				return fmt.Sprintf(`{"type":"research","prompt":"p","egress_profile":%q}`, e.siteList())
			},
			"provider_lane_unsupported", "this run type does not support a model-provider source yet"},
		{"a source with no revision", func(u uuid.UUID) { e.selectProvider(u) }, func() string { return v1MinimalJob },
			"provider_source_unusable", ""},
		{"a base url off the operator allowlist", func(u uuid.UUID) { e.selectProviderAt(u, "https://other.example.com/x", v1ProviderMap) }, func() string { return v1MinimalJob },
			"provider_source_unusable", ""},
		{"a model map without the default sonnet alias", func(u uuid.UUID) { e.selectUsableProvider(u, v1ProviderMapNoSonnet) }, func() string { return v1MinimalJob },
			"provider_model_unmapped", `"sonnet" (job)`},
	}
	for _, tc := range refusals {
		t.Run(tc.name+": typed 409 and no row", func(t *testing.T) {
			user, tok := e.user()
			tc.setup(user)
			r := e.call(http.MethodPost, "/api/v1/jobs", tok, tc.body())
			e.want(r, http.StatusConflict, tc.reason)
			if tc.msg != "" && !strings.Contains(string(r.body), strings.ReplaceAll(tc.msg, `"`, `\"`)) {
				t.Errorf("body = %s, want it to contain %s", r.body, tc.msg)
			}
			if n := e.jobCount(tok); n != 0 {
				t.Fatalf("job runs = %d, want 0", n)
			}
			if n := e.jobOriginCount(user); n != 0 {
				t.Fatalf("job origins = %d, want 0", n)
			}
		})
	}
}

// TestV1ProviderJobOutputReadsLiveDB: for a provider-bound job, a clean sibling output and a refusal row
// for a withheld file (the shape the worker's content scan publishes) read back through GET /files
// (files and refused_files) and GET /result, and neither response carries the withheld file's content.
func TestV1ProviderJobOutputReadsLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	jf := e.wireFiles(workersvc.JobFileLimits{InputFileMaxBytes: 1000, PerOwnerBytes: 100000})
	owner, tok := e.user()
	e.selectUsableProvider(owner, v1ProviderMap)
	job := e.create(tok, v1MinimalJob)
	clean := e.storeOutput(jf, owner, job.ID, "clean.txt", []byte("a clean sibling"))
	e.exec(`INSERT INTO job_results (run_id, status, report_md) VALUES ($1, 'completed', '# done')`, job.ID)
	e.exec(`INSERT INTO job_output_refusals (run_id, display_name, byte_size, reason) VALUES ($1, 'withheld.txt', 17, 'worker_unreadable')`, job.ID)
	e.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, job.ID)

	files := e.jobFiles(tok, job.ID)
	if len(files.Files) != 1 || files.Files[0].DisplayName != "clean.txt" || files.Files[0].ID != clean.ID.String() {
		t.Fatalf("files = %+v, want exactly the clean sibling", files.Files)
	}
	if len(files.RefusedFiles) != 1 || files.RefusedFiles[0] != (apitypes.V1JobRefusedFileDTO{DisplayName: "withheld.txt", ByteSize: 17, Reason: "worker_unreadable"}) {
		t.Fatalf("refused_files = %+v, want the withheld file with reason worker_unreadable", files.RefusedFiles)
	}
	r := e.call(http.MethodGet, "/api/v1/jobs/"+job.ID+"/result", tok, "")
	if r.status != http.StatusOK {
		t.Fatalf("GET result: %d %s", r.status, r.body)
	}
	var res apitypes.V1JobResultDTO
	r.decode(t, &res)
	if res.JobStatus != "completed" || res.Result == nil {
		t.Fatalf("result = %+v, want a completed job with a result", res)
	}
	if res.Result.Status != "completed" || res.Result.ReportMd != "# done" || len(res.Result.Findings) != 0 {
		t.Fatalf("result body = %+v, want the clean status, report and no findings", res.Result)
	}
	if len(res.Result.Files) != 1 || res.Result.Files[0].DisplayName != "clean.txt" || len(res.Result.RefusedFiles) != 1 || res.Result.RefusedFiles[0].Reason != "worker_unreadable" {
		t.Fatalf("result files/refused = %+v / %+v, want the clean sibling and the worker_unreadable refusal", res.Result.Files, res.Result.RefusedFiles)
	}
	cleanBytes := []byte("a clean sibling")
	if res.Result.Files[0].Sha256 != sha256Hex(cleanBytes) {
		t.Fatalf("clean sibling sha256 = %s, want %s", res.Result.Files[0].Sha256, sha256Hex(cleanBytes))
	}
	if dl := e.call(http.MethodGet, "/api/v1/files/"+clean.ID.String(), tok, ""); dl.status != http.StatusOK || string(dl.body) != string(cleanBytes) {
		t.Fatalf("download clean sibling: %d %q", dl.status, dl.body)
	}
}

// TestV1ProviderJobWorkerResultReadsLiveDB is the same read-back as TestV1ProviderJobOutputReadsLiveDB, but
// every output is ingested the way a worker does it: the clean sibling is uploaded through
// POST /api/worker/runs/{id}/files, and the withheld output, the finding and the report arrive in one
// POST /api/worker/runs/{id}/job-result carrying refused_outputs, then the run is completed through
// POST /api/worker/runs/{id}/state. The job itself is created through POST /api/v1/jobs (provider-bound);
// only the claim is seeded (the run is set running under a worker at claim generation 1), because the
// claim machinery is not what is under test.
//
// MUTATION CHECK: dropping the refused_outputs ingest loop (SubmitJobResult) makes the refused_files
// assertions red; dropping the findings ingest makes the finding assertion red.
func TestV1ProviderJobWorkerResultReadsLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{InputFileMaxBytes: 1000, PerOwnerBytes: 100000})
	owner, tok := e.user()
	e.selectUsableProvider(owner, v1ProviderMap)
	job := e.create(tok, v1MinimalJob)

	workerID := uuid.New()
	workerToken, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO workers (id,user_id,name,token_hash,protocol_capabilities) VALUES ($1,$2,$3,$4,'{}')`, workerID, owner, "pjob-result-"+workerID.String(), hash)
	e.exec(`UPDATE runs SET status = 'running', worker_id = $2, started_at = now(), claimed_at = now(), claim_generation = 1 WHERE id = $1`, job.ID, workerID)
	t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), `DELETE FROM runs WHERE id = $1`, job.ID) })
	router := e.h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))
	workerPost := func(path, meta, body string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+job.ID+path, strings.NewReader(body))
		if meta != "" {
			req.Header.Set(jobFileMetaHeader, meta)
		}
		req.Header.Set("Authorization", "Bearer "+workerToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	cleanBytes := []byte("a clean sibling")
	if code, out := workerPost("/files", outMeta(1, "clean.txt", len(cleanBytes), shaHex(cleanBytes)), string(cleanBytes)); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("worker upload of the clean sibling = %d %s", code, out)
	}
	result := `{"claim_generation":1,"status":"completed","report_md":"# done",` +
		`"findings":[{"severity":"warning","message_md":"withheld.txt held a credential-shaped value","file":"withheld.txt","line":3}],` +
		`"refused_outputs":[{"display_name":"withheld.txt","reason":"worker_unreadable"}]}`
	if code, out := workerPost("/job-result", "", result); code != http.StatusOK {
		t.Fatalf("worker job-result = %d %s", code, out)
	}
	if code, out := workerPost("/state", "", `{"status":"completed","claim_generation":1}`); code != http.StatusOK {
		t.Fatalf("worker completed state = %d %s", code, out)
	}

	files := e.jobFiles(tok, job.ID)
	byName := v1FilesByName(files.Files)
	clean, ok := byName["clean.txt"]
	if !ok {
		t.Fatalf("files = %+v, want the uploaded clean sibling listed", files.Files)
	}
	if _, leaked := byName["withheld.txt"]; leaked {
		t.Fatalf("files = %+v, the withheld output must not be listed as a file", files.Files)
	}
	if len(files.RefusedFiles) != 1 || files.RefusedFiles[0].DisplayName != "withheld.txt" || files.RefusedFiles[0].Reason != "worker_unreadable" {
		t.Fatalf("refused_files = %+v, want exactly withheld.txt with reason worker_unreadable", files.RefusedFiles)
	}
	r := e.call(http.MethodGet, "/api/v1/jobs/"+job.ID+"/result", tok, "")
	if r.status != http.StatusOK {
		t.Fatalf("GET result: %d %s", r.status, r.body)
	}
	var res apitypes.V1JobResultDTO
	r.decode(t, &res)
	if res.JobStatus != "completed" || res.Result == nil || res.Result.Status != "completed" || res.Result.ReportMd != "# done" {
		t.Fatalf("result = %+v, want a completed job with the worker's report", res)
	}
	if len(res.Result.Findings) != 1 || res.Result.Findings[0].Severity != "warning" || res.Result.Findings[0].File == nil || *res.Result.Findings[0].File != "withheld.txt" {
		t.Fatalf("findings = %+v, want the worker's finding", res.Result.Findings)
	}
	if len(res.Result.RefusedFiles) != 1 || res.Result.RefusedFiles[0].Reason != "worker_unreadable" {
		t.Fatalf("result refused_files = %+v, want the worker_unreadable refusal", res.Result.RefusedFiles)
	}
	if got := v1FilesByName(res.Result.Files)["clean.txt"]; got.ID != clean.ID || got.Sha256 != sha256Hex(cleanBytes) {
		t.Fatalf("result clean sibling = %+v, want id %s with sha256 %s", got, clean.ID, sha256Hex(cleanBytes))
	}
	if dl := e.call(http.MethodGet, "/api/v1/files/"+clean.ID, tok, ""); dl.status != http.StatusOK || string(dl.body) != string(cleanBytes) {
		t.Fatalf("download clean sibling: %d %q", dl.status, dl.body)
	}
}
