package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1908 M5: the /api/v1/jobs endpoints through the PRODUCTION router (h.Routes), so the mount
// order (RequireV1Caller, v1Limiter, RequireScope, the create's authLimiter) is what is measured.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

// v1JobCapFake is the per-user active-job cap reader (workersvc's jobCapSettings); the embedded
// nil Settings is never called on the create path.
type v1JobCapFake struct {
	workersvc.Settings
	cap int
}

func (f v1JobCapFake) JobMaxActivePerUser(context.Context) (int, error) { return f.cap, nil }

type v1JobsEnv struct {
	t      *testing.T
	h      *Handler
	pool   *pgxpool.Pool
	routes http.Handler // generous limiters
	tight  http.Handler // v1Limiter budget 1 per user: the second request of a user is a 429
}

func newV1JobsEnv(t *testing.T, jobCap int) *v1JobsEnv {
	t.Helper()
	h, pool := v1LiveDB(t)
	if jobCap > 0 {
		h.wsvc.SetHealthSettings(v1JobCapFake{cap: jobCap})
	}
	lim := func() *mw.Limiter { return mw.NewLimiter(1_000_000, time.Hour, nil) }
	return &v1JobsEnv{
		t: t, h: h, pool: pool,
		routes: h.Routes(lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim()),
		tight:  h.Routes(lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), mw.NewLimiter(1, time.Hour, nil)),
	}
}

func (e *v1JobsEnv) exec(sql string, args ...any) {
	e.t.Helper()
	cliMustExec(e.t, e.pool, sql, args...)
}

