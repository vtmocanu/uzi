package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestR3ManualOverflowRetryLiveDB(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		for _, mode := range []string{"503", "metadata", "issue"} {
			if mode == "metadata" && kind != "github" {
				continue
			}
			t.Run(kind+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				f := newStartRunGuardFixture(ctx, t)
				repo := f.seedEnabledRepoWithIssue(ctx, t, 42, 7, protClean)
				wire, _ := r3Forge(t, kind, mode)
				// Replace the connection endpoint before its first use; the production service builds the driver.
				mustExecT(ctx, t, f.pool, "UPDATE forge_connections SET forge_type=$1,base_url=$2 WHERE id=$3", kind, wire.server.URL, f.connID)
				for attempt := int64(1); attempt <= 2; attempt++ {
					rec := f.createRun(t, repo, 7)
					want := http.StatusBadGateway
					if mode == "issue" {
						want = http.StatusUnprocessableEntity
					}
					if rec.Code != want || strings.Contains(rec.Body.String(), "REMOTE_PRIVATE") {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					if mode == "issue" && !strings.Contains(rec.Body.String(), "issue description is too large to run") {
						t.Fatalf("unsafe message: %s", rec.Body.String())
					}
					if mode != "issue" && !strings.Contains(rec.Body.String(), "forge response body exceeds size limit") {
						t.Fatalf("missing safe overflow error: %s", rec.Body.String())
					}
					if n := f.runCount(ctx, t, repo); n != 0 {
						t.Fatalf("runs=%d", n)
					}
					meta, target := int64(0), attempt
					if kind == "github" {
						meta = attempt
					}
					if mode == "metadata" {
						target = 0
					}
					wire.assertAttempts(t, meta, target, 0, 0)
				}
			})
		}
	}
}
