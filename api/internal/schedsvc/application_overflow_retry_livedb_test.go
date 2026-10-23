package schedsvc

import (
	"bytes"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
	"testing"
	"time"
)

func TestR3IssueSchedulePersistenceOverflowRetryLiveDB(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		for _, mode := range []string{"503", "metadata", "issue"} {
			if mode == "metadata" && kind != "github" {
				continue
			}
			for _, timing := range []string{"once", "recurring"} {
				t.Run(kind+"/"+mode+"/"+timing, func(t *testing.T) {
					ctx, pool, q, box := openScheduleFireLiveDB(t)
					user, repo := scheduleFireUser(ctx, t, pool)
					seedAnthropicDefault(ctx, t, pool, box, user)
					wire, driver := r3Forge(t, kind, mode)
					if _, err := pool.Exec(ctx, "UPDATE repos SET forge_project_id=42 WHERE id=$1", repo); err != nil {
						t.Fatal(err)
					}
					if _, err := pool.Exec(ctx, `INSERT INTO issues(repo_id,forge_issue_iid,title,state,labels,web_url,forge_updated_at,synced_at) VALUES($1,7,'cached','opened','["uzi","PRD"]','https://example.test/i',now(),now())`, repo); err != nil {
						t.Fatal(err)
					}
					runs := workersvc.New(q, box, workersvc.Params{})
					runs.SetTxBeginner(pool)
					id := uuid.New()
					sched := New(&m2OverflowLiveStore{Queries: q, scheduleID: id}, runs, m2OverflowBuilder{&fakeBuilder{}, driver}, nil, nil, nil, time.Minute, nil)
					due := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
					t.Cleanup(func() {
						if _, err := pool.Exec(ctx, "UPDATE run_schedules SET enabled=false WHERE id=$1", id); err != nil {
							t.Error(err)
						}
					})
					if _, err := pool.Exec(ctx, `INSERT INTO run_schedules(id,user_id,repo_id,target,issue_iid,timing,run_at,cron_expr,next_fire_at,timezone,auto_approve,enabled,status,origin)
 VALUES($1,$2,$3,'issue',7,$4,$5,CASE WHEN $4='recurring' THEN '0 * * * *' ELSE NULL END,$5,'UTC',false,true,'active','user')`, id, user, repo, timing, due); err != nil {
						t.Fatal(err)
					}
					initial, err := q.GetRunSchedule(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					for attempt := int64(1); attempt <= 2; attempt++ {
						sched.Boot(ctx)
						got, err := q.GetRunSchedule(ctx, id)
						if err != nil {
							t.Fatal(err)
						}
						var n int
						if err := pool.QueryRow(ctx, "SELECT count(*) FROM runs WHERE repo_id=$1", repo).Scan(&n); err != nil {
							t.Fatal(err)
						}
						if n != 0 {
							t.Fatalf("runs=%d", n)
						}
						meta, target := int64(0), attempt
						if kind == "github" {
							meta = 1
						}
						if mode == "metadata" {
							meta, target = attempt, 0
						}
						if mode == "issue" {
							target = 1
							if timing == "once" {
								if got.Status != "fired" || got.NextFireAt.Valid {
									t.Fatal("once own overflow not consumed")
								}
							} else if got.Status != "active" || !got.NextFireAt.Valid || !got.NextFireAt.Time.After(due) {
								t.Fatal("recurring own overflow not advanced")
							}
							_, reasons := heldLastFire(t, got.LastFire)
							if len(reasons) != 1 || reasons[0] != string(SkipDescriptionTooLarge) {
								t.Fatalf("reasons=%v", reasons)
							}
							if len(scheduleRecentFires(t, got)) != 1 {
								t.Fatal("own overflow should persist one fire")
							}
						} else if got.Status != initial.Status || got.Enabled != initial.Enabled || got.NextFireAt != initial.NextFireAt || !bytes.Equal(got.LastFire, initial.LastFire) || !bytes.Equal(got.RecentFires, initial.RecentFires) {
							t.Fatal("transient failure changed persisted schedule")
						}
						wire.assertAttempts(t, meta, target, 0, 0)
					}
				})
			}
		}
	}
}
