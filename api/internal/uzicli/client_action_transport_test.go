package uzicli

import (
	"context"
	"encoding/json"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestActionTransportGuidanceNeverFetchesRun(t *testing.T) {
	gets, posts := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gets++
			t.Errorf("transport must POST only; got %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		posts++
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"guidance":"Narrow scope"}` || r.Header.Get("If-Match") != "" {
			t.Errorf("header=%q body=%s", r.Header.Get("If-Match"), body)
		}
		_, _ = w.Write([]byte(`{"run":{"status":"queued"}}`))
	}))
	defer srv.Close()
	_, err := newTestClient(srv).ResumeRunNowWithOptions(context.Background(), "r1", RunResumeOptions{Guidance: "Narrow scope"})
	if err != nil || gets != 0 || posts != 1 {
		t.Fatalf("err=%v gets=%d posts=%d", err, gets, posts)
	}
}

func TestActionTransportExplicitBindings(t *testing.T) {
	token := `"captured-hold"`
	ctx := context.Background()
	type action struct {
		name, path, body string
		call             func(Client, string) error
	}
	actions := []action{
		{"resume-empty", "/resume-now", "", func(c Client, token string) error {
			_, e := c.ResumeRunNowWithOptions(ctx, "r1", RunResumeOptions{ActionIfMatch: token})
			return e
		}},
		{"resume-guidance", "/resume-now", `{"guidance":"Narrow scope"}`, func(c Client, token string) error {
			_, e := c.ResumeRunNowWithOptions(ctx, "r1", RunResumeOptions{Guidance: "Narrow scope", ActionIfMatch: token})
			return e
		}},
		{"resume-preserve", "/resume-now", `{"mode":"preserve"}`, func(c Client, token string) error {
			_, e := c.ResumeRunNowWithOptions(ctx, "r1", RunResumeOptions{Mode: "preserve", ActionIfMatch: token})
			return e
		}},
		{"resume-fresh", "/resume-now", `{"guidance":"Narrow scope","mode":"fresh"}`, func(c Client, token string) error {
			_, e := c.ResumeRunNowWithOptions(ctx, "r1", RunResumeOptions{Mode: "fresh", Guidance: "Narrow scope", ActionIfMatch: token})
			return e
		}},
		{"credential", "/credential", `{"mode":"pinned","secret_id":"s1"}`, func(c Client, token string) error {
			_, _, e := c.SetRunCredential(ctx, "r1", SetRunCredentialOverride{Mode: "pinned", SecretID: "s1", ActionIfMatch: token})
			return e
		}},
		{"credential-auto", "/credential", `{"mode":"auto"}`, func(c Client, token string) error {
			_, _, e := c.SetRunCredential(ctx, "r1", SetRunCredentialOverride{Mode: "auto", ActionIfMatch: token})
			return e
		}},
		{"stop", "/inputs", `{"kind":"cancel","body":"","selection":null,"override_capabilities":false}`, func(c Client, token string) error {
			_, e := c.SubmitRunInputWithOptions(ctx, "r1", RunInputOptions{Kind: "cancel", ActionIfMatch: token})
			return e
		}},
		{"continue", "/completion/decision", `{"decision":"continue","guidance":"Narrow scope"}`, func(c Client, token string) error {
			_, e := c.ContinueCompletionDecisionWithOptions(ctx, "r1", "Narrow scope", RunActionOptions{ActionIfMatch: token})
			return e
		}},
		{"partial", "/completion/decision", `{"decision":"partial","guidance":"","keep":["m1"],"reason":"scope","contract_revision":2}`, func(c Client, token string) error {
			_, e := c.PartialCompletionDecisionWithOptions(ctx, "r1", []string{"m1"}, "scope", 2, RunActionOptions{ActionIfMatch: token})
			return e
		}},
		{"accept", "/completion/decision", `{"decision":"accept","guidance":"","criteria":["c1"],"reason":"scope","contract_revision":2}`, func(c Client, token string) error {
			_, e := c.AcceptCompletionDecisionWithOptions(ctx, "r1", []string{"c1"}, "scope", 2, RunActionOptions{ActionIfMatch: token})
			return e
		}},
	}
	for _, a := range actions {
		for _, supplied := range []string{"", token} {
			t.Run(a.name+"/"+supplied, func(t *testing.T) {
				gets, posts := 0, 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost {
						gets++
						t.Errorf("unexpected %s", r.Method)
						w.WriteHeader(405)
						return
					}
					posts++
					body, _ := io.ReadAll(r.Body)
					if r.URL.Path != "/api/runs/r1"+a.path || string(body) != a.body || r.Header.Get("If-Match") != supplied {
						t.Errorf("path=%s header=%q body=%s", r.URL.Path, r.Header.Get("If-Match"), body)
					}
					// The current hold changes after each write. Both stale submissions
					// must keep the captured token; this transport makes no retry.
					w.Header().Set("ETag", `"replacement-hold"`)
					if posts > 1 {
						w.WriteHeader(409)
						_, _ = w.Write([]byte(`{"error":"hold changed"}`))
						return
					}
					_, _ = w.Write([]byte(`{"run":{"status":"queued"}}`))
				}))
				defer srv.Close()
				c := newTestClient(srv)
				if err := a.call(c, supplied); err != nil {
					t.Fatal(err)
				}
				// Two explicit duplicate submissions, each bounded to one POST.
				for i := 0; i < 2; i++ {
					if err := a.call(c, supplied); ExitCodeFor(err) != ExitConflict {
						t.Fatalf("conflict=%v", err)
					}
				}
				if gets != 0 || posts != 3 {
					t.Fatalf("gets=%d posts=%d", gets, posts)
				}
			})
		}
	}
}

func TestActionTransportFakeRecordsBindings(t *testing.T) {
	ctx := context.Background()
	token := `"captured"`
	f := &FakeClient{Err: Exitf(ExitConflict, "changed")}
	resume := RunResumeOptions{Guidance: "same", Mode: "fresh", ActionIfMatch: token}
	_, _ = f.ResumeRunNowWithOptions(ctx, "r1", resume)
	if f.LastResumeOptions != resume {
		t.Fatalf("resume=%+v", f.LastResumeOptions)
	}
	override := SetRunCredentialOverride{Mode: "auto", ActionIfMatch: token}
	_, _, _ = f.SetRunCredential(ctx, "r1", override)
	if f.LastSetTokenOverride == nil || *f.LastSetTokenOverride != override {
		t.Fatalf("override=%+v", f.LastSetTokenOverride)
	}
	_, _ = f.SubmitRunInputWithOptions(ctx, "r1", RunInputOptions{Kind: "cancel", ActionIfMatch: token})
	if f.LastInputOptions.ActionIfMatch != token {
		t.Fatal("input token lost")
	}
	opts := RunActionOptions{ActionIfMatch: token}
	calls := []func(){
		func() { _, _ = f.ContinueCompletionDecisionWithOptions(ctx, "r1", "same", opts) },
		func() { _, _ = f.PartialCompletionDecisionWithOptions(ctx, "r1", []string{"m1"}, "scope", 2, opts) },
		func() { _, _ = f.AcceptCompletionDecisionWithOptions(ctx, "r1", []string{"c1"}, "scope", 2, opts) },
	}
	for _, call := range calls {
		call()
		if f.LastDecideOptions != opts || f.LastDecideRunID != "r1" {
			t.Fatal("decision binding lost")
		}
	}
	_, _ = f.ContinueCompletionDecision(ctx, "r1", "")
	if f.LastDecideOptions.ActionIfMatch != "" {
		t.Fatal("legacy decision reused token")
	}
	for _, verb := range f.RunVerbCalls {
		if verb == "get_run" {
			t.Fatalf("fake fetched run: %s", verb)
		}
	}
}

func TestActionTransportMetadataExcludedFromJSON(t *testing.T) {
	for _, value := range []any{
		RunResumeOptions{ActionIfMatch: "transport-only"},
		SetRunCredentialOverride{ActionIfMatch: "transport-only"},
		RunActionOptions{ActionIfMatch: "transport-only"},
		apitypes.RunInputRequest{ActionIfMatch: "transport-only"},
	} {
		b, err := json.Marshal(value)
		if err != nil || strings.Contains(string(b), "transport-only") || strings.Contains(string(b), "ActionIfMatch") || strings.Contains(string(b), "action_if_match") {
			t.Fatalf("metadata leaked: %s err=%v", b, err)
		}
	}
}

func TestActionTransportAuthAndUnprocessableError(t *testing.T) {
	posts := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		cookie, err := r.Cookie("session")
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs/r1/credential" ||
			r.Header.Get("Authorization") != "Bearer owner-fixture" ||
			r.Header.Get("If-Match") != `"captured"` ||
			err != nil || cookie.Value != "cookie-fixture" {
			t.Errorf("method=%s path=%s auth=%q match=%q cookie=%v err=%v", r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("If-Match"), cookie, err)
		}
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":"Codex credential override unsupported"}`))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	c.Token = "owner-fixture"
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "cookie-fixture", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}})
	c.HTTP.Jar = jar
	_, _, err = c.SetRunCredential(context.Background(), "r1", SetRunCredentialOverride{Mode: "auto", ActionIfMatch: `"captured"`})
	if ExitCodeFor(err) != ExitUsage || posts != 1 {
		t.Fatalf("err=%v posts=%d", err, posts)
	}
}
