package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1909 M2 rework: the upload's resource bounds (concurrent-write slots, no connection before
// the first chunk, a read deadline), the error mapping of a broken body, and the attach-time
// stored-files lock. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

const stallBoundary = "stallboundary"

// stalledClient is one raw HTTP upload that sends its multipart prefix and `send` bytes of the
// file, then goes quiet, keeping the connection open: the shape of a stalled or slow uploader.
type stalledClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func openStalledUpload(t *testing.T, addr, bearer string, declared, send int) *stalledClient {
	t.Helper()
	prefix := "--" + stallBoundary + "\r\n" +
		`Content-Disposition: form-data; name="file"; filename="big.txt"` + "\r\n" +
		"Content-Type: text/plain\r\n\r\n"
	suffix := "\r\n--" + stallBoundary + "--\r\n"
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	hdr := fmt.Sprintf("POST /api/v1/files HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n"+
		"X-Uzi-File-Size: %d\r\nContent-Type: multipart/form-data; boundary=%s\r\nContent-Length: %d\r\n\r\n",
		addr, bearer, declared, stallBoundary, len(prefix)+declared+len(suffix))
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	// A write error is expected when the server refuses early and closes: the response is read next.
	_, _ = conn.Write([]byte(hdr + prefix))
	if send > 0 {
		_, _ = conn.Write(bytes.Repeat([]byte("a"), send))
	}
	return &stalledClient{conn: conn, br: bufio.NewReader(conn)}
}

// response reads the server's answer within wait, or reports that none came (the upload is still
// being held open).
func (c *stalledClient) response(wait time.Duration) (*http.Response, bool) {
	_ = c.conn.SetReadDeadline(time.Now().Add(wait))
	resp, err := http.ReadResponse(c.br, nil)
	if err != nil {
		return nil, false
	}
	return resp, true
}

func reasonOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var b struct{ Reason string }
	_ = json.NewDecoder(resp.Body).Decode(&b)
	return b.Reason
}

