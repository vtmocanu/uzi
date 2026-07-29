package uzicli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
)

// GenerateVerifier must produce a challenge the SERVER will accept: the server
// computes base64url(sha256(<verifier string>)) and compares it to the stored
// challenge (handler/cli_auth_flow.go s256Challenge). Recompute it here and require
// an exact match, so a future encoding drift (padding, std vs url alphabet) fails
// loudly instead of only at a live login.
func TestGenerateVerifierMatchesServerS256(t *testing.T) {
	verifier, challenge, err := GenerateVerifier()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Fatalf("challenge = %q, want base64url(sha256(verifier)) = %q", challenge, want)
	}
	// Two calls must differ (fresh entropy each time).
	v2, _, _ := GenerateVerifier()
	if v2 == verifier {
		t.Fatal("two GenerateVerifier calls returned the same verifier")
	}
}

// StartCLIAuth sends {code_challenge, client_desc} and decodes
// {request_id, user_code, expires_in, interval}.
func TestStartCLIAuthWireShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/auth/cli/start" {
			t.Errorf("start: %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var body struct {
			CodeChallenge string `json:"code_challenge"`
			ClientDesc    string `json:"client_desc"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode start body: %v", err)
		}
		if body.CodeChallenge != "the-challenge" || body.ClientDesc != "my-laptop" {
			t.Errorf("start body = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"request_id":"req-1","user_code":"ABCD-1234","expires_in":300,"interval":5}`)
	}))
	defer srv.Close()

	// A login client carries no token yet (start is unauthenticated).
	c := &HTTPClient{BaseURL: srv.URL, HTTP: srv.Client()}
	got, err := c.StartCLIAuth(context.Background(), "the-challenge", "my-laptop")
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestID != "req-1" || got.UserCode != "ABCD-1234" || got.ExpiresIn != 300 || got.Interval != 5 {
		t.Fatalf("start result = %+v", got)
	}
}

// PollCLIAuth classifies 202/200/410 into pending/authorized/terminal, and sends
// {request_id, verifier} as a POST (the verifier must never ride a GET query).
func TestPollCLIAuthStatusMapping(t *testing.T) {
	t.Run("202 pending", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/auth/cli/poll" {
				t.Errorf("poll: %s %s", r.Method, r.URL.Path)
			}
			var body struct {
				RequestID string `json:"request_id"`
				Verifier  string `json:"verifier"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.RequestID != "req-1" || body.Verifier != "ver-1" {
				t.Errorf("poll body = %+v", body)
			}
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"status":"pending"}`)
		}))
		defer srv.Close()
		res, err := (&HTTPClient{BaseURL: srv.URL, HTTP: srv.Client()}).PollCLIAuth(context.Background(), "req-1", "ver-1")
		if err != nil || res.Status != CLIAuthPending {
			t.Fatalf("res=%+v err=%v, want pending", res, err)
		}
	})

	t.Run("200 authorized", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"token":"uzc_minted","user":{"id":"u1","email":"a@b.c"}}`)
		}))
		defer srv.Close()
		res, err := (&HTTPClient{BaseURL: srv.URL, HTTP: srv.Client()}).PollCLIAuth(context.Background(), "r", "v")
		if err != nil || res.Status != CLIAuthAuthorized {
			t.Fatalf("res=%+v err=%v, want authorized", res, err)
		}
		if res.Token != "uzc_minted" || res.User.Email != "a@b.c" {
			t.Fatalf("authorized payload = %+v", res)
		}
	})

	t.Run("410 terminal carries reason", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusGone)
			io.WriteString(w, `{"status":"denied"}`)
		}))
		defer srv.Close()
		res, err := (&HTTPClient{BaseURL: srv.URL, HTTP: srv.Client()}).PollCLIAuth(context.Background(), "r", "v")
		if err != nil {
			t.Fatalf("410 should not be an error (it is a terminal status): %v", err)
		}
		if res.Status != CLIAuthTerminal || res.Reason != "denied" {
			t.Fatalf("terminal res = %+v, want denied", res)
		}
	})
}

// CreateRun posts {issue_iid} to /api/repos/{id}/runs and decodes {run}.
func TestCreateRunWireShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/repos/p1/runs" {
			t.Errorf("create: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			IssueIID int64 `json:"issue_iid"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.IssueIID != 42 {
			t.Errorf("issue_iid = %d, want 42", body.IssueIID)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"run":{"id":"run-1","status":"queued","kind":"issue"}}`)
	}))
	defer srv.Close()
	run, err := newTestClient(srv).CreateRun(context.Background(), "p1", 42, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != "run-1" || run.Status != "queued" {
		t.Fatalf("run = %+v", run)
	}
}

// SubmitRunInput posts {kind, body, selection} to /api/runs/{id}/inputs. A nil
// selection must serialize as an explicit null (the field is not omitempty), and a
// non-nil selection must ride as {source, exclusions}.
func TestSubmitRunInputWireShape(t *testing.T) {
	t.Run("cancel, no selection", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/runs/r1/inputs" {
				t.Errorf("path = %s", r.URL.Path)
			}
			raw, _ := io.ReadAll(r.Body)
			var body map[string]json.RawMessage
			json.Unmarshal(raw, &body)
			if string(body["kind"]) != `"cancel"` {
				t.Errorf("kind = %s, want cancel", body["kind"])
			}
			if string(body["selection"]) != "null" {
				t.Errorf("selection = %s, want null", body["selection"])
			}
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"server_side":true}`)
		}))
		defer srv.Close()
		res, err := newTestClient(srv).SubmitRunInput(context.Background(), "r1", "cancel", "", nil)
		if err != nil || !res.ServerSide {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})

	t.Run("approve with selection", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body apitypes.RunInputRequest
			json.NewDecoder(r.Body).Decode(&body)
			if body.Kind != "approve_plan" || body.Selection == nil {
				t.Fatalf("body = %+v", body)
			}
			if body.Selection.Source != "own" || len(body.Selection.Exclusions) != 1 || body.Selection.Exclusions[0] != "tester" {
				t.Errorf("selection = %+v", body.Selection)
			}
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"server_side":false}`)
		}))
		defer srv.Close()
		sel := &apitypes.AgentSelection{Source: "own", Exclusions: []string{"tester"}}
		_, err := newTestClient(srv).SubmitRunInput(context.Background(), "r1", "approve_plan", "", sel)
		if err != nil {
			t.Fatal(err)
		}
	})
}
