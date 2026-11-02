package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ---- fixture ----------------------------------------------------------------------------------

var parkCaps = []string{capability.WorkerMemoryPressureV1, capability.MemoryParkSettlementV1}

type parkFixture struct{ memoryFixture }

// newParkFixture is the memory fixture with a worker that advertises the settlement capability and
// a user whose claims assemble end to end (Service.Claim).
func newParkFixture(t *testing.T) parkFixture {
	t.Helper()
	f := newMemoryFixture(t)
	f.e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.w.ID, parkCaps)
	enableClaimAssembly(t, f.e, f.w.UserID)
	f.w = wkrRow(t, f.e, f.w.ID)
	return parkFixture{f}
}

// receipt is the typical shared fixture rebound to this fixture's park (binding, parkAck) and
// intervention ids. mutate, when set, edits the decoded map before it is marshalled.
func (f parkFixture) receipt(t *testing.T, mutate func(map[string]any), ids ...uuid.UUID) []byte {
	t.Helper()
	m := typicalParkMap(t)
	for _, b := range []map[string]any{mapAt(m, "binding"), mapAt(m, "settlement", "evidence", "parkAck")} {
		b["runId"], b["workerId"] = f.b.RunID.String(), f.w.ID.String()
		b["registerNonce"], b["claimGeneration"], b["memoryEpisode"] = f.b.RegisterNonce, f.b.ClaimGeneration, f.b.MemoryEpisode
	}
	list := []any{}
	for _, id := range ids {
		list = append(list, id.String())
	}
	mapAt(m, "settlement", "evidence")["interventionIds"] = list
	if mutate != nil {
		mutate(m)
	}
	return marshalPark(t, m)
}

func (f parkFixture) settle(raw []byte) (MemoryParkSettlementResult, error) {
	return f.s.RecordMemoryParkSettlement(f.e.ctx, f.w, f.b.RunID, raw)
}

func (f parkFixture) mustSettle(t *testing.T, raw []byte) {
	t.Helper()
	sum := sha256.Sum256(raw)
	res, err := f.settle(raw)
	if err != nil || res.SettlementSha256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("settle = %+v, %v", res, err)
	}
}

func (f parkFixture) resume(ctx context.Context) error {
	return ResumeMemoryHold(ctx, f.e.pool, f.b.RunID, f.w.UserID, 86400)
}

