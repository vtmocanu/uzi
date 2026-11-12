package issuetracker

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

func TestReadBounded(t *testing.T) {
	got, err := readBounded(strings.NewReader("hello"), 5, forge.ResponseSuccess, 200)
	if err != nil || string(got) != "hello" {
		t.Fatalf("at limit: %q, %v", got, err)
	}
	_, err = readBounded(strings.NewReader("hello!"), 5, forge.ResponseIssueSuccess, 200)
	if !errors.Is(err, forge.ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	if !forge.IsIssueResponseTooLarge(err) {
		t.Fatal("provenance not issue success")
	}
	_, err = readBounded(strings.NewReader("hello!"), 5, forge.ResponseError, 200)
	if forge.IsIssueResponseTooLarge(err) {
		t.Fatal("error provenance reported as issue success")
	}
}

func TestReadBoundedCapsDecompressedBytes(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(bytes.Repeat([]byte("a"), 64<<10)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if gz.Len() > 1<<10 {
		t.Fatalf("fixture not small: %d", gz.Len())
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz.Bytes())
	}))
	defer srv.Close()

	resp, err := newHTTPClient(0, nil).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength != -1 {
		t.Fatalf("declared length %d", resp.ContentLength)
	}
	_, err = readBounded(resp.Body, 1<<10, forge.ResponseIssueSuccess, 200)
	if !forge.IsIssueResponseTooLarge(err) {
		t.Fatalf("decompressed overflow not capped: %v", err)
	}
}

func TestReadBoundedOversizeClassFromStatus(t *testing.T) {
	_, err := readBounded(strings.NewReader("hello!"), 5, forge.ResponseError, 503)
	if got := forge.Class(err); got != forge.ErrorClassServerError {
		t.Fatalf("Class = %q, want server_error", got)
	}
	r := newRedactor(fixtureCredential())
	if got := forge.Class(r.errorStatus(&forge.ResponseTooLargeError{Provenance: forge.ResponseError}, 503)); got != forge.ErrorClassServerError {
		t.Fatalf("redacted Class = %q, want server_error", got)
	}
	bare := fmt.Errorf("x: %w", forge.ErrResponseTooLarge)
	if got := r.errorStatus(bare, 429); !errors.Is(got, forge.ErrResponseTooLarge) || forge.Class(got) != forge.ErrorClassRateLimited {
		t.Fatalf("bare oversize: %v class %q", got, forge.Class(got))
	}
}

func TestReadBoundedOverflowBeatsReadError(t *testing.T) {
	r := io.MultiReader(strings.NewReader("hello!"), iotest.ErrReader(errors.New("late")))
	if _, err := readBounded(r, 5, forge.ResponseIssueSuccess, 200); !forge.IsIssueResponseTooLarge(err) {
		t.Fatalf("err = %v", err)
	}
}
