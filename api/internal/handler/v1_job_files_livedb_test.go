package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1909 M5: GET /api/v1/jobs/{id}/files, GET /api/v1/files/{id} and the result's sources, files
// and refused_files, through the PRODUCTION router (h.Routes). Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres. The 401 (cookie session, uza_) and 403 (scope) rows of both
// routes are in TestV1JobsAuthAndScopeLiveDB's table.

// storeOutput stores an output file of run the way the worker route does (Reserve then Write).
func (e *v1JobsEnv) storeOutput(jf *workersvc.JobFiles, owner uuid.UUID, run, name string, data []byte) store.JobFile {
	e.t.Helper()
	runID := uuid.MustParse(run)
	gen := int64(1)
	ctx := context.Background()
	f, err := jf.Reserve(ctx, workersvc.ReserveParams{
		UserID: owner, RunID: &runID, Direction: workersvc.JobFileOutput, ClaimGeneration: &gen,
		DisplayName: name, DeclaredSize: int64(len(data)), DeclaredSHA256: sha256Hex(data),
	})
	if err != nil {
		e.t.Fatalf("reserve output %s: %v", name, err)
	}
	f, err = jf.Write(ctx, f.ID, owner, strings.NewReader(string(data)), workersvc.WriteOptions{ContentType: "text/plain"})
	if err != nil {
		e.t.Fatalf("write output %s: %v", name, err)
	}
	return f
}

// fetch records one fetch-service row for run.
func (e *v1JobsEnv) fetch(run, verdict, url, finalURL, sha string) {
	e.t.Helper()
	now := time.Now()
	e.exec(`INSERT INTO run_fetches (run_id, reservation_id, url, final_url, verdict, sha256, bytes, started_at, finished_at)
	        VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $7)`, run, uuid.New(), url, finalURL, verdict, sha, now)
}

func (e *v1JobsEnv) jobFiles(bearer, run string) apitypes.V1JobFilesDTO {
	e.t.Helper()
	r := e.call(http.MethodGet, "/api/v1/jobs/"+run+"/files", bearer, "")
	if r.status != http.StatusOK {
		e.t.Fatalf("GET files of %s: %d %s", run, r.status, r.body)
	}
	var out apitypes.V1JobFilesDTO
	r.decode(e.t, &out)
	return out
}

func v1FilesByName(files []apitypes.V1JobFileDTO) map[string]apitypes.V1JobFileDTO {
	m := map[string]apitypes.V1JobFileDTO{}
	for _, f := range files {
		m[f.DisplayName] = f
	}
	return m
}