// user is a fresh user holding a usable Anthropic credential, plus a uzc_ token.
func (e *v1JobsEnv) user() (uuid.UUID, string) {
	e.t.Helper()
	u := cliSeedUser(e.t, e.pool, false)
	e.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	        VALUES ($1, $2, 'anthropic_token', 'anthropic-default', true, $3, 'master')`, uuid.New(), u, []byte("ct"))
	return u, cliMintToken(e.t, e.pool, u, clitoken.ScopeUser)
}

// product is a product allowing the given job types.
func (e *v1JobsEnv) product(owner uuid.UUID, allowed ...string) uuid.UUID {
	e.t.Helper()
	p := v1SeedProduct(e.t, e.h.q, owner)
	if allowed == nil {
		allowed = []string{}
	}
	e.exec(`UPDATE products SET allowed_job_types = $2 WHERE id = $1`, p, allowed)
	return p
}

type v1CallResult struct {
	status int
	header http.Header
	body   []byte
}

func (r v1CallResult) decode(t *testing.T, dst any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(r.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		t.Fatalf("decode %q into %T: %v", r.body, dst, err)
	}
}

// reason returns the body's machine reason, "" when it has none.
func (r v1CallResult) reason() string {
	var b struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(r.body, &b)
	return b.Reason
}

func v1Call(router http.Handler, method, path, bearer, body string) v1CallResult {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd).WithContext(ctx)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return v1CallResult{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

func (e *v1JobsEnv) call(method, path, bearer, body string) v1CallResult {
	e.t.Helper()
	return v1Call(e.routes, method, path, bearer, body)
}

func (e *v1JobsEnv) create(bearer, body string) apitypes.V1JobDTO {
	e.t.Helper()
	r := e.call(http.MethodPost, "/api/v1/jobs", bearer, body)
	if r.status != http.StatusCreated {
		e.t.Fatalf("create %s: %d %s, want 201", body, r.status, r.body)
	}
	var job apitypes.V1JobDTO
	r.decode(e.t, &job)
	return job
}

const v1MinimalJob = `{"type":"research","title":"Summarize","prompt":"read the inputs and summarize"}`

func (e *v1JobsEnv) want(r v1CallResult, status int, reason string) {
	e.t.Helper()
	if r.status != status || r.reason() != reason {
		e.t.Fatalf("got %d reason %q body %s; want %d reason %q", r.status, r.reason(), r.body, status, reason)
	}
}

// TestV1JobsAuthAndScopeLiveDB: a cookie session and a uza_ token are refused on every job route
// with RequireV1Caller's 401; a token without the route's scope is a 403 insufficient_scope, and
// the scope is checked BEFORE anything touches a job.
func TestV1JobsAuthAndScopeLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	owner, uzc := e.user()
	job := e.create(uzc, v1MinimalJob)
	product := e.product(owner, "research")
	readOnly := v1MintProductToken(t, e.h.q, owner, product, []string{producttoken.ScopeJobsRead}, nil)
	runOnly := v1MintProductToken(t, e.h.q, owner, product, []string{producttoken.ScopeJobsRun}, nil)
	uza := cliMintToken(t, e.pool, cliSeedUser(t, e.pool, true), clitoken.ScopeAdminRO)
	jwt := cliMintJWT(t, e.pool, owner)

	id := job.ID
	routes := []struct {
		method, path, body, scope string
	}{
		{"POST", "/api/v1/jobs", v1MinimalJob, producttoken.ScopeJobsRun},
		{"GET", "/api/v1/jobs", "", producttoken.ScopeJobsRead},
		{"GET", "/api/v1/jobs/" + id, "", producttoken.ScopeJobsRead},
		{"GET", "/api/v1/jobs/" + id + "/result", "", producttoken.ScopeJobsRead},
		{"GET", "/api/v1/jobs/" + id + "/messages", "", producttoken.ScopeJobsRead},
		{"POST", "/api/v1/jobs/" + id + "/cancel", "", producttoken.ScopeJobsRun},
		// PRD #1909 M2: the upload needs jobs:run. Its scope check runs before the body is read.
		{"POST", "/api/v1/files", "", producttoken.ScopeJobsRun},
	}
	const unauthorized = "{\"error\":\"invalid token\"}\n"
	for _, rt := range routes {
		name := rt.method + " " + rt.path
		t.Run(name, func(t *testing.T) {
			// 401: a valid cookie session (with its CSRF header) alone, and a uza_ token.
			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
			req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only request cookie.
			req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
			rec := httptest.NewRecorder()
			e.routes.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || rec.Body.String() != unauthorized {
				t.Errorf("cookie session: %d %q, want RequireV1Caller's 401", rec.Code, rec.Body.String())
			}
			if got := e.call(rt.method, rt.path, uza, rt.body); got.status != http.StatusUnauthorized || string(got.body) != unauthorized {
				t.Errorf("uza_ token: %d %q, want RequireV1Caller's 401", got.status, got.body)
			}
			// 403: the token holding only the OTHER scope.
			other := readOnly
			if rt.scope == producttoken.ScopeJobsRead {
				other = runOnly
			}
			e.want(e.call(rt.method, rt.path, other.token, rt.body), http.StatusForbidden, "insufficient_scope")
			// Control: the right scope is not refused as a scope failure.
			right := runOnly
			if rt.scope == producttoken.ScopeJobsRead {
				right = readOnly
			}
			if got := e.call(rt.method, rt.path, right.token, rt.body); got.status == http.StatusForbidden && got.reason() == "insufficient_scope" {
				t.Errorf("the right scope was refused as insufficient_scope: %s", got.body)
			}
		})
	}
}

// TestV1JobsCreateLiveDB: the create endpoint's success shape, the caller kinds' recorded origin,
// and every refusal with its status and reason.
func TestV1JobsCreateLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 3)
	owner, uzc := e.user()
	product := e.product(owner, "research")
	emptyProduct := e.product(owner)
	pTok := v1MintProductToken(t, e.h.q, owner, product, producttoken.Scopes, nil)
	emptyTok := v1MintProductToken(t, e.h.q, owner, emptyProduct, producttoken.Scopes, nil)

	t.Run("a user token creates a queued job and records no product", func(t *testing.T) {
		r := e.call(http.MethodPost, "/api/v1/jobs", uzc,
			`{"type":"research","title":"Summarize","prompt":"read","requested_by_label":"ada@example.com","wall_seconds":600,`+
				`"inputs":[{"name":"notes.md","content":"hello"},{"name":"b.txt","content":"x"}]}`)
		if r.status != http.StatusCreated {
			t.Fatalf("status %d %s, want 201", r.status, r.body)
		}
		var job apitypes.V1JobDTO
		r.decode(t, &job)
		if job.Status != "queued" || job.Type != "research" || job.Title != "Summarize" ||
			job.RequestedByLabel == nil || *job.RequestedByLabel != "ada@example.com" ||
			job.WallSeconds == nil || *job.WallSeconds != 600 || job.StartedAt != nil || job.FinishedAt != nil || job.FailureReason != nil {
			t.Errorf("job = %+v", job)
		}
		var product, token *uuid.UUID
		var nInputs int
		if err := e.pool.QueryRow(context.Background(), `SELECT product_id, product_token_id FROM job_origins WHERE run_id = $1`, job.ID).Scan(&product, &token); err != nil {
			t.Fatal(err)
		}
		if product != nil || token != nil {
			t.Errorf("a uzc_ caller's origin recorded product %v token %v, want none (its token id is a cli token id)", product, token)
		}
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM job_inputs WHERE run_id = $1`, job.ID).Scan(&nInputs); err != nil || nInputs != 2 {
			t.Errorf("inputs stored = %d (%v), want 2", nInputs, err)
		}
	})

	t.Run("a product token records its product and token, and the title is derived", func(t *testing.T) {
		job := e.create(pTok.token, `{"type":"research","prompt":"\n  Compare   the two   vendors\nsecond line"}`)
		if job.Title != "Compare the two vendors" {
			t.Errorf("derived title = %q", job.Title)
		}
		var product, token uuid.UUID
		if err := e.pool.QueryRow(context.Background(), `SELECT product_id, product_token_id FROM job_origins WHERE run_id = $1`, job.ID).Scan(&product, &token); err != nil {
			t.Fatal(err)
		}
		if product != pTok.productID || token != pTok.tokenID {
			t.Errorf("origin = product %s token %s, want %s / %s", product, token, pTok.productID, pTok.tokenID)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		other, otherTok := e.user()
		_ = other
		noCred := cliSeedUser(t, e.pool, false)
		noCredTok := cliMintToken(t, e.pool, noCred, clitoken.ScopeUser)
		for _, c := range []struct {
			name, token, body string
			status            int
			reason            string
		}{
			{"unknown type", otherTok, `{"type":"translate","prompt":"p"}`, 422, "unknown_job_type"},
			{"missing type", otherTok, `{"prompt":"p"}`, 422, "unknown_job_type"},
			{"egress profile", otherTok, `{"type":"research","prompt":"p","egress_profile":"open"}`, 422, "not_supported"},
			{"missing prompt", otherTok, `{"type":"research"}`, 422, "invalid_request"},
			{"bad input name", otherTok, `{"type":"research","prompt":"p","inputs":[{"name":"../x","content":"c"}]}`, 422, "invalid_request"},
			{"duplicate input name", otherTok, `{"type":"research","prompt":"p","inputs":[{"name":"a","content":"c"},{"name":"a","content":"d"}]}`, 422, "invalid_request"},
			{"control characters in the title", otherTok, `{"type":"research","title":"a\u001b[31mb","prompt":"p"}`, 422, "invalid_request"},
			{"oversize label", otherTok, `{"type":"research","prompt":"p","requested_by_label":"` + strings.Repeat("l", 201) + `"}`, 422, "invalid_request"},
			{"non-positive wall", otherTok, `{"type":"research","prompt":"p","wall_seconds":0}`, 422, "invalid_request"},
			{"unknown field", otherTok, `{"type":"research","prompt":"p","surprise":1}`, 422, "invalid_request"},
			{"wrong field type", otherTok, `{"type":"research","prompt":5}`, 422, "invalid_request"},
			{"malformed json", otherTok, `{"type":`, 422, "invalid_request"},
			{"trailing value", otherTok, v1MinimalJob + ` {}`, 422, "invalid_request"},
			{"empty body", otherTok, ``, 422, "invalid_request"},
			{"no model credential", noCredTok, v1MinimalJob, 422, "no_model_credential"},
			{"empty allow-list", emptyTok.token, v1MinimalJob, 403, "job_type_not_allowed"},
			{"oversize body", otherTok, `{"type":"research","prompt":"` + strings.Repeat("p", v1JobCreateMaxBodyBytes) + `"}`, 413, "payload_too_large"},
		} {
			t.Run(c.name, func(t *testing.T) {
				before := e.jobCount(otherTok)
				r := e.call(http.MethodPost, "/api/v1/jobs", c.token, c.body)
				e.want(r, c.status, c.reason)
				if c.status >= 400 && e.jobCount(otherTok) != before {
					t.Errorf("a refused create left a job row")
				}
			})
		}
	})

	t.Run("a decode error names the field, never a Go type", func(t *testing.T) {
		_, tok := e.user()
		r := e.call(http.MethodPost, "/api/v1/jobs", tok, `{"type":"research","prompt":5}`)
		e.want(r, http.StatusUnprocessableEntity, "invalid_request")
		if !strings.Contains(string(r.body), `field \"prompt\" has the wrong type`) || strings.Contains(string(r.body), "V1JobCreateRequest") || strings.Contains(string(r.body), "Go struct") {
			t.Errorf("decode error body = %s, want the field named and no Go type names", r.body)
		}
		r = e.call(http.MethodPost, "/api/v1/jobs", tok, `{"type":`)
		if strings.Contains(string(r.body), "unexpected") {
			t.Errorf("a syntax error leaks the decoder's own wording: %s", r.body)
		}
	})

	t.Run("over the active-job cap is 429 over_cap", func(t *testing.T) {
		_, tok := e.user()
		for range 3 {
			e.create(tok, v1MinimalJob)
		}
		r := e.call(http.MethodPost, "/api/v1/jobs", tok, v1MinimalJob)
		e.want(r, http.StatusTooManyRequests, "over_cap")
		if r.header.Get("Retry-After") != "" {
			t.Errorf("the job cap is not a rate limit, but the response carries Retry-After %q", r.header.Get("Retry-After"))
		}
	})

	t.Run("a product token cannot create a type the product does not allow", func(t *testing.T) {
		e.exec(`UPDATE products SET allowed_job_types = '{}' WHERE id = $1`, product)
		e.want(e.call(http.MethodPost, "/api/v1/jobs", pTok.token, v1MinimalJob), http.StatusForbidden, "job_type_not_allowed")
		e.exec(`UPDATE products SET allowed_job_types = '{research}' WHERE id = $1`, product)
	})
}