func (f parkFixture) mustResume(t *testing.T) {
	t.Helper()
	if err := f.resume(f.e.ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

// latestRelease reads the whole newest release row, settlement bytes included. The production
// queries deliberately omit those bytes, so the tests that assert on them read the table directly.
func latestRelease(e codexTestEnv, runID uuid.UUID) (store.MemoryParkRelease, error) {
	rows, err := e.pool.Query(e.ctx, "SELECT * FROM memory_park_releases WHERE run_id=$1 ORDER BY claim_generation DESC LIMIT 1", runID)
	if err != nil {
		return store.MemoryParkRelease{}, err
	}
	return pgx.CollectOneRow(rows, pgx.RowToStructByPos[store.MemoryParkRelease])
}

func (f parkFixture) release(t *testing.T) store.MemoryParkRelease {
	t.Helper()
	rel, err := latestRelease(f.e, f.b.RunID)
	if err != nil {
		t.Fatalf("release row: %v", err)
	}
	return rel
}

func (f parkFixture) claim(t *testing.T, workerID uuid.UUID) *ClaimPayload {
	t.Helper()
	p, err := f.s.Claim(f.e.ctx, wkrRow(t, f.e, workerID), nil)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return p
}

// otherWorker is a second online worker of the same owner with the given capabilities.
func (f parkFixture) otherWorker(t *testing.T, nonce string, caps []string) uuid.UUID {
	t.Helper()
	wid := seedSnapshotWorker(t, f.e, f.w.UserID, nonce)
	f.e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", wid, caps)
	return wid
}

func (f parkFixture) requeue(t *testing.T) {
	t.Helper()
	f.e.exec("UPDATE runs SET status='queued', worker_id=NULL, updated_at=now() - interval '3 hours' WHERE id=$1", f.b.RunID)
}

func wantRef(rel store.MemoryParkRelease) MemoryParkReference {
	return MemoryParkReference{ClaimGeneration: rel.ClaimGeneration, MemoryEpisode: rel.MemoryEpisode,
		WorkerID: rel.WorkerID, RegisterNonce: rel.RegisterNonce, SettlementSha256: hex.EncodeToString(rel.SettlementSha256)}
}

func counters(r store.Run) [6]any {
	return [6]any{r.ClaimGeneration, r.WorkerRecoveryEpisode, r.RequeueCount, r.BudgetWallSeconds, r.BudgetExtensionSeconds, r.IterationCount}
}

// ---- the settled same-incarnation path ---------------------------------------------------------

func TestMemoryParkSettledResumeSameIncarnationLiveDB(t *testing.T) {
	f := newParkFixture(t)
	f.reserve(t, f.b)
	held := f.hold(t)
	rel := f.release(t)
	if rel.WorkerID != f.w.ID || rel.RegisterNonce != f.b.RegisterNonce || rel.ClaimGeneration != 2 || rel.MemoryEpisode != 0 ||
		!rel.ReleasedAt.Time.Equal(held.ClaimReleasedAt.Time) || rel.Settlement != nil || rel.LiftedAt.Valid {
		t.Fatalf("park wrote release row %+v for run %+v", rel, held)
	}
	raw := f.receipt(t, nil, f.b.InterventionID)
	f.mustSettle(t, raw)
	if p := f.claim(t, f.w.ID); p != nil {
		t.Fatalf("a settled but still parked run was claimed: %+v", p)
	}
	var holds int
	if err := f.e.pool.QueryRow(f.e.ctx, "SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1", f.b.RunID).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	f.mustResume(t)
	resumed := f.run(t)
	if resumed.Status != "queued" || resumed.MemoryEpisode != 1 || resumed.ReleasedWorkerID.Valid || resumed.ReleasedWorkerNonce.Valid ||
		!resumed.ClaimReleasedAt.Valid || resumed.WorkerID.Valid || counters(resumed) != counters(held) {
		t.Fatalf("lifted Resume row: %+v", resumed)
	}
	rel = f.release(t)
	if !rel.LiftedAt.Valid || rel.SupersededAt.Valid {
		t.Fatalf("release row after Resume: %+v", rel)
	}
	p := f.claim(t, f.w.ID)
	if p == nil {
		t.Fatal("the released incarnation could not reclaim after a settled Resume")
	}
	if p.ClaimGeneration != 3 || p.MemoryEpisode != 1 || p.MemoryParkReference == nil || *p.MemoryParkReference != wantRef(rel) {
		t.Fatalf("claim payload: gen=%d episode=%d ref=%+v want %+v", p.ClaimGeneration, p.MemoryEpisode, p.MemoryParkReference, wantRef(rel))
	}
	sum := sha256.Sum256(raw)
	if p.MemoryParkReference.SettlementSha256 != hex.EncodeToString(sum[:]) {
		t.Fatal("reference names a different receipt than the one posted")
	}
	after := f.run(t)
	if after.MemoryEpisode != 1 || after.Status != "claimed" || after.ClaimReleasedAt.Valid || after.ReleasedWorkerID.Valid ||
		after.WorkerRecoveryEpisode != held.WorkerRecoveryEpisode || after.RequeueCount != held.RequeueCount ||
		after.BudgetWallSeconds != held.BudgetWallSeconds || after.BudgetExtensionSeconds != held.BudgetExtensionSeconds {
		t.Fatalf("reclaim changed counters: %+v", after)
	}
	var holdsAfter int
	if err := f.e.pool.QueryRow(f.e.ctx, "SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1", f.b.RunID).Scan(&holdsAfter); err != nil || holdsAfter != holds {
		t.Fatalf("custody holds %d -> %d (%v)", holds, holdsAfter, err)
	}
	// The wire shape carries the reference only when present.
	if f.release(t).SupersededAt.Valid {
		t.Fatal("the named incarnation superseded its own reference")
	}
}

func TestMemoryParkResumeWithoutReceiptKeepsExclusionLiveDB(t *testing.T) {
	f := newParkFixture(t)
	f.hold(t)
	f.mustResume(t)
	resumed := f.run(t)
	if !resumed.ReleasedWorkerID.Valid || !resumed.ReleasedWorkerNonce.Valid || f.release(t).LiftedAt.Valid {
		t.Fatalf("an unsettled park was lifted: %+v", resumed)
	}
	if p := f.claim(t, f.w.ID); p != nil {
		t.Fatalf("released incarnation reclaimed without a receipt: %+v", p)
	}
	if !reflect.DeepEqual(resumed, f.run(t)) {
		t.Fatal("refused claim changed the resumed row")
	}
	w, nonce, err := f.s.Register(f.e.ctx, f.w, "fixture", "", nil, nil, parkCaps, nil)
	if err != nil || nonce == f.b.RegisterNonce {
		t.Fatalf("re-register nonce=%q err=%v", nonce, err)
	}
	p := f.claim(t, w.ID)
	if p == nil || p.MemoryParkReference != nil {
		t.Fatalf("re-registered incarnation claim=%+v", p)
	}
}

// ---- receipt currency and replay -------------------------------------------------------------

func TestMemoryParkSettlementReplayCurrencyLiveDB(t *testing.T) {
	t.Run("replay while parked is a no-write 200; different bytes conflict", func(t *testing.T) {
		f := newParkFixture(t)
		f.hold(t)
		raw := f.receipt(t, nil)
		f.mustSettle(t, raw)
		stored := f.release(t)
		f.mustSettle(t, raw)
		if !reflect.DeepEqual(stored, f.release(t)) {
			t.Fatal("replay rewrote the stored receipt")
		}
		other := f.receipt(t, func(m map[string]any) { sourcesOf(m)[0].(map[string]any)["seq"] = 99 })
		if _, err := f.settle(other); !errors.Is(err, ErrMemoryBinding) {
			t.Fatalf("different receipt = %v, want a binding conflict", err)
		}
		if !reflect.DeepEqual(stored, f.release(t)) {
			t.Fatal("conflicting receipt changed the row")
		}
	})
	stale := map[string]func(t *testing.T, f parkFixture){
		"after Resume": func(t *testing.T, f parkFixture) { f.mustResume(t) },
		"after Cancel": func(t *testing.T, f parkFixture) {
			f.e.exec("UPDATE runs SET status='cancelled', finished_at=now() WHERE id=$1", f.b.RunID)
		},
		"re-registered": func(t *testing.T, f parkFixture) {
			f.e.exec("UPDATE workers SET snapshot_register_nonce='rotated' WHERE id=$1", f.w.ID)
		},
		"no settlement capability": func(t *testing.T, f parkFixture) {
			f.e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.w.ID, []string{capability.WorkerMemoryPressureV1})
		},
		"legacy park without a release row": func(t *testing.T, f parkFixture) {
			f.e.exec("DELETE FROM memory_park_releases WHERE run_id=$1", f.b.RunID)
		},
		"released time moved": func(t *testing.T, f parkFixture) {
			f.e.exec("UPDATE memory_park_releases SET released_at=released_at - interval '1 second' WHERE run_id=$1", f.b.RunID)
		},
		"after a newer claim": func(t *testing.T, f parkFixture) {
			f.mustResume(t)
			other := f.otherWorker(t, "newer", parkCaps)
			if p := f.claim(t, other); p == nil {
				t.Fatal("other worker did not claim")
			}
		},
	}
	for name, change := range stale {
		for _, settled := range []bool{false, true} {
			t.Run(name+map[bool]string{false: ", first receipt", true: ", identical replay"}[settled], func(t *testing.T) {
				f := newParkFixture(t)
				f.hold(t)
				raw := f.receipt(t, nil)
				if settled {
					f.mustSettle(t, raw)
				}
				change(t, f)
				run := f.run(t)
				rel, relErr := latestRelease(f.e, f.b.RunID)
				if _, err := f.settle(raw); !errors.Is(err, ErrMemoryStale) {
					t.Fatalf("stale receipt = %v, want ErrMemoryStale", err)
				}
				if !reflect.DeepEqual(run, f.run(t)) {
					t.Fatal("stale receipt changed the run")
				}
				if after, err := latestRelease(f.e, f.b.RunID); !reflect.DeepEqual(rel, after) || !reflect.DeepEqual(relErr, err) {
					t.Fatalf("stale receipt changed the release row: %+v -> %+v (%v)", rel, after, err)
				}
			})
		}
	}
	t.Run("a binding that is not the current park", func(t *testing.T) {
		for name, edit := range map[string]func(b map[string]any){
			"generation": func(b map[string]any) { b["claimGeneration"] = 3 },
			"episode":    func(b map[string]any) { b["memoryEpisode"] = 1 },
			"nonce":      func(b map[string]any) { b["registerNonce"] = "another" },
		} {
			t.Run(name, func(t *testing.T) {
				f := newParkFixture(t)
				f.hold(t)
				raw := f.receipt(t, func(m map[string]any) { edit(mapAt(m, "binding")); edit(mapAt(m, "settlement", "evidence", "parkAck")) })
				if _, err := f.settle(raw); !errors.Is(err, ErrMemoryStale) {
					t.Fatalf("wrong binding %s = %v", name, err)
				}
				if f.release(t).Settlement != nil {
					t.Fatal("wrong binding stored a receipt")
				}
			})
		}
	})
	t.Run("a receipt that is not for this worker or run is a bad request", func(t *testing.T) {
		f := newParkFixture(t)
		f.hold(t)
		if _, err := f.s.RecordMemoryParkSettlement(f.e.ctx, f.w, uuid.New(), f.receipt(t, nil)); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("other run id = %v", err)
		}
		if _, err := f.settle([]byte(`{"version":1}`)); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("malformed receipt = %v", err)
		}
	})
}