// TestV1FilesStalledUploadsBoundedLiveDB is the pool-exhaustion probe: many stalled uploads from one
// user, against a six-connection pool. Only the per-owner share (2) may stream, the rest are
// refused 503 uploads_busy with a Retry-After, an unrelated query and another user's upload still
// succeed, the process-wide cap (4) holds across users, and the slots come back when the stalled
// clients disconnect.
func TestV1FilesStalledUploadsBoundedLiveDB(t *testing.T) {
	e := newV1JobsEnvMax(t, 0, 6)
	e.wireFiles(workersvc.JobFileLimits{RequestDeadline: 60 * time.Second})
	srv := httptest.NewServer(e.routes)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	ownerA, uzcA := e.user()
	_, uzcB := e.user()
	_, uzcC := e.user()

	const big = 3 << 20 // declared: past one 1 MiB chunk, so the write is mid-stream when it stalls.
	var held []*stalledClient
	refused := 0
	for i := 0; i < 8; i++ {
		c := openStalledUpload(t, addr, uzcA, big, 1500<<10)
		resp, answered := c.response(400 * time.Millisecond)
		switch {
		case !answered:
			held = append(held, c)
		case resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") != "" && reasonOf(t, resp) == "uploads_busy":
			refused++
		default:
			t.Fatalf("stalled upload %d: unexpected answer %d", i, resp.StatusCode)
		}
	}
	if len(held) != 2 || refused != 6 {
		t.Fatalf("one user's stalled uploads: %d streaming, %d refused; want the per-owner share of 2 streaming and 6 refused", len(held), refused)
	}

	// The pool is not exhausted: an unrelated query and a small upload by another user succeed.
	qctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var one int
	if err := e.pool.QueryRow(qctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("an unrelated query failed while one user's uploads stall: %v", err)
	}
	e.uploadOK(uzcB, textBytes(20), v1UploadOpts{filename: "b.txt"})

	// Across users the process-wide cap of 4 holds: B takes two more, then C is refused.
	for i := 0; i < 2; i++ {
		c := openStalledUpload(t, addr, uzcB, big, 1500<<10)
		if resp, answered := c.response(400 * time.Millisecond); answered {
			t.Fatalf("user B's stalled upload %d answered %d, want it streaming", i, resp.StatusCode)
		}
		held = append(held, c)
	}
	e.want(e.upload(uzcC, textBytes(20), v1UploadOpts{filename: "c.txt"}), http.StatusServiceUnavailable, "uploads_busy")
	if err := e.pool.QueryRow(qctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("an unrelated query failed with four uploads streaming: %v", err)
	}

	// The stalled clients disconnect: their slots and reservations come back.
	for _, c := range held {
		_ = c.conn.Close()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := e.upload(uzcC, textBytes(20), v1UploadOpts{filename: "c.txt"})
		if r.status == http.StatusCreated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slots were not freed after the stalled clients disconnected: last answer %d %s", r.status, r.body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.eventually(func() bool { return e.reservedRows(ownerA) == 0 }, "a disconnected upload's reservation is released")
}

// TestV1FilesStalledUploadsHalfPoolLiveDB: on the smallest production pool (pgx's default floor of
// four connections) with the slots clamped as cmd/server clamps them (workersvc.ClampWriteSlots),
// stalled uploads from two users never hold more than half the pool, and an unrelated query plus
// another user's authenticated request still succeed.
func TestV1FilesStalledUploadsHalfPoolLiveDB(t *testing.T) {
	e := newV1JobsEnvMax(t, 0, 4)
	e.wireFiles(workersvc.ClampWriteSlots(e.pool.Config().MaxConns, workersvc.JobFileLimits{RequestDeadline: 60 * time.Second}))
	srv := httptest.NewServer(e.routes)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	_, uzcA := e.user()
	_, uzcB := e.user()
	_, uzcC := e.user()

	const big = 3 << 20
	var held []*stalledClient
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.conn.Close()
		}
	})
	for i, tok := range []string{uzcA, uzcA, uzcB, uzcB} {
		c := openStalledUpload(t, addr, tok, big, 1500<<10)
		resp, answered := c.response(400 * time.Millisecond)
		if answered {
			if resp.StatusCode != http.StatusServiceUnavailable || reasonOf(t, resp) != "uploads_busy" {
				t.Fatalf("stalled upload %d: unexpected answer %d", i, resp.StatusCode)
			}
			continue
		}
		held = append(held, c)
	}
	if len(held) != 2 {
		t.Fatalf("%d stalled uploads streaming on a 4-connection pool, want the clamped 2", len(held))
	}
	if n := e.pool.Stat().AcquiredConns(); n > 2 {
		t.Fatalf("%d of 4 pooled connections held by stalled uploads, want at most half", n)
	}
	qctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var one int
	if err := e.pool.QueryRow(qctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("an unrelated query failed while uploads stall: %v", err)
	}
	req, err := http.NewRequestWithContext(qctx, http.MethodGet, "http://"+addr+"/api/v1/jobs", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+uzcC)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("another user's authenticated request failed while uploads stall: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("another user's authenticated request answered %d, want 200", resp.StatusCode)
	}
}

