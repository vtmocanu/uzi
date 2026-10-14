package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
	"slices"
	"strings"
	"testing"
)

func TestReportOnlyDeliveryMatrixLiveDB(t *testing.T) {
	for _, name := range []string{
		"missing milestone", "final declaration union", "invalid declaration", "duplicate declaration",
		"SQLNULL frozen", "jsonnull frozen", "object frozen", "duplicate frozen", "empty frozen",
		"naked criteria", "bare empty criteria", "SQLNULL contract", "missing criteria", "missing findings", "null findings", "zero revision", "zero contract revision", "null criteria", "missing audit", "nonempty findings", "wrong criterion",
		"wrong title", "bad profile", "bad version", "missing revision", "revision mismatch",
		"bad accepted", "bad scope", "waived undeclared", "deferred undeclared", "deferred completed",
		"corrupt completed", "null completed", "foreign completed", "duplicate completed",
		"missing generation", "matching zero generation", "mismatching generation", "released", "foreign owner",
		"stored branch", "stored MR IID", "stored MR URL", "stored final head", "legacy headless", "legacy capped headless",
		"NULL report retry",
	} {
		t.Run(name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			wid := e.seedWorker(t, []string{"completion_interlock_v1"})
			id := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1", "m2"}, false)
			e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", id)
			w := store.Worker{ID: wid}
			req := reportOnlyRequest()
			want := false
			contract := func(change func(*completionContract)) {
				run, err := svc.runOwnedByWorker(e.ctx, id, w)
				if err != nil {
					t.Fatal(err)
				}
				var c completionContract
				if err := json.Unmarshal(run.CompletionContract, &c); err != nil {
					t.Fatal(err)
				}
				change(&c)
				raw, err := json.Marshal(c)
				if err != nil {
					t.Fatal(err)
				}
				e.exec(t, "UPDATE runs SET completion_contract=$2 WHERE id=$1", id, raw)
			}
			switch name {
			case "missing milestone":
				e.exec(t, "UPDATE runs SET milestones_completed='[\"m1\"]' WHERE id=$1", id)
			case "final declaration union":
				e.exec(t, "UPDATE runs SET milestones_completed='[\"m1\"]' WHERE id=$1", id)
				declared := []string{"m2"}
				req.MilestonesCompleted = &declared
				want = true
			case "invalid declaration", "duplicate declaration":
				e.exec(t, "UPDATE runs SET milestones_completed='[\"m1\"]' WHERE id=$1", id)
				declared := []string{"m2", "foreign"}
				if name == "duplicate declaration" {
					declared = []string{"m2", "m2"}
				}
				req.MilestonesCompleted = &declared
			case "SQLNULL frozen":
				e.exec(t, "UPDATE runs SET milestones_frozen=NULL WHERE id=$1", id)
			case "jsonnull frozen":
				assertReportOnlyFixtureConstraint(t, e, id, "UPDATE runs SET milestones_frozen='null' WHERE id=$1")
				return
			case "object frozen":
				assertReportOnlyFixtureConstraint(t, e, id, "UPDATE runs SET milestones_frozen='{}' WHERE id=$1")
				return
			case "duplicate frozen":
				e.exec(t, "UPDATE runs SET milestones_frozen='[{\"id\":\"m1\",\"title\":\"x\"},{\"id\":\"m1\",\"title\":\"x\"}]' WHERE id=$1", id)
			case "empty frozen":
				raw, err := buildCompletionContract([]byte("[]"))
				if err != nil {
					t.Fatal(err)
				}
				e.exec(t, "UPDATE runs SET milestones_frozen='[]',milestones_completed='[]',completion_contract=$2 WHERE id=$1", id, raw)
				want = true
			case "bare empty criteria":
				e.exec(t, "UPDATE runs SET milestones_frozen='[]',milestones_completed='[]',completion_contract='{\"criteria\":[]}' WHERE id=$1", id)
			case "SQLNULL contract":
				e.exec(t, "UPDATE runs SET completion_contract=NULL WHERE id=$1", id)
			case "missing criteria":
				e.exec(t, "UPDATE runs SET completion_contract='{}' WHERE id=$1", id)
			case "missing findings":
				e.exec(t, "UPDATE runs SET completion_contract=jsonb_set(completion_contract,'{criteria,0}',(completion_contract->'criteria'->0)-'finding_ids') WHERE id=$1", id)
			case "null findings":
				contract(func(c *completionContract) { c.Criteria[0].FindingIDs = nil })
			case "zero revision":
				e.exec(t, "UPDATE runs SET contract_revision=0 WHERE id=$1", id)
			case "zero contract revision":
				contract(func(c *completionContract) { c.Revision = 0 })
			case "naked criteria":
				e.exec(t, "UPDATE runs SET completion_contract='[]' WHERE id=$1", id)
			case "null criteria":
				contract(func(c *completionContract) { c.Criteria = nil })
			case "missing audit":
				e.exec(t, "UPDATE runs SET completion_contract=jsonb_set(completion_contract,'{criteria,0}',(completion_contract->'criteria'->0)-'audit') WHERE id=$1", id)
			case "nonempty findings":
				contract(func(c *completionContract) { c.Criteria[0].FindingIDs = []string{"f"} })
			case "wrong criterion":
				contract(func(c *completionContract) { c.Criteria[0].ID = "wrong" })
			case "wrong title":
				contract(func(c *completionContract) { c.Criteria[0].Text = "wrong" })
			case "bad profile":
				contract(func(c *completionContract) { c.Profile = "semantic" })
			case "bad version":
				e.exec(t, "UPDATE runs SET completion_contract_version=2 WHERE id=$1", id)
			case "missing revision":
				e.exec(t, "UPDATE runs SET contract_revision=NULL WHERE id=$1", id)
			case "revision mismatch":
				contract(func(c *completionContract) { c.Revision = 2 })
			case "bad accepted":
				contract(func(c *completionContract) {
					c.Accepted = []acceptedEntry{{ID: "unknown", Reason: "owner", Revision: 1}}
				})
			case "bad scope":
				contract(func(c *completionContract) { c.Scope = &scopeBlock{In: []string{"m1"}} })
			case "waived undeclared":
				e.exec(t, "UPDATE runs SET milestones_completed='[\"m1\"]' WHERE id=$1", id)
				contract(func(c *completionContract) {
					cr := c.Criteria[1]
					c.Accepted = []acceptedEntry{{ID: cr.ID, MilestoneID: cr.MilestoneID, Text: cr.Text, Reason: "owner", Revision: 1}}
				})
			case "deferred undeclared", "deferred completed":
				if name == "deferred undeclared" {
					e.exec(t, "UPDATE runs SET milestones_completed='[\"m1\"]' WHERE id=$1", id)
				} else {
					want = true
				}
				contract(func(c *completionContract) {
					c.Scope = &scopeBlock{In: []string{"m1"}, Out: []deferredEntry{{MilestoneID: "m2", Reason: "later", Revision: 1}}}
				})
			case "corrupt completed":
				assertReportOnlyFixtureConstraint(t, e, id, "UPDATE runs SET milestones_completed='{}' WHERE id=$1")
				return
			case "null completed":
				assertReportOnlyFixtureConstraint(t, e, id, "UPDATE runs SET milestones_completed='null' WHERE id=$1")
				return
			case "foreign completed":
				e.exec(t, "UPDATE runs SET milestones_completed='[\"m1\",\"m2\",\"foreign\"]' WHERE id=$1", id)
			case "duplicate completed":
				e.exec(t, "UPDATE runs SET milestones_completed='[\"m1\",\"m2\",\"m2\"]' WHERE id=$1", id)
			case "missing generation":
				req.ClaimGeneration = nil
			case "matching zero generation":
				zero := int64(0)
				req.ClaimGeneration = &zero
				e.exec(t, "UPDATE runs SET claim_generation=0 WHERE id=$1", id)
			case "mismatching generation":
				two := int64(2)
				req.ClaimGeneration = &two
			case "released":
				e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", id)
			case "foreign owner":
				w.ID = e.seedWorker(t, nil)
			case "stored branch":
				e.exec(t, "UPDATE runs SET branch='' WHERE id=$1", id)
			case "stored MR IID":
				e.exec(t, "UPDATE runs SET mr_iid=0 WHERE id=$1", id)
			case "stored MR URL":
				e.exec(t, "UPDATE runs SET mr_web_url='' WHERE id=$1", id)
			case "stored final head":
				// The publication trigger retains heads only at a permitted terminal write.
				head := strings.Repeat("a", 40)
				e.exec(t, "UPDATE workers SET protocol_capabilities=ARRAY['recovery_completed_publication_v1'] WHERE id=$1", wid)
				p, err := svc.RequestCompletionPermit(e.ctx, w, id, CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: head})
				if err != nil || !p.Granted {
					t.Fatalf("stored-head permit: %+v %v", p, err)
				}
				e.exec(t, "UPDATE run_completion_permits SET consumed_at=now() WHERE run_id=$1", id)
				e.exec(t, "UPDATE runs SET status='completed',report_only=true,report_md='Findings',completion_final_head=$2 WHERE id=$1", id, head)
				var stored string
				if err := e.pool.QueryRow(e.ctx, "SELECT completion_final_head FROM runs WHERE id=$1", id).Scan(&stored); err != nil || stored != head {
					t.Fatalf("publication head fixture not retained: %q %v", stored, err)
				}
			case "legacy headless", "legacy capped headless":
				if name == "legacy capped headless" {
					yes := true
					req.ScopeCapped = &yes
				}
				id = e.seedLegacyRunningRun(t, wid)
				req.ClaimGeneration = nil
				want = true
			case "NULL report retry":
				req.ReportMd = nil
				want = true
			}
			var wantErr error
			switch name {
			case "missing generation":
				wantErr = ErrMissingClaimGeneration
			case "matching zero generation", "mismatching generation", "released":
				wantErr = ErrStaleClaim
			case "foreign owner":
				wantErr = ErrRunNotOwned
			}
			before := reportOnlySnapshot(t, e, id)
			run, ok, err := svc.SetState(e.ctx, w, id, req)
			if ok != want || (want && err != nil) {
				t.Fatalf("applied=%v want=%v status=%s err=%v", ok, want, run.Status, err)
			}
			if wantErr != nil && !errors.Is(err, wantErr) {
				t.Fatalf("error=%v want=%v", err, wantErr)
			}
			if !want && !bytes.Equal(before, reportOnlySnapshot(t, e, id)) {
				t.Fatal("refusal mutated run")
			}
			if want && name == "deferred completed" && run.StopKind.String != "scope_reduced" {
				t.Fatalf("completed owner-deferred contract lost disposition: %v", run.StopKind)
			}
			if want && name == "final declaration union" {
				var ids []byte
				if err := e.pool.QueryRow(e.ctx, "SELECT milestones_completed FROM runs WHERE id=$1", id).Scan(&ids); err != nil {
					t.Fatal(err)
				}
				var done []string
				if err := json.Unmarshal(ids, &done); err != nil {
					t.Fatalf("persisted union=%s err=%v", ids, err)
				}
				slices.Sort(done)
				if !slices.Equal(done, []string{"m1", "m2"}) {
					t.Fatalf("persisted union=%s want exactly [m1 m2]", ids)
				}
			}
			if name == "NULL report retry" {
				before = reportOnlySnapshot(t, e, id)
				_, ok, err = svc.SetState(e.ctx, w, id, req)
				if err != nil || !ok || !bytes.Equal(before, reportOnlySnapshot(t, e, id)) {
					t.Fatalf("NULL retry: %v %v", ok, err)
				}
			}
		})
	}
}