// ---- races -----------------------------------------------------------------------------------

// holdRun locks the run row so every contender queues on a real PostgreSQL row lock.
func (f parkFixture) holdRun(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := f.e.pool.Begin(f.e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(f.e.ctx) })
	if _, err := tx.Exec(f.e.ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", f.b.RunID); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestMemoryParkSettlementResumeRaceLiveDB(t *testing.T) {
	for _, receiptFirst := range []bool{true, false} {
		name := map[bool]string{true: "receipt queued before Resume", false: "Resume queued before receipt"}[receiptFirst]
		t.Run(name, func(t *testing.T) {
			f := newParkFixture(t)
			f.hold(t)
			raw := f.receipt(t, nil)
			blocker := f.holdRun(t)
			receiptPIDs, resumePIDs := make(chan uint32, 1), make(chan uint32, 1)
			f.s.SetTxBeginner(memoryBeginProbe{pool: f.e.pool, pids: receiptPIDs})
			ctx, cancel := context.WithTimeout(f.e.ctx, 20*time.Second)
			defer cancel()
			receiptDone, resumeDone := make(chan error, 1), make(chan error, 1)
			startReceipt := func() {
				go func() { _, err := f.settle(raw); receiptDone <- err }()
				memoryWaitBlocked(t, f.e, receiptPIDs, 1)
			}
			startResume := func() {
				go func() {
					resumeDone <- ResumeMemoryHold(ctx, memoryBeginProbe{pool: f.e.pool, pids: resumePIDs}, f.b.RunID, f.w.UserID, 86400)
				}()
				memoryWaitBlocked(t, f.e, resumePIDs, 1)
			}
			if receiptFirst {
				startReceipt()
				startResume()
			} else {
				startResume()
				startReceipt()
			}
			if err := blocker.Commit(f.e.ctx); err != nil {
				t.Fatal(err)
			}
			rerr, serr := <-receiptDone, <-resumeDone
			if serr != nil {
				t.Fatalf("Resume: %v", serr)
			}
			rel := f.release(t)
			if receiptFirst {
				if rerr != nil || !rel.LiftedAt.Valid || rel.Settlement == nil {
					t.Fatalf("receipt first: err=%v row=%+v, want settled and lifted", rerr, rel)
				}
			} else if !errors.Is(rerr, ErrMemoryStale) || rel.Settlement != nil || rel.LiftedAt.Valid || !f.run(t).ReleasedWorkerID.Valid {
				t.Fatalf("Resume first: err=%v row=%+v, want a stale receipt and an intact exclusion", rerr, rel)
			}
			if f.run(t).MemoryEpisode != 1 {
				t.Fatalf("episode = %d, want exactly one renewal", f.run(t).MemoryEpisode)
			}
		})
	}
	for _, cancelFirst := range []bool{true, false} {
		name := map[bool]string{true: "Cancel queued before Resume", false: "Resume queued before Cancel"}[cancelFirst]
		t.Run(name, func(t *testing.T) {
			f := newParkFixture(t)
			f.hold(t)
			f.mustSettle(t, f.receipt(t, nil))
			blocker := f.holdRun(t)
			resumePIDs := make(chan uint32, 1)
			cancelTx, err := f.e.pool.Begin(f.e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cancelTx.Rollback(f.e.ctx) }()
			cancelPID := cancelTx.Conn().PgConn().PID()
			ctx, cancel := context.WithTimeout(f.e.ctx, 20*time.Second)
			defer cancel()
			resumeDone, cancelDone := make(chan error, 1), make(chan error, 1)
			startResume := func() {
				go func() {
					resumeDone <- ResumeMemoryHold(ctx, memoryBeginProbe{pool: f.e.pool, pids: resumePIDs}, f.b.RunID, f.w.UserID, 86400)
				}()
				memoryWaitBlocked(t, f.e, resumePIDs, 1)
			}
			startCancel := func() {
				go func() {
					_, err := cancelTx.Exec(ctx, "UPDATE runs SET status='cancelled', finished_at=now() WHERE id=$1 AND status IN ('recovery_wait','queued')", f.b.RunID)
					if err == nil {
						err = cancelTx.Commit(ctx)
					}
					cancelDone <- err
				}()
				pids := make(chan uint32, 1)
				pids <- cancelPID
				memoryWaitBlocked(t, f.e, pids, 1)
			}
			if cancelFirst {
				startCancel()
				startResume()
			} else {
				startResume()
				startCancel()
			}
			if err := blocker.Commit(f.e.ctx); err != nil {
				t.Fatal(err)
			}
			serr, cerr := <-resumeDone, <-cancelDone
			if cerr != nil {
				t.Fatalf("Cancel: %v", cerr)
			}
			run, rel := f.run(t), f.release(t)
			if run.Status != "cancelled" {
				t.Fatalf("run status = %s, want cancelled", run.Status)
			}
			if cancelFirst && (!errors.Is(serr, pgx.ErrNoRows) || rel.LiftedAt.Valid || !run.ReleasedWorkerID.Valid) {
				t.Fatalf("Cancel first: resume=%v row=%+v", serr, rel)
			}
			if !cancelFirst && (serr != nil || !rel.LiftedAt.Valid) {
				t.Fatalf("Resume first: resume=%v row=%+v", serr, rel)
			}
			if p := f.claim(t, f.w.ID); p != nil {
				t.Fatalf("a cancelled run was claimed: %+v", p)
			}
		})
	}
}