// TestV1FilesStalledBeforeDataHoldsNoConnectionLiveDB: a client that sends the part header and then
// nothing must not hold a database connection: Write reads the first chunk before it begins its
// transaction.
func TestV1FilesStalledBeforeDataHoldsNoConnectionLiveDB(t *testing.T) {
	e := newV1JobsEnvMax(t, 0, 6)
	e.wireFiles(workersvc.JobFileLimits{RequestDeadline: 60 * time.Second})
	srv := httptest.NewServer(e.routes)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	_, uzcA := e.user()
	_, uzcB := e.user()

	for _, tok := range []string{uzcA, uzcA, uzcB, uzcB} {
		c := openStalledUpload(t, addr, tok, 1000, 0)
		if resp, answered := c.response(300 * time.Millisecond); answered {
			t.Fatalf("a stalled upload was answered %d, want it waiting for data", resp.StatusCode)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := e.pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("%d pooled connections are held by uploads that have sent no data, want 0", n)
	}
}

// TestV1FilesReadDeadlineLiveDB: a stalled client is cut off at the job-files request deadline with
// 408 request_timeout (not 500), and its slot and reservation are released. The deadline is the
// route's own read deadline (the test server has no ReadTimeout at all).
func TestV1FilesReadDeadlineLiveDB(t *testing.T) {
	e := newV1JobsEnvMax(t, 0, 6)
	e.wireFiles(workersvc.JobFileLimits{RequestDeadline: 700 * time.Millisecond})
	srv := httptest.NewServer(e.routes)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	owner, uzc := e.user()

	for _, send := range []int{0, 1500 << 10} { // stalled before any data, and mid-stream
		start := time.Now()
		c := openStalledUpload(t, addr, uzc, 3<<20, send)
		resp, answered := c.response(5 * time.Second)
		if !answered {
			t.Fatalf("send=%d: a stalled upload was not cut off within 5s of a 700ms deadline", send)
		}
		if took := time.Since(start); took < 500*time.Millisecond || took > 4*time.Second {
			t.Errorf("send=%d: cut off after %v, want about the 700ms deadline", send, took)
		}
		if resp.StatusCode != http.StatusRequestTimeout || reasonOf(t, resp) != "request_timeout" {
			t.Fatalf("send=%d: stalled upload answered %d, want 408 request_timeout", send, resp.StatusCode)
		}
	}
	// Both slots (the owner's share is 2) are free again, and nothing is left reserved.
	e.uploadOK(uzc, textBytes(20), v1UploadOpts{filename: "after.txt"})
	e.eventually(func() bool { return e.reservedRows(owner) == 0 }, "a timed-out upload's reservation is released")
}

// TestV1FilesLongUploadOutlivesServerTimeoutsLiveDB: the route's deadlines replace the server's
// 15 s ones, so an upload that takes longer than the server ReadTimeout and WriteTimeout still
// completes and is answered. The server timeouts are shortened to 1 s and the upload lasts 2 s.
func TestV1FilesLongUploadOutlivesServerTimeoutsLiveDB(t *testing.T) {
	e := newV1JobsEnvMax(t, 0, 6)
	e.wireFiles(workersvc.JobFileLimits{RequestDeadline: 30 * time.Second})
	srv := httptest.NewUnstartedServer(e.routes)
	srv.Config.ReadTimeout = time.Second
	srv.Config.WriteTimeout = time.Second
	srv.Start()
	t.Cleanup(srv.Close)
	_, uzc := e.user()

	pr, pw := io.Pipe()
	mwriter := func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("--" + stallBoundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"slow.txt\"\r\n" +
			"Content-Type: text/plain\r\n\r\nhello"))
		time.Sleep(2 * time.Second)
		_, _ = pw.Write([]byte("\r\n--" + stallBoundary + "--\r\n"))
	}
	go mwriter()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/files", pr)
	req.Header.Set("Authorization", "Bearer "+uzc)
	req.Header.Set("X-Uzi-File-Size", "5")
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+stallBoundary)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("a 2 s upload against 1 s server timeouts failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("a 2 s upload answered %d %s, want 201", resp.StatusCode, b)
	}
}