func TestReportOnlyBeginRacesLiveDB(t *testing.T) {
	for _, name := range []string{"generation drift", "contract drift", "progress removal", "publication insertion"} {
		t.Run(name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			wid := e.seedWorker(t, nil)
			id := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
			e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", id)
			hook := &reportOnlyBeginHook{e: e}
			var afterRace []byte
			hook.mutate = func() {
				switch name {
				case "generation drift":
					e.exec(t, "UPDATE runs SET claim_generation=2 WHERE id=$1", id)
				case "contract drift":
					e.exec(t, "UPDATE runs SET completion_contract=jsonb_set(completion_contract,'{revision}','2') WHERE id=$1", id)
				case "progress removal":
					e.exec(t, "UPDATE runs SET milestones_completed='[]' WHERE id=$1", id)
				case "publication insertion":
					e.exec(t, "UPDATE runs SET branch='b' WHERE id=$1", id)
				}
				afterRace = reportOnlySnapshot(t, e, id)
			}
			svc.SetTxBeginner(hook)
			_, ok, _ := svc.SetState(e.ctx, store.Worker{ID: wid}, id, reportOnlyRequest())
			if !hook.fired {
				t.Fatal("Begin hook did not fire")
			}
			if ok || !bytes.Equal(afterRace, reportOnlySnapshot(t, e, id)) {
				t.Fatal("raced refusal applied or wrote run")
			}
		})
	}
}

