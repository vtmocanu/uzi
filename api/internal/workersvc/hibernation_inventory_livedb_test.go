package workersvc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4208 M1: the volume inventory over a real database. Every case starts from ONE shared happy
// path and flips exactly one fact, so a red test names the single fact that mattered. Skipped
// without UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh).

const (
	invNonce = "reg-nonce-1"
	invRV    = "rv-12340"
	invFence = int64(7)
)

type invFx struct {
	*hibFx
	id    uuid.UUID
	pod   uuid.UUID
	other uuid.UUID
	uids  map[string]string // pvc role -> uid
}

// newInvFx seeds a capable persistent hosted worker that is preparing to hibernate.
func newInvFx(t *testing.T) *invFx {
	t.Helper()
	f := &invFx{hibFx: newHibFx(t), other: uuid.New(),
		uids: map[string]string{"data": "pvc-data-1", "nix": "pvc-nix-1", "dind_data": "pvc-dind-1"}}
	f.id, f.pod = f.worker()
	f.env.exec(`UPDATE workers SET protocol_capabilities = ARRAY[$2]::text[], snapshot_register_nonce = $3 WHERE id = $1`,
		f.id, capability.HibernationInventoryV1, invNonce)
	f.hibernate(f.id)
	f.want(f.id, "preparing")
	return f
}

func (f *invFx) pvcs() []HibernationPVC {
	var out []HibernationPVC
	for _, role := range []string{"data", "nix", "dind_data"} {
		out = append(out, HibernationPVC{Role: role, Name: "hw-" + role, UID: f.uids[role], ClassReclaimPolicy: "Retain"})
	}
	return out
}

func podSet(ids ...uuid.UUID) *[]string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return &out
}

// observedT is the happy pre-mutation observation: exactly the bound pod, nothing else on the volumes.
func (f *invFx) observedT(mod ...func(*HibernationTransition)) HibernationTransition {
	t := HibernationTransition{OpID: f.op(f.id), Transition: TransitionObserved, DeploymentUID: f.deploy, ResourceVersion: invRV,
		PodUID: f.pod.String(), PVCs: f.pvcs(), PodUIDs: podSet(f.pod), PVCUserPods: podSet(f.pod)}
	for _, m := range mod {
		m(&t)
	}
	return t
}

func (f *invFx) scalingDownT(mod ...func(*HibernationTransition)) HibernationTransition {
	fence, gen, replicas := invFence, invFence, int32(0)
	t := HibernationTransition{OpID: f.op(f.id), Transition: TransitionScalingDown, DeploymentUID: f.deploy, ResourceVersion: "rv-sd",
		PodUID: f.pod.String(), PodUIDs: podSet(f.pod), PVCUserPods: podSet(f.pod),
		DeploymentGeneration: &gen, DeploymentReplicas: &replicas, FenceGeneration: &fence}
	for _, m := range mod {
		m(&t)
	}
	return t
}

// afterExitT is scaled_down or held: the pod is gone, the deployment holds its fence.
func (f *invFx) afterExitT(name string, mod ...func(*HibernationTransition)) HibernationTransition {
	gen, replicas := invFence, int32(0)
	t := HibernationTransition{OpID: f.op(f.id), Transition: name, DeploymentUID: f.deploy,
		PodUIDs: podSet(), PVCUserPods: podSet(), DeploymentGeneration: &gen, DeploymentReplicas: &replicas}
	for _, m := range mod {
		m(&t)
	}
	return t
}

func (f *invFx) report(mod ...func(*HibernationInventoryReport)) HibernationInventoryReport {
	var refs []HibernationPVCRef
	for _, p := range f.pvcs() {
		refs = append(refs, HibernationPVCRef{Role: p.Role, Name: p.Name, UID: p.UID})
	}
	op, _ := uuid.Parse(f.op(f.id))
	r := HibernationInventoryReport{OpID: op, PodUID: f.pod, RegisterNonce: invNonce, DeploymentResourceVersion: invRV,
		PVCs: sortedPVCRefs(refs), Complete: true,
		Entries:     []byte(`[{"kind":"bare_repository","path":"/data/runner/g-r/.bare","classification":"protected"}]`),
		ReceiptKeys: []byte(`[]`), Digest: strings.Repeat("b", 64), Fingerprint: strings.Repeat("a", 64), Lane: "dind", Mechanism: "epoch_ex"}
	for _, m := range mod {
		m(&r)
	}
	return r
}

