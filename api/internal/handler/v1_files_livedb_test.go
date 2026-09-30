package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1909 M2: POST /api/v1/files and input_file_ids, through the PRODUCTION router (h.Routes), so
// the mount order (RequireV1Caller, v1Limiter, RequireScope) is what is measured. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// wireFiles installs a job-file store with the given limits (the rest defaulted) on the env's
// service, the way cmd/server does, and returns it.
func (e *v1JobsEnv) wireFiles(l workersvc.JobFileLimits) *workersvc.JobFiles {
	e.t.Helper()
	jf := workersvc.NewJobFiles(e.pool, newHandlerTestBox(e.t), l, nil)
	e.h.wsvc.SetJobFiles(jf)
	return jf
}

type v1UploadOpts struct {
	filename string
	ctype    string            // the part's Content-Type; "" omits the header.
	size     *int64            // X-Uzi-File-Size; nil = the body's length, negative = omit.
	sha      string            // X-Uzi-File-Sha256; "" omits.
	hdr      map[string]string // extra or overriding request headers.
	field    string            // the part's form name; "" = "file".
}

func sizePtr(n int64) *int64 { return &n }

func pdfBytes(n int) []byte {
	b := []byte("%PDF-1.7\n")
	for len(b) < n {
		b = append(b, "0123456789abcdef"...)
	}
	return b[:n]
}

func textBytes(n int) []byte { return bytes.Repeat([]byte("a"), n) }

// upload posts one multipart file to POST /api/v1/files.
func (e *v1JobsEnv) upload(bearer string, data []byte, o v1UploadOpts) v1CallResult {
	e.t.Helper()
	return e.uploadTo(e.routes, bearer, data, o)
}