func TestReportOnlyRawShapeLiveDB(t *testing.T) {
	for _, field := range []string{"Head", "Branch", "CompletionFinalHead", "MrWebURL", "MrIID"} {
		for _, value := range []string{"", " ", "\x00", " \x00 ", "matching permit"} {
			for _, public := range []bool{false, true} {
				t.Run(field+"/"+value+fmt.Sprint(public), func(t *testing.T) {
					e := setupInterlockLiveDB(t)
					svc := e.permitService(t)
					wid := e.seedWorker(t, nil)
					id := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
					e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", id)
					req := reportOnlyRequest()
					// A valid ordinary permit must not authorize a mixed report.
					p, err := svc.RequestCompletionPermit(e.ctx, store.Worker{ID: wid}, id, CompletionPermitRequest{ContractRevision: 1, Branch: "b", Head: "h"})
					if err != nil || !p.Granted {
						t.Fatalf("permit: %+v %v", p, err)
					}
					switch field {
					case "Head":
						req.Head = &value
					case "Branch":
						req.Branch = &value
					case "CompletionFinalHead":
						req.CompletionFinalHead = &value
					case "MrWebURL":
						req.MrWebURL = &value
					case "MrIID":
						zero := int64(0)
						req.MrIID = &zero
					}
					if value == "matching permit" {
						req.Head = strPtr("h")
						req.Branch = strPtr("b")
					}
					before := reportOnlySnapshot(t, e, id)
					var ok bool
					if public {
						res, eerr := svc.SetStateReportWithReconciliation(e.ctx, store.Worker{ID: wid}, id, req)
						ok, err = res.Applied, eerr
					} else {
						_, ok, err = svc.SetState(e.ctx, store.Worker{ID: wid}, id, req)
					}
					if ok || !errors.Is(err, ErrInvalidState) || !bytes.Equal(before, reportOnlySnapshot(t, e, id)) {
						t.Fatalf("shape: %v %v", ok, err)
					}
					var consumed bool
					if err := e.pool.QueryRow(e.ctx, "SELECT consumed_at IS NOT NULL FROM run_completion_permits WHERE run_id=$1", id).Scan(&consumed); err != nil || consumed {
						t.Fatalf("permit consumed=%v err=%v", consumed, err)
					}
				})
			}
		}
	}
}

