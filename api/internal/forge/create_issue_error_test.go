package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestCreateIssueDefinitiveRejection(t *testing.T) {
	const token = "test-secret-value-123456"
	for _, driver := range []struct {
		name string
		new  func(*testing.T, int) Forge
	}{
		{"gitlab", func(t *testing.T, status int) Forge {
			m := newMockGitLab(t, map[string]http.HandlerFunc{
				"/api/v4/projects/7/issues": issueReject(status, token),
			})
			return newTestDriver(t, m, token)
		}},
		{"github", func(t *testing.T, status int) Forge {
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/issues": issueReject(status, token),
			})
			return newGitHubDriver(t, m, token)
		}},
		{"forgejo", func(t *testing.T, status int) Forge {
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/issues": issueReject(status, token),
			})
			return newForgejoDriver(t, m, token)
		}},
	} {
		for _, tc := range []struct {
			status     int
			definitive bool
		}{
			{400, true}, {401, true}, {403, false}, {408, false},
			{409, false}, {422, true}, {500, false},
		} {
			t.Run(fmt.Sprintf("%s/%d", driver.name, tc.status), func(t *testing.T) {
				d := driver.new(t, tc.status)
				_, err := d.CreateIssue(context.Background(), 7, "title", "body", nil)
				if err == nil {
					t.Fatal("expected create error")
				}
				if got := IsCreateIssueDefinitiveRejection(err); got != tc.definitive {
					t.Fatalf("definitive = %v, want %v: %v", got, tc.definitive, err)
				}
				if strings.Contains(err.Error(), token) || strings.Contains(fmt.Sprintf("%+v", err), token) {
					t.Fatal("create error leaked token")
				}
			})
		}
	}
}

func issueReject(status int, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"message":"rejected %s"}`, token)
	}
}

func TestCreateIssueClassificationRequiresPostResponse(t *testing.T) {
	if IsCreateIssueDefinitiveRejection(errors.New("status 422")) {
		t.Fatal("error text is not a definitive response")
	}
	for _, status := range []int{0, 403, 408, 409, 429, 500} {
		if IsCreateIssueDefinitiveRejection(createIssueError(status, errors.New("failure"))) {
			t.Fatalf("status %d must remain ambiguous", status)
		}
	}
}
