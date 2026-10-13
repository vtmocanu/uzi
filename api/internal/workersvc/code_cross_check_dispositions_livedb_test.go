package workersvc

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func completedDispositionFixture(t *testing.T) (crossCheckContentionFixture, store.Worker, store.CrossCheck) {
	t.Helper()
	f := codeFixture(t)
	w := wkrRow(t, f.env, f.workerID)
	cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
	p := laneClaim(t, f, "cross_check", nil)
	if p == nil {
		t.Fatal("checker not claimed")
	}
	cc, err = f.svc.DecideCodeCrossCheck(f.env.ctx, w, f.runID, p.ClaimGeneration, "completed", "",
		[]CodeCrossCheckFinding{{ID: "F-1", Severity: "major", Title: "first"}, {ID: "F_2", Severity: "minor", Title: "second"}})
	if err != nil {
		t.Fatal(err)
	}
	return f, w, cc
}

func TestCodeCrossCheckDispositionsWriteOnceMissingFinalizedLiveDB(t *testing.T) {
	f, w, _ := completedDispositionFixture(t)
	batch := []CodeCrossCheckDisposition{{ID: "F-1", Disposition: "addressed", Reason: "verified and fixed"}}
	cc, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, w, f.lead, 1, batch)
	if err != nil || !cc.FinalizedAt.Valid {
		t.Fatalf("finalize: %+v %v", cc, err)
	}
	var reason, disposition string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT dispositions->1->>'disposition',dispositions->1->>'reason' FROM cross_checks WHERE id=$1`, cc.ID).Scan(&disposition, &reason); err != nil ||
		disposition != "not_reported" || reason != "" {
		t.Fatalf("missing finding: %q %q %v", disposition, reason, err)
	}
	again, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, w, f.lead, 1, batch)
	if err != nil || again.FinalizedAt != cc.FinalizedAt || string(again.Dispositions) != string(cc.Dispositions) {
		t.Fatalf("lost ACK retry: %v", err)
	}
	batch[0].Reason = "different"
	if _, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, w, f.lead, 1, batch); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatal("conflicting retry accepted")
	}
	if _, err := f.env.pool.Exec(f.env.ctx, "UPDATE cross_checks SET dispositions='[]' WHERE id=$1", cc.ID); err == nil {
		t.Fatal("durable evidence overwritten")
	}
	if _, err := f.env.pool.Exec(f.env.ctx, "UPDATE cross_checks SET finalized_at=NULL,dispositions=NULL WHERE id=$1", cc.ID); err == nil {
		t.Fatal("finalization cleared")
	}
	if _, err := f.env.pool.Exec(f.env.ctx, "UPDATE cross_checks SET finalized_at=finalized_at+interval '1 second' WHERE id=$1", cc.ID); err == nil {
		t.Fatal("durable marker overwritten")
	}
	if lead := mustRun(t, f.env, f.lead); lead.Status != "running" || lead.AutoApprove {
		t.Fatal("advice changed lead authority")
	}
}

func TestCodeCrossCheckDispositionsStrictIDsBoundsLiveDB(t *testing.T) {
	f, w, cc := completedDispositionFixture(t)
	for _, batch := range [][]CodeCrossCheckDisposition{
		{{ID: "", Disposition: "addressed", Reason: "ok"}},
		{{ID: strings.Repeat("x", 65), Disposition: "addressed", Reason: "ok"}},
		{{ID: "new\nline", Disposition: "addressed", Reason: "ok"}},
		{{ID: "ansi\x1b[31m", Disposition: "addressed", Reason: "ok"}},
		{{ID: "control\x00", Disposition: "addressed", Reason: "ok"}},
		{{ID: "bidi\u202e", Disposition: "addressed", Reason: "ok"}},
		{{ID: " F-1", Disposition: "addressed", Reason: "ok"}},
		{{ID: "f-1", Disposition: "addressed", Reason: "ok"}},
		{{ID: "unknown", Disposition: "declined", Reason: "ok"}},
		{{ID: "F-1", Disposition: "addressed", Reason: "ok"}, {ID: "F-1", Disposition: "declined", Reason: "no"}},
		{{ID: "F-1", Disposition: "not_reported", Reason: ""}},
		{{ID: "F-1", Disposition: "approved", Reason: "ok"}},
		{{ID: "F-1", Disposition: "addressed", Reason: ""}},
		{{ID: "F-1", Disposition: "addressed", Reason: strings.Repeat("é", 512) + "x"}},
		{{ID: "F-1", Disposition: "addressed", Reason: string([]byte{0xff})}},
		make([]CodeCrossCheckDisposition, 21),
	} {
		if _, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, w, f.lead, 1, batch); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("invalid batch accepted: %v", err)
		}
	}
	saved, err := f.env.q.GetCodeCrossCheck(f.env.ctx, f.lead)
	if err != nil || saved.FinalizedAt.Valid || saved.Dispositions != nil {
		t.Fatal("refusals partially wrote batch")
	}
	f.env.exec("UPDATE cross_checks SET findings=$2 WHERE id=$1", cc.ID, `[{"id":"invalid id","severity":"major"}]`)
	if _, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, w, f.lead, 1, nil); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatal("legacy invalid persisted ID accepted")
	}
	f.env.exec("UPDATE cross_checks SET findings=$2 WHERE id=$1", cc.ID, cc.Findings)
	if _, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, w, f.lead, 1, []CodeCrossCheckDisposition{{ID: "F-1", Disposition: "declined", Reason: strings.Repeat("é", 512)}}); err != nil {
		t.Fatalf("exact 1024 UTF-8 bytes refused: %v", err)
	}
}

func TestCodeCrossCheckDispositionsCurrentCustodyEvenRetryLiveDB(t *testing.T) {
	for _, finalized := range []bool{false, true} {
		t.Run(map[bool]string{false: "first write", true: "retry"}[finalized], func(t *testing.T) {
			f, w, _ := completedDispositionFixture(t)
			batch := []CodeCrossCheckDisposition{{ID: "F-1", Disposition: "declined", Reason: "verified false positive"}}
			if finalized {
				if _, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, w, f.lead, 1, batch); err != nil {
					t.Fatal(err)
				}
			}
			for _, identity := range []struct {
				worker     store.Worker
				generation int64
			}{
				{store.Worker{ID: uuid.New(), UserID: w.UserID}, 1},
				{store.Worker{ID: w.ID, UserID: uuid.New()}, 1},
				{w, 2},
			} {
				if _, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, identity.worker, f.lead, identity.generation, batch); !errors.Is(err, ErrCrossCheckRefused) {
					t.Fatal("foreign/stale identity accepted")
				}
			}
			f.env.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", f.lead)
			for _, generation := range []int64{1, 2} {
				if _, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, w, f.lead, generation, batch); !errors.Is(err, ErrCrossCheckRefused) {
					t.Fatal("interrupted row accepted")
				}
			}
			persisted, err := f.env.q.GetCodeCrossCheck(f.env.ctx, f.lead)
			if err != nil || !persisted.InterruptedAt.Valid || persisted.FinalizedAt.Valid != finalized {
				t.Fatalf("interruption lost durable marker: %+v %v", persisted, err)
			}
		})
	}
}

func TestCodeCrossCheckDispositionsEmptyBatchDurableMissingLiveDB(t *testing.T) {
	f, w, _ := completedDispositionFixture(t)
	cc, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, w, f.lead, 1, []CodeCrossCheckDisposition{})
	want := `[{"finding_id":"F-1","disposition":"not_reported","reason":""},{"finding_id":"F_2","disposition":"not_reported","reason":""}]`
	var got, expected []CodeCrossCheckDisposition
	if err != nil || !cc.FinalizedAt.Valid || json.Unmarshal(cc.Dispositions, &got) != nil || json.Unmarshal([]byte(want), &expected) != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("empty batch: %s %v", cc.Dispositions, err)
	}
	saved, err := f.env.q.GetCodeCrossCheck(f.env.ctx, f.lead)
	if err != nil || saved.FinalizedAt != cc.FinalizedAt || string(saved.Dispositions) != string(cc.Dispositions) {
		t.Fatalf("missing dispositions not durable: %+v %v", saved, err)
	}
}

func TestCodeCrossCheckDispositionsRequiresCompletedLiveDB(t *testing.T) {
	for _, outcome := range []string{"pending", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			f := codeFixture(t)
			w := wkrRow(t, f.env, f.workerID)
			if outcome == "failed" {
				if _, err := f.svc.RecordCodeCrossCheckFailure(f.env.ctx, w, f.lead, 1, "", "", "snapshot_failed"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.svc.FinalizeCodeCrossCheckDispositions(f.env.ctx, w, f.lead, 1, nil); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatal("undecided/failed row finalized")
			}
		})
	}
}
