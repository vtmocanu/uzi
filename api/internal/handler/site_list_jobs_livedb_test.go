package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/fetchctl"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/hub"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// site_list_jobs_livedb_test.go pins PRD #1976 M1 through the PRODUCTION routers: the admin
// product allowance API (its auth split) and a profile-bound job from create to result on a lane
// worker. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh.

type siteListEnv struct {
	t      *testing.T
	h      *Handler
	pool   *pgxpool.Pool
	routes http.Handler
	v1     *v1JobsEnv
	svcTok string
}

func newSiteListEnv(t *testing.T) *siteListEnv {
	t.Helper()
	h, pool := v1LiveDBMax(t, 0)
	// A service the lane flow can claim through: real run budgets, the transaction source
	// isolateClaim mints its credential with, and a job-file store.
	svc := workersvc.New(h.q, h.box, workersvc.Params{
		RunTimeout: 2 * time.Hour, RunWallCeiling: 8 * time.Hour, RunIdleTimeout: 10 * time.Minute, RunMaxIterations: 5, PlanMaxRevisions: 3,
		QuestionMax: 5, QuestionTimeoutSeconds: 86400, RunMaxRequeues: 1,
		WorkerHeartbeatStale: 45 * time.Second, WorkerHeartbeatInterval: 15 * time.Second,
		TerminalPendingLease: time.Hour, ActiveSnapshotMaxEntries: 256, WorkerOutboxMaxPending: 32,
		WorkerGapFillMax: 10000, WorkerAffinityGrace: 2 * time.Minute,
	})
	svc.SetTxBeginner(pool)
	svc.SetJobFiles(workersvc.NewJobFiles(pool, h.box, workersvc.JobFileLimits{}, nil))
	svcTok := "fetcher-svc-" + uuid.NewString()
	sum := sha256.Sum256([]byte(svcTok))
	cfg := h.cfg
	cfg.FetcherTokenSHA256 = sum[:]
	h = New(pool, h.q, cfg, h.box, nil, svc, nil, hub.New(), settings.New(h.q, time.Minute))
	h.SetHostedSvc(hostedsvc.New(h.q, h.box, time.Now, cfg.WorkerHeartbeatStale))
	lim := func() *mw.Limiter { return mw.NewLimiter(1_000_000, time.Hour, nil) }
	routes := h.Routes(lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim())
	return &siteListEnv{
		t: t, h: h, pool: pool, routes: routes, svcTok: svcTok,
		v1: &v1JobsEnv{t: t, h: h, pool: pool, routes: routes},
	}
}

// profile seeds a site list and removes the runs bound to it, then the list, at the end.
func (e *siteListEnv) profile(hosts string) (id uuid.UUID, name string) {
	e.t.Helper()
	id = uuid.New()
	name = "sl-" + strings.ReplaceAll(id.String(), "-", "")[:20]
	cliMustExec(e.t, e.pool, `INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, string_to_array($3, ','))`, id, name, hosts)
	e.t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM runs WHERE egress_profile_id = $1`, id)
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM egress_profiles WHERE id = $1`, id)
	})
	return id, name
}

// admin is an admin user plus its session JWT (the cookie-only write path).
func (e *siteListEnv) admin() (uuid.UUID, string) {
	e.t.Helper()
	u := cliSeedUser(e.t, e.pool, true)
	return u, cliMintJWT(e.t, e.pool, u)
}

func (e *siteListEnv) cookie(method, path, jwt string) *httptest.ResponseRecorder {
	e.t.Helper()
	return cookieReq(e.t, e.routes, method, path, jwt, "")
}

func allowancePath(product uuid.UUID, name string) string {
	return "/api/admin/products/" + product.String() + "/egress-profiles/" + name
}

type allowanceList struct {
	EgressProfiles []struct {
		Name        string    `json:"name"`
		Description string    `json:"description"`
		CreatedAt   time.Time `json:"created_at"`
	} `json:"egress_profiles"`
}

func listNames(t *testing.T, body []byte) []string {
	t.Helper()
	var l allowanceList
	if err := json.Unmarshal(body, &l); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	out := []string{}
	for _, p := range l.EgressProfiles {
		out = append(out, p.Name)
	}
	return out
}

