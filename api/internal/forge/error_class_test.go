package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

type unknownClassError struct{}

func (unknownClassError) Error() string     { return "untrusted" }
func (unknownClassError) Class() ErrorClass { return ErrorClass("untrusted-class-canary") }

func assertClassifiedSafe(t *testing.T, err error, want ErrorClass, token string) {
	t.Helper()
	if err == nil || Class(fmt.Errorf("safe context: %w", err)) != want {
		t.Fatalf("class = %q, want %q (error %v)", Class(err), want, err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("error retained canary token")
	}
	var githubError *gh.ErrorResponse
	var primary *gh.RateLimitError
	var abuse *gh.AbuseRateLimitError
	var gitlabError *gitlab.ErrorResponse
	if errors.As(err, &githubError) || errors.As(err, &primary) ||
		errors.As(err, &abuse) || errors.As(err, &gitlabError) {
		t.Fatal("raw SDK error recoverable")
	}
	// Only safe neutral wrappers may remain; their final cause is chainless.
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if strings.Contains(cause.Error(), token) {
			t.Fatal("unwrap retained canary token")
		}
		if _, safeRate := cause.(*RateLimitError); !safeRate && errors.Unwrap(cause) != nil {
			t.Fatal("classified error retained original chain")
		}
	}
}

func TestErrorClassReader(t *testing.T) {
	for _, err := range []error{nil, errors.New("429 is only text"), unknownClassError{},
		fmt.Errorf("safe wrap: %w", unknownClassError{})} {
		if got := Class(err); got != ErrorClassOther {
			t.Fatalf("Class = %q, want other", got)
		}
	}
	if got := Class(&RateLimitError{}); got != ErrorClassRateLimited {
		t.Fatalf("neutral rate class = %q", got)
	}
}

func TestDriverIssueErrorClasses(t *testing.T) {
	// Recognizable CANARY/deadbeef fixtures assembled at runtime; no complete
	// provider token appears in source.
	tokens := map[Type]string{
		TypeGitHub:  strings.Join([]string{"ghp_", "CANARY", strings.Repeat("A", 30)}, ""),
		TypeGitLab:  strings.Join([]string{"glpat-", "CANARY", strings.Repeat("B", 14)}, ""),
		TypeForgejo: strings.Join([]string{"deadbeef", strings.Repeat("c", 32)}, ""),
	}
	cases := []struct {
		name   string
		status int
		rate   string
		want   ErrorClass
	}{
		{"unauthorized", 401, "", ErrorClassAuth},
		{"permission", 403, "", ErrorClassAuth},
		{"server", 503, "", ErrorClassServerError},
		{"rate", 429, "", ErrorClassRateLimited},
		{"primary", 403, "primary", ErrorClassRateLimited},
		{"abuse", 403, "abuse", ErrorClassRateLimited},
		{"other", 422, "", ErrorClassOther},
		{"timeout", 0, "", ErrorClassTimeout},
		{"client_timeout", 0, "", ErrorClassTimeout},
	}
	for _, provider := range []Type{TypeGitHub, TypeGitLab, TypeForgejo} {
		for _, tc := range cases {
			if tc.rate != "" && provider != TypeGitHub {
				continue
			}
			t.Run(string(provider)+"/"+tc.name, func(t *testing.T) {
				token := tokens[provider]
				var requests atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if strings.Contains(r.URL.Path, "/repositories/7") {
						_ = json.NewEncoder(w).Encode(map[string]any{
							"id": 7, "name": "widgets", "owner": map[string]any{"login": "acme"},
						})
						return
					}
					requests.Add(1)
					if tc.status == 0 {
						<-r.Context().Done()
						return
					}
					if tc.rate == "primary" {
						w.Header().Set("X-RateLimit-Remaining", "0")
						w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
					}
					if tc.rate == "abuse" {
						w.Header().Set("Retry-After", "1")
					}
					w.WriteHeader(tc.status)
					payload := map[string]string{"message": "CANARY echo " + token}
					if tc.rate == "abuse" {
						payload["documentation_url"] = "https://docs.github.com/rest/overview/resources-in-the-rest-api#abuse-rate-limits"
					}
					_ = json.NewEncoder(w).Encode(payload)
				}))
				defer srv.Close()
				timeout := time.Second
				if tc.name == "client_timeout" {
					timeout = 30 * time.Millisecond
				}
				d, err := NewWithGitLabBackoff(provider, srv.URL, token, timeout,
					func(_, _ time.Duration, _ int, _ *http.Response) time.Duration { return 0 })
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				if tc.name == "timeout" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
					defer cancel()
				}
				_, err = d.ListIssues(ctx, 7, ListIssuesOptions{})
				assertClassifiedSafe(t, err, tc.want, token)
				if tc.status != 0 && !strings.Contains(err.Error(), redactPlaceholder) {
					t.Fatal("echoed token was not replaced")
				}
				if provider == TypeGitLab && (tc.status == 503 || tc.status == 429) && requests.Load() <= 1 {
					t.Fatal("fixture did not exercise SDK retries")
				}
			})
		}
	}
}