// ---- the lift --------------------------------------------------------------------------------

func TestMemoryParkLiftZeroMatchCommitsResumeLiveDB(t *testing.T) {
	cases := map[string]func(t *testing.T, f parkFixture){
		"no receipt": func(t *testing.T, f parkFixture) {},
		"legacy no row": func(t *testing.T, f parkFixture) {
			f.e.exec("DELETE FROM memory_park_releases WHERE run_id=$1", f.b.RunID)
		},
		"released time": func(t *testing.T, f parkFixture) {
			f.e.exec("UPDATE memory_park_releases SET released_at=released_at - interval '1 second' WHERE run_id=$1", f.b.RunID)
		},
		"other nonce": func(t *testing.T, f parkFixture) {
			f.e.exec("UPDATE memory_park_releases SET register_nonce='other' WHERE run_id=$1", f.b.RunID)
		},
		"episode mismatch": func(t *testing.T, f parkFixture) {
			f.e.exec("UPDATE memory_park_releases SET memory_episode=5 WHERE run_id=$1", f.b.RunID)
		},
	}
	for name, prep := range cases {
		t.Run(name, func(t *testing.T) {
			f := newParkFixture(t)
			f.hold(t)
			if name != "no receipt" {
				f.mustSettle(t, f.receipt(t, nil))
			}
			prep(t, f)
			f.mustResume(t)
			run := f.run(t)
			if run.Status != "queued" || run.MemoryEpisode != 1 || !run.ReleasedWorkerID.Valid || !run.ReleasedWorkerNonce.Valid {
				t.Fatalf("zero-match Resume row: %+v", run)
			}
			if rel, err := latestRelease(f.e, f.b.RunID); err == nil && rel.LiftedAt.Valid {
				t.Fatalf("zero-match lifted: %+v", rel)
			}
			if p := f.claim(t, f.w.ID); p != nil {
				t.Fatalf("zero-match Resume let the released incarnation back in: %+v", p)
			}
		})
	}
	t.Run("a Resume that finds no hold keeps its 409 path", func(t *testing.T) {
		f := newParkFixture(t)
		f.hold(t)
		f.mustResume(t)
		if err := f.resume(f.e.ctx); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("second Resume = %v, want pgx.ErrNoRows", err)
		}
	})
}

