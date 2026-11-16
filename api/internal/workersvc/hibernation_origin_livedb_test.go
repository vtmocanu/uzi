package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4395 (U2a): op-bound origin markers over a real database. Skipped without
// UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh).

func (f *hibFx) wantOrigins(id uuid.UUID, admit, wake string) store.Worker {
	f.t.Helper()
	w := f.row(id)
	if !originMarkersValid(&w) {
		f.t.Fatalf("markers not bound to the current op: op=%v origin_op=%v", w.HibernationOpID, w.HibernationOriginOpID)
	}
	if w.HibernationAdmitOrigin.String != admit || w.HibernationAdmitOrigin.Valid != (admit != "") ||
		w.HibernationWakeOrigin.String != wake || w.HibernationWakeOrigin.Valid != (wake != "") {
		f.t.Fatalf("origins = admit %v wake %v, want %q / %q", w.HibernationAdmitOrigin, w.HibernationWakeOrigin, admit, wake)
	}
	return w
}

// settleAwake drives the waking worker id to none through a replacement pod.
func (f *hibFx) settleAwake(id uuid.UUID) store.Worker {
	f.t.Helper()
	next := uuid.New()
	f.mustApply(id, TransitionReady, next)
	f.env.exec(`UPDATE workers SET registered_pod_uid = $2, last_heartbeat_at = now() WHERE id = $1`, id, next)
	f.register(id, &next)
	f.want(id, "none")
	return f.row(id)
}

// Every writeHibernation persists and reads back the four origin columns.
func TestWriteHibernationPersistsOriginColumnsLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, _ := f.worker()
	w := f.row(id)
	op := uuid.New()
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	w.HibernationOriginOpID = hibernationUUID(op)
	w.HibernationAdmitOrigin = pgtype.Text{String: AdmitOriginAutoSurplus, Valid: true}
	w.HibernationWakeOrigin = pgtype.Text{String: WakeOriginWarm, Valid: true}
	w.LastManualWakeSettledAt = pgtype.Timestamptz{Time: stamp, Valid: true}
	if _, err := writeHibernation(f.env.ctx, f.env.q, w); err != nil {
		t.Fatal(err)
	}
	got := f.row(id)
	if got.HibernationOriginOpID.Bytes != op || got.HibernationAdmitOrigin.String != AdmitOriginAutoSurplus ||
		got.HibernationWakeOrigin.String != WakeOriginWarm || !got.LastManualWakeSettledAt.Time.Equal(stamp) {
		t.Fatalf("columns not persisted: %+v %+v %+v %v", got.HibernationOriginOpID, got.HibernationAdmitOrigin, got.HibernationWakeOrigin, got.LastManualWakeSettledAt)
	}
}

