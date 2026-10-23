package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestM2ManualChunkedIssueOverflowLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newStartRunGuardFixture(ctx, t)
	repo := f.seedEnabledRepoWithIssue(ctx, t, 2371, 7, protClean)
	f.forge.mu.Lock()
	f.forge.oversizedIssue = true
	f.forge.mu.Unlock()
	rec := f.createRun(t, repo, 7)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "issue description is too large to run") || strings.Contains(rec.Body.String(), "RAW_OVERSIZED") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := f.runCount(ctx, t, repo); n != 0 {
		t.Fatalf("inserted %d runs", n)
	}
}