// jobCount counts the kind='job' runs of the user behind a uzc_ token.
func (e *v1JobsEnv) jobCount(uzcToken string) int {
	e.t.Helper()
	var n int
	err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs r JOIN cli_tokens t ON t.user_id = r.user_id
		  WHERE r.kind = 'job' AND t.token_hash = $1`, clitoken.Hash(uzcToken)).Scan(&n)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// TestV1JobsReadAndCancelLiveDB: get, list (with a cursor), result, messages and cancel on the
// happy path, and the visibility rules (another user's job, another product's job) as 404.
func TestV1JobsReadAndCancelLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	owner, uzc := e.user()
	prodA := e.product(owner, "research")
	prodB := e.product(owner, "research")
	tokA := v1MintProductToken(t, e.h.q, owner, prodA, producttoken.Scopes, nil)
	tokB := v1MintProductToken(t, e.h.q, owner, prodB, producttoken.Scopes, nil)
	stranger, strangerTok := e.user()
	_ = stranger

	jobA := e.create(tokA.token, `{"type":"research","title":"from A","prompt":"p","requested_by_label":"end user"}`)
	jobU := e.create(uzc, `{"type":"research","title":"from the user","prompt":"p"}`)
	path := func(id, suffix string) string { return "/api/v1/jobs/" + id + suffix }

	t.Run("get", func(t *testing.T) {
		r := e.call(http.MethodGet, path(jobA.ID, ""), tokA.token, "")
		var got apitypes.V1JobDTO
		if r.status != 200 {
			t.Fatalf("%d %s", r.status, r.body)
		}
		r.decode(t, &got)
		if got.ID != jobA.ID || got.Title != "from A" || got.Status != "queued" {
			t.Errorf("job = %+v", got)
		}
	})

	t.Run("visibility: 404 for another user, another product, and a malformed id", func(t *testing.T) {
		for _, c := range []struct{ name, token, id string }{
			{"another user", strangerTok, jobA.ID},
			{"another product", tokB.token, jobA.ID},
			{"a product token on a user-token job", tokA.token, jobU.ID},
			{"unknown id", uzc, uuid.NewString()},
			{"malformed id", uzc, "not-a-uuid"},
		} {
			for _, suffix := range []string{"", "/result", "/messages"} {
				e.want(e.call(http.MethodGet, path(c.id, suffix), c.token, ""), http.StatusNotFound, "not_found")
			}
			e.want(e.call(http.MethodPost, path(c.id, "/cancel"), c.token, ""), http.StatusNotFound, "not_found")
			t.Logf("%s: 404 on get, result, messages and cancel", c.name)
		}
		// The uzc_ caller sees the product's job (a user sees everything it owns).
		if r := e.call(http.MethodGet, path(jobA.ID, ""), uzc, ""); r.status != http.StatusOK {
			t.Errorf("uzc_ on its user's product job: %d %s, want 200", r.status, r.body)
		}
	})

	t.Run("list is scoped to the caller and pages with a cursor", func(t *testing.T) {
		_, tok := e.user()
		var ids []string
		for i := range 3 {
			ids = append(ids, e.create(tok, fmt.Sprintf(`{"type":"research","title":"job %d","prompt":"p"}`, i)).ID)
		}
		var page1 apitypes.V1JobListDTO
		r := e.call(http.MethodGet, "/api/v1/jobs?limit=2", tok, "")
		if r.status != 200 {
			t.Fatalf("%d %s", r.status, r.body)
		}
		r.decode(t, &page1)
		if len(page1.Jobs) != 2 || page1.NextCursor == nil {
			t.Fatalf("page 1 = %d jobs, cursor %v; want 2 and a cursor", len(page1.Jobs), page1.NextCursor)
		}
		var page2 apitypes.V1JobListDTO
		r = e.call(http.MethodGet, "/api/v1/jobs?limit=2&cursor="+*page1.NextCursor, tok, "")
		if r.status != 200 {
			t.Fatalf("%d %s", r.status, r.body)
		}
		r.decode(t, &page2)
		if len(page2.Jobs) != 1 || page2.NextCursor != nil {
			t.Fatalf("page 2 = %d jobs, cursor %v; want 1 and no cursor", len(page2.Jobs), page2.NextCursor)
		}
		got := []string{page1.Jobs[0].ID, page1.Jobs[1].ID, page2.Jobs[0].ID}
		want := []string{ids[2], ids[1], ids[0]} // newest first
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("order = %v, want newest first %v", got, want)
				break
			}
		}
		// A product token lists only its own product's jobs; the user token lists them all.
		var mine apitypes.V1JobListDTO
		e.call(http.MethodGet, "/api/v1/jobs", tokA.token, "").decode(t, &mine)
		if len(mine.Jobs) != 1 || mine.Jobs[0].ID != jobA.ID {
			t.Errorf("product A lists %d jobs, want just its own", len(mine.Jobs))
		}
		var all apitypes.V1JobListDTO
		e.call(http.MethodGet, "/api/v1/jobs", uzc, "").decode(t, &all)
		if len(all.Jobs) != 2 {
			t.Errorf("the owner's user token lists %d jobs, want both (%s and %s)", len(all.Jobs), jobA.ID, jobU.ID)
		}
		for _, bad := range []string{"limit=0", "limit=101", "limit=x", "cursor=***", "cursor=" + "bm90LWEtY3Vyc29y"} {
			e.want(e.call(http.MethodGet, "/api/v1/jobs?"+bad, tok, ""), http.StatusUnprocessableEntity, "invalid_request")
		}
	})

	t.Run("result: null until reported, then the report and findings", func(t *testing.T) {
		r := e.call(http.MethodGet, path(jobA.ID, "/result"), tokA.token, "")
		var res apitypes.V1JobResultDTO
		r.decode(t, &res)
		if r.status != 200 || res.Result != nil || res.JobStatus != "queued" {
			t.Fatalf("before a result: %d %s", r.status, r.body)
		}
		e.exec(`INSERT INTO job_results (run_id, status, report_md) VALUES ($1, 'completed', '# Report')`, jobA.ID)
		e.exec(`INSERT INTO job_findings (run_id, ordinal, severity, message_md, url) VALUES ($1, 0, 'warning', 'look', 'https://example.com/a')`, jobA.ID)
		e.exec(`INSERT INTO job_findings (run_id, ordinal, severity, message_md, file, line) VALUES ($1, 1, 'info', 'here', 'a.go', 7)`, jobA.ID)
		r = e.call(http.MethodGet, path(jobA.ID, "/result"), tokA.token, "")
		res = apitypes.V1JobResultDTO{}
		r.decode(t, &res)
		if r.status != 200 || res.Result == nil || res.Result.Status != "completed" || res.Result.ReportMd != "# Report" || len(res.Result.Findings) != 2 {
			t.Fatalf("with a result: %d %s", r.status, r.body)
		}
		f0, f1 := res.Result.Findings[0], res.Result.Findings[1]
		if f0.Severity != "warning" || f0.URL == nil || *f0.URL != "https://example.com/a" || f0.File != nil || f0.Line != nil ||
			f1.File == nil || *f1.File != "a.go" || f1.Line == nil || *f1.Line != 7 || f1.URL != nil {
			t.Errorf("findings = %+v", res.Result.Findings)
		}
	})

	t.Run("messages: only text, status and error lines, after a seq", func(t *testing.T) {
		for i, m := range []struct{ kind, payload string }{
			{"status", `{"text":"started"}`},
			{"tool_use", `{"text":"SECRET-TOOL-INPUT"}`},
			{"text", `{"text":"working"}`},
			{"error", `{"text":"oops"}`},
		} {
			e.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, $2, $3, $4)`, jobA.ID, i+1, m.kind, []byte(m.payload))
		}
		var msgs apitypes.V1JobMessagesDTO
		r := e.call(http.MethodGet, path(jobA.ID, "/messages"), tokA.token, "")
		r.decode(t, &msgs)
		if r.status != 200 || len(msgs.Messages) != 3 || strings.Contains(string(r.body), "SECRET-TOOL-INPUT") {
			t.Fatalf("messages: %d %s", r.status, r.body)
		}
		if msgs.Messages[0].Seq != 1 || msgs.Messages[0].Type != "status" || msgs.Messages[2].Seq != 4 || msgs.Messages[2].Type != "error" {
			t.Errorf("messages = %+v", msgs.Messages)
		}
		msgs = apitypes.V1JobMessagesDTO{}
		e.call(http.MethodGet, path(jobA.ID, "/messages")+"?after=3", tokA.token, "").decode(t, &msgs)
		if len(msgs.Messages) != 1 || msgs.Messages[0].Seq != 4 {
			t.Errorf("after=3 = %+v, want just seq 4", msgs.Messages)
		}
		for _, bad := range []string{"after=-1", "after=x", "after=99999999999"} {
			e.want(e.call(http.MethodGet, path(jobA.ID, "/messages")+"?"+bad, tokA.token, ""), http.StatusUnprocessableEntity, "invalid_request")
		}
	})

	t.Run("cancel: a queued job ends cancelled, a second cancel is 409", func(t *testing.T) {
		r := e.call(http.MethodPost, path(jobU.ID, "/cancel"), uzc, "")
		var got apitypes.V1JobDTO
		r.decode(t, &got)
		if r.status != 200 || got.Status != "cancelled" || got.FinishedAt == nil {
			t.Fatalf("cancel: %d %s", r.status, r.body)
		}
		e.want(e.call(http.MethodPost, path(jobU.ID, "/cancel"), uzc, ""), http.StatusConflict, "job_terminal")
		// The other product's token cannot cancel a job it cannot see.
		e.want(e.call(http.MethodPost, path(jobA.ID, "/cancel"), tokB.token, ""), http.StatusNotFound, "not_found")
	})
}

