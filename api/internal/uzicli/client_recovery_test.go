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

// TestRecoveryHoldsDecodes: the owner-wide holds read hits /api/recovery/holds and decodes
// the aggregate + hold rows (PRD #1349 M5).
func TestRecoveryHoldsDecodes(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"aggregate":{"open_holds":2,"custody_hold_limit":8,"decision_needed":1,"blocked_runs":0},` +
			`"holds":[{"id":"h1","run_id":"r1","generation":1,"state":"open","attention":"source_only",` +
			`"worker_id":"w1","worker_name":"alpha","has_available_capture":false,` +
			`"created_at":"2026-09-13T00:00:00Z","updated_at":"2026-09-13T00:00:00Z"}]}`))
	}))
	defer srv.Close()

	got, err := newTestClient(srv).RecoveryHolds(context.Background())
	if err != nil {
		t.Fatalf("RecoveryHolds: %v", err)
	}
	if gotPath != "/api/recovery/holds" {
		t.Errorf("path = %q, want /api/recovery/holds", gotPath)
	}
	if gotAuth != "Bearer uzc_test" {
		t.Errorf("auth = %q, want Bearer uzc_test", gotAuth)
	}
	if got.Aggregate.OpenHolds != 2 || got.Aggregate.CustodyHoldLimit != 8 || got.Aggregate.DecisionNeeded != 1 {
		t.Errorf("aggregate = %+v, want open 2 / limit 8 / decision 1", got.Aggregate)
	}
	if len(got.Holds) != 1 || got.Holds[0].ID != "h1" || got.Holds[0].Attention != "source_only" {
		t.Errorf("holds = %+v, want the single source_only hold h1", got.Holds)
	}
}

// TestDiscardRecoveryHoldSendsConfirm: the discard hits DELETE
// /api/runs/{run}/recovery-holds/{hold} and ALWAYS carries ?confirm=discard (the server's only
// mutating form), and a 200 is success (PRD #1349 M5).
func TestDiscardRecoveryHoldSendsConfirm(t *testing.T) {
	var gotMethod, gotPath, gotConfirm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotConfirm = r.URL.Query().Get("confirm")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"discarded":true}`))
	}))
	defer srv.Close()

	if err := newTestClient(srv).DiscardRecoveryHold(context.Background(), "r1", "h1"); err != nil {
		t.Fatalf("DiscardRecoveryHold: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/runs/r1/recovery-holds/h1" {
		t.Errorf("request = %s %s, want DELETE /api/runs/r1/recovery-holds/h1", gotMethod, gotPath)
	}
	if gotConfirm != "discard" {
		t.Errorf("confirm query = %q, want discard (the server's only mutating form)", gotConfirm)
	}
}

// TestDiscardRecoveryHoldNotFoundMapsExit: a 404 (a foreign/absent/already-settled hold) maps
// to ExitNotFound, so `uzi run discard` reports "not found" rather than a generic failure.
func TestDiscardRecoveryHoldNotFoundMapsExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"custody hold not found"}`))
	}))
	defer srv.Close()

	err := newTestClient(srv).DiscardRecoveryHold(context.Background(), "r1", "h1")
	if got := ExitCodeFor(err); got != ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found) (err: %v)", got, ExitNotFound, err)
	}
}