// TestAdminProductEgressProfilesLiveDB: the allowance API's auth split and behaviour through the
// real router. Reads take a session or a uza_ token; PUT and DELETE are cookie-only admin writes.
func TestAdminProductEgressProfilesLiveDB(t *testing.T) {
	e := newSiteListEnv(t)
	admin, jwt := e.admin()
	nonAdmin := cliSeedUser(t, e.pool, false)
	nonAdminJWT := cliMintJWT(t, e.pool, nonAdmin)
	uza := cliMintToken(t, e.pool, admin, clitoken.ScopeAdminRO)
	product := v1SeedProduct(t, e.h.q, admin)
	_, nameA := e.profile("docs.example.com")
	_, nameB := e.profile("other.example.com")
	list := "/api/admin/products/" + product.String() + "/egress-profiles"

	t.Run("a uza_ Bearer cannot write", func(t *testing.T) {
		for _, m := range []string{http.MethodPut, http.MethodDelete} {
			if r := v1Call(e.routes, m, allowancePath(product, nameA), uza, ""); r.status != http.StatusUnauthorized {
				t.Errorf("uza_ %s = %d, want 401", m, r.status)
			}
		}
		if names := e.listAs(t, jwt, list); len(names) != 0 {
			t.Fatalf("a refused write changed the allowance: %v", names)
		}
	})

	t.Run("a non-admin session is refused on read and write", func(t *testing.T) {
		for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			path := list
			if m != http.MethodGet {
				path = allowancePath(product, nameA)
			}
			if rec := e.cookie(m, path, nonAdminJWT); rec.Code != http.StatusForbidden {
				t.Errorf("non-admin %s = %d, want 403", m, rec.Code)
			}
		}
	})

	t.Run("an admin session allows, lists and revokes; a uza_ token lists", func(t *testing.T) {
		for range 2 { // PUT is idempotent
			rec := e.cookie(http.MethodPut, allowancePath(product, nameB), jwt)
			if rec.Code != http.StatusOK {
				t.Fatalf("PUT = %d %s, want 200", rec.Code, rec.Body.String())
			}
		}
		if rec := e.cookie(http.MethodPut, allowancePath(product, nameA), jwt); rec.Code != http.StatusOK {
			t.Fatalf("PUT A = %d", rec.Code)
		}
		r := v1Call(e.routes, http.MethodGet, list, uza, "")
		if r.status != http.StatusOK {
			t.Fatalf("uza_ GET = %d %s", r.status, r.body)
		}
		got := listNames(t, r.body)
		want := []string{nameA, nameB}
		if nameB < nameA {
			want = []string{nameB, nameA}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("list = %v, want %v (ordered by name, each once)", got, want)
		}
		if rec := e.cookie(http.MethodDelete, allowancePath(product, nameA), jwt); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE = %d", rec.Code)
		}
		if got := e.listAs(t, jwt, list); strings.Join(got, ",") != nameB {
			t.Fatalf("list after revoke = %v, want [%s]", got, nameB)
		}
		if rec := e.cookie(http.MethodDelete, allowancePath(product, nameA), jwt); rec.Code != http.StatusNotFound {
			t.Fatalf("second DELETE = %d, want 404", rec.Code)
		}
	})

	t.Run("unknown targets are 404 and a deleted product is 409", func(t *testing.T) {
		if rec := e.cookie(http.MethodPut, allowancePath(uuid.New(), nameA), jwt); rec.Code != http.StatusNotFound {
			t.Errorf("unknown product PUT = %d, want 404", rec.Code)
		}
		if rec := e.cookie(http.MethodGet, "/api/admin/products/"+uuid.NewString()+"/egress-profiles", jwt); rec.Code != http.StatusNotFound {
			t.Errorf("unknown product GET = %d, want 404", rec.Code)
		}
		if rec := e.cookie(http.MethodPut, allowancePath(product, "no-such-list"), jwt); rec.Code != http.StatusNotFound {
			t.Errorf("unknown list PUT = %d, want 404", rec.Code)
		}
		if rec := e.cookie(http.MethodPut, allowancePath(product, "Bad_Name"), jwt); rec.Code != http.StatusNotFound {
			t.Errorf("invalid list name PUT = %d, want 404", rec.Code)
		}
		gone := v1SeedProduct(t, e.h.q, admin)
		cliMustExec(t, e.pool, `UPDATE products SET enabled = false, deleted_at = now() WHERE id = $1`, gone)
		if rec := e.cookie(http.MethodPut, allowancePath(gone, nameA), jwt); rec.Code != http.StatusConflict {
			t.Errorf("deleted product PUT = %d, want 409", rec.Code)
		}
	})
}

func (e *siteListEnv) listAs(t *testing.T, jwt, path string) []string {
	t.Helper()
	rec := e.cookie(http.MethodGet, path, jwt)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
	}
	return listNames(t, rec.Body.Bytes())
}

