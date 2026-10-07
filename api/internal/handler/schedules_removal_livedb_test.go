package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestScheduleRemovalCRUDLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	for _, body := range []string{
		`{"target":"issue","issue_iid":1,"timing":"recurring","cron_expr":"0 * * * *","remove_label_on_dispatch":true}`,
		`{"target":"prompt","prompt":"x","timing":"recurring","cron_expr":"0 * * * *","remove_label_on_dispatch":true}`,
		`{"target":"sweep","labels":["bug"],"timing":"once","run_at":"2099-01-01T00:00:00Z","remove_label_on_dispatch":true}`,
		`{"target":"sweep","labels":[],"timing":"recurring","cron_expr":"0 * * * *","remove_label_on_dispatch":true}`,
		`{"target":"sweep","labels":["bug","other"],"timing":"recurring","cron_expr":"0 * * * *","remove_label_on_dispatch":true}`,
		`{"target":"sweep","labels":[" uzi "],"timing":"recurring","cron_expr":"0 * * * *","remove_label_on_dispatch":true}`,
	} {
		if _, code := f.createSchedule(t, f.owner.ID, f.repoID, body); code != 400 {
			t.Fatalf("create %s: %d", body, code)
		}
	}
	d, code := f.createSchedule(t, f.owner.ID, f.repoID, `{"target":"sweep","labels":[" bug ",""],"timing":"recurring","cron_expr":"0 * * * *","remove_label_on_dispatch":true}`)
	if code != 201 || !d.RemoveLabelOnDispatch {
		t.Fatalf("create=%d %+v", code, d)
	}
	for _, body := range []string{`{"cron_expr":"0 2 * * *"}`, `{"enabled":false}`, `{"enabled":true}`} {
		got, status := f.patchSchedule(t, f.owner.ID, d.ID, body)
		if status != 200 || !got.RemoveLabelOnDispatch {
			t.Fatalf("patch=%d %+v", status, got)
		}
	}
	clone, code := f.cloneSchedule(t, f.owner.ID, d.ID, `{}`)
	if code != 201 || !clone.RemoveLabelOnDispatch {
		t.Fatalf("clone=%d %+v", code, clone)
	}
	repo := f.insertRepo(ctx, t, f.owner, 7841, "g/removal-sibling")
	sibling, code := f.addRepo(t, f.owner.ID, d.ID, repo.String())
	if code != 201 || !sibling.RemoveLabelOnDispatch {
		t.Fatalf("add repo=%d %+v", code, sibling)
	}
	got, code := f.patchSchedule(t, f.owner.ID, d.ID, `{"enabled":true,"remove_label_on_dispatch":false}`)
	if code != 200 || got.RemoveLabelOnDispatch {
		t.Fatalf("clear=%d %+v", code, got)
	}
}