func (f *invFx) inv() store.WorkerHibernationInventory {
	f.t.Helper()
	op, _ := uuid.Parse(f.op(f.id))
	row, err := f.env.q.GetHibernationInventoryForOp(f.env.ctx, store.GetHibernationInventoryForOpParams{WorkerID: f.id, OpID: op})
	if err != nil {
		f.t.Fatalf("inventory row: %v", err)
	}
	return row
}

func (f *invFx) noInv() {
	f.t.Helper()
	op, _ := uuid.Parse(f.op(f.id))
	if _, err := f.env.q.GetHibernationInventoryForOp(f.env.ctx, store.GetHibernationInventoryForOpParams{WorkerID: f.id, OpID: op}); !errors.Is(err, pgx.ErrNoRows) {
		f.t.Fatalf("an inventory row exists: %v", err)
	}
}

func (f *invFx) apply2(t HibernationTransition) error {
	_, err := f.apply(f.id, t)
	return err
}

func (f *invFx) must(t HibernationTransition) {
	f.t.Helper()
	if err := f.apply2(t); err != nil {
		f.t.Fatalf("%s: %v", t.Transition, err)
	}
}

func (f *invFx) accept(r HibernationInventoryReport) error {
	return f.svc.AcceptHibernationInventory(f.env.ctx, f.id, r)
}

// observed runs the happy observation and leaves an awaiting row.
func (f *invFx) observed() {
	f.t.Helper()
	f.must(f.observedT())
	if got := f.inv().Status; got != InventoryAwaiting {
		f.t.Fatalf("status after observed = %q", got)
	}
}

// accepted is the shared happy start: observed, then the worker's inventory bound.
func (f *invFx) accepted() {
	f.t.Helper()
	f.observed()
	if err := f.accept(f.report()); err != nil {
		f.t.Fatalf("accept: %v", err)
	}
	if got := f.inv().Status; got != InventoryAccepted {
		f.t.Fatalf("status after accept = %q", got)
	}
}

// hibernatedHeld extends accepted to the end of the chain: scaling_down, scaled_down.
func (f *invFx) hibernatedHeld() {
	f.t.Helper()
	f.accepted()
	f.must(f.scalingDownT())
	f.must(f.afterExitT(TransitionScaledDown))
	f.want(f.id, "hibernated")
}

func (f *invFx) wantValid() {
	f.t.Helper()
	if r := f.inv(); r.InvalidatedReason.Valid || r.InvalidatedAt.Valid {
		f.t.Fatalf("inventory invalidated (%q), want valid", r.InvalidatedReason.String)
	}
}

func (f *invFx) wantInvalid(reason string) {
	f.t.Helper()
	r := f.inv()
	if !r.InvalidatedReason.Valid || r.InvalidatedReason.String != reason || !r.InvalidatedAt.Valid {
		f.t.Fatalf("invalidated = %+v, want %q", r.InvalidatedReason, reason)
	}
}

func (f *invFx) heartbeat(excl *HibernationExclusion) *HibernationInventoryMember {
	f.t.Helper()
	m, err := f.svc.HibernationHeartbeat(f.env.ctx, f.id, excl)
	if err != nil {
		f.t.Fatalf("HibernationHeartbeat: %v", err)
	}
	return m
}

