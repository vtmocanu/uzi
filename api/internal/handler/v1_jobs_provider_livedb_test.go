package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
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
	var res struct {
		Result struct {
			Files        []apitypes.V1JobFileDTO        `json:"files"`
			RefusedFiles []apitypes.V1JobRefusedFileDTO `json:"refused_files"`
		} `json:"result"`
	}
	r.decode(t, &res)
	if len(res.Result.Files) != 1 || res.Result.Files[0].DisplayName != "clean.txt" || len(res.Result.RefusedFiles) != 1 || res.Result.RefusedFiles[0].Reason != "worker_unreadable" {
		t.Fatalf("result files/refused = %+v / %+v, want the clean sibling and the worker_unreadable refusal", res.Result.Files, res.Result.RefusedFiles)
	}
}
