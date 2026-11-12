package workersvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4205 M2: the hibernation state machine over a real database. Skipped without
// UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh).

type fakeHibSettings struct{ on bool }

func (f *fakeHibSettings) WorkerHibernationEnabled(context.Context) (bool, error) { return f.on, nil }

type hibFx struct {
	t         *testing.T
	env       codexTestEnv
	svc       *Service
	set       *fakeHibSettings
	user      uuid.UUID
	repo      uuid.UUID
	actor     HibernationActor
	deploy    string
	assembled bool
}

func newHibFx(t *testing.T) *hibFx {
	t.Helper()
	env := setupCodexLiveDB(t)
	user, _, repo := env.seedCodexInfra(t)
	t.Cleanup(func() { _, _ = env.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, user) })
	f := &hibFx{t: t, env: env, set: &fakeHibSettings{on: true}, user: user, repo: repo, deploy: uuid.NewString()}
	f.svc = New(env.q, env.box, testParams())
	f.svc.SetTxBeginner(env.pool)
	f.svc.SetVault(env.vlt)
	f.svc.SetHibernationSettings(f.set)
	f.actor = HibernationActor{ID: user, Label: "admin"}
	f.advertise(1, 5, 0)
	return f
}

// advertise records the controller advertisement, `ago` old.
func (f *hibFx) advertise(version, poll int32, ago time.Duration) {
	f.t.Helper()
	err := f.env.q.UpsertControllerHibernationAdvertisement(f.env.ctx, store.UpsertControllerHibernationAdvertisementParams{
		AdvertisedAt:        pgtype.Timestamptz{Time: time.Now().Add(-ago), Valid: true},
		HibernationVersion:  pgtype.Int4{Int32: version, Valid: true},
		PollIntervalSeconds: pgtype.Int4{Int32: poll, Valid: true},
	})
	if err != nil {
		f.t.Fatalf("advertise: %v", err)
	}
}

// worker seeds an idle online persistent hosted worker with a registered pod uid.
func (f *hibFx) worker() (uuid.UUID, uuid.UUID) {
	f.t.Helper()
	id, pod := uuid.New(), uuid.New()
	f.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, registered_pod_uid)
	            VALUES ($1, $2, $3, $4, 'online', 'hosted', 'm', 'base', $5)`, id, f.user, "w-"+id.String(), id[:], pod)
	f.env.exec(`UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, id)
	return id, pod
}

func (f *hibFx) row(id uuid.UUID) store.Worker {
	f.t.Helper()
	return wkrRow(f.t, f.env, id)
}

func (f *hibFx) hibernate(id uuid.UUID) store.Worker {
	f.t.Helper()
	w, err := f.svc.HibernateWorker(f.env.ctx, f.actor, id, "test")
	if err != nil {
		f.t.Fatalf("HibernateWorker: %v", err)
	}
	return w
}

func (f *hibFx) op(id uuid.UUID) string { return uuid.UUID(f.row(id).HibernationOpID.Bytes).String() }

func (f *hibFx) tr(id uuid.UUID, name string, pod uuid.UUID) HibernationTransition {
	t := HibernationTransition{OpID: f.op(id), Transition: name, DeploymentUID: f.deploy, ResourceVersion: "rv-" + uuid.NewString()[:8]}
	switch name {
	case TransitionScalingDown, TransitionReady:
		t.PodUID = pod.String()
	case TransitionRefused:
		t.Reason = "roll in flight"
	}
	if name == TransitionScaledDown {
		t.ResourceVersion = ""
	}
	return t
}

func (f *hibFx) apply(id uuid.UUID, t HibernationTransition) (store.Worker, error) {
	return f.svc.ApplyTransition(f.env.ctx, id, t)
}

func (f *hibFx) mustApply(id uuid.UUID, name string, pod uuid.UUID) store.Worker {
	f.t.Helper()
	w, err := f.apply(id, f.tr(id, name, pod))
	if err != nil {
		f.t.Fatalf("%s: %v", name, err)
	}
	return w
}