func TestForgejoResponseErrorClasses(t *testing.T) {
	token := strings.Join([]string{"deadbeef", strings.Repeat("d", 32)}, "")
	for _, route := range []string{"resolve", "get", "patch"} {
		for _, status := range []int{401, 403, 429, 503} {
			t.Run(fmt.Sprintf("%s/%d", route, status), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if route != "resolve" && strings.Contains(r.URL.Path, "/repositories/7") {
						_, _ = w.Write([]byte("{\"id\":7,\"name\":\"widgets\",\"owner\":{\"login\":\"acme\"}}"))
						return
					}
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(map[string]string{"message": "CANARY echo " + token})
				}))
				defer srv.Close()
				d, err := New(TypeForgejo, srv.URL, token, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				switch route {
				case "resolve":
					_, err = d.ListIssues(context.Background(), 7, ListIssuesOptions{})
				case "get":
					_, err = d.ListIssueLabelEvents(context.Background(), 7, 1)
				case "patch":
					err = d.UpdateIssueDescription(context.Background(), 7, 1, "description")
				}
				want := ErrorClassAuth
				if status == 429 {
					want = ErrorClassRateLimited
				}
				if status == 503 {
					want = ErrorClassServerError
				}
				assertClassifiedSafe(t, err, want, token)
				if !strings.Contains(err.Error(), redactPlaceholder) {
					t.Fatal("missing scrubbed echo")
				}
			})
		}
	}
}

func TestForgejoTruncatedResponseErrorClasses(t *testing.T) {
	token := strings.Join([]string{"deadbeef", "CANARY", strings.Repeat("d", 26)}, "")
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		for _, tc := range []struct {
			status int
			want   ErrorClass
		}{
			{401, ErrorClassAuth},
			{429, ErrorClassRateLimited},
			{503, ErrorClassServerError},
		} {
			t.Run(fmt.Sprintf("%s/%d", method, tc.status), func(t *testing.T) {
				var requests atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repositories/7" {
						_, _ = w.Write([]byte("{\"id\":7,\"name\":\"widgets\",\"owner\":{\"login\":\"acme\"}}"))
						return
					}
					if r.Method != method {
						t.Errorf("method = %s, want %s", r.Method, method)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					requests.Add(1)
					body := "CANARY echo " + token
					// Closing the response short of its declared length makes the
					// real HTTP client's body read fail with unexpected EOF.
					w.Header().Set("Content-Length", fmt.Sprint(len(body)+1))
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(body))
				}))
				defer srv.Close()
				d, err := New(TypeForgejo, srv.URL, token, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				if method == http.MethodGet {
					_, err = d.ListIssueLabelEvents(context.Background(), 7, 1)
				} else {
					err = d.UpdateIssueDescription(context.Background(), 7, 1, "description")
				}
				if requests.Load() != 1 {
					t.Fatalf("target requests = %d, want 1", requests.Load())
				}
				wantMessage := "forgejo: read response: unexpected EOF"
				if method == http.MethodPatch {
					wantMessage = "forgejo: update issue description: unexpected EOF"
				}
				if err == nil || err.Error() != wantMessage {
					t.Fatalf("error = %v, want %q", err, wantMessage)
				}
				assertClassifiedSafe(t, err, tc.want, token)
				if errors.Unwrap(err) != nil {
					t.Fatal("classified error retained original read error")
				}
			})
		}
	}
}

func TestRawTypedErrorSevered(t *testing.T) {
	token := strings.Join([]string{"glpat-", "CANARY", strings.Repeat("E", 14)}, "")
	response := &http.Response{StatusCode: 503, Request: &http.Request{Header: http.Header{"Private-Token": {token}}}}
	raw := &gitlab.ErrorResponse{StatusCode: 503, Response: response, Body: []byte(token), Message: token}
	err := (&gitLab{redact: newRedactor(token)}).wrapErr("list issues", raw)
	assertClassifiedSafe(t, err, ErrorClassServerError, token)
	if errors.Unwrap(err) != nil {
		t.Fatal("original response retained via unwrap")
	}
	if Class(newRedactor(token).error(fmt.Errorf("status 401: %s", token))) != ErrorClassOther {
		t.Fatal("classification parsed message text")
	}
}
