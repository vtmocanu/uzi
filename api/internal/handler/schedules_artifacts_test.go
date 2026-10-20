package handler

import (
	"encoding/json"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"testing"
	"time"
)

func TestCIArtifactsScheduleContract(t *testing.T) {
	for _, body := range []string{`{}`, `{"ci_artifacts":null}`, `{"ci_artifacts":[]}`, `{"ci_artifacts":[" report-* "]}`} {
		var req apitypes.ScheduleRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		cur := store.RunSchedule{Target: "ci_watch", CiArtifacts: []byte(`["old"]`), CiWorkflows: []byte(`["workflow"]`)}
		merged := mergeSchedule(cur, req)
		if len(merged.CIWorkflows.Value) != 1 {
			t.Fatal("artifact edit cleared workflows")
		}
		if body == `{}` {
			if len(merged.CIArtifacts.Value) != 1 || merged.CIArtifacts.Value[0] != "old" {
				t.Fatal("absent did not keep")
			}
		} else if !req.CIArtifacts.Present {
			t.Fatal("presence lost")
		}
	}
	req := ciWatchReq(func(r *apitypes.ScheduleRequest) {
		r.CIArtifacts = apitypes.OptionalCIArtifacts{Present: true, Value: []string{" report-* "}}
	})
	got, status, msg := validateScheduleConfig(req, time.Now(), false)
	if status != 0 || got.CIArtifacts.Value[0] != "report-*" {
		t.Fatalf("%d %s %+v", status, msg, got)
	}
	req.Target = "prompt"
	if _, status, _ := validateScheduleConfig(req, time.Now(), false); status == 0 {
		t.Fatal("non-ci target accepted")
	}
}