func (f *hibFx) register(id uuid.UUID, pod *uuid.UUID) {
	f.t.Helper()
	var pods []uuid.UUID
	if pod != nil {
		pods = append(pods, *pod)
	}
	if _, _, err := f.svc.RegisterWithCrossCheckSlots(f.env.ctx, f.row(id), "v1", "", nil, nil, nil, nil, nil, pods...); err != nil {
		f.t.Fatalf("register: %v", err)
	}
}

func (f *hibFx) heartbeatNow(id uuid.UUID) {
	f.env.exec(`UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, id)
}

func (f *hibFx) state(id uuid.UUID) string { return f.row(id).HibernationState }

func (f *hibFx) want(id uuid.UUID, state string) {
	f.t.Helper()
	if got := f.state(id); got != state {
		f.t.Fatalf("state = %q, want %q", got, state)
	}
}

func (f *hibFx) events(id uuid.UUID, action string) int {
	f.t.Helper()
	var n int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM worker_hibernation_events WHERE worker_id=$1 AND action=$2`, id, action).Scan(&n); err != nil {
		f.t.Fatalf("count events: %v", err)
	}
	return n
}

func refusalCode(err error) string {
	var r *HibernationRefusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// queuedRun seeds an unassigned queued issue run old enough that affinity and spread never decide.
func (f *hibFx) queuedRun() uuid.UUID {
	f.t.Helper()
	run := seedUnassignedRun(f.t, f.env, f.user, f.repo, 0)
	f.env.exec(`UPDATE runs SET updated_at = now() - interval '3 hours' WHERE id = $1`, run)
	return run
}

// fullCycle drives hibernate -> scaling_down -> scaled_down -> wake -> ready + register -> none.
func (f *hibFx) fullCycle(id uuid.UUID, pod uuid.UUID) uuid.UUID {
	f.t.Helper()
	f.hibernate(id)
	f.want(id, "preparing")
	f.mustApply(id, TransitionScalingDown, pod)
	f.mustApply(id, TransitionScaledDown, uuid.Nil)
	f.want(id, "hibernated")
	if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, "wake"); err != nil {
		f.t.Fatalf("wake: %v", err)
	}
	f.want(id, "waking")
	next := uuid.New()
	f.mustApply(id, TransitionReady, next)
	f.want(id, "waking") // ready alone never settles a replacement pod
	f.env.exec(`UPDATE workers SET registered_pod_uid = $2 WHERE id = $1`, id, next)
	f.heartbeatNow(id)
	f.register(id, &next)
	f.want(id, "none")
	return next
}

