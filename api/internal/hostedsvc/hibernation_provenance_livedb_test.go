package hostedsvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Candidate selection and wake recording are separate transactions. A real claim in
// between must invalidate both the old candidate and a wake recorded before the claim.
func TestHibernationWakeRechecksCodexProvenanceLiveDB(t *testing.T) {
	for _, recordFirst := range []bool{false, true} {
		name := "selected candidate becomes stale"
		if recordFirst {
			name = "recorded wake becomes stale"
		}
		t.Run(name, func(t *testing.T) {
			e := newWakeEnv(t, false)
			caps := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2}
			a := e.sleeper(nil, caps, time.Hour)
			b := e.sleeper(nil, caps, 2*time.Hour)
			run := e.queuedRun()
			e.exec(`UPDATE runs SET harness = 'codex' WHERE id = $1`, run)
			list := func() []store.ListHibernationWakeDemandRow {
				t.Helper()
				now := time.Now()
				rows, err := e.q.ListHibernationWakeDemand(e.ctx, store.ListHibernationWakeDemandParams{
					CrossCheckEvaluatedAt: pgconv.Time(now), CrossCheckAffinityCutoff: pgconv.Time(now.Add(-2 * time.Minute)),
					CodexCuratedModels: workersvc.CodexCuratedModels(), AffinityCutoff: pgconv.Time(now.Add(-2 * time.Hour)),
					DockerRepoAllowlist: []uuid.UUID{}, HeartbeatCutoff: pgconv.Time(now.Add(-45 * time.Second)),
					EphemeralLease: workersvc.LeaseInterval(0), BackgroundGraceCutoff: pgconv.Time(now.Add(-15 * time.Minute)), MaxRows: 100,
				})
				if err != nil {
					t.Fatal(err)
				}
				return rows
			}
			candidates := list()
			if len(candidates) != 1 || candidates[0].RunID != run || candidates[0].WorkerID != b {
				t.Fatalf("initial candidates = %+v, want run on oldest sleeper B", candidates)
			}
			if recordFirst {
				if result, err := e.svc.DemandWake(e.ctx, run, b); err != nil || result != workersvc.DemandWakeWoke {
					t.Fatalf("initial wake for B = %v, %v", result, err)
				}
			}
			// A comes online, claims through the real ClaimRun, then returns to sleep
			// while its run requeues. No wake sweep observes the intermediate claim.
			e.exec(`UPDATE workers SET status='online', last_heartbeat_at=now(), hibernation_state='none',
				hibernation_op_id=NULL, hibernated_at=NULL, wake_requested_at=NULL WHERE id=$1`, a)
			now := time.Now()
			claimed, err := e.q.ClaimRun(e.ctx, store.ClaimRunParams{
				WorkerID: pgconv.UUID(a), UserID: e.user, WorkerProtocolCaps: caps,
				AffinityCutoff: pgconv.Time(now.Add(-2 * time.Hour)), SpreadCutoff: pgconv.Time(now.Add(-9 * time.Second)),
				HeartbeatCutoff: pgconv.Time(now.Add(-45 * time.Second)),
			})
			if err != nil || claimed.ID != run || claimed.ClaimProvenanceWorkerID != pgconv.UUID(a) || claimed.ClaimGeneration != 1 {
				t.Fatalf("claim as A: id=%s generation=%d provenance=%v err=%v", claimed.ID, claimed.ClaimGeneration, claimed.ClaimProvenanceWorkerID, err)
			}
			e.exec(`UPDATE runs SET status='queued', status_since=now()-interval '1 hour', updated_at=now()-interval '3 hours' WHERE id=$1`, run)
			e.exec(`UPDATE workers SET status='offline', hibernation_state='hibernated', hibernation_op_id=$2,
				hibernated_at=now()-interval '1 hour' WHERE id=$1`, a, uuid.New())
			if recordFirst {
				stale, err := e.q.ListStaleHibernationWakes(e.ctx, 100)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range stale {
					found = found || row.RunID == run && row.WorkerID == b
				}
				if !found {
					t.Fatal("wake for non-recorded B was not selected for reconciliation")
				}
				if released, err := e.svc.ReleaseStaleWake(e.ctx, run); err != nil || !released {
					t.Fatalf("release obsolete wake = %v, %v", released, err)
				}
			}
			op := e.op(b)
			state := e.state(b)
			if result, err := e.svc.DemandWake(e.ctx, run, candidates[0].WorkerID); err != nil || result != workersvc.DemandWakeNone {
				t.Fatalf("stale candidate B = %v, %v; want no wake", result, err)
			}
			if e.wakeRowsFor(run) != 0 || e.op(b) != op || e.state(b) != state {
				t.Fatal("refused candidate changed B or recorded a wake")
			}
			if got := list(); len(got) != 1 || got[0].WorkerID != a {
				t.Fatalf("fresh candidates = %+v, want recorded worker A", got)
			}
			if result, err := e.svc.DemandWake(e.ctx, run, a); err != nil || result != workersvc.DemandWakeWoke {
				t.Fatalf("recorded worker A wake = %v, %v", result, err)
			}
		})
	}
}