func TestHibernationInventoryOrderingLiveDB(t *testing.T) {
	f := newInvFx(t)
	if m := f.heartbeat(nil); m != nil {
		t.Fatalf("a member before any observation: %+v", m)
	}
	f.observed()
	f.wantValid()

	m := f.heartbeat(nil)
	if m == nil || m.Phase != InventoryAwaiting || m.OpID != f.op(f.id) || m.RegisterNonce != invNonce ||
		m.PodUID != f.pod.String() || m.DeploymentResourceVersion != invRV || len(m.PVCs) != 3 {
		t.Fatalf("awaiting member = %+v", m)
	}
	for _, p := range m.PVCs {
		if f.uids[p.Role] != p.UID {
			t.Fatalf("member pvc %+v", p)
		}
	}

	if err := f.accept(f.report()); err != nil {
		t.Fatalf("accept: %v", err)
	}
	row := f.inv()
	if row.Status != InventoryAccepted || !row.Digest.Valid || !row.Complete.Bool || row.RegisterNonce.String != invNonce ||
		row.Lane.String != "dind" || !row.ReceivedAt.Valid {
		t.Fatalf("accepted row = %+v", row)
	}
	if m := f.heartbeat(nil); m == nil || m.Phase != InventoryAccepted {
		t.Fatalf("accepted member = %+v", m)
	}

	f.must(f.scalingDownT())
	row = f.inv()
	if !row.FenceGeneration.Valid || row.FenceGeneration.Int64 != invFence || row.ScalingDownPodUid.Bytes != f.pod || !row.LastHeldAt.Valid {
		t.Fatalf("scale-down not recorded: %+v", row)
	}
	f.must(f.afterExitT(TransitionScaledDown))
	f.want(f.id, "hibernated")
	if !f.inv().ScaledDownAt.Valid {
		t.Fatal("scaled_down_at not recorded")
	}
	if m := f.heartbeat(nil); m != nil {
		t.Fatalf("a member after the worker left preparing: %+v", m)
	}
	f.must(f.afterExitT(TransitionHeld))
	f.wantValid()
	if got := f.inv().Status; got != InventoryAccepted {
		t.Fatalf("status at the end of the chain = %q", got)
	}
	// An idempotent replay of the whole chain stays valid.
	f.must(f.afterExitT(TransitionScaledDown))
	f.must(f.scalingDownT())
	f.must(f.afterExitT(TransitionHeld))
	f.wantValid()
}

func TestHibernationInventoryAcceptRefusedLiveDB(t *testing.T) {
	for name, mod := range map[string]func(r *HibernationInventoryReport){
		"wrong pod uid":    func(r *HibernationInventoryReport) { r.PodUID = uuid.New() },
		"wrong pvc uid":    func(r *HibernationInventoryReport) { r.PVCs[1].UID = "someone-else" },
		"wrong pvc name":   func(r *HibernationInventoryReport) { r.PVCs[0].Name = "renamed" },
		"missing pvc":      func(r *HibernationInventoryReport) { r.PVCs = r.PVCs[:2] },
		"wrong nonce":      func(r *HibernationInventoryReport) { r.RegisterNonce = "stale-nonce" },
		"wrong op":         func(r *HibernationInventoryReport) { r.OpID = uuid.New() },
		"wrong rv":         func(r *HibernationInventoryReport) { r.DeploymentResourceVersion = "rv-other" },
		"too long to bind": func(r *HibernationInventoryReport) { r.Digest = strings.Repeat("c", 200) }, // 23514, not a conflict
	} {
		t.Run(name, func(t *testing.T) {
			f := newInvFx(t)
			f.observed()
			err := f.accept(f.report(mod))
			if name == "too long to bind" {
				if err == nil || errors.Is(err, ErrHibernationConflict) {
					t.Fatalf("err = %v, want a check violation", err)
				}
			} else if !errors.Is(err, ErrHibernationConflict) {
				t.Fatalf("err = %v, want conflict", err)
			}
			row := f.inv()
			if row.Status != InventoryAwaiting || row.Entries != nil || row.Digest.Valid || row.ReceivedAt.Valid {
				t.Fatalf("a refused accept persisted something: %+v", row)
			}
			f.wantValid()
		})
	}
}