func TestMemoryParkSettlementInterventionHistoriesLiveDB(t *testing.T) {
	insert := func(t *testing.T, f parkFixture, admitted bool, edit func(*store.InsertMemoryInterventionParams)) uuid.UUID {
		t.Helper()
		p := store.InsertMemoryInterventionParams{InterventionID: uuid.New(), RunID: f.b.RunID, UserID: f.w.UserID,
			WorkerID: f.w.ID, RegisterNonce: f.b.RegisterNonce, ClaimGeneration: f.b.ClaimGeneration, MemoryEpisode: f.b.MemoryEpisode,
			Admitted: admitted, AllowanceUsed: 1, Policy: []byte(`{"version":1,"max_interventions":3}`)}
		if edit != nil {
			edit(&p)
		}
		if n, err := f.e.q.InsertMemoryIntervention(f.e.ctx, p); err != nil || n != 1 {
			t.Fatalf("insert intervention: %d %v", n, err)
		}
		return p.InterventionID
	}
	ledger := func(t *testing.T, f parkFixture) []store.MemoryIntervention {
		t.Helper()
		rows, err := f.e.pool.Query(f.e.ctx, "SELECT intervention_id FROM memory_interventions WHERE run_id=$1 ORDER BY intervention_id", f.b.RunID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []store.MemoryIntervention
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			row, err := f.e.q.GetMemoryIntervention(f.e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, row)
		}
		return out
	}
	accepted := map[string]func(t *testing.T, f parkFixture) []uuid.UUID{
		"admitted": func(t *testing.T, f parkFixture) []uuid.UUID { return []uuid.UUID{insert(t, f, true, nil)} },
		"denied": func(t *testing.T, f parkFixture) []uuid.UUID {
			return []uuid.UUID{insert(t, f, false, func(p *store.InsertMemoryInterventionParams) { p.AllowanceUsed = 3 })}
		},
		"missing": func(t *testing.T, f parkFixture) []uuid.UUID { return []uuid.UUID{uuid.New()} },
		"empty":   func(t *testing.T, f parkFixture) []uuid.UUID { return nil },
		"mixed": func(t *testing.T, f parkFixture) []uuid.UUID {
			return []uuid.UUID{insert(t, f, true, nil), insert(t, f, false, nil), uuid.New()}
		},
	}
	for name, seed := range accepted {
		t.Run("accepted "+name, func(t *testing.T) {
			f := newParkFixture(t)
			ids := seed(t, f)
			f.hold(t)
			before := ledger(t, f)
			f.mustSettle(t, f.receipt(t, nil, ids...))
			if !reflect.DeepEqual(before, ledger(t, f)) {
				t.Fatal("a receipt wrote intervention rows")
			}
			for _, row := range ledger(t, f) {
				if row.Outcome.Valid {
					t.Fatalf("a receipt recorded an outcome: %+v", row)
				}
			}
		})
	}
	foreign := map[string]func(p *store.InsertMemoryInterventionParams){
		"other nonce":      func(p *store.InsertMemoryInterventionParams) { p.RegisterNonce = "foreign" },
		"other generation": func(p *store.InsertMemoryInterventionParams) { p.ClaimGeneration = 1 },
		"other episode":    func(p *store.InsertMemoryInterventionParams) { p.MemoryEpisode = 4 },
	}
	for name, edit := range foreign {
		t.Run("foreign "+name, func(t *testing.T) {
			f := newParkFixture(t)
			id := insert(t, f, true, edit)
			f.hold(t)
			before := ledger(t, f)
			if _, err := f.settle(f.receipt(t, nil, id)); !errors.Is(err, ErrMemoryBinding) {
				t.Fatalf("foreign row = %v, want a binding conflict", err)
			}
			if !reflect.DeepEqual(before, ledger(t, f)) || f.release(t).Settlement != nil {
				t.Fatal("refused receipt wrote")
			}
		})
	}
	t.Run("foreign run", func(t *testing.T) {
		f := newParkFixture(t)
		otherRun := seedOutageRun(t, f.e, f.w.UserID, f.repo, f.w.ID, "completed", "issue", 2, 0)
		id := insert(t, f, true, func(p *store.InsertMemoryInterventionParams) { p.RunID = otherRun })
		f.hold(t)
		if _, err := f.settle(f.receipt(t, nil, id)); !errors.Is(err, ErrMemoryBinding) {
			t.Fatalf("foreign run row = %v, want a binding conflict", err)
		}
	})
}

// ---- the claim reference ---------------------------------------------------------------------