// TestV1FilesBodyErrorsLiveDB: an over-cap request body is 413 payload_too_large (not 422
// size_mismatch), and a body that breaks mid-upload is 408 for a deadline and 400 for a disconnect,
// each leaving no reservation behind.
func TestV1FilesBodyErrorsLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{InputFileMaxBytes: 10000})
	owner, uzc := e.user()

	// Chunked (no Content-Length to reject early); a 70000-byte field before the file plus the 10000
	// declared bytes pass the 10000 + 64 KiB cap while the file is being streamed.
	over := e.upload(uzc, textBytes(10000), v1UploadOpts{filename: "a.txt", chunked: true, preamble: 70000})
	e.want(over, http.StatusRequestEntityTooLarge, "payload_too_large")

	e.want(e.upload(uzc, textBytes(10), v1UploadOpts{filename: "a.txt", failBody: os.ErrDeadlineExceeded}), http.StatusRequestTimeout, "request_timeout")
	e.want(e.upload(uzc, textBytes(10), v1UploadOpts{filename: "a.txt", failBody: io.ErrClosedPipe}), http.StatusBadRequest, "invalid_request")
	if n := e.reservedRows(owner); n != 0 {
		t.Errorf("%d reservations left after refused uploads", n)
	}
}

// TestV1FilesRefusalDetailLiveDB: the 415 names the specific refusal in fixed server text and never
// echoes client input; an unknown extension names the allowed ones; POST /jobs' too_many_files
// message no longer says "size limit".
func TestV1FilesRefusalDetailLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{InputsMaxFiles: 1})
	_, uzc := e.user()

	cases := []struct {
		name string
		data []byte
		o    v1UploadOpts
		want string
	}{
		{"nul", []byte("ab\x00cd"), v1UploadOpts{filename: "a.txt"}, "text files must not contain NUL bytes"},
		{"utf8", []byte("ab\xff\xfecd"), v1UploadOpts{filename: "a.md"}, "text files must be valid UTF-8"},
		{"json", []byte(`{"a":`), v1UploadOpts{filename: "a.json"}, "declared as JSON but is not valid JSON"},
		{"disagree", []byte("a,b\n"), v1UploadOpts{filename: "a.md", ctype: "text/csv"}, "the declared type and the file extension disagree"},
		{"extension", []byte("hello"), v1UploadOpts{filename: "a.exe"}, "the allowed extensions are .csv, .docx, .jpeg, .jpg, .json, .markdown, .md, .pdf, .png, .txt, .xlsx"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := e.upload(uzc, c.data, c.o)
			e.want(r, http.StatusUnsupportedMediaType, "unsupported_file_type")
			if !strings.Contains(string(r.body), c.want) {
				t.Errorf("415 body %s does not say %q", r.body, c.want)
			}
		})
	}
	t.Run("client input is never echoed", func(t *testing.T) {
		r := e.upload(uzc, []byte("hello"), v1UploadOpts{filename: "x.qzqzqz", ctype: "application/x-zzzzzz"})
		e.want(r, http.StatusUnsupportedMediaType, "unsupported_file_type")
		if strings.Contains(string(r.body), "qzqzqz") || strings.Contains(string(r.body), "zzzzzz") {
			t.Errorf("the refusal echoes client input: %s", r.body)
		}
	})
	t.Run("too many files message", func(t *testing.T) {
		a := e.uploadOK(uzc, textBytes(5), v1UploadOpts{filename: "1.txt"})
		b := e.uploadOK(uzc, textBytes(5), v1UploadOpts{filename: "2.txt"})
		r := e.createWithFiles(uzc, a.ID, b.ID)
		e.want(r, http.StatusRequestEntityTooLarge, "too_many_files")
		if strings.Contains(string(r.body), "size limit") {
			t.Errorf("too_many_files says it is a size limit: %s", r.body)
		}
	})
}

// TestV1FilesLongNamesKeepTheirTypeLiveDB (B2): a long file name is shortened in its stem, keeps
// its extension, and is still typed by that extension, so a valid PDF is not refused.
func TestV1FilesLongNamesKeepTheirTypeLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{})
	_, uzc := e.user()
	for _, name := range []string{
		strings.Repeat("r", 196) + ".v2.pdf",
		strings.Repeat("報", 67) + ".pdf",
		strings.Repeat("x", 500) + ".pdf",
	} {
		f := e.uploadOK(uzc, pdfBytes(300), v1UploadOpts{filename: name, ctype: "application/pdf"})
		if !strings.HasSuffix(f.DisplayName, ".pdf") || len(f.DisplayName) > v1FileDisplayNameMaxBytes {
			t.Errorf("display name %q (%d bytes): want a .pdf name within %d bytes", f.DisplayName, len(f.DisplayName), v1FileDisplayNameMaxBytes)
		}
		f = e.uploadOK(uzc, pdfBytes(300), v1UploadOpts{filename: name})
		if f.ContentType != "application/pdf" {
			t.Errorf("a long %d-byte name typed the file %q", len(name), f.ContentType)
		}
	}
}