func TestHibernationInventoryDelayedAcceptLiveDB(t *testing.T) {
	t.Run("after timed_out", func(t *testing.T) {
		f := newInvFx(t)
		f.observed()
		f.env.exec(`UPDATE worker_hibernation_inventories SET observed_at = now() - interval '4 minutes' WHERE worker_id = $1`, f.id)
		if _, err := f.svc.HibernationSettlePass(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		if got := f.inv().Status; got != InventoryTimedOut {
			t.Fatalf("status = %q, want timed_out", got)
		}
		if err := f.accept(f.report()); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("late accept: %v", err)
		}
		if got := f.inv().Status; got != InventoryTimedOut {
			t.Fatalf("late accept moved the row to %q", got)
		}
	})
	t.Run("fresh awaiting is not timed out", func(t *testing.T) {
		f := newInvFx(t)
		f.observed()
		if _, err := f.svc.HibernationSettlePass(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		if got := f.inv().Status; got != InventoryAwaiting {
			t.Fatalf("status = %q", got)
		}
	})
	t.Run("after scaling_down", func(t *testing.T) {
		f := newInvFx(t)
		f.observed()
		f.must(f.scalingDownT())
		if got := f.inv().Status; got != InventoryClosedByScaleDown {
			t.Fatalf("status = %q, want closed_by_scale_down", got)
		}
		if err := f.accept(f.report()); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("late accept: %v", err)
		}
		row := f.inv()
		if row.Status != InventoryClosedByScaleDown || row.Entries != nil {
			t.Fatalf("late accept persisted: %+v", row)
		}
	})
	t.Run("status report after a settled row", func(t *testing.T) {
		f := newInvFx(t)
		f.accepted()
		err := f.svc.ReportHibernationInventoryStatus(f.env.ctx, f.id, HibernationInventoryStatusReport{
			OpID: uuid.MustParse(f.op(f.id)), PodUID: f.pod, RegisterNonce: invNonce, Status: InventoryBusy})
		if !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("status after accept: %v", err)
		}
	})
}

func TestHibernationInventoryStatusReportLiveDB(t *testing.T) {
	f := newInvFx(t)
	f.observed()
	rep := HibernationInventoryStatusReport{OpID: uuid.MustParse(f.op(f.id)), PodUID: f.pod, RegisterNonce: invNonce,
		Status: InventoryExclusionFailed, Reason: "could not lock"}
	bad := rep
	bad.RegisterNonce = "nope"
	if err := f.svc.ReportHibernationInventoryStatus(f.env.ctx, f.id, bad); !errors.Is(err, ErrHibernationConflict) {
		t.Fatalf("wrong nonce: %v", err)
	}
	if err := f.svc.ReportHibernationInventoryStatus(f.env.ctx, f.id, rep); err != nil {
		t.Fatal(err)
	}
	row := f.inv()
	if row.Status != InventoryExclusionFailed || row.StatusReason.String != "could not lock" {
		t.Fatalf("row = %+v", row)
	}
}

func TestHibernationInventoryNoCapabilityLiveDB(t *testing.T) {
	f := newInvFx(t)
	f.env.exec(`UPDATE workers SET protocol_capabilities = '{}' WHERE id = $1`, f.id)
	f.must(f.observedT())
	if got := f.inv().Status; got != InventoryNoCapability {
		t.Fatalf("status = %q, want no_capability", got)
	}
	if m := f.heartbeat(nil); m != nil {
		t.Fatalf("a member for a worker without the capability: %+v", m)
	}
	if err := f.accept(f.report()); !errors.Is(err, ErrHibernationConflict) {
		t.Fatalf("accept: %v", err)
	}
}