// TestV1JobSiteListCreateLiveDB: acceptance examples 1 to 4 at POST /api/v1/jobs, with the
// allowance granted and revoked through the real admin API.
//
// CALIBRATION: delete the ProductEgressProfileAllowed call from workersvc.CreateJobRun; the "not
// allowed" and "after revoke" steps then create a job (201) and go red.
func TestV1JobSiteListCreateLiveDB(t *testing.T) {
	e := newSiteListEnv(t)
	_, jwt := e.admin()
	owner, uzc := e.v1.user()
	product := e.v1.product(owner, "research")
	pTok := v1MintProductToken(t, e.h.q, owner, product, producttoken.Scopes, nil).token
	_, allowed := e.profile("docs.example.com")
	_, other := e.profile("other.example.com")
	body := func(list string) string {
		return fmt.Sprintf(`{"type":"research","prompt":"p","egress_profile":%q}`, list)
	}
	bound := func(job string) bool {
		var v pgtype.UUID
		if err := e.pool.QueryRow(context.Background(), `SELECT egress_profile_id FROM runs WHERE id = $1`, job).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v.Valid
	}

	// A product with no allowance rows is refused (fail closed), and no run row is created.
	before := e.v1.jobCount(uzc)
	e.v1.want(e.v1.call(http.MethodPost, "/api/v1/jobs", pTok, body(allowed)), http.StatusForbidden, "egress_profile_not_allowed")
	if e.v1.jobCount(uzc) != before {
		t.Fatal("a refused create left a run row")
	}

	// AC1: the admin allows the list; the product token's job is created bound to it.
	if rec := e.cookie(http.MethodPut, allowancePath(product, allowed), jwt); rec.Code != http.StatusOK {
		t.Fatalf("allow = %d %s", rec.Code, rec.Body.String())
	}
	job := e.v1.create(pTok, body(allowed))
	if !bound(job.ID) {
		t.Fatal("AC1: the created job is not bound to its site list")
	}

	// AC2: a list the product is not allowed is a 403 and creates no run.
	n := e.v1.jobCount(uzc)
	e.v1.want(e.v1.call(http.MethodPost, "/api/v1/jobs", pTok, body(other)), http.StatusForbidden, "egress_profile_not_allowed")
	if e.v1.jobCount(uzc) != n {
		t.Fatal("AC2: a refused create left a run row")
	}

	// AC3: a user token names any existing list.
	if j := e.v1.create(uzc, body(other)); !bound(j.ID) {
		t.Fatal("AC3: a user job naming a list is not bound")
	}

	// An unknown list is 404 unknown_egress_profile for a product token too.
	e.v1.want(e.v1.call(http.MethodPost, "/api/v1/jobs", pTok, body("no-such-list")), http.StatusNotFound, "unknown_egress_profile")

	// AC4: revoking the allowance refuses the next create and leaves the earlier job as it was.
	if rec := e.cookie(http.MethodDelete, allowancePath(product, allowed), jwt); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d", rec.Code)
	}
	e.v1.want(e.v1.call(http.MethodPost, "/api/v1/jobs", pTok, body(allowed)), http.StatusForbidden, "egress_profile_not_allowed")
	if !bound(job.ID) {
		t.Fatal("AC4: revoking the allowance unbound an existing job")
	}
}

// laneCall sends one request as the lane worker holding bearer through the production router.
func (e *siteListEnv) laneCall(method, path, bearer string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	if len(body) > 0 && hdr["Content-Type"] == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.routes.ServeHTTP(rec, req)
	return rec
}

// fetcherCall is a call as the fetcher service (its service Bearer).
func (e *siteListEnv) fetcherCall(path string, v any) *httptest.ResponseRecorder {
	e.t.Helper()
	b, _ := json.Marshal(v)
	return e.laneCall(http.MethodPost, path, e.svcTok, b, nil)
}

