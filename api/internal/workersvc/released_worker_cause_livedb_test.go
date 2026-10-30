package workersvc

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3639: the released-worker health rung words a memory-pressure park differently from a wall
// park, off the released_worker_cause stamp. Every step below is its own committed statement; the
// health reason is read through ListActiveRunsForHealth and queuedReason, never a hand-built row.

const (
	releasedReasonTimeLimit = "waiting for another worker, or restart worker %s (its previous process was released at the time limit)"
	releasedReasonMemory    = "waiting for another worker, or restart worker %s (its previous process was released by a memory-pressure park)"
)

func releasedCauseHealthReason(t *testing.T, f memoryFixture) string {
	t.Helper()
	rows, err := f.s.q.ListActiveRunsForHealth(f.e.ctx, nil)
	if err != nil {
		t.Fatalf("ListActiveRunsForHealth: %v", err)
	}
	for _, r := range rows {
		if r.ID == f.b.RunID {
			return f.s.queuedReason(f.e.ctx, time.Now(), r, thWithLimit(testCustodyLimit))
		}
	}
	t.Fatalf("run %s not listed for health", f.b.RunID)
	return ""
}

func releasedCauseOwnerResume(t *testing.T, f memoryFixture) store.Run {
	t.Helper()
	if _, err := f.e.q.ResumeMemoryEpisode(f.e.ctx, store.ResumeMemoryEpisodeParams{
		ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400}); err != nil {
		t.Fatal(err)
	}
	r := f.run(t)
	if r.Status != "queued" || !r.ClaimReleasedAt.Valid || r.ReleasedWorkerID != pgconv.UUID(f.w.ID) {
		t.Fatalf("resumed row status=%q released=%v worker=%v", r.Status, r.ClaimReleasedAt.Valid, r.ReleasedWorkerID)
	}
	return r
}

func requireReleasedCause(t *testing.T, r store.Run, want string) {
	t.Helper()
	if !r.ReleasedWorkerCause.Valid || r.ReleasedWorkerCause.String != want ||
		!r.ReleasedWorkerCauseAt.Valid || !r.ClaimReleasedAt.Valid ||
		!r.ReleasedWorkerCauseAt.Time.Equal(r.ClaimReleasedAt.Time) {
		t.Fatalf("cause=%+v at=%+v claim_released_at=%+v, want %q stamped with claim_released_at",
			r.ReleasedWorkerCause, r.ReleasedWorkerCauseAt, r.ClaimReleasedAt, want)
	}
}

func requireNoReleasedCause(t *testing.T, r store.Run) {
	t.Helper()
	if r.ReleasedWorkerCause.Valid || r.ReleasedWorkerCauseAt.Valid {
		t.Fatalf("cause=%+v at=%+v, want both NULL", r.ReleasedWorkerCause, r.ReleasedWorkerCauseAt)
	}
}

