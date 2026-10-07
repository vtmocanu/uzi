package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestScheduleCapacityCRUDLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)
	dto, code := f.createSchedule(t, f.owner.ID, f.repoID, `{"target":"sweep","labels":["uzi","bug"],"timing":"recurring","cron_expr":"0 * * * *","capacity_limit":4,"capacity_room_needed":2}`)
	if code != 201 {
		t.Fatalf("create=%d", code)
	}
	assert := func(d apitypes.ScheduleDTO, c, k int) {
		t.Helper()
		if d.CapacityLimit == nil || d.CapacityRoomNeeded == nil || *d.CapacityLimit != c || *d.CapacityRoomNeeded != k {
			t.Fatalf("capacity=%+v/%+v", d.CapacityLimit, d.CapacityRoomNeeded)
		}
	}
	assert(dto, 4, 2)
	for _, body := range []string{`{"cron_expr":"0 2 * * *"}`, `{"enabled":false}`, `{"enabled":true}`, `{"enabled":true,"capacity_limit":4,"capacity_room_needed":2}`} {
		d, status := f.patchSchedule(t, f.owner.ID, dto.ID, body)
		if status != 200 {
			t.Fatalf("patch %s=%d", body, status)
		}
		assert(d, 4, 2)
	}
	for _, body := range []string{`{"enabled":true,"capacity_limit":null}`, `{"capacity_room_needed":null,"capacity_limit":5}`, `{"enabled":true,"capacity_limit":4,"capacity_room_needed":null}`, `{"enabled":true,"capacity_limit":1,"capacity_room_needed":2}`, `{"capacity_limit":null,"capacity_room_needed":2}`, `{"target":"prompt","prompt":"x"}`, `{"timing":"once","run_at":"2099-01-01T00:00:00Z"}`} {
		_, status := f.patchSchedule(t, f.owner.ID, dto.ID, body)
		if status != 400 {
			t.Fatalf("patch %s=%d want 400", body, status)
		}
	}
	d, status := f.patchSchedule(t, f.owner.ID, dto.ID, `{"capacity_limit":5}`)
	if status != 200 {
		t.Fatal(status)
	}
	assert(d, 5, 2)
	clone, status := f.cloneSchedule(t, f.owner.ID, dto.ID, `{}`)
	if status != 201 {
		t.Fatalf("clone=%d", status)
	}
	assert(clone, 5, 2)
	repo := f.insertRepo(ctx, t, f.owner, 7802, "g/capacity-sibling")
	sibling, status := f.addRepo(t, f.owner.ID, dto.ID, repo.String())
	if status != 201 {
		t.Fatalf("add repo=%d", status)
	}
	assert(sibling, 5, 2)
	d, status = f.patchSchedule(t, f.owner.ID, dto.ID, `{"enabled":true,"capacity_limit":null,"capacity_room_needed":null}`)
	if status != 200 || d.CapacityLimit != nil || d.CapacityRoomNeeded != nil {
		t.Fatalf("clear=%d %+v", status, d)
	}
}

func TestDefaultScheduleCapacityLiveDB(t *testing.T) {
	f := newScheduleFixture(context.Background(), t)
	dto, status := f.enableCatalog(t, f.owner.ID, f.repoID, "bug-triage")
	if status != 201 {
		t.Fatal(status)
	}
	for _, body := range []string{`{"enabled":true,"capacity_limit":4,"capacity_room_needed":2}`, `{"cron_expr":"0 2 * * *"}`, `{"enabled":false}`, `{"enabled":true}`, `{"capacity_limit":5}`} {
		d, code := f.patchSchedule(t, f.owner.ID, dto.ID, body)
		wantLimit := 4
		if body == `{"capacity_limit":5}` {
			wantLimit = 5
		}
		if code != 200 || !d.Customized || d.CapacityLimit == nil || d.CapacityRoomNeeded == nil || *d.CapacityRoomNeeded != 2 || *d.CapacityLimit != wantLimit {
			t.Fatalf("default patch %s: %d %+v", body, code, d)
		}
	}
	for _, body := range []string{`{"enabled":true,"capacity_limit":null}`, `{"capacity_limit":null,"capacity_room_needed":2}`, `{"enabled":true,"capacity_room_needed":null}`, `{"capacity_limit":4,"capacity_room_needed":null}`, `{"enabled":true,"capacity_limit":1,"capacity_room_needed":2}`} {
		_, code := f.patchSchedule(t, f.owner.ID, dto.ID, body)
		if code != 400 {
			t.Fatalf("half clear=%d", code)
		}
	}
	clone, code := f.cloneSchedule(t, f.owner.ID, dto.ID, `{}`)
	if code != 201 || clone.CapacityLimit == nil || *clone.CapacityLimit != 5 || clone.CapacityRoomNeeded == nil || *clone.CapacityRoomNeeded != 2 {
		t.Fatalf("default clone=%d %+v", code, clone)
	}
	repo := f.insertRepo(context.Background(), t, f.owner, 7803, "g/default-capacity-sibling")
	sibling, code := f.addRepo(t, f.owner.ID, clone.ID, repo.String())
	if code != 201 || sibling.CapacityLimit == nil || sibling.CapacityRoomNeeded == nil || *sibling.CapacityLimit != 5 || *sibling.CapacityRoomNeeded != 2 {
		t.Fatalf("default addrepo=%d %+v", code, sibling)
	}
	cleared, code := f.patchSchedule(t, f.owner.ID, dto.ID, `{"enabled":true,"capacity_limit":null,"capacity_room_needed":null}`)
	if code != 200 || cleared.CapacityLimit != nil || cleared.CapacityRoomNeeded != nil {
		t.Fatalf("default clear=%d %+v", code, cleared)
	}
	_, code = f.patchSchedule(t, f.owner.ID, dto.ID, `{"capacity_limit":4,"capacity_room_needed":2}`)
	if code != 200 {
		t.Fatal(code)
	}
	r := userReq(http.MethodPost, "/api/schedules/"+dto.ID+"/reset", "", f.owner.ID, map[string]string{"id": dto.ID})
	w := httptest.NewRecorder()
	f.h.ResetSchedule(w, r)
	var reset apitypes.ScheduleDTO
	if err := json.Unmarshal(w.Body.Bytes(), &reset); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || reset.Customized || reset.CapacityLimit != nil || reset.CapacityRoomNeeded != nil {
		t.Fatalf("reset=%d %+v", w.Code, reset)
	}
	for _, slug := range []string{"assigned-sweep", "docs-hygiene"} {
		d, code := f.enableCatalog(t, f.owner.ID, f.repoID, slug)
		if code != 201 {
			t.Fatal(code)
		}
		_, code = f.patchSchedule(t, f.owner.ID, d.ID, `{"capacity_limit":4,"capacity_room_needed":2}`)
		if code != 400 {
			t.Fatalf("%s gate=%d", slug, code)
		}
	}
}