// TestV1FilesAliasesAndSizeHeaderLiveDB: the common type aliases are accepted, JSON with a byte
// order mark parses, and X-Uzi-File-Size takes plain decimal digits only.
func TestV1FilesAliasesAndSizeHeaderLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{})
	_, uzc := e.user()
	jpg := append([]byte{0xff, 0xd8, 0xff, 0xe0}, textBytes(32)...)
	ok := []struct {
		name string
		data []byte
		o    v1UploadOpts
		want string
	}{
		{"json with BOM", append([]byte("\xef\xbb\xbf"), `{"a":1}`...), v1UploadOpts{filename: "a.json", ctype: "application/json"}, "application/json"},
		{"excel type on a csv", []byte("a,b\n1,2\n"), v1UploadOpts{filename: "a.csv", ctype: "application/vnd.ms-excel"}, "text/csv"},
		{"text/x-markdown", []byte("# t\n"), v1UploadOpts{filename: "a.md", ctype: "text/x-markdown"}, "text/markdown"},
		{"image/jpg", jpg, v1UploadOpts{filename: "a.jpg", ctype: "image/jpg"}, "image/jpeg"},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			if f := e.uploadOK(uzc, c.data, c.o); f.ContentType != c.want {
				t.Errorf("stored as %q, want %q", f.ContentType, c.want)
			}
		})
	}
	// The excel alias is for a .csv only: on any other name it is still a mismatch.
	e.want(e.upload(uzc, []byte("a,b\n"), v1UploadOpts{filename: "a.txt", ctype: "application/vnd.ms-excel"}), http.StatusUnsupportedMediaType, "unsupported_file_type")
	for _, bad := range []string{"+5", "5 ", " 5", "0x5", "5.0", "-0", "1_0"} {
		e.want(e.upload(uzc, []byte("hello"), v1UploadOpts{filename: "a.txt", hdr: map[string]string{"X-Uzi-File-Size": bad}}), http.StatusUnprocessableEntity, "invalid_request")
	}
}

// TestV1JobCreateAttachHoldsStoredFilesLockLiveDB (N3): a job create that attaches files takes the
// stored-files locks (the reclaim and expiry passes rely on it), so it waits for a holder of those
// keys and proceeds when they are released.
func TestV1JobCreateAttachHoldsStoredFilesLockLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{})
	owner, uzc := e.user()
	f := e.uploadOK(uzc, textBytes(10), v1UploadOpts{filename: "in.txt"})

	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.LockStoredFiles(ctx, tx, owner); err != nil {
		t.Fatal(err)
	}

	done := make(chan v1CallResult, 1)
	go func() { done <- e.createWithFiles(uzc, f.ID) }()
	select {
	case r := <-done:
		t.Fatalf("a job create that attaches files completed (%d %s) while the stored-files locks were held elsewhere", r.status, r.body)
	case <-time.After(700 * time.Millisecond):
	}
	// A create with no files does not need the keys (another user's: the same user's create would
	// queue behind the blocked one on the job-create lock).
	_, otherUzc := e.user()
	e.create(otherUzc, v1MinimalJob)

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.status != http.StatusCreated {
			t.Fatalf("the create after the locks were released: %d %s, want 201", r.status, r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the create did not proceed after the stored-files locks were released")
	}
}

func (e *v1JobsEnv) reservedRows(owner uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM job_files WHERE user_id = $1 AND state = 'reserved'`, owner).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *v1JobsEnv) eventually(cond func() bool, what string) {
	e.t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting: %s", what)
}