// The origin each live call site stamps, with the carry-forward of an earlier legacy settle.
func TestHibernationOriginsAtCallSitesLiveDB(t *testing.T) {
	legacySettle := func(f *hibFx, id uuid.UUID) time.Time {
		at := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
		f.env.exec(`UPDATE workers SET last_wake_settled_at = $2 WHERE id = $1`, id, at)
		return at
	}
	wantCarried := func(t *testing.T, f *hibFx, id uuid.UUID, at time.Time) {
		t.Helper()
		if got := f.row(id).LastManualWakeSettledAt; !got.Valid || !got.Time.Equal(at) {
			t.Fatalf("legacy settle %v not carried forward: %v", at, got)
		}
	}
	t.Run("manual hibernate stamps manual", func(t *testing.T) {
		f := newHibFx(t)
		id, _ := f.worker()
		at := legacySettle(f, id)
		f.hibernate(id)
		f.wantOrigins(id, AdmitOriginManual, "")
		wantCarried(t, f, id, at)
	})
	t.Run("WakeWorker with an actor is manual, without is system", func(t *testing.T) {
		for _, c := range []struct {
			actor bool
			want  string
		}{{true, WakeOriginManual}, {false, WakeOriginSystem}} {
			f := newHibFx(t)
			id, pod := f.worker()
			at := legacySettle(f, id) // a settle from before the migration; the admission carries it
			f.sleep(id, pod)
			actor := &f.actor
			if !c.actor {
				actor = nil
			}
			if _, err := f.svc.WakeWorker(f.env.ctx, actor, id, ""); err != nil {
				t.Fatal(err)
			}
			f.wantOrigins(id, "", c.want)
			wantCarried(t, f, id, at)
		}
	})
	t.Run("refused after scale-down is system", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		f.hibernate(id)
		f.mustApply(id, TransitionScalingDown, pod)
		f.mustApply(id, TransitionRefused, pod)
		f.want(id, "waking")
		f.wantOrigins(id, "", WakeOriginSystem)
	})
	t.Run("preparation deadline is system", func(t *testing.T) {
		f := newHibFx(t)
		id, _ := f.worker()
		f.hibernate(id)
		f.env.exec(`UPDATE workers SET hibernation_requested_at = now() - interval '11 minutes' WHERE id = $1`, id)
		if _, err := f.svc.HibernationSettlePass(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		f.want(id, "waking")
		f.wantOrigins(id, "", WakeOriginSystem)
	})
	t.Run("hibernation disabled is system", func(t *testing.T) {
		f := newHibFx(t)
		id := f.sleepingWorker()
		f.set.on = false
		if _, err := f.svc.HibernationSettlePass(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		f.want(id, "waking")
		f.wantOrigins(id, "", WakeOriginSystem)
	})
	t.Run("demand wake is demand", func(t *testing.T) {
		f := newHibFx(t)
		id := f.sleepingWorker()
		run := f.queuedRun()
		if res, err := f.svc.DemandWake(f.env.ctx, run, id); err != nil || res != DemandWakeWoke {
			t.Fatalf("DemandWake = %v, %v", res, err)
		}
		f.wantOrigins(id, "", WakeOriginDemand)
	})
	t.Run("cache cleanup withdrawal is system", func(t *testing.T) {
		f := newHibFx(t)
		id, _ := f.started()
		f.mustReport(id, f.resultRep(id, false, okSteps())) // gate not exclusive: withdrawForCleanup
		f.want(id, "waking")
		f.wantOrigins(id, "", WakeOriginSystem)
	})
	t.Run("automatic admission is auto_long", func(t *testing.T) {
		a := newAutoFx(t)
		id, _ := a.idle(400 * 24 * time.Hour)
		at := a.clock.Add(-399 * 24 * time.Hour).UTC()
		a.env.exec(`UPDATE workers SET last_wake_settled_at = $2 WHERE id = $1`, id, at)
		a.mustPass(1)
		a.hibFx.wantOrigins(id, AdmitOriginAutoLong, "")
		wantCarried(t, a.hibFx, id, at)
	})
	t.Run("surplus admission is auto_surplus", func(t *testing.T) {
		p := newPoolFx(t)
		older := p.member(poolSurplusIdle + 3*time.Hour)
		newer := p.member(poolSurplusIdle - 62*time.Minute)
		p.pass(older, newer)
		p.wantStates(map[uuid.UUID]string{older: "preparing", newer: "none"})
		p.hibFx.wantOrigins(older, AdmitOriginAutoSurplus, "")
	})
}

// A manual wake settles through trySettle and stamps the manual grace anchor; a system wake
// stamps only last_wake_settled_at. The markers stay after the settle.
func TestSettleStampsManualGraceLiveDB(t *testing.T) {
	f := newHibFx(t)
	id := f.sleepingWorker()
	if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, "operator"); err != nil {
		t.Fatal(err)
	}
	op := f.op(id)
	row := f.settleAwake(id)
	if !row.LastWakeSettledAt.Valid || !row.LastManualWakeSettledAt.Valid ||
		!row.LastManualWakeSettledAt.Time.Equal(row.LastWakeSettledAt.Time) {
		t.Fatalf("manual settle: last_wake=%v last_manual=%v", row.LastWakeSettledAt, row.LastManualWakeSettledAt)
	}
	f.wantOrigins(id, "", WakeOriginManual)
	if f.op(id) != op {
		t.Fatal("settle replaced the op")
	}

	g := newHibFx(t)
	sys := g.sleepingWorker()
	if _, err := g.svc.WakeWorker(g.env.ctx, nil, sys, "system"); err != nil {
		t.Fatal(err)
	}
	row = g.settleAwake(sys)
	if !row.LastWakeSettledAt.Valid || row.LastManualWakeSettledAt.Valid {
		t.Fatalf("system settle: last_wake=%v last_manual=%v", row.LastWakeSettledAt, row.LastManualWakeSettledAt)
	}
}

// A demand wake joining a waking warm op upgrades the origin in the same transaction and keeps
// the op id and the first wake time.
func TestDemandJoinUpgradesWarmOpLiveDB(t *testing.T) {
	f := newHibFx(t)
	id := f.sleepingWorker()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := f.svc.hibernationTx(f.env.ctx, id, func(q *store.Queries, w store.Worker) error {
		moveToWaking(&w, now, "warm", WakeOriginWarm)
		_, err := writeHibernation(f.env.ctx, q, w)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	warm := f.wantOrigins(id, "", WakeOriginWarm)
	run := f.queuedRun()
	res, err := f.svc.DemandWake(f.env.ctx, run, id)
	if err != nil || res != DemandWakeAttached {
		t.Fatalf("DemandWake on a waking warm op = %v, %v; want attached", res, err)
	}
	got := f.wantOrigins(id, "", WakeOriginDemand)
	if got.HibernationOpID != warm.HibernationOpID || !got.WakeRequestedAt.Time.Equal(warm.WakeRequestedAt.Time) ||
		got.HibernationReason != warm.HibernationReason {
		t.Fatalf("the upgrade moved the op or first wake time: before %+v after %+v", warm, got)
	}
	// A system join afterwards changes nothing.
	if _, err := f.svc.WakeWorker(f.env.ctx, nil, id, "again"); err != nil {
		t.Fatal(err)
	}
	f.wantOrigins(id, "", WakeOriginDemand)
}

// A manual WakeWorker joining a waking warm op also persists the upgrade.
func TestManualWakeJoinUpgradesWarmOpLiveDB(t *testing.T) {
	f := newHibFx(t)
	id := f.sleepingWorker()
	if err := f.svc.hibernationTx(f.env.ctx, id, func(q *store.Queries, w store.Worker) error {
		moveToWaking(&w, time.Now().UTC(), "warm", WakeOriginWarm)
		_, err := writeHibernation(f.env.ctx, q, w)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	op := f.op(id)
	if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); err != nil {
		t.Fatal(err)
	}
	f.wantOrigins(id, "", WakeOriginManual)
	if f.op(id) != op {
		t.Fatal("upgrade replaced the op")
	}
	row := f.settleAwake(id)
	if !row.LastManualWakeSettledAt.Valid {
		t.Fatal("a warm op upgraded to manual must settle as a manual-grade wake")
	}
}