func TestReportOnlyPublicationStillRequiresPermitLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, nil)
	id := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", id)
	gen := int64(1)
	before := reportOnlySnapshot(t, e, id)
	_, ok, err := svc.SetState(e.ctx, store.Worker{ID: wid}, id, StateRequest{State: "completed", Branch: strPtr("b"), Head: strPtr("h"), ClaimGeneration: &gen})
	if err != nil || ok || !bytes.Equal(before, reportOnlySnapshot(t, e, id)) {
		t.Fatalf("MR missing permit: %v %v", ok, err)
	}
}

func reportOnlyRequest() StateRequest {
	yes, gen := true, int64(1)
	return StateRequest{State: "completed", ReportOnly: &yes, ReportMd: strPtr("Findings"), ClaimGeneration: &gen}
}

func reportOnlySnapshot(t *testing.T, e interlockLiveDB, id uuid.UUID) []byte {
	t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(e.ctx, "SELECT to_jsonb(runs) FROM runs WHERE id=$1", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

type reportOnlyBeginHook struct {
	e      interlockLiveDB
	mutate func()
	fired  bool
}

func (b *reportOnlyBeginHook) Begin(ctx context.Context) (pgx.Tx, error) {
	if !b.fired {
		b.fired = true
		b.mutate()
	}
	return b.e.pool.Begin(ctx)
}

func TestReportOnlyFullDeliveryLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	id := e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", id)
	yes, gen := true, int64(1)
	req := StateRequest{State: "completed", ReportOnly: &yes, ReportMd: strPtr("Findings"), ClaimGeneration: &gen}
	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: wid}, id, req)
	if err != nil || !applied || run.Status != "completed" {
		t.Fatalf("headless full delivery: applied=%v status=%s err=%v", applied, run.Status, err)
	}
	var report string
	var permits int
	if err := e.pool.QueryRow(e.ctx, "SELECT report_md FROM runs WHERE id=$1", id).Scan(&report); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_completion_permits WHERE run_id=$1", id).Scan(&permits); err != nil {
		t.Fatal(err)
	}
	var publicationAbsent bool
	if err := e.pool.QueryRow(e.ctx, "SELECT branch IS NULL AND mr_iid IS NULL AND mr_web_url IS NULL AND completion_final_head IS NULL AND report_only FROM runs WHERE id=$1", id).Scan(&publicationAbsent); err != nil {
		t.Fatal(err)
	}
	if !publicationAbsent {
		t.Fatal("headless completion persisted publication or omitted report_only")
	}
	if report != "Findings" || permits != 0 {
		t.Fatalf("report=%q permits=%d", report, permits)
	}
	before := reportOnlySnapshot(t, e, id)
	_, applied, err = svc.SetState(e.ctx, store.Worker{ID: wid}, id, req)
	if err != nil || !applied {
		t.Fatalf("identical retry: %v %v", applied, err)
	}
	if !bytes.Equal(before, reportOnlySnapshot(t, e, id)) {
		t.Fatal("retry changed row including timestamps/report")
	}
	changed := req
	changed.ReportMd = strPtr("Changed")
	_, applied, err = svc.SetState(e.ctx, store.Worker{ID: wid}, id, changed)
	if err != nil || applied {
		t.Fatalf("changed retry: %v %v", applied, err)
	}
	mrRetry := StateRequest{State: "completed", Branch: strPtr("b"), Head: strPtr("h"), ClaimGeneration: &gen}
	_, applied, err = svc.SetState(e.ctx, store.Worker{ID: wid}, id, mrRetry)
	if err != nil || applied {
		t.Fatalf("MR retry: %v %v", applied, err)
	}
	if !bytes.Equal(before, reportOnlySnapshot(t, e, id)) {
		t.Fatal("refused retry mutated row")
	}
}