func TestHibernationInventoryObservedRulesLiveDB(t *testing.T) {
	t.Run("only while preparing and before scale-down", func(t *testing.T) {
		f := newInvFx(t)
		f.must(f.scalingDownT())
		if err := f.apply2(f.observedT()); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("observed after scaling_down: %v", err)
		}
		f.must(f.afterExitT(TransitionScaledDown))
		if err := f.apply2(f.observedT()); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("observed when hibernated: %v", err)
		}
	})
	t.Run("stale op", func(t *testing.T) {
		f := newInvFx(t)
		if err := f.apply2(f.observedT(func(tr *HibernationTransition) { tr.OpID = uuid.NewString() })); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("stale op: %v", err)
		}
		f.noInv()
	})
	t.Run("the same identity again does not reset", func(t *testing.T) {
		f := newInvFx(t)
		f.accepted()
		f.must(f.observedT())
		if got := f.inv().Status; got != InventoryAccepted {
			t.Fatalf("a same-identity re-send reset the row to %q", got)
		}
		f.wantValid()
	})
	t.Run("a different identity restarts an awaiting row", func(t *testing.T) {
		f := newInvFx(t)
		f.observed()
		f.uids["data"] = "pvc-data-2"
		f.must(f.observedT(func(tr *HibernationTransition) { tr.ResourceVersion = "rv-new" }))
		row := f.inv()
		if row.Status != InventoryAwaiting || row.DeploymentResourceVersion != "rv-new" {
			t.Fatalf("row = %+v", row)
		}
	})
	t.Run("a different identity after accept leaves identity_changed", func(t *testing.T) {
		f := newInvFx(t)
		f.accepted()
		f.uids["nix"] = "pvc-nix-2"
		f.must(f.observedT())
		row := f.inv()
		if row.Status != InventoryIdentityChanged || row.Entries != nil || row.Digest.Valid {
			t.Fatalf("row = %+v", row)
		}
		if err := f.accept(f.report()); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("accept after identity change: %v", err)
		}
	})
	t.Run("a new op replaces the old op's row", func(t *testing.T) {
		f := newInvFx(t)
		f.accepted()
		firstOp := f.op(f.id)
		if _, err := f.svc.WakeWorker(f.env.ctx, nil, f.id, "again"); err != nil {
			t.Fatal(err)
		}
		f.env.exec(`UPDATE workers SET hibernation_state='none', hibernation_op_id=NULL WHERE id=$1`, f.id)
		f.hibernate(f.id)
		if f.op(f.id) == firstOp {
			t.Fatal("op did not change")
		}
		f.noInv()
		f.observed()
		if got := f.inv().Status; got != InventoryAwaiting {
			t.Fatalf("new op's row = %q", got)
		}
	})
}

