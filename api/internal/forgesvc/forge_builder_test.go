package forgesvc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

func TestForgeForConnectionUsesInstanceGitLabBackoff(t *testing.T) {
	const token = "glpat-" + "fake-retry-secret-0123456789"
	var requests int
	var backoffCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("PRIVATE-TOKEN"); got != token {
			t.Errorf("PRIVATE-TOKEN = %q", got)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, token)
	}))
	defer server.Close()

	box, err := secretbox.New([]byte("0123456789abcdefghijklmnopqrstuv"))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewWithForgeBuilder(nil, box, 5*time.Second, nil, func(kind forge.Type, baseURL, pat string, timeout time.Duration) (forge.Forge, error) {
		return forge.NewWithGitLabBackoff(kind, baseURL, pat, timeout,
			func(min, max time.Duration, attempt int, resp *http.Response) time.Duration {
				backoffCalls.Add(1)
				if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
					return retryablehttp.DefaultBackoff(min, max, attempt, resp)
				}
				return 0
			})
	})
	sealed, err := svc.EncryptToken(token)
	if err != nil {
		t.Fatal(err)
	}
	client, err := svc.ForgeForConnection(string(forge.TypeGitLab), server.URL, sealed)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.VerifyToken(context.Background())
	if err == nil {
		t.Fatal("VerifyToken succeeded against HTTP 500")
	}
	if requests != 6 {
		t.Errorf("requests = %d, want 6", requests)
	}
	if got := backoffCalls.Load(); got != 5 {
		t.Errorf("backoff calls = %d, want 5", got)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error contains plaintext token: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("error did not redact echoed token: %v", err)
	}
}
