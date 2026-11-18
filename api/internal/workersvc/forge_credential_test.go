package workersvc

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"strings"
	"testing"
	"time"
)

func TestForgeCredentialClaimFence(t *testing.T) {
	worker := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	base := store.Run{UserID: worker.UserID, WorkerID: pgconv.UUID(worker.ID), RepoID: pgconv.UUID(uuid.New()), ClaimGeneration: 4, Status: "running"}
	if !forgeCredentialRunAllowed(base, worker, 4, false) {
		t.Fatal("live matching claim refused")
	}
	for _, tc := range []struct {
		name       string
		change     func(*store.Run)
		generation int64
		cancelled  bool
	}{
		{"missing_generation", func(*store.Run) {}, 0, false},
		{"stale_generation", func(*store.Run) {}, 3, false},
		{"cancel_requested", func(*store.Run) {}, 4, true},
		{"foreign_owner", func(r *store.Run) { r.UserID = uuid.New() }, 4, false},
		{"foreign_worker", func(r *store.Run) { r.WorkerID = pgconv.UUID(uuid.New()) }, 4, false},
		{"no_worker", func(r *store.Run) { r.WorkerID = pgtype.UUID{} }, 4, false},
		{"no_repo", func(r *store.Run) { r.RepoID = pgtype.UUID{} }, 4, false},
		{"isolated", func(r *store.Run) { r.EgressProfileID = pgconv.UUID(uuid.New()) }, 4, false},
		{"released", func(r *store.Run) { r.ClaimReleasedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true} }, 4, false},
		{"queued", func(r *store.Run) { r.Status = "queued" }, 4, false},
		{"completed", func(r *store.Run) { r.Status = "completed" }, 4, false},
		{"failed", func(r *store.Run) { r.Status = "failed" }, 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := base
			tc.change(&run)
			if forgeCredentialRunAllowed(run, worker, tc.generation, tc.cancelled) {
				t.Fatal("credential authority was not refused")
			}
		})
	}
	for _, status := range []string{"claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup"} {
		run := base
		run.Status = status
		if !forgeCredentialRunAllowed(run, worker, 4, false) {
			t.Fatalf("active state %s refused", status)
		}
	}
	if forgeExpiryPointer(time.Time{}) != nil {
		t.Fatal("PAT received App-only expiry metadata")
	}
}

func TestForgeCredentialRefreshReauthorizesAfterExchange(t *testing.T) {
	for _, change := range []string{"none", "generation", "cancelled", "released", "terminal", "connection", "foreign_owner", "foreign_worker"} {
		t.Run(change, func(t *testing.T) {
			worker := store.Worker{ID: uuid.New(), UserID: uuid.New()}
			runID := uuid.New()
			q := &fakeStore{runOwned: store.Run{ID: runID, UserID: worker.UserID, WorkerID: pgconv.UUID(worker.ID), RepoID: pgconv.UUID(uuid.New()), ClaimGeneration: 7, Status: "running"}, claimCtx: store.GetRunClaimContextRow{ForgeType: "github", BaseUrl: "https://github.com", ForgeProjectID: 42, TokenCiphertext: []byte("sealed-app-credential-canary")}}
			svc := New(q, nil, Params{})
			resolved := false
			token, expiry, err := svc.refreshForgeCredential(context.Background(), worker, runID, 7, func(_ context.Context, kind, base string, sealed []byte) (string, time.Time, error) {
				resolved = true
				if kind != "github" || base != "https://github.com" || string(sealed) != "sealed-app-credential-canary" {
					t.Fatal("credential coordinates changed")
				}
				switch change {
				case "generation":
					q.runOwned.ClaimGeneration++
				case "cancelled":
					q.cancellationRequested = true
				case "released":
					q.runOwned.ClaimReleasedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
				case "terminal":
					q.runOwned.Status = "completed"
				case "connection":
					q.claimCtx.TokenCiphertext = []byte("new-connection-canary")
				case "foreign_owner":
					q.runOwned.UserID = uuid.New()
				case "foreign_worker":
					q.runOwned.WorkerID = pgconv.UUID(uuid.New())
				}
				return "installation-result-canary", time.Now().Add(time.Hour), nil
			})
			if !resolved {
				t.Fatal("exchange was not exercised")
			}
			if change == "none" {
				if err != nil || token != "installation-result-canary" || expiry.IsZero() {
					t.Fatalf("valid refresh failed: %v", err)
				}
				return
			}
			if err == nil || token != "" || !expiry.IsZero() {
				t.Fatal("credential released after authorization changed")
			}
		})
	}
}

func TestForgeCredentialRefreshRejectsBeforeExchange(t *testing.T) {
	worker := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	q := &fakeStore{runOwned: store.Run{UserID: worker.UserID, WorkerID: pgconv.UUID(worker.ID), RepoID: pgconv.UUID(uuid.New()), ClaimGeneration: 5, Status: "running"}}
	svc := New(q, nil, Params{})
	called := false
	token, _, err := svc.refreshForgeCredential(context.Background(), worker, uuid.New(), 4, func(context.Context, string, string, []byte) (string, time.Time, error) {
		called = true
		return "should-not-release-canary", time.Now().Add(time.Hour), nil
	})
	if called || token != "" || !errors.Is(err, ErrRunNotOwned) {
		t.Fatal("stale claim reached credential exchange")
	}
}

func TestForgeAppPrivateKeyNeverBecomesGitCredential(t *testing.T) {
	box := newBox(t)
	material := `{"auth_type":"github_app","app_id":"1","installation_id":2,"private_key":"private-key-canary-only","repositories":["aifactory"]}`
	sealed, err := box.Seal([]byte(material))
	if err != nil {
		t.Fatal(err)
	}
	svc := New(&fakeStore{}, box, Params{})
	token, expiry, err := svc.openGitCredential(context.Background(), "github", "https://github.com", sealed)
	if err == nil || token != "" || !expiry.IsZero() {
		t.Fatal("App credential envelope escaped as a git token")
	}
	if strings.Contains(err.Error(), "private-key-canary") {
		t.Fatal("private key leaked in error")
	}
}

func TestGitCredentialUsernameKeepsBotAttributionSeparate(t *testing.T) {
	bot := "multica-bot[bot]"
	for _, tc := range []struct{ name, kind, token, want string }{
		{"installation", "github", "ghs_" + "installation-test-canary", "x-access-token"},
		{"classic_pat", "github", "ghp_" + "classic-test-canary", bot},
		{"legacy_opaque", "github", "opaque-test-canary", bot},
		{"gitlab", "gitlab", "ghs_" + "installation-test-canary", bot},
		{"forgejo", "forgejo", "ghs_" + "installation-test-canary", bot},
		{"empty", "github", "", bot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitCredentialUsername(tc.kind, bot, tc.token); got != tc.want {
				t.Fatalf("git auth username = %q, want %q", got, tc.want)
			}
		})
	}
}