func TestHibernationInventoryInvalidatorsLiveDB(t *testing.T) {
	seven := invFence
	one := int32(1)
	cases := []struct {
		name   string
		reason string
		run    func(f *invFx)
	}{
		{"other pod in observed", InvalidatedOtherPod, func(f *invFx) {
			f.must(f.observedT(func(tr *HibernationTransition) { tr.PodUIDs = podSet(f.pod, f.other) }))
		}},
		{"pods not exactly the bound pod in observed", InvalidatedOtherPod, func(f *invFx) {
			f.must(f.observedT(func(tr *HibernationTransition) { tr.PodUIDs = podSet() }))
		}},
		{"foreign pvc user pod in observed", InvalidatedOtherPod, func(f *invFx) {
			f.must(f.observedT(func(tr *HibernationTransition) { tr.PVCUserPods = podSet(f.other) }))
		}},
		{"pod set unproven in observed", InvalidatedPodSetUnproven, func(f *invFx) {
			f.must(f.observedT(func(tr *HibernationTransition) { tr.PodUIDs = nil }))
		}},
		{"pvc user pods unproven in observed", InvalidatedPodSetUnproven, func(f *invFx) {
			f.must(f.observedT(func(tr *HibernationTransition) { tr.PVCUserPods = nil }))
		}},
		{"extra pod in scaling_down", InvalidatedOtherPod, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT(func(tr *HibernationTransition) { tr.PodUIDs = podSet(f.pod, f.other) }))
		}},
		{"scaling_down names another pod", InvalidatedOtherPod, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT(func(tr *HibernationTransition) { tr.PodUID = f.other.String() }))
		}},
		{"foreign pvc user pod in scaling_down", InvalidatedOtherPod, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT(func(tr *HibernationTransition) { tr.PVCUserPods = podSet(f.other) }))
		}},
		{"pod set unproven in scaling_down", InvalidatedPodSetUnproven, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT(func(tr *HibernationTransition) { tr.PodUIDs = nil }))
		}},
		{"pvc user pod after exit", InvalidatedOtherPod, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT())
			f.must(f.afterExitT(TransitionScaledDown, func(tr *HibernationTransition) { tr.PVCUserPods = podSet(f.other) }))
		}},
		{"pod still present after exit", InvalidatedOtherPod, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT())
			f.must(f.afterExitT(TransitionScaledDown, func(tr *HibernationTransition) { tr.PodUIDs = podSet(f.pod) }))
		}},
		{"pod set absent on scaled_down", InvalidatedPodSetUnproven, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT())
			f.must(f.afterExitT(TransitionScaledDown, func(tr *HibernationTransition) { tr.PodUIDs = nil }))
		}},
		{"pvc user pods absent on held", InvalidatedPodSetUnproven, func(f *invFx) {
			f.hibernatedHeld()
			f.must(f.afterExitT(TransitionHeld, func(tr *HibernationTransition) { tr.PVCUserPods = nil }))
		}},
		{"generation differs from the fence on scaling_down", InvalidatedDeploymentChanged, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT(func(tr *HibernationTransition) { g := seven + 1; tr.DeploymentGeneration = &g }))
		}},
		{"re-sent scaling_down without a generation", InvalidatedDeploymentChanged, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT())
			f.must(f.scalingDownT(func(tr *HibernationTransition) { tr.DeploymentGeneration = nil }))
		}},
		{"re-sent scaling_down with another fence", InvalidatedDeploymentChanged, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT())
			f.must(f.scalingDownT(func(tr *HibernationTransition) { g := seven + 1; tr.FenceGeneration = &g; tr.DeploymentGeneration = &g }))
		}},
		{"replicas not zero on scaling_down", InvalidatedDeploymentChanged, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT(func(tr *HibernationTransition) { tr.DeploymentReplicas = &one }))
		}},
		{"replicas not zero on scaled_down", InvalidatedDeploymentChanged, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT())
			f.must(f.afterExitT(TransitionScaledDown, func(tr *HibernationTransition) { tr.DeploymentReplicas = &one }))
		}},
		{"generation moved before scaled_down", InvalidatedDeploymentChanged, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT())
			f.must(f.afterExitT(TransitionScaledDown, func(tr *HibernationTransition) { g := seven + 1; tr.DeploymentGeneration = &g }))
		}},
		{"generation moved while held", InvalidatedDeploymentChanged, func(f *invFx) {
			f.hibernatedHeld()
			f.must(f.afterExitT(TransitionHeld, func(tr *HibernationTransition) { g := seven + 5; tr.DeploymentGeneration = &g }))
		}},
		{"scaled_down without a fence", InvalidatedScaleDownPodUnproven, func(f *invFx) {
			f.accepted()
			f.must(f.afterExitT(TransitionScaledDown))
		}},
		{"scaled_down after a scaling_down without a fence", InvalidatedScaleDownPodUnproven, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT(func(tr *HibernationTransition) { tr.FenceGeneration = nil; tr.DeploymentGeneration = nil }))
			f.must(f.afterExitT(TransitionScaledDown))
		}},
		{"observation gap before scaled_down", InvalidatedObservationGap, func(f *invFx) {
			f.accepted()
			f.must(f.scalingDownT())
			f.env.exec(`UPDATE worker_hibernation_inventories SET last_held_at = now() - interval '1 hour' WHERE worker_id = $1`, f.id)
			f.must(f.afterExitT(TransitionScaledDown))
		}},
		{"observation gap before held", InvalidatedObservationGap, func(f *invFx) {
			f.hibernatedHeld()
			f.env.exec(`UPDATE worker_hibernation_inventories SET last_held_at = now() - interval '1 hour' WHERE worker_id = $1`, f.id)
			f.must(f.afterExitT(TransitionHeld))
		}},
		{"register from another pod", InvalidatedOtherPod, func(f *invFx) {
			f.accepted()
			f.register(f.id, &f.other)
		}},
		{"register from another pod while awaiting", InvalidatedOtherPod, func(f *invFx) {
			f.observed()
			f.register(f.id, &f.other)
		}},
		{"register from another pod after hibernation", InvalidatedOtherPod, func(f *invFx) {
			f.hibernatedHeld()
			f.register(f.id, &f.other)
		}},
		{"process restarted", InvalidatedProcessRestarted, func(f *invFx) {
			f.accepted()
			f.register(f.id, &f.pod)
		}},
		{"exclusion lost on heartbeat", InvalidatedExclusionLost, func(f *invFx) {
			f.accepted()
			f.heartbeat(&HibernationExclusion{OpID: uuid.MustParse(f.op(f.id)), Held: false})
		}},
		{"exclusion lost while awaiting", InvalidatedExclusionLost, func(f *invFx) {
			f.observed()
			f.heartbeat(&HibernationExclusion{OpID: uuid.MustParse(f.op(f.id)), Held: false})
		}},
		{"exclusion lost after hibernation", InvalidatedExclusionLost, func(f *invFx) {
			f.hibernatedHeld()
			f.heartbeat(&HibernationExclusion{OpID: uuid.MustParse(f.op(f.id)), Held: false})
		}},
		{"written after inventory (digest)", InvalidatedWrittenAfterInventory, func(f *invFx) {
			f.accepted()
			f.exclusionFinal(func(r *HibernationExclusionFinal) { r.Digest = strings.Repeat("d", 64) })
		}},
		{"written after inventory (fingerprint)", InvalidatedWrittenAfterInventory, func(f *invFx) {
			f.accepted()
			f.exclusionFinal(func(r *HibernationExclusionFinal) { r.Fingerprint = strings.Repeat("e", 64) })
		}},
		{"exclusion released in the final report", InvalidatedExclusionLost, func(f *invFx) {
			f.accepted()
			f.exclusionFinal(func(r *HibernationExclusionFinal) { r.Held = false })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newInvFx(t)
			tc.run(f)
			f.wantInvalid(tc.reason)
		})
	}
}