func TestMemoryParkReferenceLiveDB(t *testing.T) {
	settledResumed := func(t *testing.T) parkFixture {
		f := newParkFixture(t)
		f.hold(t)
		f.mustSettle(t, f.receipt(t, nil))
		f.mustResume(t)
		return f
	}
	t.Run("repeats for the same incarnation", func(t *testing.T) {
		f := settledResumed(t)
		first := f.claim(t, f.w.ID)
		f.requeue(t)
		second := f.claim(t, f.w.ID)
		if first == nil || second == nil || first.MemoryParkReference == nil || !reflect.DeepEqual(first.MemoryParkReference, second.MemoryParkReference) {
			t.Fatalf("references: %+v / %+v", first, second)
		}
		if second.ClaimGeneration != 4 {
			t.Fatalf("second claim generation = %d", second.ClaimGeneration)
		}
	})
	t.Run("a claim carrying an active snapshot gets the reference", func(t *testing.T) {
		f := settledResumed(t)
		snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: f.b.RegisterNonce, Active: []ActiveRunEntry{}}
		p, err := f.s.Claim(f.e.ctx, wkrRow(t, f.e, f.w.ID), snap)
		if err != nil || p == nil || p.MemoryParkReference == nil {
			t.Fatalf("snapshot claim = %+v, %v", p, err)
		}
		if want := wantRef(f.release(t)); *p.MemoryParkReference != want {
			t.Fatalf("reference = %+v, want %+v", *p.MemoryParkReference, want)
		}
	})
	t.Run("a foreign claim supersedes it for good", func(t *testing.T) {
		f := settledResumed(t)
		other := f.otherWorker(t, "foreign", parkCaps)
		if p := f.claim(t, other); p == nil || p.MemoryParkReference != nil {
			t.Fatalf("foreign claim = %+v", p)
		}
		if !f.release(t).SupersededAt.Valid {
			t.Fatal("foreign claim left the reference live")
		}
		f.requeue(t)
		if p := f.claim(t, f.w.ID); p == nil || p.MemoryParkReference != nil {
			t.Fatalf("former incarnation got a superseded reference: %+v", p)
		}
	})
	t.Run("a newer park replaces it", func(t *testing.T) {
		f := settledResumed(t)
		first := f.claim(t, f.w.ID)
		if first == nil || first.MemoryParkReference == nil {
			t.Fatalf("first claim = %+v", first)
		}
		f.b.ClaimGeneration, f.b.MemoryEpisode, f.b.InterventionID = first.ClaimGeneration, first.MemoryEpisode, uuid.New()
		f.hold(t)
		f.mustResume(t) // no receipt: the newer park stays unlifted and excluded
		if p := f.claim(t, f.w.ID); p != nil {
			t.Fatalf("incarnation reclaimed a newer unsettled park: %+v", p)
		}
		w, _, err := f.s.Register(f.e.ctx, f.w, "fixture", "", nil, nil, parkCaps, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p := f.claim(t, w.ID); p == nil || p.MemoryParkReference != nil {
			t.Fatalf("an old reference outlived a newer park: %+v", p)
		}
	})
	t.Run("only the named incarnation with the capability", func(t *testing.T) {
		f := settledResumed(t)
		f.e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.w.ID, []string{capability.WorkerMemoryPressureV1})
		if p := f.claim(t, f.w.ID); p == nil || p.MemoryParkReference != nil {
			t.Fatalf("an incapable named worker got a reference: %+v", p)
		}
		if f.release(t).SupersededAt.Valid {
			t.Fatal("the named incarnation superseded its own reference by claiming without the capability")
		}
		f.e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.w.ID, parkCaps)
		f.requeue(t)
		if p := f.claim(t, f.w.ID); p == nil || p.MemoryParkReference == nil {
			t.Fatalf("reference lost after the capability returned: %+v", p)
		}
	})
	t.Run("a re-registered former incarnation never gets it", func(t *testing.T) {
		f := settledResumed(t)
		w, nonce, err := f.s.Register(f.e.ctx, f.w, "fixture", "", nil, nil, parkCaps, nil)
		if err != nil || nonce == f.b.RegisterNonce {
			t.Fatalf("register: %q %v", nonce, err)
		}
		if p := f.claim(t, w.ID); p == nil || p.MemoryParkReference != nil {
			t.Fatalf("new incarnation claim = %+v", p)
		}
		if !f.release(t).SupersededAt.Valid {
			t.Fatal("a different incarnation's claim did not retire the reference")
		}
	})
	t.Run("an ordinary run's claim has no reference member", func(t *testing.T) {
		f := newParkFixture(t)
		f.e.exec("UPDATE runs SET status='queued', worker_id=NULL WHERE id=$1", f.b.RunID)
		if p := f.claim(t, f.w.ID); p == nil || p.MemoryParkReference != nil {
			t.Fatalf("ordinary claim = %+v", p)
		}
	})
}

// ---- limits the lift must not bypass ---------------------------------------------------------

