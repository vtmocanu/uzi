package schedsvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// Keep the creator deliberately permissive: if either scheduler precheck stops using
// the persisted continued-issue identity, the observable outcome becomes a start.
func TestLinkedTaskScheduleGuardsLiveDB(t *testing.T) {
	ctx, pool, q, _ := openScheduleFireLiveDB(t)
	for _, target := range []string{"pinned", "backlog"} {
		for _, tc := range []struct {
			name, status                           string
			linked, otherIssue, otherRepo, blocked bool
		}{
			{"queued", "queued", true, false, false, true},
			{"running", "running", true, false, false, true},
			{"pool_wait", "pool_wait", true, false, false, true},
			{"terminal", "completed", true, false, false, false},
			{"unlinked", "running", false, false, false, false},
			{"other_issue", "running", true, true, false, false},
			{"other_repo", "running", true, false, true, false},
		} {
			t.Run(target+"/"+tc.name, func(t *testing.T) {
				user, repo := scheduleFireUser(ctx, t, pool)
				runRepo := repo
				if tc.otherRepo {
					_, runRepo = scheduleFireUser(ctx, t, pool)
				}
				var continued any
				if tc.linked {
					continued = int64(7)
				}
				if tc.otherIssue {
					continued = int64(8)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO runs
     (id,user_id,repo_id,kind,continued_issue_iid,issue_title,issue_description,status,branch)
     VALUES($1,$2,$3,'task',$4,'linked','body',$5,$6)`,
					uuid.New(), user, runRepo, continued, tc.status, "uzi/task/"+uuid.NewString()); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO issues
     (repo_id,forge_issue_iid,title,state,labels,web_url,has_prd_link,forge_updated_at,synced_at)
     VALUES($1,7,'candidate','opened','["uzi","PRD"]','https://forge.e2e/7',false,now(),now())`, repo); err != nil {
					t.Fatal(err)
				}

				h := newHarness()
				h.owner, h.repoID = user, repo
				// All scheduler reads, including backlog selection, use the real queries.
				sched := New(q, h.runs, h.fb, h.set, h.notif, h.vault, time.Minute, nil)
				sc := h.issueSchedule()
				if target == "backlog" {
					sc = h.sweepSchedule(pgtype.Int4{})
				}
				out, err := sched.RunNow(ctx, sc)
				if err != nil {
					t.Fatal(err)
				}
				calls := len(h.runs.autopilot) + len(h.runs.runs)
				if out.Matched != 1 {
					t.Fatalf("outcome=%+v, want one candidate", out)
				}
				if tc.blocked {
					if len(out.Started) != 0 || calls != 0 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipAlreadyRunning ||
						out.Skips[0].IssueIID == nil || *out.Skips[0].IssueIID != 7 {
						t.Fatalf("linked task must skip without creator call: outcome=%+v calls=%d", out, calls)
					}
				} else if len(out.Started) != 1 || len(out.Skips) != 0 || calls != 1 {
					t.Fatalf("positive control must proceed: outcome=%+v calls=%d", out, calls)
				}
			})
		}
	}
}