// TestV1JobsRateLimitsLiveDB (PRD #1908 D-B): the /api/v1 subtree rides v1Limiter, and the create
// ADDITIONALLY rides authLimiter per user; a refused (401/403) request spends neither.
func TestV1JobsRateLimitsLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	lim := func() *mw.Limiter { return mw.NewLimiter(1_000_000, time.Hour, nil) }
	const budget = 2

	t.Run("the v1 budget is shared across endpoints and per user", func(t *testing.T) {
		routes := h.Routes(lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), mw.NewLimiter(budget, time.Hour, nil))
		a, b := cliSeedUser(t, pool, false), cliSeedUser(t, pool, false)
		ta, tb := cliMintToken(t, pool, a, clitoken.ScopeUser), cliMintToken(t, pool, b, clitoken.ScopeUser)
		// A missing-scope 403 is not reachable with a uzc_ token; a 401 spends nothing.
		for range budget + 1 {
			if r := v1Call(routes, http.MethodGet, "/api/v1/jobs", v1UnknownProductToken(t), ""); r.status != http.StatusUnauthorized {
				t.Fatalf("unknown token: %d", r.status)
			}
		}
		for _, p := range []string{"/api/v1/jobs", "/api/v1/whoami"} {
			if r := v1Call(routes, http.MethodGet, p, ta, ""); r.status != http.StatusOK {
				t.Fatalf("GET %s: %d %s, want 200", p, r.status, r.body)
			}
		}
		r := v1Call(routes, http.MethodGet, "/api/v1/jobs/"+uuid.NewString(), ta, "")
		if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" || r.reason() != "" {
			t.Fatalf("over the v1 budget: %d Retry-After %q reason %q, want 429 with Retry-After and no reason", r.status, r.header.Get("Retry-After"), r.reason())
		}
		if r := v1Call(routes, http.MethodGet, "/api/v1/jobs", tb, ""); r.status != http.StatusOK {
			t.Fatalf("another user has its own budget: %d", r.status)
		}
	})

	t.Run("the create also rides authLimiter, keyed by its route", func(t *testing.T) {
		routes := h.Routes(mw.NewLimiter(budget, time.Hour, nil), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim())
		owner := cliSeedUser(t, pool, false)
		cliMustExec(t, pool, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		                      VALUES ($1, $2, 'anthropic_token', 'anthropic-default', true, $3, 'master')`, uuid.New(), owner, []byte("ct"))
		tok := cliMintToken(t, pool, owner, clitoken.ScopeUser)
		for i := range budget {
			if r := v1Call(routes, http.MethodPost, "/api/v1/jobs", tok, v1MinimalJob); r.status != http.StatusCreated {
				t.Fatalf("create %d: %d %s, want 201", i+1, r.status, r.body)
			}
		}
		r := v1Call(routes, http.MethodPost, "/api/v1/jobs", tok, v1MinimalJob)
		if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" {
			t.Fatalf("create over the auth budget: %d %s, want 429 with Retry-After", r.status, r.body)
		}
		// Reads are untouched by authLimiter.
		if r := v1Call(routes, http.MethodGet, "/api/v1/jobs", tok, ""); r.status != http.StatusOK {
			t.Fatalf("GET /api/v1/jobs after the create budget is spent: %d %s, want 200", r.status, r.body)
		}
	})

	t.Run("a refused scope spends none of the create budget", func(t *testing.T) {
		routes := h.Routes(mw.NewLimiter(budget, time.Hour, nil), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim(), lim())
		owner := cliSeedUser(t, pool, false)
		cliMustExec(t, pool, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		                      VALUES ($1, $2, 'anthropic_token', 'anthropic-default', true, $3, 'master')`, uuid.New(), owner, []byte("ct"))
		prod := v1SeedProduct(t, h.q, owner)
		cliMustExec(t, pool, `UPDATE products SET allowed_job_types = '{research}' WHERE id = $1`, prod)
		readOnly := v1MintProductToken(t, h.q, owner, prod, []string{producttoken.ScopeJobsRead}, nil)
		runner := v1MintProductToken(t, h.q, owner, prod, []string{producttoken.ScopeJobsRun}, nil)
		for range budget + 1 {
			if r := v1Call(routes, http.MethodPost, "/api/v1/jobs", readOnly.token, v1MinimalJob); r.status != http.StatusForbidden {
				t.Fatalf("read-only create: %d, want 403", r.status)
			}
		}
		for i := range budget {
			if r := v1Call(routes, http.MethodPost, "/api/v1/jobs", runner.token, v1MinimalJob); r.status != http.StatusCreated {
				t.Fatalf("create %d after the 403s: %d %s, want 201 (the 403s must not have spent the budget)", i+1, r.status, r.body)
			}
		}
	})
}
