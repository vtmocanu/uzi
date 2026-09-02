package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPublishWorkflowScopeRejectedSkipsCleanly pins PRD #456 M4 at the service seam: when
// the go-git broker reports ErrWorkflowScopeRejected (the branch is behind on
// .github/workflows/** and the bot's repo-only PAT cannot push the checkpoint), Publish
// returns a BENIGN skip — Published=false, Skipped="workflow_scope", Ref set, and a NIL
// error — so it never reaches the 5xx/slog.Error default arm and never fails the run.
// Checkpoints stay best-effort; the finalize base-align (M1) is the real safety net.
func TestPublishWorkflowScopeRejectedSkipsCleanly(t *testing.T) {
	box := newBox(t)
	sealed, err := box.Seal([]byte("pat"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	fs := &fakeStore{
		// A running ISSUE run the worker owns, with its issue iid set but an empty branch
		// column (the mid-run shape) — the service derives agent/issue-<iid> from the iid.
		runOwned: store.Run{
			Kind:     runkind.Issue,
			IssueIid: pgtype.Int8{Int64: 456, Valid: true},
			Branch:   pgtype.Text{},
		},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl:      "https://gitlab.example.com/team/repo",
			DefaultBranch:   pgtype.Text{String: "main", Valid: true},
			BaseUrl:         "https://gitlab.example.com",
			BotUsername:     "uzi-bot",
			TokenCiphertext: sealed,
		},
	}
	svc := New(fs, box, testParams())
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://gitlab.example.com" })
	// The broker rejects the push for a missing workflow scope.
	svc.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		return pushbroker.Result{}, pushbroker.ErrWorkflowScopeRejected
	})

	res, err := svc.Publish(context.Background(), worker(), uuid.New(), "0123456789abcdef0123456789abcdef01234567", []byte("pack"))
	if err != nil {
		t.Fatalf("Publish returned a non-nil error for a workflow-scope rejection: %v (must be a benign skip, not a run failure)", err)
	}
	if res.Published {
		t.Errorf("Published = true, want false (the checkpoint push was rejected)")
	}
	if res.Skipped != "workflow_scope" {
		t.Errorf("Skipped = %q, want %q", res.Skipped, "workflow_scope")
	}
	if res.Ref != "refs/uzi-checkpoints/agent/issue-456" {
		t.Errorf("Ref = %q, want the server-derived checkpoint ref", res.Ref)
	}
}