// uploadTo is upload through a given production router (the contract test's tight-budget one).
func (e *v1JobsEnv) uploadTo(router http.Handler, bearer string, data []byte, o v1UploadOpts) v1CallResult {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	field := o.field
	if field == "" {
		field = "file"
	}
	h := textproto.MIMEHeader{}
	// Quote by hand: %q would spell a non-ASCII or invisible character as a \u escape, which the
	// server's header parser reads back as a literal "u202e".
	quote := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, quote(field), quote(o.filename)))
	if o.ctype != "" {
		h.Set("Content-Type", o.ctype)
	}
	pw, err := mw.CreatePart(h)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = pw.Write(data)
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/files", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	switch {
	case o.size == nil:
		req.Header.Set("X-Uzi-File-Size", fmt.Sprint(len(data)))
	case *o.size >= 0:
		req.Header.Set("X-Uzi-File-Size", fmt.Sprint(*o.size))
	}
	if o.sha != "" {
		req.Header.Set("X-Uzi-File-Sha256", o.sha)
	}
	for k, v := range o.hdr {
		req.Header.Set(k, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req.WithContext(ctx))
	return v1CallResult{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

func (e *v1JobsEnv) uploadOK(bearer string, data []byte, o v1UploadOpts) apitypes.V1FileDTO {
	e.t.Helper()
	r := e.upload(bearer, data, o)
	if r.status != http.StatusCreated {
		e.t.Fatalf("upload %q: %d %s, want 201", o.filename, r.status, r.body)
	}
	var f apitypes.V1FileDTO
	r.decode(e.t, &f)
	return f
}

func (e *v1JobsEnv) ownerFileCount(owner uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM job_files WHERE user_id = $1`, owner).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *v1JobsEnv) ownerJobBytes(owner uuid.UUID) int64 {
	e.t.Helper()
	s, err := store.SumStoredFiles(context.Background(), store.New(e.pool), owner, uuid.Nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return s.OwnerJobBytes
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestV1FilesUploadTypesLiveDB: the allowlisted types are stored under a content-derived name with
// the DETECTED type, unattached with the upload TTL; spoofed and unsupported content is 415
// unsupported_file_type and leaves nothing (no row, no reserved bytes) behind.
func TestV1FilesUploadTypesLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{UploadTTL: 30 * time.Minute})
	owner, uzc := e.user()

	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, textBytes(32)...)
	jpg := append([]byte{0xff, 0xd8, 0xff, 0xe0}, textBytes(32)...)
	zip := append([]byte{'P', 'K', 3, 4}, textBytes(32)...)
	const docx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	const xlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

	ok := []struct {
		name string
		data []byte
		o    v1UploadOpts
		want string
		ext  string
	}{
		{"pdf", pdfBytes(300), v1UploadOpts{filename: "brief.pdf", ctype: "application/pdf"}, "application/pdf", "pdf"},
		{"png", png, v1UploadOpts{filename: "chart.png"}, "image/png", "png"},
		{"jpeg", jpg, v1UploadOpts{filename: "photo.jpeg", ctype: "image/jpeg"}, "image/jpeg", "jpg"},
		{"docx", zip, v1UploadOpts{filename: "memo.docx"}, docx, "docx"},
		{"xlsx", zip, v1UploadOpts{filename: "data", ctype: xlsx}, xlsx, "xlsx"},
		{"txt", []byte("héllo\n"), v1UploadOpts{filename: "n.txt", ctype: "text/plain; charset=utf-8"}, "text/plain", "txt"},
		{"md", []byte("# t\n"), v1UploadOpts{filename: "n.md"}, "text/markdown", "md"},
		{"csv", []byte("a,b\n1,2\n"), v1UploadOpts{filename: "n.csv", ctype: "text/csv"}, "text/csv", "csv"},
		{"json", []byte(`{"a":[1,2]}`), v1UploadOpts{filename: "n.json", ctype: "application/json"}, "application/json", "json"},
	}
	for _, c := range ok {
		t.Run("accept "+c.name, func(t *testing.T) {
			f := e.uploadOK(uzc, c.data, c.o)
			sum := sha256Hex(c.data)
			if f.ContentType != c.want || f.StorageName != sum+"."+c.ext || f.Sha256 != sum ||
				f.ByteSize != int64(len(c.data)) || f.State != "unattached" || f.ExpiresAt == nil {
				t.Fatalf("file = %+v, want type %s name %s.%s", f, c.want, sum, c.ext)
			}
			if d := time.Until(*f.ExpiresAt); d < 25*time.Minute || d > 31*time.Minute {
				t.Errorf("expires_at is %v from now, want about the 30m upload TTL", d)
			}
			var product *uuid.UUID
			if err := e.pool.QueryRow(context.Background(), `SELECT product_id FROM job_files WHERE id = $1`, f.ID).Scan(&product); err != nil || product != nil {
				t.Errorf("a uzc_ upload recorded product %v (err %v), want NULL", product, err)
			}
		})
	}

	bad := []struct {
		name string
		data []byte
		o    v1UploadOpts
	}{
		{"png bytes named pdf", png, v1UploadOpts{filename: "a.pdf", ctype: "application/pdf"}},
		{"pdf bytes typed png", pdfBytes(64), v1UploadOpts{filename: "a.png", ctype: "image/png"}},
		{"pdf bytes named txt", pdfBytes(64), v1UploadOpts{filename: "a.txt"}},
		{"exe named pdf", append([]byte("MZ\x90\x00"), textBytes(60)...), v1UploadOpts{filename: "a.pdf", ctype: "application/pdf"}},
		{"elf named png", append([]byte("\x7fELF"), textBytes(60)...), v1UploadOpts{filename: "a.png"}},
		{"zip named zip", zip, v1UploadOpts{filename: "a.zip"}},
		{"html", []byte("<p>x</p>"), v1UploadOpts{filename: "a.html", ctype: "text/html"}},
		{"nul in text", []byte("ab\x00cd"), v1UploadOpts{filename: "a.txt"}},
		{"invalid utf8", []byte("ab\xff\xfecd"), v1UploadOpts{filename: "a.md"}},
		{"bad json", []byte(`{"a":`), v1UploadOpts{filename: "a.json"}},
		{"unknown extension", []byte("hello"), v1UploadOpts{filename: "a.exe"}},
	}
	before := e.ownerFileCount(owner)
	bytesBefore := e.ownerJobBytes(owner)
	for _, c := range bad {
		t.Run("refuse "+c.name, func(t *testing.T) {
			e.want(e.upload(uzc, c.data, c.o), http.StatusUnsupportedMediaType, "unsupported_file_type")
		})
	}
	if got := e.ownerFileCount(owner); got != before {
		t.Errorf("refused uploads left %d file rows, want none", got-before)
	}
	if got := e.ownerJobBytes(owner); got != bytesBefore {
		t.Errorf("refused uploads left reserved bytes: %d, want %d", got, bytesBefore)
	}
}

// TestV1FilesDisplayNameLiveDB: the stored display name is the uploader's file name reduced to its
// basename (both separators), free of control and format characters, bounded, and defaulted when
// nothing usable is left. The storage name never derives from it.
func TestV1FilesDisplayNameLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{})
	_, uzc := e.user()
	rlo, zw := string(rune(0x202e)), string(rune(0x200b))
	for _, c := range []struct {
		filename string
		data     []byte
		want     string
	}{
		{"../../etc/passwd", []byte("root"), "passwd"},
		{`C:\x\y.pdf`, pdfBytes(40), "y.pdf"},
		{"re" + rlo + "port.txt", []byte("hi"), "report.txt"},
		{"z" + zw + "ero.md", []byte("hi"), "zero.md"},
		{"", []byte("hi"), "upload"},
		{"dir/", []byte("hi"), "dir"}, // the multipart reader already takes the basename.
		{strings.Repeat("n", 400) + ".txt", []byte("hi"), strings.Repeat("n", v1FileDisplayNameMaxBytes)},
	} {
		f := e.uploadOK(uzc, c.data, v1UploadOpts{filename: c.filename})
		if f.DisplayName != c.want {
			t.Errorf("filename %q: display_name %q, want %q", c.filename, f.DisplayName, c.want)
		}
		if strings.Contains(f.StorageName, f.DisplayName) && f.DisplayName != "" && !strings.HasPrefix(f.StorageName, sha256Hex(c.data)) {
			t.Errorf("storage name %q is not content-derived", f.StorageName)
		}
	}
}

// TestV1FilesCapsAndDeclaredSizeLiveDB: a file exactly at the per-file cap is accepted and one byte
// over is 413; the reservation is the DECLARED size and any mismatch (short, long, digest) refuses
// the upload and gives the reservation back; the size header is required.
func TestV1FilesCapsAndDeclaredSizeLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{InputFileMaxBytes: 100})
	owner, uzc := e.user()

	e.uploadOK(uzc, textBytes(100), v1UploadOpts{filename: "at-cap.txt"})
	over := e.upload(uzc, textBytes(101), v1UploadOpts{filename: "over.txt"})
	e.want(over, http.StatusRequestEntityTooLarge, "file_too_large")
	if got := e.ownerFileCount(owner); got != 1 {
		t.Fatalf("file rows = %d after the refused over-cap upload, want the 1 accepted", got)
	}

	// A declaration over the cap is refused before any byte is read, even for a tiny body.
	e.want(e.upload(uzc, textBytes(10), v1UploadOpts{filename: "lie.txt", size: sizePtr(5000)}), http.StatusRequestEntityTooLarge, "file_too_large")

	held := e.ownerJobBytes(owner)
	for _, c := range []struct {
		name   string
		data   []byte
		o      v1UploadOpts
		status int
		reason string
	}{
		{"streamed longer than declared", textBytes(60), v1UploadOpts{filename: "a.txt", size: sizePtr(50)}, 422, "size_mismatch"},
		{"streamed shorter than declared", textBytes(40), v1UploadOpts{filename: "a.txt", size: sizePtr(50)}, 422, "size_mismatch"},
		{"sha256 mismatch", textBytes(50), v1UploadOpts{filename: "a.txt", sha: strings.Repeat("0", 64)}, 422, "sha256_mismatch"},
		{"missing size header", textBytes(50), v1UploadOpts{filename: "a.txt", size: sizePtr(-1)}, 422, "invalid_request"},
		{"malformed size header", textBytes(50), v1UploadOpts{filename: "a.txt", hdr: map[string]string{"X-Uzi-File-Size": "fifty"}}, 422, "invalid_request"},
		{"negative size header", textBytes(50), v1UploadOpts{filename: "a.txt", size: sizePtr(0), hdr: map[string]string{"X-Uzi-File-Size": "-5"}}, 422, "invalid_request"},
		{"zero size header", textBytes(50), v1UploadOpts{filename: "a.txt", size: sizePtr(0)}, 422, "empty_file"},
		{"uppercase sha256", textBytes(50), v1UploadOpts{filename: "a.txt", sha: strings.ToUpper(sha256Hex(textBytes(50)))}, 422, "invalid_request"},
		{"short sha256", textBytes(50), v1UploadOpts{filename: "a.txt", sha: "abcd"}, 422, "invalid_request"},
		{"no file part", textBytes(50), v1UploadOpts{filename: "a.txt", field: "other"}, 422, "invalid_request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.want(e.upload(uzc, c.data, c.o), c.status, c.reason)
		})
	}
	if got := e.ownerFileCount(owner); got != 1 {
		t.Errorf("file rows = %d after the refused uploads, want the 1 accepted", got)
	}
	if got := e.ownerJobBytes(owner); got != held {
		t.Errorf("owner job bytes = %d after refused uploads, want the prior %d: a reservation leaked", got, held)
	}
	// A correct digest is accepted.
	e.uploadOK(uzc, textBytes(50), v1UploadOpts{filename: "a.txt", sha: sha256Hex(textBytes(50))})

	// A body over the per-file cap plus the multipart overhead is refused outright.
	e.want(e.upload(uzc, textBytes(100+v1FileMultipartOverhead+1), v1UploadOpts{filename: "huge.txt", size: sizePtr(10)}), http.StatusRequestEntityTooLarge, "payload_too_large")
}

// TestV1FilesRemainingQuotaLiveDB: admission is at the DECLARED size against what is left of the
// owner's quota, not the per-file cap: a small file fits when the remaining quota is below the cap,
// the one that does not fit is 507, and another user is unaffected.
func TestV1FilesRemainingQuotaLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{InputFileMaxBytes: 60, PerOwnerBytes: 100})
	_, uzc := e.user()
	_, other := e.user()

	e.uploadOK(uzc, textBytes(60), v1UploadOpts{filename: "one.txt"})
	// 40 bytes remain, below the 60-byte per-file cap: a 30-byte file must still be admitted.
	e.uploadOK(uzc, textBytes(30), v1UploadOpts{filename: "two.txt"})
	r := e.upload(uzc, textBytes(20), v1UploadOpts{filename: "three.txt"})
	e.want(r, http.StatusInsufficientStorage, "storage_quota_exceeded")
	e.uploadOK(uzc, textBytes(10), v1UploadOpts{filename: "four.txt"})
	e.uploadOK(other, textBytes(60), v1UploadOpts{filename: "other.txt"})
}

// TestV1FilesRouterAuthLiveDB: through the production router, no credential, a cookie session and a
// uza_ token are RequireV1Caller's 401; a product token without jobs:run is 403 insufficient_scope;
// a uzc_ token and a uzp_ token with jobs:run succeed, and the uzp_ upload is recorded against its
// product.
func TestV1FilesRouterAuthLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{})
	owner, uzc := e.user()
	product := e.product(owner, "research")
	runTok := v1MintProductToken(t, e.h.q, owner, product, []string{producttoken.ScopeJobsRun}, nil)
	readTok := v1MintProductToken(t, e.h.q, owner, product, []string{producttoken.ScopeJobsRead}, nil)
	uza := cliMintToken(t, e.pool, cliSeedUser(t, e.pool, true), clitoken.ScopeAdminRO)
	jwt := cliMintJWT(t, e.pool, owner)
	data := []byte("hello")
	o := v1UploadOpts{filename: "a.txt"}

	const unauthorized = "{\"error\":\"invalid token\"}\n"
	if r := e.upload("", data, o); r.status != http.StatusUnauthorized || string(r.body) != unauthorized {
		t.Errorf("no credential: %d %q, want 401", r.status, r.body)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files", strings.NewReader("x"))
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only request cookie.
	req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
	rec := httptest.NewRecorder()
	e.routes.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Body.String() != unauthorized {
		t.Errorf("cookie session: %d %q, want 401", rec.Code, rec.Body.String())
	}
	if r := e.upload(uza, data, o); r.status != http.StatusUnauthorized || string(r.body) != unauthorized {
		t.Errorf("uza_ token: %d %q, want 401", r.status, r.body)
	}
	e.want(e.upload(readTok.token, data, o), http.StatusForbidden, "insufficient_scope")
	if got := e.ownerFileCount(owner); got != 0 {
		t.Fatalf("a refused caller stored %d files", got)
	}

	fUzc := e.uploadOK(uzc, data, o)
	fUzp := e.uploadOK(runTok.token, []byte("hello two"), o)
	for id, want := range map[string]*uuid.UUID{fUzc.ID: nil, fUzp.ID: &product} {
		var got *uuid.UUID
		if err := e.pool.QueryRow(context.Background(), `SELECT product_id FROM job_files WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("file %s product_id = %v, want %v", id, got, want)
		}
	}
}

// TestV1FilesUnwiredLiveDB: a service with no job-file store answers 503 files_unavailable on the
// upload and on a job create that names files, and never panics.
func TestV1FilesUnwiredLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	_, uzc := e.user()
	e.want(e.upload(uzc, []byte("hello"), v1UploadOpts{filename: "a.txt"}), http.StatusServiceUnavailable, "files_unavailable")
	e.want(e.call(http.MethodPost, "/api/v1/jobs", uzc,
		`{"type":"research","prompt":"p","input_file_ids":["`+uuid.NewString()+`"]}`), http.StatusServiceUnavailable, "files_unavailable")
}

