package slacksvc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubSlack serves the two Web API methods the Validator hits (auth.test,
// apps.connections.open). ok controls whether each returns success or an error
// envelope, and it records the presented token (slack-go sends it in the form
// body for some methods, as a Bearer header for others) so a test can assert the
// right token reached Slack regardless of transport.
func stubSlack(t *testing.T, ok bool) (*httptest.Server, *string) {
	t.Helper()
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if tok := r.FormValue("token"); tok != "" {
			gotToken = tok
		} else if h := r.Header.Get("Authorization"); h != "" {
			gotToken = strings.TrimPrefix(h, "Bearer ")
		}
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth.test"):
			_, _ = w.Write([]byte(`{"ok":true,"url":"https://x.slack.com/","team":"T","user":"bot","team_id":"T1","user_id":"U1","bot_id":"B1"}`))
		case strings.HasSuffix(r.URL.Path, "/apps.connections.open"):
			_, _ = w.Write([]byte(`{"ok":true,"url":"wss://wss-primary.slack.com/link/?ticket=abc"}`))
		default:
			t.Errorf("unexpected Slack method: %s", r.URL.Path)
			_, _ = w.Write([]byte(`{"ok":false,"error":"unknown_method"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &gotToken
}

func TestValidateBotToken(t *testing.T) {
	srv, gotToken := stubSlack(t, true)
	v := Validator{APIURL: srv.URL}
	if err := v.ValidateBotToken(context.Background(), "xoxb-good"); err != nil {
		t.Fatalf("ValidateBotToken(valid) = %v, want nil", err)
	}
	if *gotToken != "xoxb-good" {
		t.Errorf("bot token not presented to Slack; got %q", *gotToken)
	}

	bad, _ := stubSlack(t, false)
	vb := Validator{APIURL: bad.URL}
	if err := vb.ValidateBotToken(context.Background(), "xoxb-bad"); err == nil {
		t.Error("ValidateBotToken(rejected) = nil, want an error")
	}
}

func TestValidateAppToken(t *testing.T) {
	srv, gotToken := stubSlack(t, true)
	v := Validator{APIURL: srv.URL}
	if err := v.ValidateAppToken(context.Background(), "xapp-good"); err != nil {
		t.Fatalf("ValidateAppToken(valid) = %v, want nil", err)
	}
	if *gotToken != "xapp-good" {
		t.Errorf("app token not presented to Slack; got %q", *gotToken)
	}

	bad, _ := stubSlack(t, false)
	vb := Validator{APIURL: bad.URL}
	if err := vb.ValidateAppToken(context.Background(), "xapp-bad"); err == nil {
		t.Error("ValidateAppToken(rejected) = nil, want an error")
	}
}

// TestValidatorBoundedByTimeout proves the default client is bounded: against a
// server slower than the timeout, validation fails rather than hanging (reviewer
// M2 requirement — no http.DefaultClient fallback that could hang the admin PUT).
func TestValidatorBoundedByTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"url":"https://x/","team_id":"T","user_id":"U","bot_id":"B"}`))
	}))
	t.Cleanup(slow.Close)

	v := Validator{APIURL: slow.URL, Timeout: 20 * time.Millisecond}
	start := time.Now()
	err := v.ValidateBotToken(context.Background(), "xoxb-good")
	if err == nil {
		t.Fatal("ValidateBotToken did not time out against a slow server")
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("validation took %v; the bounded client should have cut it off near 20ms", elapsed)
	}
}
