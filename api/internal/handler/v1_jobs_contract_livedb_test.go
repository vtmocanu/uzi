package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
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

	// PRD #1909 M2: a small file store, so the upload's 413 and 507 are reachable.
	jf := e.wireFiles(workersvc.JobFileLimits{InputFileMaxBytes: 1000, PerOwnerBytes: 2000})
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

	// PRD #1909 M5: a stored output with a matching allowed fetch (so source_url is non-null), one
	// without (null), a refusal, and an expired output, for the file reads.
	fetchedBody := []byte("a fetched page")
	outWithSource := e.storeOutput(jf, owner, seeded.ID, "page.txt", fetchedBody)
	e.storeOutput(jf, owner, seeded.ID, "plain.txt", []byte("no source"))
	e.fetch(seeded.ID, "allowed", "https://example.com/p", "https://example.com/p2", sha256Hex(fetchedBody))
	e.fetch(seeded.ID, "refused", "https://blocked.example/", "", "")
	e.exec(`INSERT INTO job_output_refusals (run_id, display_name, byte_size, reason) VALUES ($1, 'big.pdf', 5, 'file_too_large')`, seeded.ID)
	expired := e.storeOutput(jf, owner, seeded.ID, "old.txt", []byte("expired"))
	e.exec(`DELETE FROM job_file_chunks WHERE file_id = $1`, expired.ID)
	e.exec(`UPDATE job_files SET state = 'expired' WHERE id = $1`, expired.ID)

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
		// A multipart upload case (POST /files): mpData is the file, mp its part and headers.
		mp     *v1UploadOpts
		mpData []byte
		pre    func() // runs before the call (the 503 case unwires the file store).
		// binary: the 200 body is the file's raw bytes (GET /files/{id}); its headers are checked
		// instead of a JSON schema.
		binary bool
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

		// GET /jobs/{id}/files and GET /files/{id} (PRD #1909 M5)
		{name: "files 200", method: "GET", specPath: "/jobs/{id}/files", url: "/api/v1/jobs/" + id + "/files", token: pTok.token, want: 200},
		{name: "files 401", method: "GET", specPath: "/jobs/{id}/files", url: "/api/v1/jobs/" + id + "/files", token: unknown, want: 401},
		{name: "files 403", method: "GET", specPath: "/jobs/{id}/files", url: "/api/v1/jobs/" + id + "/files", token: runOnly.token, want: 403},
		{name: "files 404", method: "GET", specPath: "/jobs/{id}/files", url: "/api/v1/jobs/" + uuid.NewString() + "/files", token: uzc, want: 404},
		{name: "files 429", method: "GET", specPath: "/jobs/{id}/files", url: "/api/v1/jobs/" + id + "/files", want: 429, tight: true, primeThenCall: true},
		{name: "download 200", method: "GET", specPath: "/files/{id}", url: "/api/v1/files/" + outWithSource.ID.String(), token: pTok.token, want: 200, binary: true},
		{name: "download 401", method: "GET", specPath: "/files/{id}", url: "/api/v1/files/" + outWithSource.ID.String(), token: unknown, want: 401},
		{name: "download 403", method: "GET", specPath: "/files/{id}", url: "/api/v1/files/" + outWithSource.ID.String(), token: runOnly.token, want: 403},
		{name: "download 404", method: "GET", specPath: "/files/{id}", url: "/api/v1/files/" + uuid.NewString(), token: uzc, want: 404},
		{name: "download 410", method: "GET", specPath: "/files/{id}", url: "/api/v1/files/" + expired.ID.String(), token: pTok.token, want: 410},
		{name: "download 429", method: "GET", specPath: "/files/{id}", url: "/api/v1/files/" + outWithSource.ID.String(), want: 429, tight: true, primeThenCall: true},

		// POST /files (PRD #1909 D5)
		{name: "upload 201", method: "POST", specPath: "/files", url: "/api/v1/files", token: uzc, want: 201, mp: &v1UploadOpts{filename: "a.txt"}, mpData: []byte("hello")},
		{name: "upload 401", method: "POST", specPath: "/files", url: "/api/v1/files", token: unknown, want: 401, mp: &v1UploadOpts{filename: "a.txt"}, mpData: []byte("hello")},
		{name: "upload 403", method: "POST", specPath: "/files", url: "/api/v1/files", token: readOnly.token, want: 403, mp: &v1UploadOpts{filename: "a.txt"}, mpData: []byte("hello")},
		{name: "upload 413", method: "POST", specPath: "/files", url: "/api/v1/files", token: uzc, want: 413, mp: &v1UploadOpts{filename: "a.txt"}, mpData: textBytes(1001)},
		{name: "upload 415", method: "POST", specPath: "/files", url: "/api/v1/files", token: uzc, want: 415, mp: &v1UploadOpts{filename: "a.pdf"}, mpData: []byte("not a pdf")},
		{name: "upload 422", method: "POST", specPath: "/files", url: "/api/v1/files", token: uzc, want: 422, mp: &v1UploadOpts{filename: "a.txt", size: sizePtr(-1)}, mpData: []byte("hello")},
		{name: "upload 422 size mismatch", method: "POST", specPath: "/files", url: "/api/v1/files", token: uzc, want: 422, mp: &v1UploadOpts{filename: "a.txt", size: sizePtr(9)}, mpData: []byte("hello")},
		{name: "upload 429", method: "POST", specPath: "/files", url: "/api/v1/files", want: 429, tight: true, primeThenCall: true, mp: &v1UploadOpts{filename: "a.txt"}, mpData: []byte("hello")},
		{name: "upload 507", method: "POST", specPath: "/files", url: "/api/v1/files", token: func() string {
			_, tok := e.user()
			for range 2 {
				e.uploadOK(tok, textBytes(1000), v1UploadOpts{filename: "fill.txt"})
			}
			return tok
		}(), want: 507, mp: &v1UploadOpts{filename: "a.txt"}, mpData: []byte("hello")},
		{name: "job create 422 file_unavailable", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: uzc, body: `{"type":"research","prompt":"p","input_file_ids":["` + uuid.NewString() + `"]}`, want: 422},
		{name: "job create 413 too_many_files", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: uzc, body: `{"type":"research","prompt":"p","input_file_ids":[` + strings.TrimSuffix(strings.Repeat(`"`+uuid.NewString()+`",`, 11), ",") + `]}`, want: 413},

		// A body that breaks mid-upload: a disconnect is 400, a read deadline is 408.
		{name: "upload 400 body read failed", method: "POST", specPath: "/files", url: "/api/v1/files", token: uzc, want: 400,
			mp: &v1UploadOpts{filename: "a.txt", failBody: io.ErrClosedPipe}, mpData: []byte("hello")},
		{name: "upload 408 body read deadline", method: "POST", specPath: "/files", url: "/api/v1/files", token: uzc, want: 408,
			mp: &v1UploadOpts{filename: "a.txt", failBody: os.ErrDeadlineExceeded}, mpData: []byte("hello")},

		// whoami's 429 (the rate limiter, on the tight router)
		{name: "whoami 429", method: "GET", specPath: "/whoami", url: "/api/v1/whoami", want: 429, tight: true, primeThenCall: true},

		// Last: it unwires the file store, so no later case may need one.
		{name: "upload 503", method: "POST", specPath: "/files", url: "/api/v1/files", token: uzc, want: 503, mp: &v1UploadOpts{filename: "a.txt"}, mpData: []byte("hello"),
			pre: func() { e.h.wsvc.SetJobFiles(nil) }},
		{name: "download 503", method: "GET", specPath: "/files/{id}", url: "/api/v1/files/" + uuid.NewString(), token: uzc, want: 503},
		{name: "job create 503 files_unavailable", method: "POST", specPath: "/jobs", url: "/api/v1/jobs", token: uzc, body: `{"type":"research","prompt":"p","input_file_ids":["` + uuid.NewString() + `"]}`, want: 503},
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
			if c.pre != nil {
				c.pre()
			}
			var r v1CallResult
			if c.mp != nil {
				r = e.uploadTo(router, token, c.mpData, *c.mp)
			} else {
				r = v1Call(router, c.method, c.url, token, c.body)
			}
			if r.status != c.want {
				t.Fatalf("status %d %s, want %d", r.status, truncate(string(r.body), 300), c.want)
			}
			status := strconv.Itoa(r.status)
			if c.binary {
				if r.header.Get("Content-Type") != "application/octet-stream" || r.header.Get("X-Content-Type-Options") != "nosniff" ||
					!strings.HasPrefix(r.header.Get("Content-Disposition"), "attachment;") {
					t.Errorf("download headers = %v", r.header)
				}
				produced[c.method+" "+c.specPath+" "+status] = true
				return
			}
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