// (a) and (d): a real memory park stamps the cause with claim_released_at, owner Resume keeps it, the
// health reason names the memory park, and a fresh claim clears both columns (issue and chat lanes).
func TestReleasedWorkerCauseMemoryParkLiveDB(t *testing.T) {
	for _, chat := range []bool{false, true} {
		name := "issue"
		if chat {
			name = "chat"
		}
		t.Run(name, func(t *testing.T) {
			f := newMemoryFixture(t)
			if chat {
				f.e.exec("UPDATE runs SET status='completed' WHERE id=$1", f.b.RunID)
				f.b.RunID = seedOutageChatRun(t, f.e, f.w.UserID, f.w.ID, 2, 2)
			}
			f.reserve(t, f.b)
			requireNoReleasedCause(t, f.run(t))
			requireReleasedCause(t, f.hold(t), "worker_memory_pressure")
			releasedCauseOwnerResume(t, f)
			requireReleasedCause(t, f.run(t), "worker_memory_pressure")
			if !chat {
				if got, w := releasedCauseHealthReason(t, f), fmt.Sprintf(releasedReasonMemory, f.w.Name); got != w {
					t.Fatalf("queued reason = %q, want %q", got, w)
				}
			}

			w, _, err := f.s.Register(f.e.ctx, f.w, "fixture", "", nil, nil,
				[]string{capability.WorkerMemoryPressureV1}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var claimed store.Run
			if chat {
				claimed, err = f.e.q.ClaimChatRun(f.e.ctx, store.ClaimChatRunParams{
					WorkerID: pgconv.UUID(w.ID), UserID: w.UserID,
					AffinityCutoff: pgconv.Time(time.Now().Add(-time.Hour))})
			} else {
				claimed, err = f.e.q.ClaimRun(f.e.ctx, claimRunParams(w))
			}
			if err != nil || claimed.ID != f.b.RunID {
				t.Fatalf("fresh claim = %v err=%v", claimed.ID, err)
			}
			requireNoReleasedCause(t, claimed)
			requireNoReleasedCause(t, f.run(t))
		})
	}
}

// (b): a wall park by a NEW incarnation after a memory park overwrites the cause, so the wall wording
// returns while memory_episode stays above zero.
func TestReleasedWorkerCauseWallAfterMemoryLiveDB(t *testing.T) {
	f := newMemoryFixture(t)
	f.reserve(t, f.b)
	f.hold(t)
	releasedCauseOwnerResume(t, f)

	w, _, err := f.s.Register(f.e.ctx, f.w, "fixture", "", nil, nil,
		[]string{capability.WorkerMemoryPressureV1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.e.q.ClaimRun(f.e.ctx, claimRunParams(w))
	if err != nil || claimed.ID != f.b.RunID {
		t.Fatalf("new incarnation claim = %v err=%v", claimed.ID, err)
	}
	requireNoReleasedCause(t, claimed)
	// Out of time on a worker that lacks wall_park_v1, so ParkRunsAtWall parks it server-side.
	f.e.exec(`UPDATE runs SET status='running', started_at=now()-interval '3 hours', budget_wall_seconds=60 WHERE id=$1`, f.b.RunID)
	parked, err := f.e.q.ParkRunsAtWall(f.e.ctx, store.ParkRunsAtWallParams{
		Now: pgconv.Time(time.Now()), GlobalTimeoutSeconds: 3600,
		WorkerStaleCutoff: pgconv.Time(time.Now().Add(-45 * time.Second)), GraceSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range parked {
		found = found || p.ID == f.b.RunID
	}
	if !found {
		t.Fatalf("ParkRunsAtWall did not park %s", f.b.RunID)
	}
	requireReleasedCause(t, f.run(t), "wall")

	f.e.exec(`UPDATE runs SET budget_wall_seconds=86400 WHERE id=$1`, f.b.RunID)
	if _, err := f.e.q.ResumePausedRun(f.e.ctx, store.ResumePausedRunParams{
		ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400}); err != nil {
		t.Fatal(err)
	}
	r := f.run(t)
	if r.Status != "queued" || r.MemoryEpisode == 0 {
		t.Fatalf("status=%q memory_episode=%d, want queued with episode > 0", r.Status, r.MemoryEpisode)
	}
	if got, want := releasedCauseHealthReason(t, f), fmt.Sprintf(releasedReasonTimeLimit, w.Name); got != want {
		t.Fatalf("queued reason = %q, want %q", got, want)
	}
}

// (c): rollback regressions. A writer that predates the cause columns moves claim_released_at without
// touching the stamp, so a stale memory cause must not name a memory park. The archived ClaimRun
// literal (testdata/task-checkpoint-legacy) cannot execute on the current schema (42846, see
// TestArchivedClaimRunRejectsOrdinaryCandidateLiveDB), so the legacy claim and the old-shape park are
// raw UPDATEs copying the pre-change SET lists.
func TestReleasedWorkerCauseRollbackLiveDB(t *testing.T) {
	t.Run("legacy claim then old-shape park", func(t *testing.T) {
		f := newMemoryFixture(t)
		f.reserve(t, f.b)
		f.hold(t)
		releasedCauseOwnerResume(t, f)

		w2 := seedSnapshotWorker(t, f.e, f.w.UserID, "legacy-nonce")
		f.e.exec("UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1", f.w.ID)
		// Old ClaimRun: clears the pair and claim_released_at, never the cause columns.
		f.e.exec(`UPDATE runs SET status='running', worker_id=$2, claim_generation=claim_generation+1,
			claimed_at=now(), claim_released_at=NULL, released_worker_id=NULL, released_worker_nonce=NULL WHERE id=$1`, f.b.RunID, w2)
		stale := f.run(t)
		if !stale.ReleasedWorkerCause.Valid || stale.ClaimReleasedAt.Valid {
			t.Fatalf("legacy claim row cause=%+v released=%v, want stale cause and cleared pair", stale.ReleasedWorkerCause, stale.ClaimReleasedAt.Valid)
		}
		// Old ParkRunsAtWall: the pair and claim_released_at, no cause columns.
		f.e.exec(`UPDATE runs SET status='paused', status_since=now(), hold_reason='budget_exhausted', hold_captured_head=NULL,
			claim_released_at=now(), released_worker_id=worker_id, released_worker_nonce='legacy-nonce', updated_at=now() WHERE id=$1`, f.b.RunID)
		f.e.exec(`UPDATE runs SET budget_wall_seconds=86400 WHERE id=$1`, f.b.RunID)
		if _, err := f.e.q.ResumePausedRun(f.e.ctx, store.ResumePausedRunParams{
			ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400}); err != nil {
			t.Fatal(err)
		}
		r := f.run(t)
		if r.Status != "queued" || r.ReleasedWorkerCause.String != "worker_memory_pressure" ||
			r.ReleasedWorkerCauseAt.Time.Equal(r.ClaimReleasedAt.Time) {
			t.Fatalf("row status=%q cause=%+v at=%+v released=%+v, want queued with a stale memory stamp",
				r.Status, r.ReleasedWorkerCause, r.ReleasedWorkerCauseAt, r.ClaimReleasedAt)
		}
		name := "wsnap-" + w2.String()
		if got, want := releasedCauseHealthReason(t, f), fmt.Sprintf(releasedReasonTimeLimit, name); got != want {
			t.Fatalf("queued reason = %q, want %q", got, want)
		}
	})

	t.Run("old-shape park over a memory cause", func(t *testing.T) {
		f := newMemoryFixture(t)
		f.reserve(t, f.b)
		f.hold(t)
		releasedCauseOwnerResume(t, f)
		// An old writer re-records the pair and moves claim_released_at, leaving the memory stamp behind.
		f.e.exec(`UPDATE runs SET claim_released_at=now(), released_worker_id=$2, released_worker_nonce=$3 WHERE id=$1`,
			f.b.RunID, f.w.ID, f.b.RegisterNonce)
		r := f.run(t)
		if r.ReleasedWorkerCause.String != "worker_memory_pressure" || r.ReleasedWorkerCauseAt.Time.Equal(r.ClaimReleasedAt.Time) {
			t.Fatalf("row cause=%+v at=%+v released=%+v, want a stale memory stamp", r.ReleasedWorkerCause, r.ReleasedWorkerCauseAt, r.ClaimReleasedAt)
		}
		if got, want := releasedCauseHealthReason(t, f), fmt.Sprintf(releasedReasonTimeLimit, f.w.Name); got != want {
			t.Fatalf("queued reason = %q, want %q", got, want)
		}
	})
}

// A memory-hold row is refused by the claim fence until a new incarnation registers; this pins that the
// stamp helpers above did not widen D19.
func TestReleasedWorkerCauseDoesNotChangeClaimFenceLiveDB(t *testing.T) {
	f := newMemoryFixture(t)
	f.reserve(t, f.b)
	f.hold(t)
	releasedCauseOwnerResume(t, f)
	if _, err := f.e.q.ClaimRun(f.e.ctx, claimRunParams(f.w)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("released incarnation reclaimed after Resume: %v", err)
	}
}
