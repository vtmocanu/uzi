package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestAutoDismissDeniedCLIRecommendations exercises the PostReview hook's deterministic
// net (issue #167) directly, without a DB — the live-DB integration test only runs when
// UZI_TEST_DATABASE_URL is set, so this covers the category filter, the RationaleHash
// stamping, the coordinate that gets dismissed, and the best-effort error swallow that
// plain `go test ./...` otherwise never reaches. It calls the unexported
// autoDismissDeniedCLIRecommendations against fakeStore.systemDismissed, the recording the
// fake's SystemDismissDeniedCLIRecommendation writes.
func TestAutoDismissDeniedCLIRecommendations(t *testing.T) {
	reviewID := uuid.New()

	t.Run("in-scope denied rec is dismissed", func(t *testing.T) {
		fs := &fakeStore{}
		svc := New(fs, newBox(t), testParams())
		rec := ReviewRecommendation{
			Category:    "install_worker_tool",
			Target:      "glab",
			RationaleMd: "worker needs glab to file MRs",
		}
		svc.autoDismissDeniedCLIRecommendations(context.Background(), reviewID, []ReviewRecommendation{rec})

		if len(fs.systemDismissed) != 1 {
			t.Fatalf("expected exactly one SystemDismiss call, got %d", len(fs.systemDismissed))
		}
		got := fs.systemDismissed[0]
		if got.ReviewID != reviewID {
			t.Errorf("ReviewID = %v, want %v", got.ReviewID, reviewID)
		}
		if got.Category != rec.Category {
			t.Errorf("Category = %q, want %q", got.Category, rec.Category)
		}
		if got.Target != rec.Target {
			t.Errorf("Target = %q, want %q", got.Target, rec.Target)
		}
		if want := RationaleHash(rec.RationaleMd); got.RationaleHash != want {
			t.Errorf("RationaleHash = %q, want %q", got.RationaleHash, want)
		}
	})

	t.Run("in-scope mixed target is dismissed", func(t *testing.T) {
		fs := &fakeStore{}
		svc := New(fs, newBox(t), testParams())
		rec := ReviewRecommendation{
			Category:    "enable_tool",
			Target:      "file, glab",
			RationaleMd: "edit a file and run glab",
		}
		svc.autoDismissDeniedCLIRecommendations(context.Background(), reviewID, []ReviewRecommendation{rec})

		if len(fs.systemDismissed) != 1 {
			t.Fatalf("expected exactly one SystemDismiss call for the mixed target, got %d", len(fs.systemDismissed))
		}
		if got := fs.systemDismissed[0]; got.Target != rec.Target || got.Category != rec.Category {
			t.Errorf("recorded (%q,%q), want (%q,%q)", got.Category, got.Target, rec.Category, rec.Target)
		}
	})

	t.Run("out-of-scope category is not dismissed", func(t *testing.T) {
		fs := &fakeStore{}
		svc := New(fs, newBox(t), testParams())
		// A denied token ("aws") appears in the target, but the category is not one whose
		// target IS a tool the rec proposes to add, so the net must leave it alone.
		rec := ReviewRecommendation{
			Category:    "improve_uzi",
			Target:      "improve aws integration",
			RationaleMd: "the aws path could be better",
		}
		svc.autoDismissDeniedCLIRecommendations(context.Background(), reviewID, []ReviewRecommendation{rec})

		if len(fs.systemDismissed) != 0 {
			t.Fatalf("out-of-scope category must not be dismissed, got %d calls", len(fs.systemDismissed))
		}
	})

	t.Run("clean in-scope rec is not dismissed", func(t *testing.T) {
		fs := &fakeStore{}
		svc := New(fs, newBox(t), testParams())
		rec := ReviewRecommendation{
			Category:    "install_worker_tool",
			Target:      "ripgrep",
			RationaleMd: "worker would benefit from ripgrep",
		}
		svc.autoDismissDeniedCLIRecommendations(context.Background(), reviewID, []ReviewRecommendation{rec})

		if len(fs.systemDismissed) != 0 {
			t.Fatalf("a non-denied tool must not be dismissed, got %d calls", len(fs.systemDismissed))
		}
	})

	t.Run("dispose error is swallowed and the loop continues", func(t *testing.T) {
		fs := &fakeStore{systemDismissErr: errors.New("boom")}
		svc := New(fs, newBox(t), testParams())
		recs := []ReviewRecommendation{
			{Category: "install_worker_tool", Target: "glab", RationaleMd: "needs glab"},
			{Category: "enable_tool", Target: "gh", RationaleMd: "needs gh"},
		}
		// Must return normally (no panic, no propagation) even though every dispose fails.
		svc.autoDismissDeniedCLIRecommendations(context.Background(), reviewID, recs)

		// A failing dispose must not abort the loop: BOTH in-scope denied recs were attempted.
		if len(fs.systemDismissed) != 2 {
			t.Fatalf("expected both in-scope recs attempted despite the error, got %d", len(fs.systemDismissed))
		}
	})
}
