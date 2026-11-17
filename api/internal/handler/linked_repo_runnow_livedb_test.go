package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// A manual "run schedule now" on a sweep or issue schedule of a repo linked to an issue
// tracker must resolve its issue side. The Handler under test comes from New, the
// production constructor: the scheduler it builds for run-now is what main.go does not
// wire, so a hand-built scheduler here would hide a missing SetIssueSources. Before the
// fix the answer was 502 (issuesource.ErrNotWired inside issueSide).
func TestLinkedRepoRunNowResolvesTheIssueTrackerLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	real := New(f.pool, f.h.q, f.h.cfg, f.h.box, f.h.svc, f.h.wsvc, nil, nil, f.h.settings)

	sweep, issue := uuid.New(), uuid.New()
	ctx := context.Background()
	mustExecT(ctx, t, f.pool,
		`INSERT INTO run_schedules (id, user_id, repo_id, target, labels, timing, cron_expr, timezone, next_fire_at, auto_approve, status, enabled, max_issues)
		 VALUES ($1, $2, $3, 'sweep', '["uzi"]'::jsonb, 'recurring', '0 3 * * *', 'UTC', now() + interval '1 day', false, 'active', true, 1)`,
		sweep, f.user.ID, f.repo)
	mustExecT(ctx, t, f.pool,
		`INSERT INTO run_schedules (id, user_id, repo_id, target, issue_iid, timing, cron_expr, timezone, next_fire_at, auto_approve, status, enabled)
		 VALUES ($1, $2, $3, 'issue', $4, 'recurring', '0 3 * * *', 'UTC', now() + interval '1 day', false, 'active', true)`,
		issue, f.user.ID, f.repo, f.issueN)

	// The sweep reads its candidates from the cached cards' labels; make the seeded card one.
	mustExecT(ctx, t, f.pool, `UPDATE issues SET labels = '["uzi"]' WHERE repo_id = $1`, f.repo)

	for name, id := range map[string]uuid.UUID{"sweep": sweep, "issue": issue} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", id.String())
			r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), f.user), chi.RouteCtxKey, rctx))
			rr := httptest.NewRecorder()
			f.srv.ResetRequests()
			real.RunScheduleNow(rr, r)
			if rr.Code != http.StatusOK && rr.Code != http.StatusAccepted {
				t.Fatalf("run-now status = %d body = %s, want 200/202, not 502 (the issue side of a linked repo must resolve)", rr.Code, rr.Body.String())
			}
			if len(f.srv.Requests()) == 0 {
				t.Fatalf("the issue tracker saw no request: a run-now that returns %d without resolving the issue side must not pass", rr.Code)
			}
		})
	}
}