func TestHibernateAdmissionLiveDB(t *testing.T) {
	cases := []struct {
		name string
		want string
		prep func(f *hibFx, id uuid.UUID)
	}{
		{"setting off", RefusalSettingOff, func(f *hibFx, _ uuid.UUID) { f.set.on = false }},
		{"not persistent hosted", RefusalNotPersistentHosted, func(f *hibFx, id uuid.UUID) {
			f.env.exec(`UPDATE workers SET kind='external', hosted_size=NULL WHERE id=$1`, id)
		}},
		{"null pod uid", RefusalNoPodIdentity, func(f *hibFx, id uuid.UUID) {
			f.env.exec(`UPDATE workers SET registered_pod_uid=NULL WHERE id=$1`, id)
		}},
		{"advertisement version zero", RefusalControllerNotAdvertise, func(f *hibFx, _ uuid.UUID) { f.advertise(0, 5, 0) }},
		{"advertisement stale", RefusalControllerNotAdvertise, func(f *hibFx, _ uuid.UUID) { f.advertise(1, 5, 11*time.Second) }},
		{"advertisement missing", RefusalControllerNotAdvertise, func(f *hibFx, _ uuid.UUID) {
			f.env.exec(`UPDATE controller_report_status SET hibernation_version=NULL, hibernation_advertised_at=NULL, poll_interval_seconds=NULL`)
		}},
		{"busy", RefusalBusy, func(f *hibFx, id uuid.UUID) {
			seedOutageRun(f.t, f.env, f.user, f.repo, id, "queued", "issue", 0, 0)
		}},
		{"custody hold", RefusalCustodyHold, func(f *hibFx, id uuid.UUID) {
			run := uuid.New()
			f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, claim_generation)
			            VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed', 1)`, run, f.user, f.repo, nextOutageIID())
			f.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
			            VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, uuid.New(), f.user, f.repo, run, id)
		}},
		{"manual cordon", RefusalIntakeFenced, func(f *hibFx, id uuid.UUID) {
			f.env.exec(`UPDATE workers SET manual_cordoned_at=now(), manual_cordoned_by=$2, manual_cordoned_by_label='a' WHERE id=$1`, id, f.user)
		}},
		{"draining", RefusalIntakeFenced, func(f *hibFx, id uuid.UUID) {
			f.env.exec(`UPDATE workers SET draining_since=now() WHERE id=$1`, id)
		}},
		{"maintenance fence", RefusalIntakeFenced, func(f *hibFx, id uuid.UUID) {
			f.env.exec(`UPDATE workers SET maintenance_fenced=true WHERE id=$1`, id)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newHibFx(t)
			id, _ := f.worker()
			c.prep(f, id)
			before := f.row(id)
			_, err := f.svc.HibernateWorker(f.env.ctx, f.actor, id, "")
			if got := refusalCode(err); got != c.want {
				t.Fatalf("refusal = %q (%v), want %q", got, err, c.want)
			}
			after := f.row(id)
			if after.HibernationState != "none" || after.HibernationOpID.Valid || before.HibernationState != "none" {
				t.Fatalf("a refusal changed the row: %q op=%v", after.HibernationState, after.HibernationOpID)
			}
			if f.events(id, "hibernate") != 0 {
				t.Fatal("a refusal wrote an audit event")
			}
		})
	}

	t.Run("already hibernating", func(t *testing.T) {
		f := newHibFx(t)
		id, _ := f.worker()
		f.hibernate(id)
		if _, err := f.svc.HibernateWorker(f.env.ctx, f.actor, id, ""); refusalCode(err) != RefusalAlreadyActive {
			t.Fatalf("second hibernate: %v", err)
		}
	})
	t.Run("unknown worker", func(t *testing.T) {
		f := newHibFx(t)
		if _, err := f.svc.HibernateWorker(f.env.ctx, f.actor, uuid.New(), ""); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unknown worker: %v", err)
		}
	})
	t.Run("success writes the admission fields and the event", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		got := f.hibernate(id)
		row := f.row(id)
		if got.HibernationState != "preparing" || row.HibernationState != "preparing" || !row.HibernationOpID.Valid ||
			!row.HibernationRequestedAt.Valid || row.HibernationAdmissionPodUid.Bytes != pod ||
			row.HibernationScalingDownAt.Valid || row.WakeReadyOpID.Valid || row.WakeError.Valid {
			t.Fatalf("admission row = %+v", row)
		}
		if f.events(id, "hibernate") != 1 {
			t.Fatal("no hibernate audit event")
		}
		var actor, label string
		var op uuid.UUID
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT actor_id::text, actor_label, op_id FROM worker_hibernation_events WHERE worker_id=$1`, id).Scan(&actor, &label, &op); err != nil {
			t.Fatal(err)
		}
		if actor != f.user.String() || label != "admin" || op != uuid.UUID(row.HibernationOpID.Bytes) {
			t.Fatalf("event = %s %s %s", actor, label, op)
		}
	})
	t.Run("a wake clears nothing it must keep, and admission after a cycle clears wake state", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		pod2 := f.fullCycle(id, pod)
		f.hibernate(id)
		row := f.row(id)
		if row.HibernationAdmissionPodUid.Bytes != pod2 || row.WakeRegisterOpID.Valid || row.WakeReadyOpID.Valid || row.HibernationScalingDownAt.Valid {
			t.Fatalf("re-admission kept stale evidence: %+v", row)
		}
	})
}

func TestHibernationTransitionsAndFencingLiveDB(t *testing.T) {
	t.Run("stale op is a conflict and changes nothing", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		f.hibernate(id)
		stale := f.tr(id, TransitionScalingDown, pod)
		stale.OpID = uuid.NewString()
		before := f.row(id)
		if _, err := f.apply(id, stale); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("stale op: %v", err)
		}
		if after := f.row(id); after.HibernationScalingDownAt.Valid || after.HibernationState != before.HibernationState {
			t.Fatalf("stale op changed the row: %+v", after)
		}
	})

	t.Run("illegal transitions are conflicts", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		// Awake worker: nothing is legal.
		for _, name := range []string{TransitionScalingDown, TransitionScaledDown, TransitionReady, TransitionRefused} {
			if _, err := f.apply(id, f.tr(id, name, pod)); !errors.Is(err, ErrHibernationConflict) {
				t.Fatalf("%s on an awake worker: %v", name, err)
			}
		}
		f.hibernate(id)
		// preparing: ready is illegal.
		if _, err := f.apply(id, f.tr(id, TransitionReady, pod)); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("ready from preparing: %v", err)
		}
		f.mustApply(id, TransitionScalingDown, pod)
		f.mustApply(id, TransitionScaledDown, uuid.Nil)
		// hibernated: refused and ready are illegal.
		for _, name := range []string{TransitionRefused, TransitionReady} {
			if _, err := f.apply(id, f.tr(id, name, pod)); !errors.Is(err, ErrHibernationConflict) {
				t.Fatalf("%s from hibernated: %v", name, err)
			}
		}
		f.want(id, "hibernated")
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); err != nil {
			t.Fatal(err)
		}
		// waking: the hibernate-side steps are illegal.
		for _, name := range []string{TransitionRefused, TransitionScalingDown, TransitionScaledDown} {
			if _, err := f.apply(id, f.tr(id, name, pod)); !errors.Is(err, ErrHibernationConflict) {
				t.Fatalf("%s from waking: %v", name, err)
			}
		}
		f.want(id, "waking")
	})

	t.Run("validation", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		f.hibernate(id)
		good := f.tr(id, TransitionScalingDown, pod)
		long := make([]rune, 501)
		for i := range long {
			long[i] = 'x'
		}
		mut := map[string]func(*HibernationTransition){
			"no op":             func(x *HibernationTransition) { x.OpID = "" },
			"bad op":            func(x *HibernationTransition) { x.OpID = "nope" },
			"no transition":     func(x *HibernationTransition) { x.Transition = "" },
			"unknown":           func(x *HibernationTransition) { x.Transition = "explode" },
			"no deployment":     func(x *HibernationTransition) { x.DeploymentUID = "" },
			"no rv":             func(x *HibernationTransition) { x.ResourceVersion = "" },
			"no pod":            func(x *HibernationTransition) { x.PodUID = "" },
			"bad pod":           func(x *HibernationTransition) { x.PodUID = "pod" },
			"refused no reason": func(x *HibernationTransition) { x.Transition = TransitionRefused; x.Reason = "" },
			"refused long reason": func(x *HibernationTransition) {
				x.Transition = TransitionRefused
				x.Reason = string(long)
			},
		}
		for name, m := range mut {
			x := good
			m(&x)
			var v *HibernationValidationError
			if _, err := f.apply(id, x); !errors.As(err, &v) {
				t.Fatalf("%s: err = %v, want a validation error", name, err)
			}
		}
		if row := f.row(id); row.HibernationScalingDownAt.Valid || row.HibernationState != "preparing" {
			t.Fatalf("a validation failure changed the row: %+v", row)
		}
		// ready needs pod and rv; scaled_down needs neither.
		if _, err := f.apply(id, HibernationTransition{OpID: f.op(id), Transition: TransitionScaledDown, DeploymentUID: f.deploy}); err != nil {
			t.Fatalf("scaled_down with only the required fields: %v", err)
		}
	})

	t.Run("refused with no scaling_down restores none", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		f.hibernate(id)
		f.mustApply(id, TransitionRefused, pod)
		row := f.row(id)
		if row.HibernationState != "none" || row.HibernationReason.String != "roll in flight" {
			t.Fatalf("after refused: %q reason=%q", row.HibernationState, row.HibernationReason.String)
		}
	})

	t.Run("refused after scaling_down goes to waking and keeps the fence", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		f.hibernate(id)
		opBefore := f.op(id)
		f.mustApply(id, TransitionScalingDown, pod)
		f.mustApply(id, TransitionScalingDown, pod) // idempotent
		f.mustApply(id, TransitionRefused, pod)
		row := f.row(id)
		if row.HibernationState != "waking" || f.op(id) == opBefore || !row.HibernationScalingDownAt.Valid ||
			row.HibernationAdmissionPodUid.Bytes != pod || !row.WakeRequestedAt.Valid {
			t.Fatalf("after late refusal: %+v", row)
		}
		f.assertNoClaim(id)
	})

	t.Run("deadline moves preparing to waking", func(t *testing.T) {
		f := newHibFx(t)
		id, _ := f.worker()
		f.hibernate(id)
		fresh, _ := f.worker()
		f.hibernate(fresh)
		f.env.exec(`UPDATE workers SET hibernation_requested_at = now() - interval '11 minutes' WHERE id = $1`, id)
		if _, err := f.svc.HibernationSettlePass(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		f.want(id, "waking")
		f.want(fresh, "preparing")
		if got := f.row(id).HibernationReason.String; got != "preparation deadline" {
			t.Fatalf("reason = %q", got)
		}
	})

	t.Run("claims are fenced in every non-none state", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		f.hibernate(id)
		f.assertNoClaim(id)
		f.mustApply(id, TransitionScalingDown, pod)
		f.mustApply(id, TransitionScaledDown, uuid.Nil)
		f.assertNoClaim(id)
		if _, err := f.svc.WakeWorker(f.env.ctx, nil, id, "demand"); err != nil {
			t.Fatal(err)
		}
		f.assertNoClaim(id)
	})
}

// assertNoClaim seeds a claimable run and proves the worker claims nothing.
func (f *hibFx) assertNoClaim(id uuid.UUID) {
	f.t.Helper()
	if !f.assembled {
		enableClaimAssembly(f.t, f.env, f.user)
		f.assembled = true
	}
	run := f.queuedRun()
	defer f.env.exec(`DELETE FROM runs WHERE id = $1`, run)
	payload, err := f.svc.Claim(f.env.ctx, f.row(id), nil)
	if err != nil || payload != nil {
		f.t.Fatalf("Claim in state %q = %v, %v; want idle", f.state(id), payload, err)
	}
}

func TestHibernationCyclesLiveDB(t *testing.T) {
	t.Run("two full cycles, then hibernate-wake before scaling_down settles on the same pod", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		pod = f.fullCycle(id, pod)
		pod = f.fullCycle(id, pod)

		f.hibernate(id)
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, "changed my mind"); err != nil {
			t.Fatal(err)
		}
		f.want(id, "waking")
		f.heartbeatNow(id)
		f.mustApply(id, TransitionReady, pod)
		f.want(id, "none")
		if f.events(id, "hibernate") != 3 || f.events(id, "wake") != 3 {
			t.Fatalf("events hibernate=%d wake=%d, want 3/3", f.events(id, "hibernate"), f.events(id, "wake"))
		}
	})

	t.Run("same pod with a stale heartbeat does not settle; a fresh heartbeat on the next pass does", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		f.hibernate(id)
		if _, err := f.svc.WakeWorker(f.env.ctx, nil, id, "demand"); err != nil {
			t.Fatal(err)
		}
		f.env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '5 minutes' WHERE id = $1`, id)
		f.mustApply(id, TransitionReady, pod)
		f.want(id, "waking")
		f.heartbeatNow(id)
		if n, err := f.svc.HibernationSettlePass(f.env.ctx); err != nil || n < 1 {
			t.Fatalf("pass = %d, %v", n, err)
		}
		f.want(id, "none")
	})

	t.Run("after a scale-down the original pod never settles, however fresh the heartbeat", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		f.hibernate(id)
		f.mustApply(id, TransitionScalingDown, pod)
		f.mustApply(id, TransitionScaledDown, uuid.Nil)
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); err != nil {
			t.Fatal(err)
		}
		f.heartbeatNow(id)
		f.mustApply(id, TransitionReady, pod)
		f.register(id, &pod)
		f.want(id, "waking")
	})

	t.Run("settle in both orders", func(t *testing.T) {
		for _, registerFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("registerFirst=%v", registerFirst), func(t *testing.T) {
				f := newHibFx(t)
				id, pod := f.worker()
				f.hibernate(id)
				f.mustApply(id, TransitionScalingDown, pod)
				f.mustApply(id, TransitionScaledDown, uuid.Nil)
				if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); err != nil {
					t.Fatal(err)
				}
				next := uuid.New()
				f.env.exec(`UPDATE workers SET registered_pod_uid = $2 WHERE id = $1`, id, next)
				if registerFirst {
					f.register(id, &next)
					f.want(id, "waking")
					f.mustApply(id, TransitionReady, next)
				} else {
					f.mustApply(id, TransitionReady, next)
					f.want(id, "waking")
					f.register(id, &next)
				}
				f.want(id, "none")
			})
		}
	})

	t.Run("a replacement pod is required: register by a different pod does not settle", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		f.hibernate(id)
		f.mustApply(id, TransitionScalingDown, pod)
		f.mustApply(id, TransitionScaledDown, uuid.Nil)
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); err != nil {
			t.Fatal(err)
		}
		ready, other := uuid.New(), uuid.New()
		f.mustApply(id, TransitionReady, ready)
		f.register(id, &other)
		f.want(id, "waking")
	})

	t.Run("a stale ready for an older op cannot settle a newer wake", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		f.hibernate(id)
		f.mustApply(id, TransitionScalingDown, pod)
		f.mustApply(id, TransitionScaledDown, uuid.Nil)
		if _, err := f.svc.WakeWorker(f.env.ctx, nil, id, ""); err != nil {
			t.Fatal(err)
		}
		oldReady := f.tr(id, TransitionReady, uuid.New())
		oldReady.OpID = uuid.NewString()
		if _, err := f.apply(id, oldReady); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("old-op ready: %v", err)
		}
	})

	t.Run("register without pod identity records an error and does not settle", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		f.hibernate(id)
		f.mustApply(id, TransitionScalingDown, pod)
		f.mustApply(id, TransitionScaledDown, uuid.Nil)
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); err != nil {
			t.Fatal(err)
		}
		f.mustApply(id, TransitionReady, uuid.New())
		f.register(id, nil)
		row := f.row(id)
		if row.HibernationState != "waking" || row.WakeError.String != "worker registered without pod identity" {
			t.Fatalf("row = %q err=%q", row.HibernationState, row.WakeError.String)
		}
	})

	t.Run("wake of an awake worker conflicts; waking to waking keeps the op", func(t *testing.T) {
		f := newHibFx(t)
		id, _ := f.worker()
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("wake awake: %v", err)
		}
		f.hibernate(id)
		f.set.on = false // a wake is not gated by the setting
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); err != nil {
			t.Fatal(err)
		}
		op := f.op(id)
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); err != nil {
			t.Fatal(err)
		}
		if f.op(id) != op || f.events(id, "wake") != 1 {
			t.Fatalf("second wake minted a new op or event: wake events %d", f.events(id, "wake"))
		}
	})
}

