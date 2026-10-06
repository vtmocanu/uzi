package schedsvc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The wrapper observes the public store seams while executing the real queries.
type capacityLiveStore struct {
	*store.Queries
	scheduleID            uuid.UUID
	counts, lists, probes int
	scan                  pgtype.Int4
}

// Other packages leave due schedules in the shared test database. Execute the real
// claim query, then give this scheduler only the fixture it owns.
func (s *capacityLiveStore) ClaimDueSchedules(ctx context.Context) ([]store.RunSchedule, error) {
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

func (s *capacityLiveStore) CountInProgressRunsForUser(ctx context.Context, id uuid.UUID) (int64, error) {
	s.counts++
	return s.Queries.CountInProgressRunsForUser(ctx, id)
}
func (s *capacityLiveStore) ListSweepCandidateIssues(ctx context.Context, p store.ListSweepCandidateIssuesParams) ([]store.ListSweepCandidateIssuesRow, error) {
	s.lists++
	s.scan = p.MaxIssues
	return s.Queries.ListSweepCandidateIssues(ctx, p)
}
func (s *capacityLiveStore) CountSweepCandidateIssues(ctx context.Context, p store.CountSweepCandidateIssuesParams) (store.CountSweepCandidateIssuesRow, error) {
	s.probes++
	return s.Queries.CountSweepCandidateIssues(ctx, p)
}

func capacityExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}
func capacityLiveSchedule(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q *store.Queries, user, repo uuid.UUID, c, k int32, n pgtype.Int4) store.RunSchedule {
	t.Helper()
	due := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	id := uuid.New()
	capacityExec(t, ctx, pool, `INSERT INTO run_schedules
 (id,user_id,repo_id,target,labels,timing,cron_expr,timezone,next_fire_at,enabled,status,origin,auto_approve,capacity_limit,capacity_room_needed,max_issues)
 VALUES ($1,$2,$3,'sweep','["uzi","bug"]','recurring','0 * * * *','UTC',$4,true,'active','user',true,$5,$6,$7)`,
		id, user, repo, due, c, k, n)
	t.Cleanup(func() { capacityExec(t, context.Background(), pool, `DELETE FROM run_schedules WHERE id=$1`, id) })
	sc, err := q.GetRunSchedule(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}
func seedCapacityWork(t *testing.T, ctx context.Context, pool *pgxpool.Pool, user, repo uuid.UUID, n int) {
	t.Helper()
	// The job is repo-less; remaining work spans two repos and parked states.
	capacityExec(t, ctx, pool, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 SELECT $2,connection_id,2,$3,'https://forge.e2e/other','main',true FROM repos WHERE id=$1`, repo, uuid.New(), "g/"+uuid.NewString())
	var other uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM repos WHERE connection_id=(SELECT connection_id FROM repos WHERE id=$1) AND id<>$1`, repo).Scan(&other); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if i == 0 {
			capacityExec(t, ctx, pool, `INSERT INTO runs (user_id,kind,job_type,status,issue_title,issue_description) VALUES ($1,'job','research','queued','job','d')`, user)
		} else {
			r := repo
			if i%2 == 1 {
				r = other
			}
			st := []string{"running", "awaiting_approval", "limit_wait", "paused", "pool_wait", "awaiting_input"}[(i-1)%6]
			capacityExec(t, ctx, pool, `INSERT INTO runs (user_id,repo_id,issue_iid,kind,status,issue_title,issue_description) VALUES ($1,$2,$3,'issue',$4,'wip','d')`, user, r, 1000+i, st)
		}
	}
	terminal := uuid.New()
	capacityExec(t, ctx, pool, `INSERT INTO runs (id,user_id,repo_id,issue_iid,kind,status,issue_title,issue_description) VALUES ($1,$2,$3,999,'issue','completed','done','d')`, terminal, user, repo)
	capacityExec(t, ctx, pool, `INSERT INTO runs (user_id,kind,status,issue_title,issue_description) VALUES ($1,'chat','running','chat','d')`, user)
	capacityExec(t, ctx, pool, `INSERT INTO runs (user_id,kind,status,target_run_id,issue_title,issue_description) VALUES ($1,'judge','running',$2,'judge','d')`, user, terminal)
	otherUser, otherRepo := scheduleFireUser(ctx, t, pool)
	capacityExec(t, ctx, pool, `INSERT INTO runs (user_id,repo_id,issue_iid,kind,status,issue_title,issue_description) VALUES ($1,$2,888,'issue','running','foreign','d')`, otherUser, otherRepo)
	// A second owner's due sweep must not contaminate the observed starts/counters.
	capacityLiveSchedule(t, ctx, pool, store.New(pool), otherUser, otherRepo, 4, 2, pgtype.Int4{Int32: 1, Valid: true})
	capacityExec(t, ctx, pool, `INSERT INTO issues (repo_id,forge_issue_iid,title,state,labels,web_url,forge_updated_at,synced_at) VALUES ($1,1,'foreign candidate','opened','["uzi","bug"]','https://forge.e2e/foreign',now(),now())`, otherRepo)
	for i := 1; i <= 5; i++ {
		capacityExec(t, ctx, pool, `INSERT INTO issues (repo_id,forge_issue_iid,title,state,labels,web_url,forge_updated_at,synced_at) VALUES ($1,$2,'candidate','opened','["uzi","bug"]','https://forge.e2e/issue',now(),now())`, repo, i)
	}
	// Older selector mismatches cannot consume the batch.
	capacityExec(t, ctx, pool, `INSERT INTO issues (repo_id,forge_issue_iid,title,state,labels,web_url,forge_updated_at,synced_at) VALUES ($1,0,'mismatch','opened','["uzi"]','https://forge.e2e/issue',now(),now())`, repo)
}

func TestCapacitySchedulerLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wip     int
		c, k    int32
		n       pgtype.Int4
		starts  int
		room    int
		blocked bool
	}{
		{"blocked", 3, 4, 2, pgtype.Int4{Int32: 1, Valid: true}, 0, 1, true},
		{"over limit", 7, 6, 2, pgtype.Int4{}, 0, 0, true},
		{"partial batch", 4, 6, 2, pgtype.Int4{Int32: 3, Valid: true}, 2, 2, false},
		{"null batch", 4, 6, 2, pgtype.Int4{}, 2, 2, false},
		{"smaller batch", 1, 4, 2, pgtype.Int4{Int32: 1, Valid: true}, 1, 3, false},
	} {
		for _, manual := range []bool{false, true} {
			mode := "tick"
			if manual {
				mode = "run now"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx, pool, q, _ := openScheduleFireLiveDB(t)
				user, repo := scheduleFireUser(ctx, t, pool)
				seedCapacityWork(t, ctx, pool, user, repo, tc.wip)
				sc := capacityLiveSchedule(t, ctx, pool, q, user, repo, tc.c, tc.k, tc.n)
				st := &capacityLiveStore{Queries: q, scheduleID: sc.ID}
				runs := &fakeRuns{}
				fb := &fakeBuilder{f: &fakeForge{issue: forge.Issue{Title: "candidate", Labels: []string{"uzi", "bug"}}}}
				sched := New(st, runs, fb, &fakeSettings{uziLabel: "uzi"}, nil, nil, time.Minute, nil)
				var out FireOutcome
				if manual {
					var err error
					out, err = sched.RunNow(ctx, sc)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					sched.Boot(ctx)
				}
				got, err := q.GetRunSchedule(ctx, sc.ID)
				if err != nil {
					t.Fatal(err)
				}
				if manual {
					if !got.NextFireAt.Time.Equal(sc.NextFireAt.Time) || len(got.LastFire) != 0 {
						t.Fatal("manual fire persisted schedule bookkeeping")
					}
				} else {
					var lf lastFireRecord
					if err := json.Unmarshal(got.LastFire, &lf); err != nil {
						t.Fatal(err)
					}
					out.Capacity = lf.Capacity
					out.Matched = lf.Matched
					if len(lf.Started) != tc.starts || len(lf.Skips) != 0 || got.Status != "active" || !got.NextFireAt.Time.After(sc.NextFireAt.Time) {
						t.Fatalf("persisted=%+v schedule=%+v", lf, got)
					}
				}
				want := CapacityCheck{InFlight: int64(tc.wip), Limit: int(tc.c), RoomNeeded: int(tc.k), Room: tc.room, Blocked: tc.blocked}
				if out.Capacity == nil || *out.Capacity != want || out.Matched != tc.starts || len(runs.autopilot) != tc.starts || st.counts != 1 {
					t.Fatalf("out=%+v starts=%d counts=%d want=%+v", out, len(runs.autopilot), st.counts, want)
				}
				if got.MaxIssues != sc.MaxIssues {
					t.Fatal("effective cap changed stored max_issues")
				}
				if tc.blocked {
					if st.lists != 0 || st.probes != 0 || forgeCalls(fb.f) != 0 {
						t.Fatalf("blocked queried candidates or forge: lists=%d probes=%d forge=%d", st.lists, st.probes, forgeCalls(fb.f))
					}
					var n int
					if err := pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE user_id=$1`, user).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n != tc.wip+3 {
						t.Fatalf("blocked created runs: %d", n)
					}
				} else {
					if st.lists != 1 || st.probes != 1 || !st.scan.Valid || int(st.scan.Int32) != tc.starts+backfillHeadroom {
						t.Fatalf("candidate queries=%+v", st)
					}
					for i, call := range runs.autopilot {
						if call.issueIID != int64(i+1) {
							t.Fatalf("oldest multi-label candidate %d: %+v", i, call)
						}
					}
				}
			})
		}
	}
}

func TestCapacityAssignedCatalogDriftLiveDB(t *testing.T) {
	ctx, pool, q, _ := openScheduleFireLiveDB(t)
	user, repo := scheduleFireUser(ctx, t, pool)
	sc := capacityLiveSchedule(t, ctx, pool, q, user, repo, 4, 2, pgtype.Int4{})
	capacityExec(t, ctx, pool, `UPDATE run_schedules SET origin='default',catalog_slug='assigned-sweep',labels=NULL WHERE id=$1`, sc.ID)
	st := &capacityLiveStore{Queries: q, scheduleID: sc.ID}
	runs := &fakeRuns{}
	fb := &fakeBuilder{f: &fakeForge{}}
	sched := New(st, runs, fb, nil, nil, nil, time.Minute, nil)
	sched.catalog = func(string) (schedtmpl.DefaultJob, bool) {
		return schedtmpl.DefaultJob{SelectorKind: schedtmpl.SelectorAssigned}, true
	}
	sched.Boot(ctx)
	got, err := q.GetRunSchedule(ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	var lf lastFireRecord
	if err := json.Unmarshal(got.LastFire, &lf); err != nil {
		t.Fatal(err)
	}
	if len(lf.Skips) != 1 || lf.Skips[0].Reason != string(SkipConfigNotSupported) || lf.Skips[0].IssueIID != nil || lf.Capacity != nil || !got.NextFireAt.Time.After(sc.NextFireAt.Time) {
		t.Fatalf("last fire=%+v", lf)
	}
	if st.counts != 0 || st.lists != 0 || st.probes != 0 || forgeCalls(fb.f) != 0 || len(runs.autopilot) != 0 {
		t.Fatal("unsupported catalog performed capacity/candidate/forge/run work")
	}
}
