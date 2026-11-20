package workersvc

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
)

// Issue #4476 M4: a Codex checker child that claim provenance holds (or leaves with no provenance
// at all) cannot be claimed by a non-recorded worker, so its cross-check deadline is the only way
// out. The deadline control settles it exactly as it settles a child the gate does not hold.

// crossCheckProvEnd is the settlement a deadline leaves behind, comparable across variants.
type crossCheckProvEnd struct {
	Failed, TimedOut, DecidedAtDeadline bool
	ChildStatus                         string
	ChildFinished, ChildReleased        bool
	ChildGenerationUnchanged            bool
	ChildEpochUnchanged, NoCapability   bool
	ChildHolds                          int
	ProvenanceUnchanged                 bool
}

type crossCheckProvChild struct {
	generation int64
	epoch      int64
	provWorker string
	provGen    string
}

func readCrossCheckProvChild(t *testing.T, f crossCheckContentionFixture) crossCheckProvChild {
	t.Helper()
	r := mustRun(t, f.env, f.runID)
	var pw, pg string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT COALESCE(claim_provenance_worker_id::text, ''),
		COALESCE(claim_provenance_generation::text, '') FROM runs WHERE id = $1`, f.runID).Scan(&pw, &pg); err != nil {
		t.Fatal(err)
	}
	return crossCheckProvChild{generation: r.ClaimGeneration, epoch: r.CodexClaimEpoch, provWorker: pw, provGen: pg}
}

// holdChild puts the queued checker child at claim_generation 1 as a previously claimed Codex run:
// recorded provenance for another worker ("provenance"), none at all ("null"), or leaves it at
// generation 0 ("paired", the control the gate does not hold).
func holdChild(t *testing.T, f crossCheckContentionFixture, variant string) (recorded uuid.UUID) {
	t.Helper()
	switch variant {
	case "paired":
	case "null":
		// Seeding the generation alone leaves provenance NULL and does not fire the guard trigger.
		f.env.exec(`UPDATE runs SET claim_generation = 1 WHERE id = $1`, f.runID)
	case "provenance":
		recorded = uuid.New()
		var submitted pgtype.UUID
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT worker_id FROM runs WHERE id = $1`, f.runID).Scan(&submitted); err != nil {
			t.Fatal(err)
		}
		f.env.exec(`INSERT INTO workers(id,user_id,name,token_hash,status,max_cross_check_slots,protocol_capabilities)
			SELECT $2::uuid,user_id,'recorded-'||$2::text,$2::uuid::text::bytea,'online',1,protocol_capabilities FROM workers WHERE id=$1`,
			f.workerID, recorded)
		// One admitted +1 claim step by the recorded worker, then the run goes back to queued.
		f.env.exec(`UPDATE runs SET claim_generation = 1, worker_id = $2, claim_provenance_worker_id = $2,
			claim_provenance_generation = 1 WHERE id = $1`, f.runID, recorded)
		// Restore the submitted worker_id so the child differs from the paired control only in
		// generation and provenance; any refusal is then the provenance gate, not a worker_id shape.
		f.env.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, f.runID, submitted)
	}
	if st := mustRun(t, f.env, f.runID).Status; st != "queued" {
		t.Fatalf("held child status = %s, want queued", st)
	}
	return recorded
}

// refuseHeldCheckerClaim: before the deadline, a checker held by provenance (or with none recorded)
// is handed to no worker and a refused claim moves nothing. The held child keeps the paired
// control's submitted worker_id and differs only in generation and provenance, and that control is
// claimable (TestCodexProvenanceCrossCheckPairedChildIsClaimableLiveDB), so the refusal is the gate.
func refuseHeldCheckerClaim(t *testing.T, stage, variant string) {
	t.Helper()
	f, w := admissionCheckerBase(t, stage)
	f, _ = admissionCheckerSubmit(t, f, w, stage, 0, nil)
	holdChild(t, f, variant)
	before := readCrossCheckProvChild(t, f)
	if p := laneClaim(t, f, "cross_check", nil); p != nil {
		t.Fatalf("%s: a held Codex checker was claimed by a non-recorded worker", variant)
	}
	if got := readCrossCheckProvChild(t, f); got != before {
		t.Fatalf("refused checker claim changed the child: %+v -> %+v", before, got)
	}
	if st := mustRun(t, f.env, f.runID).Status; st != "queued" {
		t.Fatalf("child status after refused claim = %s, want queued", st)
	}
}