func TestHibernationWakeTimeoutAndPacingLiveDB(t *testing.T) {
	t.Run("a manual wake older than 15m gets wake_error and settling clears it", func(t *testing.T) {
		f := newHibFx(t)
		id, pod := f.worker()
		f.hibernate(id)
		f.mustApply(id, TransitionScalingDown, pod)
		f.mustApply(id, TransitionScaledDown, uuid.Nil)
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); err != nil {
			t.Fatal(err)
		}
		fresh, _ := f.worker()
		f.hibernate(fresh)
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, fresh, ""); err != nil {
			t.Fatal(err)
		}
		f.env.exec(`UPDATE workers SET wake_requested_at = now() - interval '16 minutes' WHERE id = $1`, id)
		if _, err := f.svc.HibernationSettlePass(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		if got := f.row(id).WakeError.String; got != "wake not confirmed within 15m" {
			t.Fatalf("wake_error = %q", got)
		}
		if f.row(fresh).WakeError.Valid {
			t.Fatal("a fresh wake was flagged")
		}
		next := uuid.New()
		f.env.exec(`UPDATE workers SET registered_pod_uid = $2 WHERE id = $1`, id, next)
		f.mustApply(id, TransitionReady, next)
		f.register(id, &next)
		if row := f.row(id); row.HibernationState != "none" || row.WakeError.Valid {
			t.Fatalf("after settle: %q err=%q", row.HibernationState, row.WakeError.String)
		}
	})

	t.Run("disabling the setting wakes exactly 10 of 12 hibernated workers per pass", func(t *testing.T) {
		f := newHibFx(t)
		// Other tests' leftovers must not be counted.
		f.env.exec(`UPDATE workers SET hibernation_state='none' WHERE hibernation_state <> 'none'`)
		var ids []uuid.UUID
		for i := 0; i < 12; i++ {
			id, pod := f.worker()
			f.hibernate(id)
			f.mustApply(id, TransitionScalingDown, pod)
			f.mustApply(id, TransitionScaledDown, uuid.Nil)
			f.env.exec(`UPDATE workers SET hibernated_at = now() - make_interval(mins => $2) WHERE id = $1`, id, 100-i)
			ids = append(ids, id)
		}
		if n, err := f.svc.HibernationSettlePass(f.env.ctx); err != nil || n != 0 {
			t.Fatalf("pass with the setting on = %d, %v; want 0", n, err)
		}
		f.set.on = false
		n, err := f.svc.HibernationSettlePass(f.env.ctx)
		if err != nil || n != 10 {
			t.Fatalf("first disable pass = %d, %v; want 10", n, err)
		}
		waking := 0
		for i, id := range ids {
			if f.state(id) == "waking" {
				waking++
				if i >= 10 {
					t.Fatalf("worker %d (more recently hibernated) woke before an older one", i)
				}
			}
		}
		if waking != 10 {
			t.Fatalf("%d workers waking, want 10", waking)
		}
		if n, err := f.svc.HibernationSettlePass(f.env.ctx); err != nil || n != 2 {
			t.Fatalf("second disable pass = %d, %v; want 2", n, err)
		}
	})
}