// TestV1JobFilesReadLiveDB: the listing's metadata and refusals, source_url only on a same-run hash
// match of an allowed fetch (never another job's, a refused fetch's, an input's, or an agent's
// claim), the result's sources scoped to its run, and the visibility of both routes for every
// caller kind.
func TestV1JobFilesReadLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	jf := e.wireFiles(workersvc.JobFileLimits{})
	owner, uzc := e.user()
	product := e.product(owner, "research")
	otherProduct := e.product(owner, "research")
	pTok := v1MintProductToken(t, e.h.q, owner, product, producttoken.Scopes, nil).token
	qTok := v1MintProductToken(t, e.h.q, owner, otherProduct, producttoken.Scopes, nil).token
	stranger, strangerUzc := e.user()
	_ = stranger

	// Job A: a user-token job with one attached input and four outputs. Job B: another job of the
	// same owner. Job P: a job created through product P.
	inA := e.uploadOK(uzc, textBytes(40), v1UploadOpts{filename: "brief.txt"})
	jobA := func() apitypes.V1JobDTO {
		r := e.createWithFiles(uzc, inA.ID)
		if r.status != http.StatusCreated {
			t.Fatalf("create A: %d %s", r.status, r.body)
		}
		var j apitypes.V1JobDTO
		r.decode(t, &j)
		return j
	}()
	jobB := e.create(uzc, v1MinimalJob)
	jobP := e.create(pTok, v1MinimalJob)

	fetched := []byte("the fetched page body")
	plain := []byte("an output nobody fetched")
	crossRun := []byte("hash that only job B fetched")
	refusedHash := []byte("hash of a refused fetch")
	claimed := []byte("output whose origin the agent claims")
	outFetched := e.storeOutput(jf, owner, jobA.ID, "page.txt", fetched)
	e.storeOutput(jf, owner, jobA.ID, "plain.txt", plain)
	e.storeOutput(jf, owner, jobA.ID, "cross.txt", crossRun)
	e.storeOutput(jf, owner, jobA.ID, "refused.txt", refusedHash)
	e.storeOutput(jf, owner, jobA.ID, "from-https-evil.example.txt", claimed)
	outP := e.storeOutput(jf, owner, jobP.ID, "p-out.txt", []byte("product output"))

	e.fetch(jobA.ID, "allowed", "https://a.example/page", "https://a.example/final", sha256Hex(fetched))
	e.fetch(jobA.ID, "refused", "https://blocked.example/x", "", "")
	e.fetch(jobA.ID, "refused", "https://refused.example/x", "", sha256Hex(refusedHash))
	// The input's own hash equals an allowed fetch of this run: an input never carries a source.
	e.fetch(jobA.ID, "allowed", "https://a.example/input-look-alike", "", sha256Hex(textBytes(40)))
	// Another job's allowed fetch of the same bytes as A's cross.txt.
	e.fetch(jobB.ID, "allowed", "https://b.example/only-in-b", "", sha256Hex(crossRun))
	// Agent claims: a finding that names a URL for an output, and a result status/report that does.
	e.exec(`INSERT INTO job_results (run_id, status, report_md) VALUES ($1, 'completed', 'plain.txt came from https://claimed.example/plain')`, jobA.ID)
	e.exec(`INSERT INTO job_findings (run_id, ordinal, severity, message_md, url) VALUES ($1, 0, 'info', 'plain.txt came from there', 'https://claimed.example/plain')`, jobA.ID)
	e.exec(`INSERT INTO job_output_refusals (run_id, display_name, byte_size, reason) VALUES ($1, 'huge.pdf', 999, 'file_too_large')`, jobA.ID)

	t.Run("listing", func(t *testing.T) {
		got := e.jobFiles(uzc, jobA.ID)
		byName := v1FilesByName(got.Files)
		if len(got.Files) != 6 || len(byName) != 6 {
			t.Fatalf("files = %+v, want the input and five outputs", got.Files)
		}
		in := byName["brief.txt"]
		if in.Direction != "input" || in.State != "attached" || in.ID != inA.ID || in.ByteSize != 40 || in.Sha256 != sha256Hex(textBytes(40)) || in.ContentType != "text/plain" {
			t.Errorf("input = %+v", in)
		}
		if in.SourceURL != nil {
			t.Errorf("an input carries source_url %q", *in.SourceURL)
		}
		pg := byName["page.txt"]
		if pg.Direction != "output" || pg.ID != outFetched.ID.String() || pg.Sha256 != sha256Hex(fetched) || pg.ByteSize != int64(len(fetched)) {
			t.Errorf("page.txt = %+v", pg)
		}
		if pg.SourceURL == nil || *pg.SourceURL != "https://a.example/final" {
			t.Errorf("page.txt source_url = %v, want the final URL of this run's allowed fetch", pg.SourceURL)
		}
		for _, name := range []string{"plain.txt", "cross.txt", "refused.txt", "from-https-evil.example.txt"} {
			if f, ok := byName[name]; !ok || f.SourceURL != nil {
				t.Errorf("%s: present=%v source_url=%v, want present with no source_url", name, ok, f.SourceURL)
			}
		}
		if len(got.RefusedFiles) != 1 || got.RefusedFiles[0] != (apitypes.V1JobRefusedFileDTO{DisplayName: "huge.pdf", ByteSize: 999, Reason: "file_too_large"}) {
			t.Errorf("refused_files = %+v", got.RefusedFiles)
		}
		// Job B has no files: an empty, non-null listing.
		if b := e.jobFiles(uzc, jobB.ID); b.Files == nil || len(b.Files) != 0 || b.RefusedFiles == nil || len(b.RefusedFiles) != 0 {
			t.Errorf("job B listing = %+v, want empty non-null arrays", b)
		}
		// The raw body carries null nowhere.
		raw := e.call(http.MethodGet, "/api/v1/jobs/"+jobB.ID+"/files", uzc, "")
		if string(raw.body) != "{\"files\":[],\"refused_files\":[]}\n" {
			t.Errorf("empty listing body = %q", raw.body)
		}
	})

	t.Run("result", func(t *testing.T) {
		r := e.call(http.MethodGet, "/api/v1/jobs/"+jobA.ID+"/result", uzc, "")
		if r.status != http.StatusOK {
			t.Fatalf("result: %d %s", r.status, r.body)
		}
		var res apitypes.V1JobResultDTO
		r.decode(t, &res)
		if res.Result == nil {
			t.Fatal("no result body")
		}
		// Sources: this run's four fetches, none of job B's.
		if len(res.Result.Sources) != 4 {
			t.Fatalf("sources = %+v, want the four fetches of job A only", res.Result.Sources)
		}
		for _, s := range res.Result.Sources {
			if strings.Contains(s.URL, "only-in-b") {
				t.Errorf("job B's fetch leaked into job A's sources: %+v", s)
			}
		}
		first := res.Result.Sources[0]
		if first.URL != "https://a.example/page" || first.FinalURL != "https://a.example/final" || first.Verdict != "allowed" || first.Sha256 != sha256Hex(fetched) || first.ByteSize != 1 {
			t.Errorf("first source = %+v", first)
		}
		if len(res.Result.Files) != 6 || len(res.Result.RefusedFiles) != 1 {
			t.Errorf("result files/refused = %d/%d", len(res.Result.Files), len(res.Result.RefusedFiles))
		}
		var withSource []string
		for _, f := range res.Result.Files {
			if f.SourceURL != nil {
				withSource = append(withSource, f.DisplayName)
			}
		}
		if len(withSource) != 1 || withSource[0] != "page.txt" {
			t.Errorf("files with a source_url = %v, want only page.txt", withSource)
		}
		// A job with a result and nothing else still has empty, non-null arrays.
		jobC := e.create(uzc, v1MinimalJob)
		e.exec(`INSERT INTO job_results (run_id, status, report_md) VALUES ($1, 'completed', 'r')`, jobC.ID)
		rb := e.call(http.MethodGet, "/api/v1/jobs/"+jobC.ID+"/result", uzc, "")
		if !strings.Contains(string(rb.body), `"sources":[]`) || !strings.Contains(string(rb.body), `"files":[]`) || !strings.Contains(string(rb.body), `"refused_files":[]`) {
			t.Errorf("job C result body = %s", rb.body)
		}
	})

	t.Run("listing visibility", func(t *testing.T) {
		// A product caller sees only its product's jobs; another user sees none.
		e.want(e.call(http.MethodGet, "/api/v1/jobs/"+jobA.ID+"/files", pTok, ""), http.StatusNotFound, "not_found")
		e.want(e.call(http.MethodGet, "/api/v1/jobs/"+jobA.ID+"/files", strangerUzc, ""), http.StatusNotFound, "not_found")
		e.want(e.call(http.MethodGet, "/api/v1/jobs/"+jobP.ID+"/files", qTok, ""), http.StatusNotFound, "not_found")
		e.want(e.call(http.MethodGet, "/api/v1/jobs/"+jobP.ID+"/files", strangerUzc, ""), http.StatusNotFound, "not_found")
		e.want(e.call(http.MethodGet, "/api/v1/jobs/"+uuid.NewString()+"/files", uzc, ""), http.StatusNotFound, "not_found")
		e.want(e.call(http.MethodGet, "/api/v1/jobs/not-a-uuid/files", uzc, ""), http.StatusNotFound, "not_found")
		if got := e.jobFiles(pTok, jobP.ID); len(got.Files) != 1 || got.Files[0].ID != outP.ID.String() {
			t.Errorf("product P's listing of its own job = %+v", got.Files)
		}
		// The owner's user token sees the product job too, as GET /jobs/{id} does.
		if got := e.jobFiles(uzc, jobP.ID); len(got.Files) != 1 {
			t.Errorf("uzc_ listing of a product job = %+v", got.Files)
		}
	})

	t.Run("download", func(t *testing.T) {
		dl := func(bearer, id string) v1CallResult {
			return e.call(http.MethodGet, "/api/v1/files/"+id, bearer, "")
		}
		r := dl(uzc, outFetched.ID.String())
		if r.status != http.StatusOK || string(r.body) != string(fetched) {
			t.Fatalf("download: %d %q", r.status, r.body)
		}
		for h, want := range map[string]string{
			"Content-Type":           "application/octet-stream",
			"X-Content-Type-Options": "nosniff",
			"Content-Disposition":    `attachment; filename="` + outFetched.StorageName.String + `"`,
		} {
			if got := r.header.Get(h); got != want {
				t.Errorf("%s = %q, want %q", h, got, want)
			}
		}
		// The disposition filename is the content-derived storage name, never the display name.
		if strings.Contains(r.header.Get("Content-Disposition"), "page.txt") {
			t.Errorf("Content-Disposition carries the display name: %q", r.header.Get("Content-Disposition"))
		}
		// An input attached to a job the caller owns, and a product job's output for its product.
		e.want(dl(uzc, inA.ID), http.StatusOK, "")
		e.want(dl(pTok, outP.ID.String()), http.StatusOK, "")
		e.want(dl(uzc, outP.ID.String()), http.StatusOK, "")
		// Another user's, another product's, job A's output for product P, unknown and malformed.
		e.want(dl(strangerUzc, outFetched.ID.String()), http.StatusNotFound, "not_found")
		e.want(dl(strangerUzc, inA.ID), http.StatusNotFound, "not_found")
		e.want(dl(pTok, outFetched.ID.String()), http.StatusNotFound, "not_found")
		e.want(dl(qTok, outP.ID.String()), http.StatusNotFound, "not_found")
		e.want(dl(uzc, uuid.NewString()), http.StatusNotFound, "not_found")
		e.want(dl(uzc, "not-a-uuid"), http.StatusNotFound, "not_found")
		// An unattached upload: only its own product (uzp_) or, for a user-token upload, only uzc_.
		up := e.uploadOK(pTok, textBytes(12), v1UploadOpts{filename: "p-upload.txt"})
		uu := e.uploadOK(uzc, textBytes(13), v1UploadOpts{filename: "u-upload.txt"})
		e.want(dl(pTok, up.ID), http.StatusOK, "")
		e.want(dl(uzc, up.ID), http.StatusNotFound, "not_found")
		e.want(dl(qTok, up.ID), http.StatusNotFound, "not_found")
		e.want(dl(uzc, uu.ID), http.StatusOK, "")
		e.want(dl(pTok, uu.ID), http.StatusNotFound, "not_found")
		e.want(dl(strangerUzc, uu.ID), http.StatusNotFound, "not_found")
		// An expired file is 410 for its owner and still 404 for anyone else.
		e.exec(`DELETE FROM job_file_chunks WHERE file_id = $1`, uu.ID)
		e.exec(`UPDATE job_files SET state = 'expired' WHERE id = $1`, uu.ID)
		e.want(dl(uzc, uu.ID), http.StatusGone, "file_expired")
		e.want(dl(strangerUzc, uu.ID), http.StatusNotFound, "not_found")
		// The listing still shows an expired file, honestly, with its state.
		e.exec(`DELETE FROM job_file_chunks WHERE file_id = $1`, outFetched.ID)
		e.exec(`UPDATE job_files SET state = 'expired' WHERE id = $1`, outFetched.ID)
		e.want(dl(uzc, outFetched.ID.String()), http.StatusGone, "file_expired")
		if f := v1FilesByName(e.jobFiles(uzc, jobA.ID).Files)["page.txt"]; f.State != "expired" {
			t.Errorf("expired file listed as %+v", f)
		}
	})
}

// TestV1FileDownloadUnwiredLiveDB: a service with no job-file store answers 503 files_unavailable
// on the download, and never panics.
func TestV1FileDownloadUnwiredLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	_, uzc := e.user()
	e.want(e.call(http.MethodGet, "/api/v1/files/"+uuid.NewString(), uzc, ""), http.StatusServiceUnavailable, "files_unavailable")
}