func TestMemoryParkLiftLimitsLiveDB(t *testing.T) {
	t.Run("custody admission limit and older holds still apply", func(t *testing.T) {
		f := newParkFixture(t)
		f.hold(t)
		f.mustSettle(t, f.receipt(t, nil))
		f.mustResume(t)
		older := seedOutageRun(t, f.e, f.w.UserID, f.repo, f.w.ID, "completed", "issue", 1, 0)
		holdID := uuid.New()
		f.e.exec(`INSERT INTO recovery_custody_holds
			(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,inventory_guarded)
			VALUES($1,$2,$3,$4,1,'open',$5,'older',false)`, holdID, f.w.UserID, f.repo, older, f.w.ID)
		params := claimRunParams(wkrRow(t, f.e, f.w.ID))
		params.CustodyHoldLimit = 1
		if _, err := f.e.q.ClaimRun(f.e.ctx, params); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("lifted run bypassed the custody limit: %v", err)
		}
		f.e.exec(`UPDATE recovery_custody_holds SET state='discarded', live_worker_id=NULL, live_run_id=NULL, released_at=now(), updated_at=now() WHERE id=$1`, holdID)
		if run, err := f.e.q.ClaimRun(f.e.ctx, params); err != nil || run.ID != f.b.RunID {
			t.Fatalf("claim after the older hold cleared: %v", err)
		}
	})
	t.Run("lost memory authority still refuses", func(t *testing.T) {
		f := newParkFixture(t)
		f.hold(t)
		f.mustSettle(t, f.receipt(t, nil))
		f.mustResume(t)
		params := claimRunParams(wkrRow(t, f.e, f.w.ID))
		params.MemoryAuthorityLost = true
		if _, err := f.e.q.ClaimRun(f.e.ctx, params); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("lifted run claimed without memory authority: %v", err)
		}
		params.MemoryAuthorityLost = false
		if run, err := f.e.q.ClaimRun(f.e.ctx, params); err != nil || run.ID != f.b.RunID {
			t.Fatalf("claim with authority: %v", err)
		}
	})
	t.Run("only the memory Resume path lifts", func(t *testing.T) {
		mutations := map[string]string{ //nolint:gosec // G101: test SQL statements, not credentials
			"wall hold":               "UPDATE runs SET status='paused', hold_reason='budget_exhausted' WHERE id=$1",
			"compact hold":            "UPDATE runs SET status='paused', hold_reason='compact_exhausted' WHERE id=$1",
			"credential switch":       "UPDATE runs SET status='paused', hold_reason='credential_switch_settling' WHERE id=$1",
			"still parked":            "UPDATE runs SET status='recovery_wait' WHERE id=$1",
			"episode not renewed":     "UPDATE runs SET memory_episode=0 WHERE id=$1",
			"released time moved":     "UPDATE runs SET claim_released_at=claim_released_at + interval '1 second' WHERE id=$1",
			"another generation":      "UPDATE runs SET claim_generation=claim_generation + 1 WHERE id=$1",
			"another released worker": "UPDATE runs SET released_worker_id=gen_random_uuid() WHERE id=$1",
		}
		for name, sql := range mutations {
			t.Run(name, func(t *testing.T) {
				f := newParkFixture(t)
				f.hold(t)
				f.mustSettle(t, f.receipt(t, nil))
				// A queued, renewed run as ResumeMemoryEpisode leaves it, then one edit away from a match.
				f.e.exec("UPDATE runs SET status='queued', recovery_wait_cause=NULL, memory_episode=1, worker_id=NULL WHERE id=$1", f.b.RunID)
				f.e.exec(sql, f.b.RunID)
				before := f.run(t)
				n, err := f.e.q.LiftSettledMemoryPark(f.e.ctx, store.LiftSettledMemoryParkParams{ID: f.b.RunID, UserID: f.w.UserID})
				if err != nil || n != 0 || f.release(t).LiftedAt.Valid || !reflect.DeepEqual(before, f.run(t)) {
					t.Fatalf("lift matched %d rows (%v) under %q", n, err, name)
				}
			})
		}
		t.Run("another owner", func(t *testing.T) {
			f := newParkFixture(t)
			f.hold(t)
			f.mustSettle(t, f.receipt(t, nil))
			f.e.exec("UPDATE runs SET status='queued', recovery_wait_cause=NULL, memory_episode=1, worker_id=NULL WHERE id=$1", f.b.RunID)
			if n, err := f.e.q.LiftSettledMemoryPark(f.e.ctx, store.LiftSettledMemoryParkParams{ID: f.b.RunID, UserID: uuid.New()}); err != nil || n != 0 {
				t.Fatalf("foreign owner lift = %d %v", n, err)
			}
		})
	})
	t.Run("claimable counts equal actual claims", func(t *testing.T) {
		f := newParkFixture(t)
		f.hold(t)
		f.mustSettle(t, f.receipt(t, nil))
		claimable := func() int64 {
			now := time.Now()
			n, err := f.e.q.CountOnlineWorkersClaimableForRun(f.e.ctx, store.CountOnlineWorkersClaimableForRunParams{
				RunID: f.b.RunID, CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{},
				AffinityCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}, StaleRequeueCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
				HeartbeatCutoff: pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true}, CrossCheckEvaluatedAt: pgtype.Timestamptz{Time: now, Valid: true},
				CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}, CapabilityAware: true, EphemeralLease: pgtype.Interval{Valid: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			return n.Claimable
		}
		// An unlifted settled park (legacy resume path): the count and the claim agree on zero.
		if _, err := f.e.q.ResumeMemoryEpisode(f.e.ctx, store.ResumeMemoryEpisodeParams{ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.e.q.ClaimRun(f.e.ctx, claimRunParams(wkrRow(t, f.e, f.w.ID))); !errors.Is(err, pgx.ErrNoRows) || claimable() != 0 {
			t.Fatalf("unlifted: claim err=%v claimable=%d", err, claimable())
		}
		if n, err := f.e.q.LiftSettledMemoryPark(f.e.ctx, store.LiftSettledMemoryParkParams{ID: f.b.RunID, UserID: f.w.UserID}); err != nil || n != 1 {
			t.Fatalf("lift = %d %v", n, err)
		}
		if claimable() != 1 {
			t.Fatalf("lifted: claimable=%d, want 1", claimable())
		}
		if run, err := f.e.q.ClaimRun(f.e.ctx, claimRunParams(wkrRow(t, f.e, f.w.ID))); err != nil || run.ID != f.b.RunID {
			t.Fatalf("lifted claim: %v", err)
		}
	})
}

// ---- capability negotiation -----------------------------------------------------------------

func TestMemoryParkSettlementFeatureMatrixLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name      string
		caps      []string
		accepted  bool
		reference bool
	}{
		{"pressure and settlement", parkCaps, true, true},
		{"pressure only", []string{capability.WorkerMemoryPressureV1}, false, false},
		{"settlement only", []string{capability.MemoryParkSettlementV1}, false, false},
		{"neither", []string{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newParkFixture(t)
			f.hold(t)
			f.e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.w.ID, tc.caps)
			_, err := f.settle(f.receipt(t, nil))
			if tc.accepted != (err == nil) || (!tc.accepted && !errors.Is(err, ErrMemoryStale)) {
				t.Fatalf("receipt with caps %v = %v", tc.caps, err)
			}
			if !tc.accepted {
				if f.release(t).Settlement != nil {
					t.Fatal("an incapable worker's receipt was stored")
				}
				return
			}
			f.mustResume(t)
			p := f.claim(t, f.w.ID)
			if p == nil || (p.MemoryParkReference != nil) != tc.reference {
				t.Fatalf("claim reference = %+v", p)
			}
		})
	}
}

