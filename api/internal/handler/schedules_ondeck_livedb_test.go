package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type ondeckForge struct {
	forgetest.BaseFake
	labels []string
	writes int
}

func (f *ondeckForge) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return forge.Issue{Title: "next", Labels: f.labels}, nil
}
func (f *ondeckForge) UpdateIssueLabels(_ context.Context, _, _ int64, add, remove []string) error {
	f.writes++
	f.labels = slices.DeleteFunc(f.labels, func(s string) bool { return slices.Contains(remove, s) })
	f.labels = append(f.labels, add...)
	return nil
}

type ondeckBuilder struct {
	*forgesvc.Service
	f forge.Forge
}

func (b ondeckBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

type ondeckRuns struct {
	*workersvc.Service
	f scheduleFixture
}

func (r ondeckRuns) CreateScheduledAutopilotRun(ctx context.Context, user, repo uuid.UUID, iid int64, desc string, _ *bool, _ *bool, _ *string, _ bool, _ *workersvc.CredentialOverride, _ *workersvc.Harness) (store.Run, error) {
	var id uuid.UUID
	err := r.f.pool.QueryRow(ctx, `INSERT INTO runs(user_id,repo_id,issue_iid,kind,status,issue_title,issue_description) VALUES($1,$2,$3,'issue','queued','next',$4) RETURNING id`, user, repo, iid, desc).Scan(&id)
	return store.Run{ID: id}, err
}

func TestOndeckCatalogEnableRunDrainLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	d, code := f.enableCatalog(t, f.owner.ID, f.repoID, "ondeck-sweep")
	assert := func(d apitypes.ScheduleDTO, custom bool) {
		t.Helper()
		if d.Customized != custom || !d.RemoveLabelOnDispatch || d.CapacityLimit == nil || *d.CapacityLimit != 4 || d.CapacityRoomNeeded == nil || *d.CapacityRoomNeeded != 2 || d.CronExpr != "*/10 * * * *" {
			t.Fatalf("dto=%+v", d)
		}
	}
	if code != 201 {
		t.Fatal(code)
	}
	assert(d, false)
	// Config PATCH retains the existing replace semantics for max_issues.
	edited, code := f.patchSchedule(t, f.owner.ID, d.ID, `{"capacity_limit":5,"max_issues":1}`)
	if code != 200 || !edited.Customized || *edited.CapacityLimit != 5 {
		t.Fatalf("edit=%d %+v", code, edited)
	}
	restored, code := f.patchSchedule(t, f.owner.ID, d.ID, `{"capacity_limit":4,"max_issues":1}`)
	if code != 200 {
		t.Fatal(code)
	}
	assert(restored, false)
	for _, body := range []string{`{"remove_label_on_dispatch":false,"max_issues":1}`, `{"remove_label_on_dispatch":true,"max_issues":1}`} {
		got, status := f.patchSchedule(t, f.owner.ID, d.ID, body)
		if status != 200 || got.Customized == (got.RemoveLabelOnDispatch) {
			t.Fatalf("removal edit=%d %+v", status, got)
		}
	}
	if _, code := f.patchSchedule(t, f.owner.ID, d.ID, `{"capacity_limit":5,"enabled":false}`); code != 200 {
		t.Fatal(code)
	}
	w := httptest.NewRecorder()
	f.h.ResetSchedule(w, userReq(http.MethodPost, "/api/schedules/"+d.ID+"/reset", "", f.owner.ID, map[string]string{"id": d.ID}))
	var reset apitypes.ScheduleDTO
	if err := json.Unmarshal(w.Body.Bytes(), &reset); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || reset.Enabled {
		t.Fatalf("reset pause=%d %+v", w.Code, reset)
	}
	assert(reset, false)
	mustExecT(ctx, t, f.pool, `INSERT INTO issues(repo_id,forge_issue_iid,title,state,labels,web_url,forge_updated_at,synced_at) VALUES($1,1,'next','opened','["on-deck","uzi"]','https://forge.example/1',now(),now())`, f.repoID)
	q := store.New(f.pool)
	ff := &ondeckForge{labels: []string{"on-deck", "uzi"}}
	f.h.scheduler = schedsvc.New(q, ondeckRuns{Service: f.h.wsvc, f: f}, ondeckBuilder{Service: f.h.svc, f: ff}, f.h.settings, nil, nil, time.Minute, nil)
	run := func() apitypes.RunNowResponse {
		t.Helper()
		w := httptest.NewRecorder()
		f.h.RunScheduleNow(w, userReq(http.MethodPost, "/api/schedules/"+d.ID+"/run-now", "", f.owner.ID, map[string]string{"id": d.ID}))
		var out apitypes.RunNowResponse
		if w.Code != 202 {
			t.Fatalf("run=%d %s", w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := run()
	if len(out.Started) != 1 || ff.writes != 1 || slices.Contains(ff.labels, "on-deck") {
		t.Fatalf("first=%+v forge=%+v", out, ff)
	}
	issue, err := q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: f.repoID, ForgeIssueIid: 1})
	if err != nil || string(issue.Labels) != `["uzi"]` {
		t.Fatalf("cache=%s err=%v", issue.Labels, err)
	}
	out = run()
	if len(out.Started) != 0 || ff.writes != 1 || out.Started == nil || out.Skips == nil {
		t.Fatalf("drained=%+v", out)
	}
	// Edits must survive fire-time catalog resolution.
	if _, code := f.patchSchedule(t, f.owner.ID, d.ID, `{"capacity_limit":1,"capacity_room_needed":1,"remove_label_on_dispatch":false,"max_issues":1}`); code != 200 {
		t.Fatal(code)
	}
	mustExecT(ctx, t, f.pool, `UPDATE runs SET status='completed' WHERE user_id=$1 AND repo_id=$2`, f.owner.ID, f.repoID)
	mustExecT(ctx, t, f.pool, `UPDATE issues SET labels='["on-deck","uzi"]' WHERE repo_id=$1`, f.repoID)
	ff.labels = []string{"on-deck", "uzi"}
	out = run()
	if len(out.Started) != 1 || ff.writes != 1 || !slices.Contains(ff.labels, "on-deck") {
		t.Fatalf("edited fire=%+v forge=%+v", out, ff)
	}
	out = run()
	if out.Capacity == nil || out.Capacity.Limit != 1 || !out.Capacity.Blocked || len(out.Started) != 0 {
		t.Fatalf("edited capacity=%+v", out)
	}
	// Live eligibility-label validation applies to reset and enable, without requiring
	// the selector to already exist on the forge (missing-label remains advisory).
	f.h.settings = settings.New(&settingsStore{rows: []store.AppSetting{{Key: settings.KeyUziLabel, Value: "on-deck"}}}, time.Minute)
	w = httptest.NewRecorder()
	f.h.ResetSchedule(w, userReq(http.MethodPost, "/api/schedules/"+d.ID+"/reset", "", f.owner.ID, map[string]string{"id": d.ID}))
	if w.Code != 400 {
		t.Fatalf("live label reset=%d %s", w.Code, w.Body.String())
	}
	repeated, repeatCode := f.enableCatalog(t, f.owner.ID, f.repoID, "ondeck-sweep")
	if repeatCode != http.StatusOK || repeated.ID != d.ID || repeated.RemoveLabelOnDispatch || repeated.CapacityLimit == nil || *repeated.CapacityLimit != 1 || repeated.Enabled {
		t.Fatalf("repeat enable must preserve existing edited paused row: status=%d dto=%+v", repeatCode, repeated)
	}
	other := f.insertRepo(ctx, t, f.owner, 7980, "g/on-deck-other")
	if _, code := f.enableCatalog(t, f.owner.ID, other, "ondeck-sweep"); code != 400 {
		t.Fatalf("live label enable=%d", code)
	}
}
