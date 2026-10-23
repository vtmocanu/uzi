package schedsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type m2OverflowLiveStore struct {
	*store.Queries
	scheduleID uuid.UUID
}

// Isolate this scheduler's fixture from other due schedules in the shared test database.
func (s *m2OverflowLiveStore) ClaimDueSchedules(ctx context.Context) ([]store.RunSchedule, error) {
	rows, err := s.Queries.ClaimDueSchedules(ctx)
	if err != nil {
		return nil, err
	}
	owned := make([]store.RunSchedule, 0, 1)
	for _, row := range rows {
		if row.ID == s.scheduleID {
			owned = append(owned, row)
		}
	}
	return owned, nil
}

func TestM2ScheduleCaptureOverflowLiveDB(t *testing.T) {
	for _, target := range []string{"issue", "sweep"} {
		for _, timing := range []string{"once", "recurring"} {
			for _, overflow := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/overflow=%t", target, timing, overflow), func(t *testing.T) {
					ctx, pool, q, box := openScheduleFireLiveDB(t)
					user, repo := scheduleFireUser(ctx, t, pool)
					seedAnthropicDefault(ctx, t, pool, box, user)
					for _, iid := range []int{7, 8, 9} {
						if _, err := pool.Exec(ctx, `INSERT INTO issues(repo_id,forge_issue_iid,title,state,labels,web_url,forge_updated_at,synced_at) VALUES($1,$2,'cached','opened','["uzi","PRD"]','https://example.test/i',now(),now())`, repo, iid); err != nil {
							t.Fatal(err)
						}
					}
					source := error(context.DeadlineExceeded)
					want := SkipFetchFailed
					if overflow {
						source = fmt.Errorf("wrapped: %w", &forge.ResponseTooLargeError{Provenance: forge.ResponseIssueSuccess})
						want = SkipDescriptionTooLarge
					}
					f := &m2OverflowForge{fakeForge: &fakeForge{issue: forge.Issue{Title: "fresh", Description: "body", Labels: []string{"uzi"}}}, firstErr: source}
					builder := m2OverflowBuilder{&fakeBuilder{}, f}
					runs := workersvc.New(q, box, workersvc.Params{})
					runs.SetTxBeginner(pool)
					id := uuid.New()
					sched := New(&m2OverflowLiveStore{Queries: q, scheduleID: id}, runs, builder, nil, nil, nil, time.Minute, nil)
					due := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
					t.Cleanup(func() {
						if _, err := pool.Exec(ctx, "UPDATE run_schedules SET enabled=false WHERE id=$1", id); err != nil {
							t.Error(err)
						}
					})
					countRuns := func() int {
						t.Helper()
						var n int
						if err := pool.QueryRow(ctx, "SELECT count(*) FROM runs WHERE repo_id=$1", repo).Scan(&n); err != nil {
							t.Fatal(err)
						}
						return n
					}
					if _, err := pool.Exec(ctx, `INSERT INTO run_schedules(id,user_id,repo_id,target,issue_iid,labels,max_issues,timing,run_at,cron_expr,next_fire_at,timezone,auto_approve,enabled,status,origin)
      VALUES($1,$2,$3,$4,CASE WHEN $4='issue' THEN 7 ELSE NULL END,'["PRD"]',1,$5,$6,CASE WHEN $5='recurring' THEN '0 * * * *' ELSE NULL END,$6,'UTC',false,true,'active','user')`, id, user, repo, target, timing, due); err != nil {
						t.Fatal(err)
					}
					sched.Boot(ctx)
					got, err := q.GetRunSchedule(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if target == "issue" && !overflow {
						if !got.NextFireAt.Valid || !got.NextFireAt.Time.Equal(due) || len(got.LastFire) != 0 {
							t.Fatalf("transient issue advanced: %+v", got)
						}
						sched.Boot(ctx)
						if len(f.getIID) != 2 {
							t.Fatalf("transient issue not retried: %v", f.getIID)
						}
						if countRuns() != 0 {
							t.Fatal("transient issue inserted run")
						}
						again, err := q.GetRunSchedule(ctx, id)
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(got.LastFire, again.LastFire) || !bytes.Equal(got.RecentFires, again.RecentFires) || got.NextFireAt != again.NextFireAt {
							t.Fatal("transient retry changed schedule")
						}
						return
					}
					var out lastFireRecord
					if err := json.Unmarshal(got.LastFire, &out); err != nil {
						t.Fatal(err)
					}
					wantMatched, wantRuns := 1, 0
					if target == "sweep" {
						wantMatched, wantRuns = 2, 1
					}
					if out.Matched != wantMatched || len(out.Started) != wantRuns || len(out.Skips) != 1 || out.Skips[0].Reason != string(want) || out.Skips[0].IssueIID == nil || *out.Skips[0].IssueIID != 7 || out.Skips[0].Title != "" || out.Skips[0].WebURL != "" {
						t.Fatalf("last_fire=%s", got.LastFire)
					}
					recent := scheduleRecentFires(t, got)
					if len(recent) != 1 || len(recent[0].Skips) != 1 || recent[0].Skips[0].Reason != string(want) || len(recent[0].Started) != wantRuns {
						t.Fatalf("recent_fires=%s", got.RecentFires)
					}
					if timing == "once" {
						if got.Status != "fired" || got.NextFireAt.Valid {
							t.Fatalf("once not consumed: %+v", got)
						}
					} else if got.Status != "active" || !got.NextFireAt.Valid || !got.NextFireAt.Time.After(time.Now()) {
						t.Fatalf("recurring not advanced: %+v", got)
					}
					if n := countRuns(); n != wantRuns {
						t.Fatalf("runs=%d want=%d", n, wantRuns)
					}
					if target == "sweep" && (!eqInt64s(f.getIID, []int64{7, 8}) || *out.Started[0].IssueIID != 8 || out.Capped) {
						t.Fatalf("backfill=%+v calls=%v", out, f.getIID)
					}
					sched.Boot(ctx)
					again, err := q.GetRunSchedule(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got.LastFire, again.LastFire) || !bytes.Equal(got.RecentFires, again.RecentFires) || got.NextFireAt != again.NextFireAt || len(f.getIID) != wantMatched {
						t.Fatal("second tick changed consumed fire")
					}
				})
			}
		}
	}
}