// TestHibernateVersusClaimRaceLiveDB: admission and a claim race for the same worker row
// lock. Exactly one wins: either the run is claimed and the hibernate is refused busy, or
// the hibernate is admitted and the claim returns nothing. The database default isolation
// is forced to REPEATABLE READ to prove the service does not inherit it.
func TestHibernateVersusClaimRaceLiveDB(t *testing.T) {
	f := newHibFx(t)
	ctx := f.env.ctx
	var db string
	if err := f.env.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		t.Fatal(err)
	}
	if _, err := f.env.pool.Exec(ctx, fmt.Sprintf(`ALTER DATABASE %s SET default_transaction_isolation = 'repeatable read'`, pgx.Identifier{db}.Sanitize())); err != nil {
		t.Skipf("cannot change the database default isolation: %v", err)
	}
	t.Cleanup(func() {
		if _, err := f.env.pool.Exec(context.Background(), fmt.Sprintf(`ALTER DATABASE %s RESET default_transaction_isolation`, pgx.Identifier{db}.Sanitize())); err != nil {
			t.Errorf("restore the database default isolation: %v", err)
		}
	})
	pool, err := store.OpenPool(ctx, os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var iso string
	if err := pool.QueryRow(ctx, `SHOW default_transaction_isolation`).Scan(&iso); err != nil || iso != "repeatable read" {
		t.Fatalf("pool default isolation = %q (%v)", iso, err)
	}
	enableClaimAssembly(t, f.env, f.user)
	svc := New(store.New(pool), f.env.box, testParams())
	svc.SetTxBeginner(pool)
	svc.SetVault(f.env.vlt)
	svc.SetHibernationSettings(f.set)

	claimWins, hibernateWins := 0, 0
	for i := 0; i < 16; i++ {
		id, _ := f.worker()
		run := f.queuedRun()
		// Stagger the hibernate across the claim's own setup time so both orders occur.
		lag := time.Duration(i) * 500 * time.Microsecond
		wk := f.row(id)
		var wg sync.WaitGroup
		var payloadNil bool
		var claimErr, hibErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			p, e := svc.Claim(ctx, wk, nil)
			payloadNil, claimErr = p == nil, e
		}()
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(lag)
			_, hibErr = svc.HibernateWorker(ctx, f.actor, id, "")
		}()
		close(start)
		wg.Wait()
		if claimErr != nil {
			t.Fatalf("iteration %d: claim error: %v", i, claimErr)
		}
		switch {
		case !payloadNil && refusalCode(hibErr) == RefusalBusy:
			claimWins++
			if f.state(id) != "none" {
				t.Fatalf("iteration %d: claim won but state = %q", i, f.state(id))
			}
		case payloadNil && hibErr == nil:
			hibernateWins++
			if f.state(id) != "preparing" {
				t.Fatalf("iteration %d: hibernate won but state = %q", i, f.state(id))
			}
			if got := statusOf(t, f.env, run); got != "queued" {
				t.Fatalf("iteration %d: run status %q after a fenced claim", i, got)
			}
		default:
			t.Fatalf("iteration %d: claim idle=%v hibernate err=%v: both or neither won", i, payloadNil, hibErr)
		}
		f.env.exec(`UPDATE runs SET status='cancelled', worker_id=NULL WHERE id=$1`, run)
	}
	t.Logf("claim wins=%d hibernate wins=%d", claimWins, hibernateWins)
}

