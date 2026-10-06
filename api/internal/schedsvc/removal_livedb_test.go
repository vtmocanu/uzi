package schedsvc

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type removalForge struct {
	forgetest.BaseFake
	labels      []string
	writes      int
	beforeWrite func()
	writeErr    error
}

func (f *removalForge) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return forge.Issue{Title: "candidate", Labels: f.labels, WebURL: "https://forge.e2e/issue"}, nil
}
func (f *removalForge) UpdateIssueLabels(_ context.Context, project, iid int64, add, remove []string) error {
	f.writes++
	if project != 1 || iid != 1 || len(add) != 0 || !slices.Equal(remove, []string{"on-deck"}) {
		return errors.New("wrong forge label mutation")
	}
	if f.beforeWrite != nil {
		f.beforeWrite()
	}
	if f.writeErr != nil {
		return f.writeErr
	}
	f.labels = []string{"uzi"}
	return nil
}

type removalBuilder struct {
	*forgesvc.Service
	f forge.Forge
}

func (b removalBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

type removalLiveStore struct {
	*capacityLiveStore
	lookupErr error
}

func (s *removalLiveStore) GetIssueByIID(ctx context.Context, p store.GetIssueByIIDParams) (store.Issue, error) {
	if s.lookupErr != nil {
		return store.Issue{}, s.lookupErr
	}
	return s.Queries.GetIssueByIID(ctx, p)
}

type removalCache struct {
	*store.Queries
	err error
}

func (s removalCache) RemoveCachedIssueLabel(ctx context.Context, p store.RemoveCachedIssueLabelParams) (store.Issue, error) {
	if s.err != nil {
		return store.Issue{}, s.err
	}
	return s.Queries.RemoveCachedIssueLabel(ctx, p)
}

type removalRuns struct {
	fakeRuns
	pool        *pgxpool.Pool
	createErr   error
	afterCreate func()
}

type removalSettings struct {
	fakeSettings
	err error
}

func (s *removalSettings) UziLabel(context.Context) (string, error) {
	return s.uziLabel, s.err
}

func (r *removalRuns) CreateScheduledAutopilotRun(ctx context.Context, user, repo uuid.UUID, iid int64, desc string, _ *bool, _ *bool, _ *string, _ bool, _ *workersvc.CredentialOverride, _ *workersvc.Harness) (store.Run, error) {
	if r.createErr != nil {
		return store.Run{}, r.createErr
	}
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `INSERT INTO runs (user_id,repo_id,issue_iid,kind,status,issue_title,issue_description) VALUES ($1,$2,$3,'issue','queued','candidate',$4) RETURNING id`, user, repo, iid, desc).Scan(&id)
	if err == nil && r.afterCreate != nil {
		r.afterCreate()
	}
	return store.Run{ID: id}, err
}

