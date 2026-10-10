package uzicli

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestFakeClientRunSummariesProjectCountAndRecordCtx(t *testing.T) {
	plan := "heavy plan"
	row := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: "r1", Status: "running", PlanMd: &plan, IssueDescription: "body"}}
	f := &FakeClient{Runs: []apitypes.RunListItemDTO{row}, AdminRuns: []apitypes.RunListItemDTO{row}}
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "x")

	own, err := f.ListRunSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adm, err := f.AdminListRunSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, rows := range map[string][]apitypes.RunSummaryItemDTO{"owner": own, "admin": adm} {
		if len(rows) != 1 || rows[0].ID != "r1" {
			t.Fatalf("%s rows = %#v", name, rows)
		}
		b, _ := json.Marshal(rows)
		var got []map[string]json.RawMessage
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		for _, heavy := range []string{"plan_md", "repo_agents", "issue_description", "preserved_patch"} {
			if _, present := got[0][heavy]; present {
				t.Errorf("%s summary row carries heavy key %q", name, heavy)
			}
		}
	}
	if f.ListRunSummariesCalls != 1 || f.AdminListRunSummariesCalls != 1 || f.ListRunsCalls != 0 || f.AdminListRunsCalls != 0 {
		t.Errorf("counters summaries=%d/%d legacy=%d/%d", f.ListRunSummariesCalls, f.AdminListRunSummariesCalls, f.ListRunsCalls, f.AdminListRunsCalls)
	}
	if f.LastListRunSummariesCtx != ctx {
		t.Error("LastListRunSummariesCtx did not record the call's context")
	}

	f.Err = errors.New("boom")
	if _, err := f.ListRunSummaries(ctx); !errors.Is(err, f.Err) {
		t.Errorf("owner err = %v", err)
	}
	if _, err := f.AdminListRunSummaries(ctx); !errors.Is(err, f.Err) {
		t.Errorf("admin err = %v", err)
	}
}