func (e *v1JobsEnv) createWithFiles(bearer string, ids ...string) v1CallResult {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"type": "research", "prompt": "read the files", "title": "Files", "input_file_ids": ids})
	return e.call(http.MethodPost, "/api/v1/jobs", bearer, string(b))
}

type v1FileRow struct {
	state   string
	runID   *uuid.UUID
	expires *time.Time
}

func (e *v1JobsEnv) fileRow(id string) v1FileRow {
	e.t.Helper()
	var r v1FileRow
	if err := e.pool.QueryRow(context.Background(), `SELECT state, run_id, expires_at FROM job_files WHERE id = $1`, id).Scan(&r.state, &r.runID, &r.expires); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *v1JobsEnv) totalJobs(owner uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE user_id = $1 AND kind = 'job'`, owner).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// TestV1JobCreateInputFilesLiveDB: input_file_ids attaches uploaded files to the new job in the
// create transaction. Every id that cannot be attached (another user's, another product's, one
// uploaded with a user token by a product caller and the reverse, already attached, expired,
// duplicated) is 422 file_unavailable and creates no job and attaches nothing; the per-job caps
// are 413 with their reasons; inline inputs keep working beside files.
func TestV1JobCreateInputFilesLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{InputsMaxFiles: 3, InputsMaxBytes: 50})
	owner, uzc := e.user()
	_, otherUzc := e.user()
	pA, pB := e.product(owner, "research"), e.product(owner, "research")
	both := []string{producttoken.ScopeJobsRun, producttoken.ScopeJobsRead}
	tokA := v1MintProductToken(t, e.h.q, owner, pA, both, nil).token
	tokB := v1MintProductToken(t, e.h.q, owner, pB, both, nil).token

	f := func(bearer string, n int) string {
		return e.uploadOK(bearer, textBytes(n), v1UploadOpts{filename: "in.txt"}).ID
	}
	unavailable := func(name string, r v1CallResult) {
		t.Helper()
		if r.status != http.StatusUnprocessableEntity || r.reason() != "file_unavailable" {
			t.Errorf("%s: %d %s, want 422 file_unavailable", name, r.status, r.body)
		}
	}

	t.Run("happy path attaches every file and keeps inline inputs", func(t *testing.T) {
		a, b := f(uzc, 10), f(uzc, 12)
		body, _ := json.Marshal(map[string]any{
			"type": "research", "prompt": "p", "title": "T", "input_file_ids": []string{a, b},
			"inputs": []map[string]string{{"name": "note.txt", "content": "inline"}},
		})
		r := e.call(http.MethodPost, "/api/v1/jobs", uzc, string(body))
		if r.status != http.StatusCreated {
			t.Fatalf("create: %d %s", r.status, r.body)
		}
		var job apitypes.V1JobDTO
		r.decode(t, &job)
		for _, id := range []string{a, b} {
			row := e.fileRow(id)
			if row.state != "attached" || row.runID == nil || row.runID.String() != job.ID || row.expires != nil {
				t.Errorf("file %s = %+v, want attached to %s with no expiry", id, row, job.ID)
			}
		}
		var inline int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM job_inputs WHERE run_id = $1`, job.ID).Scan(&inline); err != nil || inline != 1 {
			t.Errorf("inline inputs = %d (err %v), want 1", inline, err)
		}
		// No files at all still works, as does an explicit null.
		e.create(uzc, v1MinimalJob)
		e.create(uzc, `{"type":"research","title":"T","prompt":"p","input_file_ids":null}`)
	})

	t.Run("another user's file", func(t *testing.T) {
		theirs := f(otherUzc, 10)
		before := e.totalJobs(owner)
		unavailable("cross-owner", e.createWithFiles(uzc, theirs))
		if row := e.fileRow(theirs); row.state != "unattached" {
			t.Errorf("another user's file is %s, want it untouched", row.state)
		}
		if e.totalJobs(owner) != before {
			t.Error("a refused create left a job behind")
		}
		// A malformed id and a random one read the same as an unknown file.
		e.want(e.createWithFiles(uzc, uuid.NewString()), http.StatusUnprocessableEntity, "file_unavailable")
		e.want(e.createWithFiles(uzc, "not-a-uuid"), http.StatusUnprocessableEntity, "invalid_request")
	})

	t.Run("cross-product and user-token boundaries", func(t *testing.T) {
		byB, byUzc, byA := f(tokB, 10), f(uzc, 10), f(tokA, 10)
		unavailable("A attaches B's file", e.createWithFiles(tokA, byB))
		unavailable("A attaches a uzc_ upload", e.createWithFiles(tokA, byUzc))
		unavailable("uzc_ attaches A's file", e.createWithFiles(uzc, byA))
		for _, id := range []string{byB, byUzc, byA} {
			if row := e.fileRow(id); row.state != "unattached" || row.runID != nil {
				t.Errorf("file %s = %+v after refused cross-boundary attaches, want untouched", id, row)
			}
		}
		if r := e.createWithFiles(tokA, byA); r.status != http.StatusCreated {
			t.Errorf("A attaching its own file: %d %s, want 201", r.status, r.body)
		}
	})

	t.Run("already attached, expired, duplicated", func(t *testing.T) {
		used := f(uzc, 10)
		if r := e.createWithFiles(uzc, used); r.status != http.StatusCreated {
			t.Fatalf("first attach: %d %s", r.status, r.body)
		}
		unavailable("attached twice", e.createWithFiles(uzc, used))

		stale := f(uzc, 10)
		e.exec(`UPDATE job_files SET expires_at = now() - interval '1 minute' WHERE id = $1`, stale)
		unavailable("expired by time", e.createWithFiles(uzc, stale))
		gone := f(uzc, 10)
		e.exec(`UPDATE job_files SET state = 'expired' WHERE id = $1`, gone)
		unavailable("expired state", e.createWithFiles(uzc, gone))

		dup := f(uzc, 10)
		unavailable("duplicate ids", e.createWithFiles(uzc, dup, dup))
		if row := e.fileRow(dup); row.state != "unattached" {
			t.Errorf("a duplicated id was attached: %+v", row)
		}
		// A partly valid list attaches nothing.
		ok := f(uzc, 10)
		unavailable("one bad id", e.createWithFiles(uzc, ok, stale))
		if row := e.fileRow(ok); row.state != "unattached" || row.runID != nil {
			t.Errorf("a valid file was attached by a create with a bad id: %+v", row)
		}
	})

	t.Run("per-job caps", func(t *testing.T) {
		ids := []string{f(uzc, 5), f(uzc, 5), f(uzc, 5), f(uzc, 5)}
		e.want(e.createWithFiles(uzc, ids...), http.StatusRequestEntityTooLarge, "too_many_files")
		big := []string{f(uzc, 30), f(uzc, 30)}
		before := e.totalJobs(owner)
		e.want(e.createWithFiles(uzc, big...), http.StatusRequestEntityTooLarge, "job_bytes_exceeded")
		for _, id := range append(ids, big...) {
			if row := e.fileRow(id); row.state != "unattached" || row.runID != nil {
				t.Errorf("file %s = %+v after a cap refusal, want the create rolled back", id, row)
			}
		}
		if e.totalJobs(owner) != before {
			t.Error("a cap refusal left a job behind")
		}
		// Exactly at the byte cap is fine: 30 + 20 = 50.
		if r := e.createWithFiles(uzc, f(uzc, 30), f(uzc, 20)); r.status != http.StatusCreated {
			t.Errorf("files exactly at the per-job byte cap: %d %s, want 201", r.status, r.body)
		}
	})
}
