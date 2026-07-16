package apiclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestPollSendsBearerAndAcksAndParsesDesiredState(t *testing.T) {
	var gotAuth, gotPath, gotContentType string
	var gotBody protocol.PollRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"workers":[{"id":"w1","template":"base","size":"s","generation":2,"join_token":"uzw_t"}]}`)
	}))
	defer srv.Close()

	resp, err := New(srv.URL, "the-token", 5*time.Second).Poll(context.Background(), []string{"w0"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if gotAuth != "Bearer the-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotPath != "/api/controller/poll" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q", gotContentType)
	}
	if len(gotBody.Materialized) != 1 || gotBody.Materialized[0] != "w0" {
		t.Fatalf("materialized = %v", gotBody.Materialized)
	}
	if len(resp.Workers) != 1 || resp.Workers[0].ID != "w1" || resp.Workers[0].Generation != 2 {
		t.Fatalf("workers = %+v", resp.Workers)
	}
	if resp.Workers[0].JoinToken == nil || *resp.Workers[0].JoinToken != "uzw_t" {
		t.Fatalf("join token = %v", resp.Workers[0].JoinToken)
	}
}

// A nil ack list must go out as [], not null: "I acked nothing" is a real
// statement the api acts on, and it should look like one on the wire.
func TestPollMarshalsNilAcksAsEmptyList(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		_, _ = io.WriteString(w, `{"workers":[]}`)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "t", 5*time.Second).Poll(context.Background(), nil); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !strings.Contains(raw, `"materialized":[]`) {
		t.Fatalf("body = %s, want an explicit empty ack list", raw)
	}
}

// A non-2xx must be an error, never an empty fleet — the controller reads desired
// state as authoritative, so a silently-empty 401 would read as "delete everything".
func TestPollTreatsNon200AsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid controller token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	resp, err := New(srv.URL, "wrong", 5*time.Second).Poll(context.Background(), nil)
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

	_, err := New(srv.URL, "t", 5*time.Second).Poll(context.Background(), nil)
	if err == nil {
		t.Fatal("want a decode error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("decode error leaked the token plaintext: %v", err)
	}
}
