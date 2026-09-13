package uzicli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestRecoveryArchivesDecodes: the summary read hits the owner path and decodes the DTO.
func TestRecoveryArchivesDecodes(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"supported":true,"legacy":false,"has_open_hold":false,` +
			`"counts":{"preparing":0,"uploading":0,"available":1,"needs_action":0,"expired":0,"discarded":0},` +
			`"archives":[{"id":"cap-1","run_id":"r1","state":"available","source_sha":"abcdef1234567890",` +
			`"byte_size":7,"checksum":"deadbeef","created_at":"2026-09-13T00:00:00Z"}]}`))
	}))
	defer srv.Close()

	sum, err := newTestClient(srv).RecoveryArchives(context.Background(), "r1")
	if err != nil {
		t.Fatalf("RecoveryArchives: %v", err)
	}
	if gotPath != "/api/runs/r1/archives" {
		t.Errorf("path = %q, want /api/runs/r1/archives", gotPath)
	}
	if gotAuth != "Bearer uzc_test" {
		t.Errorf("auth = %q, want Bearer uzc_test", gotAuth)
	}
	if !sum.Supported || len(sum.Archives) != 1 || sum.Archives[0].ID != "cap-1" {
		t.Errorf("decoded summary wrong: %+v", sum)
	}
}

// TestDownloadRecoveryArchiveStreams: a 200 octet-stream is copied verbatim to w and the
// byte count returned, off the JSON read path (no 32 MiB cap, no decode).
func TestDownloadRecoveryArchiveStreams(t *testing.T) {
	payload := []byte("PACK\x00\x01 raw git bundle bytes \xff\xfe")
	var gotPath, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	n, err := newTestClient(srv).DownloadRecoveryArchive(context.Background(), "r1", "cap-1", &buf)
	if err != nil {
		t.Fatalf("DownloadRecoveryArchive: %v", err)
	}
	if n != int64(len(payload)) || buf.String() != string(payload) {
		t.Errorf("streamed %d bytes %q, want %d %q", n, buf.String(), len(payload), payload)
	}
	if gotPath != "/api/runs/r1/archives/cap-1/download" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAccept != "application/octet-stream" {
		t.Errorf("Accept = %q, want application/octet-stream", gotAccept)
	}
}

// TestDownloadRecoveryArchiveStatusMapping: a non-2xx maps to the documented exit code before
// any byte reaches w.
func TestDownloadRecoveryArchiveStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"expired-409", http.StatusConflict, `{"error":"capture is not available"}`, ExitConflict},
		{"missing-404", http.StatusNotFound, `{"error":"capture not found"}`, ExitNotFound},
		{"integrity-422", http.StatusUnprocessableEntity, `{"error":"archive integrity check failed"}`, ExitUsage},
		{"server-500", http.StatusInternalServerError, `{"error":"internal error"}`, ExitUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			var buf bytes.Buffer
			n, err := newTestClient(srv).DownloadRecoveryArchive(context.Background(), "r1", "cap-1", &buf)
			if got := ExitCodeFor(err); got != tc.want {
				t.Errorf("exit = %d, want %d (err: %v)", got, tc.want, err)
			}
			if n != 0 || buf.Len() != 0 {
				t.Errorf("no bytes must reach w on a non-2xx; got n=%d buf=%q", n, buf.String())
			}
		})
	}
}

// TestDownloadRecoveryArchiveTruncatedStream: a 200 whose body is shorter than its declared
// Content-Length is an interrupted download — the client returns the bytes-so-far and an
// ExitUnreachable error, so the caller detects the short read.
func TestDownloadRecoveryArchiveTruncatedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "64") // promise 64...
		_, _ = w.Write([]byte("only-ten!!"))   // ...deliver 10, then the handler returns and closes.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	var buf bytes.Buffer
	_, err := newTestClient(srv).DownloadRecoveryArchive(context.Background(), "r1", "cap-1", &buf)
	if err == nil {
		t.Fatal("expected an interrupted-stream error on a truncated body")
	}
	if got := ExitCodeFor(err); got != ExitUnreachable {
		t.Errorf("exit = %d, want %d (unreachable) (err: %v)", got, ExitUnreachable, err)
	}
}

// TestDownloadRecoveryArchiveCredentialSafeBase: a plaintext non-loopback base URL is refused
// before the Bearer token is sent — the download goes through the same guard as every request.
func TestDownloadRecoveryArchiveCredentialSafeBase(t *testing.T) {
	c := &HTTPClient{BaseURL: "http://api.example.com", Token: "uzc_secret", HTTP: http.DefaultClient}
	var buf bytes.Buffer
	_, err := c.DownloadRecoveryArchive(context.Background(), "r1", "cap-1", &buf)
	if got := ExitCodeFor(err); got != ExitUsage {
		t.Fatalf("exit = %d, want %d (usage) (err: %v)", got, ExitUsage, err)
	}
	if err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("expected a plaintext-URL refusal, got %v", err)
	}
}
