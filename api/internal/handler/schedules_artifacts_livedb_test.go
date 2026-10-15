package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestCIArtifactsScheduleRoundTripLiveDB(t *testing.T) {
	f := newScheduleFixture(context.Background(), t)
	dto := f.createCIWatch(t, `,"ci_artifacts":[" logs-* "],"ci_workflows":["nightly"]`)
	if !reflect.DeepEqual(dto.CIArtifacts, []string{"logs-*"}) {
		t.Fatalf("create artifacts = %v", dto.CIArtifacts)
	}
	kept, code := f.patchSchedule(t, f.owner.ID, dto.ID, `{"cron_expr":"5 * * * *"}`)
	if code != http.StatusOK || !reflect.DeepEqual(kept.CIArtifacts, dto.CIArtifacts) {
		t.Fatalf("absent keeps artifacts: %d %+v", code, kept)
	}
	clone, code := f.cloneSchedule(t, f.owner.ID, dto.ID, "")
	if code != http.StatusCreated || !reflect.DeepEqual(clone.CIArtifacts, dto.CIArtifacts) {
		t.Fatalf("clone artifacts: %d %+v", code, clone)
	}
	for _, body := range []string{`{"ci_artifacts":[]}`, `{"ci_artifacts":null}`} {
		f.patchSchedule(t, f.owner.ID, dto.ID, `{"ci_artifacts":["again"]}`)
		cleared, code := f.patchSchedule(t, f.owner.ID, dto.ID, body)
		if code != http.StatusOK || len(cleared.CIArtifacts) != 0 || !reflect.DeepEqual(cleared.CIWorkflows, []string{"nightly"}) {
			t.Fatalf("clear: %d %+v", code, cleared)
		}
	}
	for _, body := range []string{`{"ci_artifacts":["a"," a "]}`, `{"ci_artifacts":["x\u202e"]}`, `{"ci_artifacts":["x\ny"]}`, `{"ci_artifacts":["["]}`} {
		if code := f.patchStatus(t, f.owner.ID, dto.ID, body); code != http.StatusBadRequest {
			t.Fatalf("invalid selectors %s accepted: %d", body, code)
		}
	}

	def, code := f.enableCatalog(t, f.owner.ID, f.repoID, "ci-repair")
	if code != http.StatusCreated {
		t.Fatalf("enable default: %d", code)
	}
	custom := f.patchDefault(t, f.owner.ID, def.ID, `{"ci_artifacts":["e2e-*"]}`)
	if !custom.Customized || !reflect.DeepEqual(custom.CIArtifacts, []string{"e2e-*"}) || custom.AutoApprove {
		t.Fatalf("custom default = %+v", custom)
	}
	clone, code = f.cloneSchedule(t, f.owner.ID, def.ID, "")
	if code != http.StatusCreated || !reflect.DeepEqual(clone.CIArtifacts, custom.CIArtifacts) || clone.AutoApprove {
		t.Fatalf("default clone: %d %+v", code, clone)
	}
	req := userReq(http.MethodPost, "/api/schedules/"+def.ID+"/reset", "", f.owner.ID, map[string]string{"id": def.ID})
	rec := httptest.NewRecorder()
	f.h.ResetSchedule(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	reset, code := f.getSchedule(t, f.owner.ID, def.ID)
	if code != http.StatusOK || reset.Customized || reset.AutoApprove || len(reset.CIArtifacts) != 0 {
		t.Fatalf("reset artifacts: %d %+v", code, reset)
	}
}