func (f *invFx) exclusionFinal(mod ...func(*HibernationExclusionFinal)) {
	f.t.Helper()
	if err := f.svc.RecordHibernationExclusionFinal(f.env.ctx, f.id, f.finalReport(mod...)); err != nil {
		f.t.Fatalf("exclusion final: %v", err)
	}
}

func (f *invFx) finalReport(mod ...func(*HibernationExclusionFinal)) HibernationExclusionFinal {
	r := HibernationExclusionFinal{OpID: uuid.MustParse(f.op(f.id)), PodUID: f.pod, RegisterNonce: invNonce, Held: true,
		Digest: strings.Repeat("b", 64), Fingerprint: strings.Repeat("a", 64)}
	for _, m := range mod {
		m(&r)
	}
	return r
}

func TestHibernationInventoryMonotonicLiveDB(t *testing.T) {
	f := newInvFx(t)
	f.accepted()
	f.must(f.scalingDownT(func(tr *HibernationTransition) { tr.PodUIDs = podSet(f.pod, f.other) }))
	f.wantInvalid(InvalidatedOtherPod)
	first := f.inv().InvalidatedAt
	time.Sleep(10 * time.Millisecond)
	f.must(f.afterExitT(TransitionScaledDown, func(tr *HibernationTransition) { tr.PodUIDs = nil }))
	f.heartbeat(&HibernationExclusion{OpID: uuid.MustParse(f.op(f.id)), Held: false})
	f.wantInvalid(InvalidatedOtherPod)
	if got := f.inv().InvalidatedAt; !got.Time.Equal(first.Time) {
		t.Fatalf("invalidated_at moved: %v -> %v", first.Time, got.Time)
	}
}

