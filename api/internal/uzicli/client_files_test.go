package uzicli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// PRD #1909 M7: the file and product-skills client methods' requests and status mapping.

func TestHTTPFileRequests(t *testing.T) {
	ctx := context.Background()

	c, seen, _ := jobServer(t, 200, "application/json", `{"files":[],"refused_files":[]}`)
	if _, err := c.JobFiles(ctx, "a/b"); err != nil || *seen != [3]string{"GET", "/api/v1/jobs/a%2Fb/files", ""} {
		t.Errorf("files = %v, %v", *seen, err)
	}

	c, seen, _ = jobServer(t, 200, "application/json", `{"config":{},"applied":{"skills":[]},"staged":null}`)
	if d, err := c.AdminProductSkills(ctx, "p/1"); err != nil || d.Staged != nil || *seen != [3]string{"GET", "/api/admin/products/p%2F1/skills", ""} {
		t.Errorf("skills = %+v %v, %v", d, *seen, err)
	}
}

func TestHTTPUploadFileHeadersAndStatuses(t *testing.T) {
	ctx := context.Background()
	var size, sum, ctype string
	var filename, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size, sum, ctype = r.Header.Get("X-Uzi-File-Size"), r.Header.Get("X-Uzi-File-Sha256"), r.Header.Get("Content-Type")
		mr, _ := r.MultipartReader()
		p, _ := mr.NextPart()
		filename = p.FileName()
		b, _ := io.ReadAll(p)
		body = string(b)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"f1","state":"unattached"}`))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	f, err := c.UploadFile(ctx, "n.txt", 5, strings.Repeat("a", 64), strings.NewReader("hello"))
	if err != nil || f.ID != "f1" {
		t.Fatalf("upload = %+v, %v", f, err)
	}
	if size != "5" || sum != strings.Repeat("a", 64) || !strings.HasPrefix(ctype, "multipart/form-data") || filename != "n.txt" || body != "hello" {
		t.Errorf("size=%q sha=%q ctype=%q name=%q body=%q", size, sum, ctype, filename, body)
	}

	for status, want := range map[int]struct {
		code int
		msg  string
	}{
		413: {ExitUsage, "too big"}, 415: {ExitUsage, "unsupported file type"},
		422: {ExitUsage, "too big"}, 507: {ExitGeneric, "storage quota exceeded"},
	} {
		c, _, _ := jobServer(t, status, "application/json", `{"error":"too big","reason":"r"}`)
		_, err := c.UploadFile(ctx, "n.txt", 5, "", strings.NewReader("hello"))
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != want.code || !strings.Contains(ee.Error(), want.msg) {
			t.Errorf("status %d: err = %v", status, err)
		} else if (status == 415 || status == 507) && ee.Reason != "r" {
			t.Errorf("status %d: Reason = %q, want r", status, ee.Reason)
		}
	}
}

func TestHTTPDownloadFile(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/files/gone" {
			w.WriteHeader(410)
			_, _ = w.Write([]byte(`{"error":"gone","reason":"file_expired"}`))
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="abc.txt"`)
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	dl, err := c.DownloadFile(ctx, "ok")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(dl.Body)
	_ = dl.Body.Close()
	if string(b) != "data" || dl.StorageName != "abc.txt" || dl.Size != 4 {
		t.Errorf("download = %q %q %d", b, dl.StorageName, dl.Size)
	}
	_, err = c.DownloadFile(ctx, "gone")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitNotFound || !strings.Contains(ee.Error(), "file expired") {
		t.Errorf("410 err = %v", err)
	}
	if ee.Reason != "file_expired" {
		t.Errorf("410 Reason = %q, want file_expired", ee.Reason)
	}
}