// TestSiteListJobLaneEndToEndLiveDB drives the api side of PRD #1976 M1 end to end through the
// real routers: a lane worker registers with isolated_job_v1, claims a profile-bound product job,
// downloads its input file, the fetcher logs one allowed and one off-list fetch with the run's
// minted credential, the worker uploads an output, posts the result and reports completed, and the
// job's result shows the allowed fetch.
//
// CALIBRATION (each must redden this test): remove POST /runs/{id}/job-result from
// laneWorkerAllowlist; drop the isolateClaimKeeping call from assembleClaim's job branch; remove
// IsolatedJobV1 from capability.protocolVocabulary.
func TestSiteListJobLaneEndToEndLiveDB(t *testing.T) {
	e := newSiteListEnv(t)
	_, adminJWT := e.admin()
	owner, _ := e.v1.user()
	// The default Anthropic credential the claim opens must be sealed with this handler's box.
	sealed, err := e.h.box.Seal([]byte("e2e-anthropic-" + uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	cliMustExec(t, e.pool, `UPDATE user_secrets SET ciphertext = $1, sealed_with = 'master' WHERE user_id = $2 AND kind = 'anthropic_token'`, sealed, owner)
	product := e.v1.product(owner, "research")
	pTok := v1MintProductToken(t, e.h.q, owner, product, producttoken.Scopes, nil).token
	_, list := e.profile("docs.example.com")
	if rec := e.cookie(http.MethodPut, allowancePath(product, list), adminJWT); rec.Code != http.StatusOK {
		t.Fatalf("allow list: %d %s", rec.Code, rec.Body.String())
	}

	// An input file uploaded through the product token and attached at create.
	input := []byte("the vendor part number is XK-4411\n")
	up := e.v1.upload(pTok, input, v1UploadOpts{filename: "input.txt", ctype: "text/plain"})
	if up.status != http.StatusCreated {
		t.Fatalf("upload = %d %s", up.status, up.body)
	}
	var inFile apitypes.V1FileDTO
	up.decode(t, &inFile)
	create := fmt.Sprintf(`{"type":"research","title":"Vendor lookup","prompt":"find the datasheet","egress_profile":%q,"input_file_ids":[%q]}`, list, inFile.ID)
	job := e.v1.create(pTok, create)
	jobID := uuid.MustParse(job.ID)

	// The run-bound lane worker the provisioner would create, registering through the real route.
	wtok, whash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.q.CreateEphemeralHostedWorker(context.Background(), store.CreateEphemeralHostedWorkerParams{
		UserID: owner, Name: "lane-" + job.ID, TokenHash: whash,
		TemplateDeclared: pgtype.Text{String: "base", Valid: true},
		HostedSize:       pgtype.Text{String: "small", Valid: true},
		DockerEnabled:    pgtype.Bool{Bool: false, Valid: true},
		EphemeralRunID:   jobID, AnthropicBindMode: workersvc.BindModeDefault, IsolatedLane: true,
	}); err != nil {
		t.Fatalf("CreateEphemeralHostedWorker: %v", err)
	}
	reg, _ := json.Marshal(map[string]any{
		"version": "test",
		"protocol_capabilities": []string{
			capability.JobRunnerV1, capability.JobFilesV1, capability.IsolatedFetchV1, capability.IsolatedJobV1,
		},
	})
	if rec := e.laneCall(http.MethodPost, "/api/worker/register", wtok, reg, nil); rec.Code != http.StatusOK {
		t.Fatalf("register = %d %s", rec.Code, rec.Body.String())
	}
	var stored []string
	if err := e.pool.QueryRow(context.Background(), `SELECT protocol_capabilities FROM workers WHERE token_hash = $1`, whash).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(stored, capability.IsolatedJobV1) {
		t.Fatalf("stored protocol_capabilities = %v, missing %s", stored, capability.IsolatedJobV1)
	}

	// Claim: the job block, its input file, and the fetch grant, with nothing of a repo or forge.
	rec := e.laneCall(http.MethodPost, "/api/worker/runs/claim", wtok, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim = %d %s", rec.Code, rec.Body.String())
	}
	var claim workersvc.ClaimPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &claim); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if claim.RunID != job.ID || claim.Job == nil || len(claim.Job.Files) != 1 {
		t.Fatalf("claim = run %s job %+v, want the job with its one input file", claim.RunID, claim.Job)
	}
	grant := claim.IsolatedFetch
	if grant == nil || !strings.HasPrefix(grant.Credential, "uzf_") || grant.Profile != list {
		t.Fatalf("isolated_fetch = %+v, want a uzf_ credential for %s", grant, list)
	}
	if claim.Secrets.ForgePAT != "" || claim.Repo.URL != "" {
		t.Fatalf("the lane claim carries forge material: pat=%q repo=%q", claim.Secrets.ForgePAT, claim.Repo.URL)
	}
	gen := claim.ClaimGeneration
	runPath := "/api/worker/runs/" + job.ID

	// The input download, on the lane allowlist.
	dl := e.laneCall(http.MethodGet, fmt.Sprintf("%s/files/%s?claim_generation=%d", runPath, claim.Job.Files[0].ID, gen), wtok, nil, nil)
	if dl.Code != http.StatusOK || !bytes.Equal(dl.Body.Bytes(), input) {
		t.Fatalf("input download = %d %q, want 200 and the uploaded bytes", dl.Code, dl.Body.String())
	}

	// Running, then the fetcher logs one allowed and one off-list fetch with the minted credential.
	if rec := e.laneCall(http.MethodPost, runPath+"/state", wtok, []byte(fmt.Sprintf(`{"status":"running","claim_generation":%d}`, gen)), nil); rec.Code != http.StatusOK {
		t.Fatalf("state running = %d %s", rec.Code, rec.Body.String())
	}
	fetch := func(url, verdict, reason string) {
		t.Helper()
		bg := e.fetcherCall(fetchctl.BeginPath, apitypes.FetcherBeginRequest{Credential: grant.Credential, URL: url})
		if bg.Code != http.StatusOK {
			t.Fatalf("begin %s = %d %s", url, bg.Code, bg.Body.String())
		}
		var adm apitypes.FetcherBeginResponse
		if err := json.Unmarshal(bg.Body.Bytes(), &adm); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().UTC()
		req := apitypes.FetcherCompleteRequest{
			Credential: grant.Credential, ReservationID: adm.ReservationID, URL: url, FinalURL: url,
			Verdict: verdict, Reason: reason, StartedAt: ts, FinishedAt: ts.Add(time.Second),
		}
		if verdict == fetchctl.VerdictAllowed {
			req.HTTPStatus, req.ContentType, req.Bytes, req.SHA256 = 200, "text/html", 100, strings.Repeat("ab", 32)
		}
		if c := e.fetcherCall(fetchctl.CompletePath, req); c.Code != http.StatusNoContent {
			t.Fatalf("complete %s = %d %s", url, c.Code, c.Body.String())
		}
	}
	fetch("https://docs.example.com/datasheet", fetchctl.VerdictAllowed, "")
	fetch("https://evil.example.com/x", fetchctl.VerdictRefused, "off_list")

	// An output file, then the result, then the completion report.
	out := []byte("# Summary\n\nXK-4411 datasheet reviewed.\n")
	sum := sha256.Sum256(out)
	meta, _ := json.Marshal(map[string]any{"claim_generation": gen, "display_name": "summary.md", "size": len(out), "sha256": hex.EncodeToString(sum[:])})
	if rec := e.laneCall(http.MethodPost, runPath+"/files", wtok, out, map[string]string{jobFileMetaHeader: string(meta), "Content-Type": "application/octet-stream"}); rec.Code != http.StatusCreated {
		t.Fatalf("output upload = %d %s", rec.Code, rec.Body.String())
	}
	result := fmt.Sprintf(`{"claim_generation":%d,"status":"completed","report_md":"found it","findings":[{"severity":"info","message_md":"checked","url":"https://docs.example.com/datasheet"}]}`, gen)
	if rec := e.laneCall(http.MethodPost, runPath+"/job-result", wtok, []byte(result), nil); rec.Code != http.StatusOK {
		t.Fatalf("job-result = %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.laneCall(http.MethodPost, runPath+"/state", wtok, []byte(fmt.Sprintf(`{"status":"completed","claim_generation":%d}`, gen)), nil); rec.Code != http.StatusOK {
		t.Fatalf("state completed = %d %s", rec.Code, rec.Body.String())
	}

	// The run is completed, and the product's result shows the allowed fetch and the output.
	var status string
	if err := e.pool.QueryRow(context.Background(), `SELECT status FROM runs WHERE id = $1`, jobID).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("run status = %q, %v, want completed", status, err)
	}
	res := e.v1.call(http.MethodGet, "/api/v1/jobs/"+job.ID+"/result", pTok, "")
	if res.status != http.StatusOK {
		t.Fatalf("result = %d %s", res.status, res.body)
	}
	var dto apitypes.V1JobResultDTO
	res.decode(t, &dto)
	if dto.Result == nil || dto.Result.Status != "completed" {
		t.Fatalf("result = %+v", dto)
	}
	var allowed, refused int
	for _, s := range dto.Result.Sources {
		switch {
		case s.URL == "https://docs.example.com/datasheet" && s.Verdict == fetchctl.VerdictAllowed:
			allowed++
		case s.URL == "https://evil.example.com/x" && s.Verdict == fetchctl.VerdictRefused && s.Reason == "off_list":
			refused++
		}
	}
	if allowed != 1 || refused != 1 {
		t.Fatalf("sources = %+v, want the allowed fetch and the off-list refusal", dto.Result.Sources)
	}
	outputs := 0
	for _, f := range dto.Result.Files {
		if f.Direction == "output" && f.DisplayName == "summary.md" {
			outputs++
		}
	}
	if outputs != 1 {
		t.Fatalf("files = %+v, want the uploaded summary.md output", dto.Result.Files)
	}
}