func TestScheduleRemovalSettingsFallbackLiveDB(t *testing.T) {
	for _, value := range []string{"nil", "", "   ", "runnable"} {
		t.Run(value, func(t *testing.T) {
			f := newScheduleFixture(context.Background(), t)
			label := settings.DefaultUziLabel
			if value == "nil" {
				f.h.settings = nil
			} else {
				f.h.settings = settings.New(&settingsStore{rows: []store.AppSetting{{Key: settings.KeyUziLabel, Value: value}}}, time.Minute)
				if value == "runnable" {
					label = value
				}
			}
			body := func(selector string) string {
				data, err := json.Marshal(map[string]any{"target": "sweep", "labels": []string{selector}, "timing": "recurring", "cron_expr": "0 * * * *", "remove_label_on_dispatch": true})
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			if _, code := f.createSchedule(t, f.owner.ID, f.repoID, body(label)); code != http.StatusBadRequest {
				t.Fatalf("eligibility selector accepted: %d", code)
			}
			if got, code := f.createSchedule(t, f.owner.ID, f.repoID, body("on-deck")); code != http.StatusCreated || !got.RemoveLabelOnDispatch {
				t.Fatalf("unrelated selector: %d %+v", code, got)
			}
		})
	}
}

// A failed settings read must refuse the removal toggle rather than validate against
// the default label, which could approve removing a custom eligibility label.
func TestScheduleRemovalSettingsReadErrorLiveDB(t *testing.T) {
	f := newScheduleFixture(context.Background(), t)
	f.h.settings = settings.New(&settingsStore{err: errors.New("settings read failed")}, time.Minute)
	body := `{"target":"sweep","labels":["on-deck"],"timing":"recurring","cron_expr":"0 * * * *","remove_label_on_dispatch":true}`
	if _, code := f.createSchedule(t, f.owner.ID, f.repoID, body); code != http.StatusServiceUnavailable {
		t.Fatalf("removal accepted on a failed settings read: %d", code)
	}
}

func TestScheduleRemovalHistoricalSnapshotLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	d, code := f.createSchedule(t, f.owner.ID, f.repoID, `{"target":"sweep","labels":["on-deck"],"timing":"recurring","cron_expr":"0 * * * *","remove_label_on_dispatch":true}`)
	if code != http.StatusCreated {
		t.Fatal(code)
	}
	mustExecT(ctx, t, f.pool, `UPDATE run_schedules SET last_fire='{"started":[{"issue_iid":7,"run_id":"historical","title":"candidate","web_url":"https://forge.e2e/7","label_removed":true,"selector_label":"on-deck"},{"issue_iid":8,"run_id":"failed-removal","title":"other","label_remove_failed":true,"selector_label":"on-deck"}]}' WHERE id=$1`, d.ID)
	if _, code := f.patchSchedule(t, f.owner.ID, d.ID, `{"labels":["other"],"remove_label_on_dispatch":false}`); code != http.StatusOK {
		t.Fatal(code)
	}
	got, code := f.getSchedule(t, f.owner.ID, d.ID)
	if code != http.StatusOK || got.LastFire == nil || len(got.LastFire.Started) != 2 {
		t.Fatalf("get=%d %+v", code, got)
	}
	a, b := got.LastFire.Started[0], got.LastFire.Started[1]
	if !a.LabelRemoved || a.LabelRemoveFailed || a.SelectorLabel != "on-deck" || a.WebURL != "https://forge.e2e/7" || b.LabelRemoved || !b.LabelRemoveFailed || b.SelectorLabel != "on-deck" {
		t.Fatalf("historical flags changed: %+v", got.LastFire.Started)
	}
}

func TestDefaultScheduleRemovalLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	d, code := f.enableCatalog(t, f.owner.ID, f.repoID, "bug-triage")
	if code != 201 || d.RemoveLabelOnDispatch {
		t.Fatalf("enable=%d %+v", code, d)
	}
	for _, body := range []string{`{"remove_label_on_dispatch":true}`, `{"cron_expr":"0 2 * * *"}`} {
		got, status := f.patchSchedule(t, f.owner.ID, d.ID, body)
		if status != 200 || !got.RemoveLabelOnDispatch || !got.Customized {
			t.Fatalf("default patch=%d %+v", status, got)
		}
	}
	clone, code := f.cloneSchedule(t, f.owner.ID, d.ID, `{}`)
	if code != 201 || !clone.RemoveLabelOnDispatch {
		t.Fatalf("default clone=%d %+v", code, clone)
	}
	got, code := f.patchSchedule(t, f.owner.ID, d.ID, `{"remove_label_on_dispatch":false}`)
	if code != 200 || got.RemoveLabelOnDispatch {
		t.Fatalf("default clear=%d %+v", code, got)
	}
	if _, code = f.patchSchedule(t, f.owner.ID, d.ID, `{"remove_label_on_dispatch":true}`); code != 200 {
		t.Fatal(code)
	}
	r := userReq(http.MethodPost, "/api/schedules/"+d.ID+"/reset", "", f.owner.ID, map[string]string{"id": d.ID})
	w := httptest.NewRecorder()
	f.h.ResetSchedule(w, r)
	var reset apitypes.ScheduleDTO
	if err := json.Unmarshal(w.Body.Bytes(), &reset); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || reset.RemoveLabelOnDispatch || reset.Customized {
		t.Fatalf("reset=%d %+v", w.Code, reset)
	}
	for _, slug := range []string{"assigned-sweep", "docs-hygiene"} {
		other, status := f.enableCatalog(t, f.owner.ID, f.repoID, slug)
		if status != 201 {
			t.Fatal(status)
		}
		if _, status = f.patchSchedule(t, f.owner.ID, other.ID, `{"remove_label_on_dispatch":true}`); status != 400 {
			t.Fatalf("%s=%d", slug, status)
		}
	}
}