func TestReportOnlyCappedMixedKindsLiveDB(t *testing.T) {
	for _, kind := range []string{"issue", "task"} {
		for _, interlocked := range []bool{false, true} {
			if kind == "issue" && interlocked {
				continue
			}
			for _, public := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/interlocked=%v/public=%v", kind, interlocked, public), func(t *testing.T) {
					e := setupInterlockLiveDB(t)
					svc := e.permitService(t)
					wid := e.seedWorker(t, nil)
					id := e.seedLegacyRunningRun(t, wid)
					if kind == "task" {
						e.exec(t, "UPDATE runs SET kind='task',issue_iid=NULL,branch='task/b',claim_generation=1 WHERE id=$1", id)
					} else {
						e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", id)
					}
					if interlocked {
						e.exec(t, "UPDATE runs SET completion_contract_version=1 WHERE id=$1", id)
					}
					req := reportOnlyRequest()
					yes := true
					req.ScopeCapped = &yes
					req.Head = strPtr("h")
					req.Branch = strPtr("b")
					before := reportOnlySnapshot(t, e, id)
					var ok bool
					var err error
					if public {
						res, eerr := svc.SetStateReportWithReconciliation(e.ctx, store.Worker{ID: wid}, id, req)
						ok, err = res.Applied, eerr
					} else {
						_, ok, err = svc.SetState(e.ctx, store.Worker{ID: wid}, id, req)
					}
					if ok || !errors.Is(err, ErrInvalidState) || !bytes.Equal(before, reportOnlySnapshot(t, e, id)) {
						t.Fatalf("mixed capped report: applied=%v err=%v", ok, err)
					}
				})
			}
		}
	}
}
