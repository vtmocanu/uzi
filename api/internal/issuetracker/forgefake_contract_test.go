package issuetracker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/issuetrackertest"
)

// forgeFakeProjectID is the project id of the Node fake's first project.
const forgeFakeProjectID = 1

// forgeFakeOtherProjectID is a project id the Node fake does not have. Under
// FORGE_FAKE_ISSUE_CONTRACT=1 the fake answers 404 for it instead of falling
// back to its first project, which is what the ProjectGuard case relies on.
const forgeFakeOtherProjectID = 4242

// TestIssueTrackerForgeFakeContracts runs the shared issue tracker contract
// against the real GitLab, GitHub and Forgejo drivers over the Node HTTPS fake
// (e2e/forge-fake/forge-fake.mjs). The launcher
// e2e/forge-fake/issue-tracker-contracts.test.mjs starts the fake with
// FORGE_FAKE_ISSUE_CONTRACT=1 and this process with SSL_CERT_FILE set before
// TLS roots load.
func TestIssueTrackerForgeFakeContracts(t *testing.T) {
	switch os.Getenv("UZI_FORGE_FAKE_REQUIRED") {
	case "":
		t.Skip("standalone contract requires task test:issue-tracker-forge-fake-contract")
	case "1":
	default:
		t.Fatal("invalid required-mode marker")
	}
	endpoint := os.Getenv("UZI_FORGE_FAKE_ENDPOINT")
	if endpoint == "" {
		t.Fatal("UZI_FORGE_FAKE_ENDPOINT is required when UZI_FORGE_FAKE_REQUIRED=1")
	}
	for _, dialect := range []forge.Type{forge.TypeGitLab, forge.TypeGitHub, forge.TypeForgejo} {
		t.Run(string(dialect), func(t *testing.T) {
			issuetrackertest.RunContract(t, func(t *testing.T) issuetrackertest.Subject {
				// The fake does not check auth; the value only has to be non-empty.
				driver, err := forge.New(dialect, endpoint, "contract-"+"fixture", 5*time.Second)
				if err != nil {
					t.Fatalf("forge.New(%s): %v", dialect, err)
				}
				return issuetrackertest.Subject{
					IssueTracker:   driver,
					ProjectID:      forgeFakeProjectID,
					OtherProjectID: forgeFakeOtherProjectID,
					SeedHumanComment: func(t *testing.T, iid int64, username, body string) {
						postFake(t, endpoint, fmt.Sprintf("/_e2e/issues/%d/comments", iid),
							map[string]any{"username": username, "body": body})
					},
					SeedLabelEvent: func(t *testing.T, iid int64, action, label, username string) {
						postFake(t, endpoint, fmt.Sprintf("/_e2e/issues/%d/label-events", iid),
							map[string]any{"action": action, "label": label, "username": username})
					},
				}
			})
		})
	}
}

// postFake POSTs a JSON body to one of the fake's /_e2e mutators and fails the
// test unless it answers 201.
func postFake(t *testing.T, endpoint, path string, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(raw)) //nolint:gosec // G704: endpoint is the local httptest fake's URL, never user input
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: request targets the local httptest fake only
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s = %d, want 201", path, resp.StatusCode)
	}
}
