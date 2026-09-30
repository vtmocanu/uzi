package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
)

// TestV1SpecContractLiveDB (PRD #1908 M5): every /api/v1 operation's success AND error responses,
// produced by the PRODUCTION router against a live database, validate against api/openapi/v1.yaml.
//
// It is closed in both directions, so the spec and the server cannot drift apart unnoticed:
//   - every case's actual status must be a status its operation DOCUMENTS, and its body must
//     validate against that response's schema (undeclared properties count as violations);
//   - every status an operation documents must be produced by at least one case, so a documented
//     response no test can reach (or a new endpoint with no case) reddens the test.
//
// The validator itself is pinned by TestV1SpecValidatorDetectsDrift.
func TestV1SpecContractLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 4)
	doc := loadV1Doc(t)

	owner, uzc := e.user()
	product := e.product(owner, "research")
	pTok := v1MintProductToken(t, e.h.q, owner, product, producttoken.Scopes, nil)
	readOnly := v1MintProductToken(t, e.h.q, owner, product, []string{producttoken.ScopeJobsRead}, nil)
	runOnly := v1MintProductToken(t, e.h.q, owner, product, []string{producttoken.ScopeJobsRun}, nil)
	unknown := v1UnknownProductToken(t)

	// A job with a result, messages and findings for the read cases, and one to cancel.
	seeded := e.create(pTok.token, `{"type":"research","title":"seeded","prompt":"p","requested_by_label":"someone",`+
		`"inputs":[{"name":"n.md","content":"c"}],"wall_seconds":600}`)
	e.exec(`INSERT INTO job_results (run_id, status, report_md) VALUES ($1, 'completed', '# r')`, seeded.ID)
	e.exec(`INSERT INTO job_findings (run_id, ordinal, severity, message_md, url) VALUES ($1, 0, 'info', 'm', 'https://example.com')`, seeded.ID)
	e.exec(`INSERT INTO job_findings (run_id, ordinal, severity, message_md, file, line) VALUES ($1, 1, 'error', 'm', 'f.go', 3)`, seeded.ID)
	e.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 1, 'text', '{"text":"hi"}')`, seeded.ID)
	toCancel := e.create(pTok.token, v1MinimalJob)

	type tc struct {
		name          string
		method        string
		specPath      string // the spec's path template
		url           string
		token         string
		body          string
		want          int
		tight         bool // run through the router whose v1 budget is one request per user
		primeThenCall bool // spend the tight budget with a first request, then make this one
	}
	id := seeded.ID
	cases := []tc{
		// whoami
		{name: "whoami 200", method: "GET", specPath: "/whoami", url: "/api/v1/whoami", token: uzc, want: 200},
		{name: "whoami 401", method: "GET", specPath: "/whoami", url: "/api/v1/whoami", token: unknown, want: 401},

		// POST /jobs
		{name: "create 201", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: uzc, body: `{"type":"research","title":"t","prompt":"p","inputs":[{"name":"a","content":"c"}],"requested_by_label":"l","wall_seconds":60}`, want: 201},
		{name: "create 401", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: unknown, body: v1MinimalJob, want: 401},
		{name: "create 403 scope", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: readOnly.token, body: v1MinimalJob, want: 403},
		{name: "create 403 type", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: v1MintProductToken(t, e.h.q, owner, e.product(owner), producttoken.Scopes, nil).token, body: v1MinimalJob, want: 403},
		{name: "create 413", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: uzc, body: `{"type":"research","prompt":"` + strings.Repeat("p", v1JobCreateMaxBodyBytes) + `"}`, want: 413},
		{name: "create 422", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: uzc, body: `{"type":"research"}`, want: 422},
		{name: "create 422 unknown type", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: uzc, body: `{"type":"x","prompt":"p"}`, want: 422},
		{name: "create 422 egress", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: uzc, body: `{"type":"research","prompt":"p","egress_profile":"x"}`, want: 422},
		{name: "create 422 credential", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: cliMintToken(t, e.pool, cliSeedUser(t, e.pool, false), clitoken.ScopeUser), body: v1MinimalJob, want: 422},
		{name: "create 429 over_cap", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: func() string {
			_, tok := e.user()
			for range 4 {
				e.create(tok, v1MinimalJob)
			}
			return tok
		}(), body: v1MinimalJob, want: 429},
		{name: "create 429 rate", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", body: v1MinimalJob, want: 429, tight: true, primeThenCall: true},

		// GET /jobs
		{name: "list 200", method: "GET", specPath: "/jobs", url: "/api/v1/jobs?limit=1", token: pTok.token, want: 200},
		{name: "list 401", method: "GET", specPath: "/jobs", url: "/api/v1/jobs", token: unknown, want: 401},
		{name: "list 403", method: "GET", specPath: "/jobs", url: "/api/v1/jobs", token: runOnly.token, want: 403},
		{name: "list 422", method: "GET", specPath: "/jobs", url: "/api/v1/jobs?limit=0", token: uzc, want: 422},
		{name: "list 429", method: "GET", specPath: "/jobs", url: "/api/v1/jobs", want: 429, tight: true, primeThenCall: true},

		// GET /jobs/{id}
		{name: "get 200", method: "GET", specPath: "/jobs/{id}", url: "/api/v1/jobs/" + id, token: pTok.token, want: 200},
		{name: "get 401", method: "GET", specPath: "/jobs/{id}", url: "/api/v1/jobs/" + id, token: unknown, want: 401},
		{name: "get 403", method: "GET", specPath: "/jobs/{id}", url: "/api/v1/jobs/" + id, token: runOnly.token, want: 403},
		{name: "get 404", method: "GET", specPath: "/jobs/{id}", url: "/api/v1/jobs/" + uuid.NewString(), token: uzc, want: 404},
		{name: "get 429", method: "GET", specPath: "/jobs/{id}", url: "/api/v1/jobs/" + id, want: 429, tight: true, primeThenCall: true},

		// GET /jobs/{id}/result
		{name: "result 200", method: "GET", specPath: "/jobs/{id}/result", url: "/api/v1/jobs/" + id + "/result", token: pTok.token, want: 200},
		{name: "result 200 pending", method: "GET", specPath: "/jobs/{id}/result", url: "/api/v1/jobs/" + toCancel.ID + "/result", token: pTok.token, want: 200},
		{name: "result 401", method: "GET", specPath: "/jobs/{id}/result", url: "/api/v1/jobs/" + id + "/result", token: unknown, want: 401},
		{name: "result 403", method: "GET", specPath: "/jobs/{id}/result", url: "/api/v1/jobs/" + id + "/result", token: runOnly.token, want: 403},
		{name: "result 404", method: "GET", specPath: "/jobs/{id}/result", url: "/api/v1/jobs/" + uuid.NewString() + "/result", token: uzc, want: 404},
		{name: "result 429", method: "GET", specPath: "/jobs/{id}/result", url: "/api/v1/jobs/" + id + "/result", want: 429, tight: true, primeThenCall: true},

		// GET /jobs/{id}/messages
		{name: "messages 200", method: "GET", specPath: "/jobs/{id}/messages", url: "/api/v1/jobs/" + id + "/messages?after=0", token: pTok.token, want: 200},
		{name: "messages 401", method: "GET", specPath: "/jobs/{id}/messages", url: "/api/v1/jobs/" + id + "/messages", token: unknown, want: 401},
		{name: "messages 403", method: "GET", specPath: "/jobs/{id}/messages", url: "/api/v1/jobs/" + id + "/messages", token: runOnly.token, want: 403},
		{name: "messages 404", method: "GET", specPath: "/jobs/{id}/messages", url: "/api/v1/jobs/" + uuid.NewString() + "/messages", token: uzc, want: 404},
		{name: "messages 422", method: "GET", specPath: "/jobs/{id}/messages", url: "/api/v1/jobs/" + id + "/messages?after=-3", token: uzc, want: 422},
		{name: "messages 429", method: "GET", specPath: "/jobs/{id}/messages", url: "/api/v1/jobs/" + id + "/messages", want: 429, tight: true, primeThenCall: true},

		// POST /jobs/{id}/cancel: 409 needs the job to be finished first, so the cancel cases
		// run 200 then 409 in order (cases execute sequentially).
		{name: "cancel 200", method: "POST", specPath: "/jobs/{id}/cancel", url: "/api/v1/jobs/" + toCancel.ID + "/cancel", token: pTok.token, want: 200},
		{name: "cancel 409", method: "POST", specPath: "/jobs/{id}/cancel", url: "/api/v1/jobs/" + toCancel.ID + "/cancel", token: pTok.token, want: 409},
		{name: "cancel 401", method: "POST", specPath: "/jobs/{id}/cancel", url: "/api/v1/jobs/" + id + "/cancel", token: unknown, want: 401},
		{name: "cancel 403", method: "POST", specPath: "/jobs/{id}/cancel", url: "/api/v1/jobs/" + id + "/cancel", token: readOnly.token, want: 403},
		{name: "cancel 404", method: "POST", specPath: "/jobs/{id}/cancel", url: "/api/v1/jobs/" + uuid.NewString() + "/cancel", token: uzc, want: 404},
		{name: "cancel 429", method: "POST", specPath: "/jobs/{id}/cancel", url: "/api/v1/jobs/" + id + "/cancel", want: 429, tight: true, primeThenCall: true},

		// whoami's 429 (the rate limiter, on the tight router)
		{name: "whoami 429", method: "GET", specPath: "/whoami", url: "/api/v1/whoami", want: 429, tight: true, primeThenCall: true},
	}

	produced := map[string]bool{} // "METHOD /path STATUS"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			router, token := e.routes, c.token
			if c.primeThenCall {
				// A fresh user whose single v1 request is spent on a cheap first call.
				_, token = e.user()
				if r := v1Call(e.tight, http.MethodGet, "/api/v1/whoami", token, ""); r.status != http.StatusOK {
					t.Fatalf("priming request: %d %s", r.status, r.body)
				}
				router = e.tight
			}
			r := v1Call(router, c.method, c.url, token, c.body)
			if r.status != c.want {
				t.Fatalf("status %d %s, want %d", r.status, truncate(string(r.body), 300), c.want)
			}
			status := strconv.Itoa(r.status)
			schema, documented, err := doc.responseSchema(c.method, c.specPath, status)
			if err != nil {
				t.Fatal(err)
			}
			if !documented {
				t.Fatalf("%s %s answered %s, which the spec does not document (documented: %v)",
					c.method, c.specPath, status, doc.documentedStatuses(c.method, c.specPath))
			}
			var body any
			if err := json.Unmarshal(r.body, &body); err != nil {
				t.Fatalf("body is not JSON: %v: %s", err, r.body)
			}
			if errs := doc.validate(schema, body); len(errs) != 0 {
				t.Fatalf("%s %s %s does not match the spec:\n  %s\nbody: %s", c.method, c.specPath, status, strings.Join(errs, "\n  "), r.body)
			}
			if r.status == http.StatusTooManyRequests && c.tight && r.header.Get("Retry-After") == "" {
				t.Errorf("a rate-limit 429 must carry Retry-After (the spec documents it)")
			}
			produced[c.method+" "+c.specPath+" "+status] = true
		})
	}

	// The other direction: every documented status was produced, and every operation has cases.
	ops, _ := doc.get("paths")
	for path, item := range ops.(map[string]any) {
		for method := range item.(map[string]any) {
			m := strings.ToUpper(method)
			if !isHTTPMethod(m) {
				continue
			}
			for _, status := range doc.documentedStatuses(m, path) {
				if !produced[m+" "+path+" "+status] {
					t.Errorf("the spec documents %s %s %s but no contract case produced it: add one (or drop the documentation)", m, path, status)
				}
			}
		}
	}
}

func isHTTPMethod(m string) bool {
	switch m {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return true
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
