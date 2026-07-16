package apiclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPollSendsBearerOnAGetAndParsesDesiredState(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"workers":[{"id":"w1","template":"base","size":"s","generation":2,"join_token":"uzw_t"}]}`)
	}))
	defer srv.Close()

	resp, err := New(srv.URL, "the-token", 5*time.Second).Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if gotAuth != "Bearer the-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotPath != "/api/controller/poll" {
		t.Fatalf("path = %q", gotPath)
	}
	// A GET with no body: the poll is a pure read, and the ack that once justified a
	// POST is gone (delivery is proved by the worker's own registration).
	if gotMethod != http.MethodGet {
		t.Fatalf("method = %q, want GET", gotMethod)
	}
	if len(gotBody) != 0 {
		t.Fatalf("request body = %q, want none", gotBody)
	}
	if len(resp.Workers) != 1 || resp.Workers[0].ID != "w1" || resp.Workers[0].Generation != 2 {
		t.Fatalf("workers = %+v", resp.Workers)
	}
	if resp.Workers[0].JoinToken == nil || *resp.Workers[0].JoinToken != "uzw_t" {
		t.Fatalf("join token = %v", resp.Workers[0].JoinToken)
	}
}

// A non-2xx must be an error, never an empty fleet — the controller reads desired
// state as authoritative, so a silently-empty 401 would read as "delete everything".
func TestPollTreatsNon200AsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid controller token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	resp, err := New(srv.URL, "wrong", 5*time.Second).Poll(context.Background())
	if err == nil {
		t.Fatal("want an error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want it to name the status", err)
	}
	if len(resp.Workers) != 0 {
		t.Fatal("a failed poll must carry no desired state")
	}
}

// The response body can contain join-token plaintext, so a decode failure must not
// quote it — json's own error messages include the offending input.
func TestPollDecodeErrorDoesNotLeakTheBody(t *testing.T) {
	const secret = "uzw_super-secret-plaintext"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"workers":[{"join_token":"`+secret+`"`) // truncated, unparseable
	}))
	defer srv.Close()

	_, err := New(srv.URL, "t", 5*time.Second).Poll(context.Background())
	if err == nil {
		t.Fatal("want a decode error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("decode error leaked the token plaintext: %v", err)
	}
}
