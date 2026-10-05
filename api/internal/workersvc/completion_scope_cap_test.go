package workersvc

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func cappedPermitRequest(t *testing.T, capped bool) CompletionPermitRequest {
	t.Helper()
	return CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: "h", ScopeCapped: &capped}
}

func TestScopeCapPermit(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cap           int32
		done          []string
		capped, grant bool
		deny          string
	}{
		{"reached nonprefix distinct", 2, []string{"m4", "m2", "m4", "unknown"}, true, true, ""},
		{"unreached", 3, []string{"m4", "m2", "m4", "unknown"}, true, false, "scope_cap_not_reached"},
		{"full never partial", 2, []string{"m1", "m2", "m3", "m4"}, true, false, "scope_cap_not_reached"},
		{"zero", 0, nil, true, true, ""},
		{"negative", -1, nil, true, false, "scope_cap_not_reached"},
		{"above total", 5, []string{"m1", "m2", "m3", "m4"}, true, false, "scope_cap_not_reached"},
		{"ordinary false", 2, []string{"m4", "m2"}, false, false, "missing_milestones"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := worker()
			run := interlockedRun(w)
			run.MilestonesFrozen = frozenJSON(t, "m1", "m2", "m3", "m4")
			run.CompletionContract = contractJSON(t, "m1", "m2", "m3", "m4")
			run.MilestonesCompleted = idsJSON(t, tc.done...)
			run.ScopeCeiling = pgtype.Int4{Int32: tc.cap, Valid: true}
			fs := &fakeStore{runOwned: run}
			svc := New(fs, newBox(t), testParams())
			res, err := svc.RequestCompletionPermit(context.Background(), w, run.ID, cappedPermitRequest(t, tc.capped))
			if err != nil || res.Granted != tc.grant || res.DenyReason != tc.deny {
				t.Fatalf("result=%+v err=%v want grant=%v deny=%s", res, err, tc.grant, tc.deny)
			}
			if tc.capped && len(fs.recordedAttempts) != 0 {
				t.Fatal("partial authorization must not alter ordinary attempts")
			}
		})
	}
}

func TestScopeCapInvalidContract(t *testing.T) {
	for _, contract := range []string{"null", "{}", "{", "[]",
		`{"profile":"structural","revision":1}`,
		`{"profile":"structural","revision":1,"criteria":null}`,
		`{"profile":"structural","revision":1,"criteria":[]}`,
		`{"profile":"other","revision":1,"criteria":[]}`,
		string(contractJSON(t, "foreign")),
	} {
		t.Run(contract, func(t *testing.T) {
			w := worker()
			run := interlockedRun(w)
			run.MilestonesFrozen = frozenJSON(t, "m1")
			run.CompletionContract = []byte(contract)
			run.ScopeCeiling = pgtype.Int4{Valid: true}
			fs := &fakeStore{runOwned: run}
			svc := New(fs, newBox(t), testParams())
			res, err := svc.RequestCompletionPermit(context.Background(), w, run.ID, cappedPermitRequest(t, true))
			if err != nil || res.DenyReason != "contract_not_frozen" || res.Granted {
				t.Fatalf("result=%+v err=%v", res, err)
			}
		})
	}
}

func TestScopeCapWaiversDoNotCount(t *testing.T) {
	w := worker()
	run := interlockedRun(w)
	run.MilestonesFrozen = frozenJSON(t, "m1", "m2")
	run.CompletionContract = []byte(`{"profile":"structural","revision":2,"criteria":[{"id":"m1.c1","milestone_id":"m1","text":"m1","audit":null,"finding_ids":[]},{"id":"m2.c1","milestone_id":"m2","text":"m2","audit":null,"finding_ids":[]}],"scope":{"in":["m1"],"out":[{"milestone_id":"m2","reason":"later","revision":2}]},"accepted":[{"id":"m1.c1","milestone_id":"m1","text":"m1","reason":"waived","revision":2}]}`)
	run.ContractRevision = pgtype.Int4{Int32: 2, Valid: true}
	run.ScopeCeiling = pgtype.Int4{Int32: 1, Valid: true}
	fs := &fakeStore{runOwned: run}
	svc := New(fs, newBox(t), testParams())
	req := cappedPermitRequest(t, true)
	req.ContractRevision = 2
	res, err := svc.RequestCompletionPermit(context.Background(), w, run.ID, req)
	if err != nil || res.Granted || res.DenyReason != "scope_cap_not_reached" {
		t.Fatalf("result=%+v err=%v", res, err)
	}
}

func TestScopeCapPersistedShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*store.Run)
		deny   string
	}{
		{"no ceiling", func(r *store.Run) { r.ScopeCeiling = pgtype.Int4{} }, "scope_cap_not_reached"},
		{"request revision mismatch", func(r *store.Run) {
			r.ContractRevision = pgtype.Int4{Int32: 2, Valid: true}
		}, CompletionDenyRevisionDrift},
		{"empty frozen", func(r *store.Run) { r.MilestonesFrozen = []byte("[]") }, "contract_not_frozen"},
		{"null frozen", func(r *store.Run) { r.MilestonesFrozen = []byte("null") }, "contract_not_frozen"},
		{"corrupt frozen", func(r *store.Run) { r.MilestonesFrozen = []byte("{}") }, "contract_not_frozen"},
		{"duplicate frozen", func(r *store.Run) { r.MilestonesFrozen = frozenJSON(t, "m1", "m1") }, "contract_not_frozen"},
		{"corrupt completed", func(r *store.Run) { r.MilestonesCompleted = []byte("{}") }, "contract_not_frozen"},
		{"revision mismatch", func(r *store.Run) {
			r.CompletionContract = []byte(strings.Replace(string(r.CompletionContract), `"revision":1`, `"revision":2`, 1))
		}, "contract_not_frozen"},
		{"profile mismatch", func(r *store.Run) {
			r.CompletionContract = []byte(strings.Replace(string(r.CompletionContract), "structural", "semantic", 1))
		}, "contract_not_frozen"},
		{"criterion malformed", func(r *store.Run) {
			r.CompletionContract = []byte(`{"profile":"structural","revision":1,"criteria":[null,null]}`)
		}, "contract_not_frozen"},
		{"missing audit", func(r *store.Run) {
			r.CompletionContract = []byte(strings.ReplaceAll(string(r.CompletionContract), `"audit":null,`, ""))
		}, "contract_not_frozen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := worker()
			run := interlockedRun(w)
			run.MilestonesFrozen = frozenJSON(t, "m1", "m2")
			run.CompletionContract = contractJSON(t, "m1", "m2")
			run.ScopeCeiling = pgtype.Int4{Valid: true}
			tc.mutate(&run)
			svc := New(&fakeStore{runOwned: run}, newBox(t), testParams())
			res, err := svc.RequestCompletionPermit(context.Background(), w, run.ID, cappedPermitRequest(t, true))
			if err != nil || res.Granted || res.DenyReason != tc.deny {
				t.Fatalf("result=%+v err=%v want=%s", res, err, tc.deny)
			}
		})
	}
}

// Keep omission on the ordinary public interface.
func TestScopeCapPermitOmission(t *testing.T) {
	w := worker()
	run := interlockedRun(w)
	run.CompletionContract = contractJSON(t, "m1", "m2")
	run.MilestonesCompleted = idsJSON(t, "m1")
	run.ScopeCeiling = pgtype.Int4{Int32: 1, Valid: true}
	svc := New(&fakeStore{runOwned: run}, newBox(t), testParams())
	res, err := svc.RequestCompletionPermit(context.Background(), w, run.ID, CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: "h"})
	if err != nil || res.Granted || res.DenyReason != CompletionDenyMissingMilestones {
		t.Fatalf("result=%+v err=%v", res, err)
	}
}