func TestSweepRemovalLiveDB(t *testing.T) {
	for _, name := range []string{"success", "off", "create failure", "create skip", "forge failure", "cache failure", "lookup failure", "live uzi changed", "foreign owner", "concurrent cache update", "normalized selector", "absent cache", "absent cache forge failure", "settings read failure", "nil settings", "blank settings"} {
		t.Run(name, func(t *testing.T) {
			ctx, pool, q, _ := openScheduleFireLiveDB(t)
			user, repo := scheduleFireUser(ctx, t, pool)
			capacityExec(t, ctx, pool, `INSERT INTO issues (repo_id,forge_issue_iid,title,state,labels,assignee_ids,web_url,forge_updated_at,synced_at,board_position) VALUES ($1,1,'candidate','opened','["on-deck","uzi"]','[1]','https://forge.e2e/issue',now(),now(),42)`, repo)
			sc := capacityLiveSchedule(t, ctx, pool, q, user, repo, 4, 2, pgtype.Int4{Int32: 1, Valid: true})
			capacityExec(t, ctx, pool, `UPDATE run_schedules SET labels='["on-deck"]',remove_label_on_dispatch=true,capacity_limit=NULL,capacity_room_needed=NULL WHERE id=$1`, sc.ID)
			sc, err := q.GetRunSchedule(ctx, sc.ID)
			if err != nil {
				t.Fatal(err)
			}
			st := &removalLiveStore{capacityLiveStore: &capacityLiveStore{Queries: q, scheduleID: sc.ID}}
			cache := removalCache{Queries: q}
			runs := &removalRuns{pool: pool}
			f := &removalForge{labels: []string{"on-deck", "uzi"}}
			settings := &removalSettings{fakeSettings: fakeSettings{uziLabel: "uzi"}}
			switch name {
			case "normalized selector":
				sc.Labels = []byte(`["  on-deck  ","","  "]`)
			case "off":
				sc.RemoveLabelOnDispatch = false
			case "create failure":
				runs.createErr = errors.New("create failed")
			case "create skip":
				runs.createErr = workersvc.ErrActiveRunExists
			case "forge failure":
				f.writeErr = errors.New("forge failed")
			case "cache failure":
				cache.err = errors.New("cache failed")
			case "lookup failure":
				st.lookupErr = errors.New("lookup failed")
			case "live uzi changed":
				settings.uziLabel = "on-deck"
			case "foreign owner":
				sc.UserID = uuid.New()
			case "absent cache", "absent cache forge failure":
				if name == "absent cache forge failure" {
					f.writeErr = errors.New("forge failed")
				}
				runs.afterCreate = func() {
					capacityExec(t, ctx, pool, `UPDATE issues SET labels='["uzi"]' WHERE repo_id=$1 AND forge_issue_iid=1`, repo)
				}
			case "settings read failure":
				runs.afterCreate = func() {
					settings.err = errors.New("settings read failed")
				}
			case "blank settings":
				settings.uziLabel = "   "
			}
			f.beforeWrite = func() {
				var n int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE user_id=$1 AND repo_id=$2 AND issue_iid=1`, user, repo).Scan(&n); err != nil || n != 1 {
					t.Fatalf("forge write before run: n=%d err=%v", n, err)
				}
				cached, err := q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repo, ForgeIssueIid: 1})
				wantCachedSelector := name != "absent cache" && name != "absent cache forge failure"
				if err != nil || slices.Contains(scheduleRemovalLabels(t, cached.Labels), "on-deck") != wantCachedSelector {
					t.Fatalf("cache removed before forge: %+v %v", cached, err)
				}
				if name == "concurrent cache update" {
					// Simulate a committed sync between the scheduler's snapshot lookup and its write.
					capacityExec(t, ctx, pool, `UPDATE issues SET labels='["unrelated","on-deck","uzi","tail"]',title='fresh',assignee_ids='[1,2]',board_position=17 WHERE repo_id=$1 AND forge_issue_iid=1`, repo)
				}
			}
			builder := removalBuilder{Service: forgesvc.New(cache, nil, time.Second, nil), f: f}
			e := New(st, runs, builder, settings, nil, nil, time.Minute, nil)
			if name == "nil settings" {
				e.settings = nil
			}
			out, err := e.RunNow(ctx, sc)
			if name == "foreign owner" {
				if !errors.Is(err, workersvc.ErrRepoNotFound) || f.writes != 0 {
					t.Fatalf("foreign owner: %+v %v", out, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantStarts := 1
			if name == "create failure" || name == "create skip" {
				wantStarts = 0
			}
			if len(out.Started) != wantStarts {
				t.Fatalf("out=%+v", out)
			}
			wantWrites := 1
			if wantStarts == 0 || name == "off" || name == "live uzi changed" || name == "lookup failure" || name == "settings read failure" {
				wantWrites = 0
			}
			if f.writes != wantWrites {
				t.Fatalf("writes=%d want=%d", f.writes, wantWrites)
			}
			failure := name == "forge failure" || name == "absent cache forge failure" || name == "cache failure" || name == "lookup failure" || name == "settings read failure"
			removed := name == "success" || name == "concurrent cache update" || name == "normalized selector" || name == "absent cache" || name == "nil settings" || name == "blank settings"
			if wantStarts == 1 {
				started := out.Started[0]
				selector := "on-deck"
				if name == "off" {
					selector = ""
				}
				if started.LabelRemoved != removed || started.LabelRemoveFailed != failure || started.SelectorLabel != selector {
					t.Fatalf("started=%+v", started)
				}
				data, err := marshalLastFire(out, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				var persisted lastFireRecord
				if err := json.Unmarshal(data, &persisted); err != nil {
					t.Fatal(err)
				}
				p := persisted.Started[0]
				if p.LabelRemoved != removed || p.LabelRemoveFailed != failure || p.SelectorLabel != selector {
					t.Fatalf("persisted=%+v", p)
				}
			}
			cached, err := q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repo, ForgeIssueIid: 1})
			if err != nil {
				t.Fatal(err)
			}
			wantLabels := []string{"on-deck", "uzi"}
			if removed || name == "absent cache forge failure" {
				wantLabels = []string{"uzi"}
			}
			if name == "concurrent cache update" {
				wantLabels = []string{"unrelated", "uzi", "tail"}
				if cached.Title != "fresh" || string(cached.AssigneeIds) != "[1, 2]" || cached.BoardPosition.Int64 != 17 {
					t.Fatalf("metadata clobbered: %+v", cached)
				}
			}
			if !slices.Equal(scheduleRemovalLabels(t, cached.Labels), wantLabels) {
				t.Fatalf("cached labels=%s", cached.Labels)
			}
			if (name == "forge failure" || name == "absent cache forge failure" || name == "settings read failure") && !slices.Contains(f.labels, "on-deck") {
				t.Fatal("failed forge changed labels")
			}
			if removed || name == "cache failure" {
				if slices.Contains(f.labels, "on-deck") {
					t.Fatal("forge selector remains")
				}
			}
			if removed {
				capacityExec(t, ctx, pool, `UPDATE runs SET status='failed' WHERE user_id=$1 AND repo_id=$2`, user, repo)
				again, err := e.RunNow(ctx, sc)
				if err != nil || len(again.Started) != 0 || again.Matched != 0 || f.writes != 1 {
					t.Fatalf("repeat=%+v err=%v writes=%d", again, err, f.writes)
				}
			}
		})
	}
}
func scheduleRemovalLabels(t *testing.T, data []byte) []string {
	t.Helper()
	var labels []string
	if err := json.Unmarshal(data, &labels); err != nil {
		t.Fatal(err)
	}
	return labels
}

func TestSweepRemovalUnsupportedLiveDB(t *testing.T) {
	for _, name := range []string{"prompt", "issue", "once", "empty", "nil", "null", "blank", "malformed", "multiple", "assigned", "default drift"} {
		t.Run(name, func(t *testing.T) {
			ctx, pool, q, _ := openScheduleFireLiveDB(t)
			user, repo := scheduleFireUser(ctx, t, pool)
			sc := store.RunSchedule{ID: uuid.New(), UserID: user, RepoID: repo, Target: "sweep", Timing: "recurring", Labels: []byte(`["on-deck"]`), RemoveLabelOnDispatch: true}
			st := &capacityLiveStore{Queries: q}
			f := &removalForge{labels: []string{"on-deck", "uzi"}}
			e := New(st, &fakeRuns{}, removalBuilder{Service: forgesvc.New(q, nil, time.Second, nil), f: f}, &fakeSettings{uziLabel: "uzi"}, nil, nil, time.Minute, nil)
			switch name {
			case "prompt", "issue":
				sc.Target = name
			case "once":
				sc.Timing = "once"
			case "empty":
				sc.Labels = []byte("[]")
			case "nil":
				sc.Labels = nil
			case "null":
				sc.Labels = []byte("null")
			case "blank":
				sc.Labels = []byte(`[" ",""]`)
			case "malformed":
				sc.Labels = []byte(`{"label":"on-deck"}`)
			case "multiple":
				sc.Labels = []byte(`["on-deck","other"]`)
			case "assigned", "default drift":
				sc.Origin = "default"
				sc.CatalogSlug = pgtype.Text{String: "test", Valid: true}
				e.catalog = func(string) (schedtmpl.DefaultJob, bool) {
					job := schedtmpl.DefaultJob{Target: "sweep", SelectorKind: schedtmpl.SelectorLabel, Labels: []string{"on-deck", "other"}}
					if name == "assigned" {
						job.SelectorKind = schedtmpl.SelectorAssigned
						job.Labels = []string{"on-deck"}
					}
					return job, true
				}
			}
			out, err := e.RunNow(ctx, sc)
			if err != nil || len(out.Started) != 0 || len(out.Skips) != 1 || out.Skips[0].Reason != SkipConfigNotSupported || st.lists != 0 || f.writes != 0 {
				t.Fatalf("unsupported: %+v err=%v lists=%d writes=%d", out, err, st.lists, f.writes)
			}
		})
	}
}