func TestHibernationInventoryExclusionFinalLiveDB(t *testing.T) {
	t.Run("recorded and valid when it matches", func(t *testing.T) {
		f := newInvFx(t)
		f.accepted()
		f.exclusionFinal()
		row := f.inv()
		if !row.ExclusionFinalAt.Valid || row.ExclusionFinalDigest.String != strings.Repeat("b", 64) {
			t.Fatalf("row = %+v", row)
		}
		f.wantValid()
		// Also legal once hibernated.
		f.must(f.scalingDownT())
		f.must(f.afterExitT(TransitionScaledDown))
		f.exclusionFinal()
	})
	for name, mod := range map[string]func(r *HibernationExclusionFinal){
		"wrong nonce": func(r *HibernationExclusionFinal) { r.RegisterNonce = "stale" },
		"wrong pod":   func(r *HibernationExclusionFinal) { r.PodUID = uuid.New() },
		"wrong op":    func(r *HibernationExclusionFinal) { r.OpID = uuid.New() },
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			f := newInvFx(t)
			f.accepted()
			if err := f.svc.RecordHibernationExclusionFinal(f.env.ctx, f.id, f.finalReport(mod)); !errors.Is(err, ErrHibernationConflict) {
				t.Fatalf("err = %v", err)
			}
			if f.inv().ExclusionFinalAt.Valid {
				t.Fatal("a refused final report was recorded")
			}
			f.wantValid()
		})
	}
	t.Run("needs an accepted inventory", func(t *testing.T) {
		f := newInvFx(t)
		f.observed()
		if err := f.svc.RecordHibernationExclusionFinal(f.env.ctx, f.id, f.finalReport()); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestHibernationHeldTransitionLiveDB(t *testing.T) {
	t.Run("only when hibernated", func(t *testing.T) {
		f := newInvFx(t)
		if err := f.apply2(f.afterExitT(TransitionHeld)); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("held while preparing: %v", err)
		}
	})
	t.Run("stale op", func(t *testing.T) {
		f := newInvFx(t)
		f.hibernatedHeld()
		if err := f.apply2(f.afterExitT(TransitionHeld, func(tr *HibernationTransition) { tr.OpID = uuid.NewString() })); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("stale op: %v", err)
		}
	})
	t.Run("without a row it is a no-op", func(t *testing.T) {
		f := newInvFx(t)
		f.must(f.scalingDownT())
		f.must(f.afterExitT(TransitionScaledDown))
		f.noInv()
		f.must(f.afterExitT(TransitionHeld))
		f.noInv()
		f.want(f.id, "hibernated")
	})
	t.Run("moves the stamp forward only", func(t *testing.T) {
		f := newInvFx(t)
		f.hibernatedHeld()
		before := f.inv().LastHeldAt.Time
		time.Sleep(10 * time.Millisecond)
		f.must(f.afterExitT(TransitionHeld))
		if after := f.inv().LastHeldAt.Time; !after.After(before) {
			t.Fatalf("last_held_at %v did not advance past %v", after, before)
		}
	})
}

// A worker that never reported an observation behaves exactly as before: the whole chain runs
// with no inventory row and nothing is created.
func TestHibernationTransitionsWithoutRowLiveDB(t *testing.T) {
	f := newInvFx(t)
	f.must(f.scalingDownT(func(tr *HibernationTransition) {
		tr.PodUIDs, tr.PVCUserPods, tr.DeploymentGeneration, tr.FenceGeneration = nil, nil, nil, nil
	}))
	f.must(f.afterExitT(TransitionScaledDown, func(tr *HibernationTransition) { tr.PodUIDs, tr.PVCUserPods = nil, nil }))
	f.want(f.id, "hibernated")
	f.noInv()
	var n int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM worker_hibernation_inventories WHERE worker_id = $1`, f.id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows = %d, %v", n, err)
	}
}

func TestHibernationHeartbeatMemberLiveDB(t *testing.T) {
	t.Run("an invalidated awaiting row is not offered", func(t *testing.T) {
		f := newInvFx(t)
		f.must(f.observedT(func(tr *HibernationTransition) { tr.PodUIDs = podSet(f.pod, f.other) }))
		if m := f.heartbeat(nil); m != nil {
			t.Fatalf("member for a doomed row: %+v", m)
		}
	})
	t.Run("a report for another op is ignored", func(t *testing.T) {
		f := newInvFx(t)
		f.accepted()
		f.heartbeat(&HibernationExclusion{OpID: uuid.New(), Held: false})
		f.wantValid()
	})
	t.Run("held=true changes nothing", func(t *testing.T) {
		f := newInvFx(t)
		f.accepted()
		f.heartbeat(&HibernationExclusion{OpID: uuid.MustParse(f.op(f.id)), Held: true})
		f.wantValid()
	})
	t.Run("the nonce is the worker's current one", func(t *testing.T) {
		f := newInvFx(t)
		f.observed()
		f.env.exec(`UPDATE workers SET snapshot_register_nonce = 'rotated' WHERE id = $1`, f.id)
		if m := f.heartbeat(nil); m == nil || m.RegisterNonce != "rotated" {
			t.Fatalf("member = %+v", m)
		}
		// The nonce the worker echoes must be that one.
		if err := f.accept(f.report()); !errors.Is(err, ErrHibernationConflict) {
			t.Fatalf("accept with the old nonce: %v", err)
		}
		if err := f.accept(f.report(func(r *HibernationInventoryReport) { r.RegisterNonce = "rotated" })); err != nil {
			t.Fatalf("accept with the current nonce: %v", err)
		}
	})
}