// settleCrossCheckProvVariant submits a checker whose deadline has already passed (the service
// clock is backdated, the persisted snapshot is never rewritten) and settles it through the
// production status path.
func settleCrossCheckProvVariant(t *testing.T, stage, variant string) crossCheckProvEnd {
	t.Helper()
	f, w := admissionCheckerBase(t, stage)
	f, created := admissionCheckerSubmit(t, f, w, stage, 31*time.Minute, nil)
	holdChild(t, f, variant)
	before := readCrossCheckProvChild(t, f)
	deadline := created.DeadlineAt.Time

	cc := admissionCheckerStatus(t, f, w, stage)
	failed := cc.Verdict == "failed"
	if stage == "code" {
		failed = cc.Outcome.String == "failed"
	}
	child := mustRun(t, f.env, f.runID)
	after := readCrossCheckProvChild(t, f)
	var holds int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1`, f.runID).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	return crossCheckProvEnd{
		Failed:                   failed,
		TimedOut:                 cc.ReasonClass.String == "timed_out",
		DecidedAtDeadline:        cc.DecidedAt.Valid && cc.DecidedAt.Time.Equal(deadline) && cc.DeadlineAt.Time.Equal(deadline),
		ChildStatus:              child.Status,
		ChildFinished:            child.FinishedAt.Valid,
		ChildReleased:            child.ClaimReleasedAt.Valid,
		ChildGenerationUnchanged: after.generation == before.generation,
		ChildEpochUnchanged:      after.epoch == before.epoch,
		NoCapability:             len(child.CodexCapHash) == 0,
		ChildHolds:               holds,
		ProvenanceUnchanged:      after.provWorker == before.provWorker && after.provGen == before.provGen,
	}
}

func TestCodexProvenanceCrossCheckDeadlineControlLiveDB(t *testing.T) {
	setupCodexLiveDB(t) // skips when UZI_TEST_DATABASE_URL is unset
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			for _, variant := range []string{"provenance", "null"} {
				t.Run("refused "+variant, func(t *testing.T) { refuseHeldCheckerClaim(t, stage, variant) })
			}
			want := crossCheckProvEnd{Failed: true, TimedOut: true, DecidedAtDeadline: true, ChildStatus: "cancelled",
				ChildFinished: true, ChildReleased: true, ChildGenerationUnchanged: true, ChildEpochUnchanged: true,
				NoCapability: true, ChildHolds: 0, ProvenanceUnchanged: true}
			for _, variant := range []string{"paired", "provenance", "null"} {
				t.Run(variant, func(t *testing.T) {
					if end := settleCrossCheckProvVariant(t, stage, variant); !reflect.DeepEqual(end, want) {
						t.Fatalf("end state = %+v, want %+v", end, want)
					}
				})
			}
		})
	}
}

// Paired children are claimable by the lane worker on both the first claim and a
// recorded-worker reclaim. These controls rule out a hidden capability, slot or reclaim gate.
func TestCodexProvenanceCrossCheckPairedChildIsClaimableLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, generation := range []int64{0, 1} {
			t.Run(fmt.Sprintf("%s/generation_%d", stage, generation), func(t *testing.T) {
				f, w := admissionCheckerBase(t, stage)
				f, _ = admissionCheckerSubmit(t, f, w, stage, 0, nil)
				if generation == 1 {
					f.env.exec(`UPDATE runs SET claim_generation = 1, worker_id = $2,
					claim_provenance_worker_id = $2, claim_provenance_generation = 1 WHERE id = $1`, f.runID, f.workerID)
				}
				p := laneClaim(t, f, "cross_check", nil)
				if p == nil {
					t.Fatalf("paired checker not claimed: run=%s old_generation=%d", f.runID, generation)
				}
				if p.RunID != f.runID.String() || p.ClaimGeneration != generation+1 {
					t.Fatalf("paired checker claim: run=%s generation=%d, want run=%s generation=%d", p.RunID, p.ClaimGeneration, f.runID, generation+1)
				}
				r := mustRun(t, f.env, f.runID)
				if r.WorkerID != pgconv.UUID(f.workerID) {
					t.Fatalf("claimed by %v, want lane worker", r.WorkerID)
				}
				if r.ClaimProvenanceWorkerID != pgconv.UUID(f.workerID) || !r.ClaimProvenanceGeneration.Valid || r.ClaimProvenanceGeneration.Int64 != generation+1 {
					t.Fatalf("stored provenance = (%v,%v), want (%s,%d)", r.ClaimProvenanceWorkerID, r.ClaimProvenanceGeneration, f.workerID, generation+1)
				}
			})
		}
	}
}