// ---- the queued reason ------------------------------------------------------------------------

func TestQueuedReasonMemoryParkReleasedWorkerLiveDB(t *testing.T) {
	const memoryReason = "its previous process was released by a memory-pressure park"
	const wallReason = "its previous process was released at the time limit"
	cases := []struct {
		name    string
		prepare func(t *testing.T, f parkFixture)
		resume  func(t *testing.T, f parkFixture)
		want    string // substring of the reason; "" means no released-worker reason at all
	}{
		{"unsettled park", func(t *testing.T, f parkFixture) {}, nil, memoryReason},
		{"settled, resume without the lift", func(t *testing.T, f parkFixture) { f.mustSettle(t, f.receipt(t, nil)) },
			func(t *testing.T, f parkFixture) {
				if _, err := f.e.q.ResumeMemoryEpisode(f.e.ctx, store.ResumeMemoryEpisodeParams{ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400}); err != nil {
					t.Fatal(err)
				}
			}, memoryReason},
		{"settled and lifted", func(t *testing.T, f parkFixture) { f.mustSettle(t, f.receipt(t, nil)) }, nil, ""},
		{"legacy park without a row", func(t *testing.T, f parkFixture) {
			f.e.exec("DELETE FROM memory_park_releases WHERE run_id=$1", f.b.RunID)
		}, nil, wallReason},
		{"stale release time", func(t *testing.T, f parkFixture) {
			f.e.exec("UPDATE memory_park_releases SET released_at=released_at - interval '1 second' WHERE run_id=$1", f.b.RunID)
		}, nil, wallReason},
		{"wall park after the memory park", func(t *testing.T, f parkFixture) {
			f.e.exec("UPDATE runs SET claim_released_at=claim_released_at + interval '1 minute' WHERE id=$1", f.b.RunID)
		}, nil, wallReason},
		{"later generation wall park", func(t *testing.T, f parkFixture) {
			f.e.exec("UPDATE runs SET claim_generation=claim_generation + 1 WHERE id=$1", f.b.RunID)
		}, nil, wallReason},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newParkFixture(t)
			f.hold(t)
			tc.prepare(t, f)
			if tc.resume != nil {
				tc.resume(t, f)
			} else {
				f.mustResume(t)
			}
			// Time out the 3h-old run's queue age so the reason ladder reaches the released-worker rung.
			f.e.exec("UPDATE runs SET status_since=now() - interval '5 minutes' WHERE id=$1", f.b.RunID)
			rows, err := f.s.q.ListActiveRunsForHealth(f.e.ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			var row store.ListActiveRunsForHealthRow
			found := false
			for _, r := range rows {
				if r.ID == f.b.RunID {
					row, found = r, true
				}
			}
			if !found {
				t.Fatal("run not listed for health")
			}
			reason := f.s.queuedReason(f.e.ctx, time.Now(), row, thWithLimit(testCustodyLimit))
			name := "wsnap-" + f.w.ID.String()
			switch {
			case tc.want == "" && strings.Contains(reason, "restart worker"):
				t.Fatalf("reason = %q, want no released-worker reason", reason)
			case tc.want != "" && !strings.Contains(reason, "restart worker "+name+" ("+tc.want+")"):
				t.Fatalf("reason = %q, want it to name %q", reason, tc.want)
			}
			if tc.want == memoryReason && !row.ReleasedByMemoryPark || tc.want == wallReason && row.ReleasedByMemoryPark {
				t.Fatalf("released_by_memory_park = %v for %q", row.ReleasedByMemoryPark, tc.name)
			}
		})
	}
}