// A pod that Registers while the worker is still preparing replaces the admission pod;
// the wake that follows (no scaling_down was ever recorded) must settle on that live pod.
func TestHibernationPreparingRegisterTracksLivePodLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, p1 := f.worker()
	f.hibernate(id)
	f.want(id, "preparing")
	if got := f.row(id).HibernationAdmissionPodUid; got.Bytes != p1 {
		t.Fatalf("admission pod = %v, want %v", got, p1)
	}
	p2 := uuid.New()
	f.register(id, &p2)
	f.want(id, "preparing")
	if got := f.row(id).HibernationAdmissionPodUid; !got.Valid || got.Bytes != p2 {
		t.Fatalf("admission pod after a preparing register = %v, want %v", got, p2)
	}
	if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, "deadline"); err != nil {
		t.Fatalf("wake: %v", err)
	}
	f.want(id, "waking")
	f.heartbeatNow(id)
	f.mustApply(id, TransitionReady, p2)
	f.want(id, "none")
}

func TestHibernationFreeTextIsSanitizedLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, pod := f.worker()
	dirty := "ro\x1b[31mll\x1b[0m‮back"
	f.actor.Label = "ad\x1b[31mmin‮"
	if _, err := f.svc.HibernateWorker(f.env.ctx, f.actor, id, dirty); err != nil {
		t.Fatal(err)
	}
	var reason, label string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT reason, actor_label FROM worker_hibernation_events WHERE worker_id=$1 AND action='hibernate'`, id).Scan(&reason, &label); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{reason, label} {
		if strings.ContainsAny(v, "\x1b‮") {
			t.Fatalf("control or bidi characters survived: %q", v)
		}
	}
	// A refused reason that sanitizes to nothing is a validation error (HTTP 400).
	bad := f.tr(id, TransitionRefused, pod)
	bad.Reason = "\x1b[31m‮"
	var verr *HibernationValidationError
	if _, err := f.apply(id, bad); !errors.As(err, &verr) {
		t.Fatalf("empty-after-sanitize refused reason: %v", err)
	}
	ok := f.tr(id, TransitionRefused, pod)
	ok.Reason = "ro\x1b[31mll‮"
	w, err := f.apply(id, ok)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(w.HibernationReason.String, "\x1b‮") {
		t.Fatalf("refused reason not sanitized: %q", w.HibernationReason.String)
	}
}
