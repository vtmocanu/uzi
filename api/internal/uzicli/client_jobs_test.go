package uzicli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// jobServer answers every request with the given status, content type and body, recording
// the last request's method, raw path and query.
func jobServer(t *testing.T, status int, ctype, body string) (*HTTPClient, *[3]string, *string) {
	t.Helper()
	var seen [3]string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = [3]string{r.Method, r.URL.EscapedPath(), r.URL.RawQuery}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return newTestClient(srv), &seen, &gotBody
}

func TestHTTPJobRequests(t *testing.T) {
	ctx := context.Background()
	job := `{"id":"j/1","type":"research","status":"queued","title":"t","created_at":"2026-09-01T00:00:00Z"}`

	c, seen, body := jobServer(t, 201, "application/json", job)
	title := "T"
	if _, err := c.JobCreate(ctx, apitypes.V1JobCreateRequest{Type: "research", Prompt: "p", Title: &title}); err != nil {
		t.Fatal(err)
	}
	if *seen != [3]string{"POST", "/api/v1/jobs", ""} {
		t.Errorf("create request = %v", *seen)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(*body), &sent); err != nil || sent["type"] != "research" || sent["title"] != "T" {
		t.Errorf("create body = %s (%v)", *body, err)
	}
	if _, has := sent["wall_seconds"]; has {
		t.Errorf("unset wall_seconds was sent: %s", *body)
	}

	c, seen, _ = jobServer(t, 200, "application/json", job)
	got, err := c.JobGet(ctx, "a/b c")
	if err != nil || got.Type != "research" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if seen[1] != "/api/v1/jobs/a%2Fb%20c" {
		t.Errorf("id was not path-escaped: %q", seen[1])
	}

	c, seen, _ = jobServer(t, 200, "application/json", `{"job_status":"running","result":null}`)
	r, err := c.JobResult(ctx, "j1")
	if err != nil || r.JobStatus != "running" || r.Result != nil || seen[1] != "/api/v1/jobs/j1/result" {
		t.Fatalf("result = %+v, %v, %v", r, err, *seen)
	}

	c, seen, _ = jobServer(t, 200, "application/json", job)
	if _, err := c.JobCancel(ctx, "j1"); err != nil || *seen != [3]string{"POST", "/api/v1/jobs/j1/cancel", ""} {
		t.Fatalf("cancel: %v %v", err, *seen)
	}

	c, seen, _ = jobServer(t, 200, "application/json", `{"jobs":[],"next_cursor":null}`)
	if _, err := c.JobList(ctx, 7, "a b&c"); err != nil || seen[2] != "cursor=a+b%26c&limit=7" {
		t.Fatalf("list: %v %v", err, *seen)
	}
	if _, err := c.JobList(ctx, 0, ""); err != nil || seen[2] != "" {
		t.Fatalf("default list query = %q (%v)", seen[2], err)
	}
}

// A hostile or broken server: every one of these must be an *ExitError, never a panic and
// never a zero DTO mistaken for success.
func TestHTTPJobHostileResponses(t *testing.T) {
	ctx := context.Background()
	huge := `{"jobs":[],"pad":"` + strings.Repeat("a", maxRespBytes+10) + `"}`
	cases := map[string]struct {
		status int
		ctype  string
		body   string
		code   int
	}{
		"html 200":           {200, "text/html", "<html>proxy login</html>", ExitGeneric},
		"empty 200":          {200, "application/json", "", ExitGeneric},
		"wrong shape 200":    {200, "application/json", `["x"]`, ExitGeneric},
		"oversize 200":       {200, "application/json", huge, ExitGeneric},
		"html 502":           {502, "text/html", "<html>bad gateway</html>", ExitUnreachable},
		"404 typed":          {404, "application/json", `{"error":"not found","reason":"not_found"}`, ExitNotFound},
		"409 job_terminal":   {409, "application/json", `{"error":"job is finished","reason":"job_terminal"}`, ExitConflict},
		"403 scope":          {403, "application/json", `{"error":"missing scope","reason":"insufficient_scope"}`, ExitAuth},
		"422 not json":       {422, "text/plain", "nope", ExitUsage},
		"401 empty":          {401, "", "", ExitAuth},
		"429 retry-after":    {429, "application/json", `{"error":"slow down"}`, ExitUnreachable},
		"413 oversize error": {413, "application/json", `{"error":"too big","reason":"payload_too_large"}`, ExitUsage},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, _, _ := jobServer(t, tc.status, tc.ctype, tc.body)
			for verb, call := range map[string]func() error{
				"get":    func() error { _, err := c.JobGet(ctx, "j"); return err },
				"result": func() error { _, err := c.JobResult(ctx, "j"); return err },
				"cancel": func() error { _, err := c.JobCancel(ctx, "j"); return err },
				"list":   func() error { _, err := c.JobList(ctx, 0, ""); return err },
				"create": func() error {
					_, err := c.JobCreate(ctx, apitypes.V1JobCreateRequest{Type: "r", Prompt: "p"})
					return err
				},
			} {
				err := call()
				var ee *ExitError
				if !errors.As(err, &ee) || ee.Code != tc.code {
					t.Errorf("%s: err = %v, want ExitError code %d", verb, err, tc.code)
				}
			}
		})
	}
}

func TestHTTPJobTypedReasonSurvives(t *testing.T) {
	c, _, _ := jobServer(t, 409, "application/json", `{"error":"job is finished","reason":"job_terminal"}`)
	_, err := c.JobCancel(context.Background(), "j")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Reason != "job_terminal" {
		t.Fatalf("err = %v", err)
	}
}

// A 503 auth_unavailable from /api/v1 is a transient server fault (ExitUnreachable, retry
// with backoff), not a bad credential (ExitAuth); a plain 401 stays ExitAuth.
func TestHTTPJobAuthUnavailableIsUnreachableNotAuth(t *testing.T) {
	c, _, _ := jobServer(t, 503, "application/json", `{"error":"authentication temporarily unavailable","reason":"auth_unavailable"}`)
	_, err := c.JobGet(context.Background(), "j")
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if ee.Code != ExitUnreachable {
		t.Fatalf("503 auth_unavailable: Code = %d, want ExitUnreachable (%d)", ee.Code, ExitUnreachable)
	}
	if ee.Reason != "auth_unavailable" {
		t.Fatalf("Reason = %q, want auth_unavailable", ee.Reason)
	}

	c, _, _ = jobServer(t, 401, "application/json", `{"error":"invalid token"}`)
	_, err = c.JobGet(context.Background(), "j")
	if !errors.As(err, &ee) || ee.Code != ExitAuth {
		t.Fatalf("401 control: err = %v, want ExitAuth", err)
	}
}
