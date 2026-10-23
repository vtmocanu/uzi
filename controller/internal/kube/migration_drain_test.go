package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

type migrationCordoner struct {
	fakeCordoner
	reasons []string
	fail    bool
}

func (c *migrationCordoner) RequestDrainWithReason(ctx context.Context, id string, reasons ...string) error {
	c.reasons = append(c.reasons, reasons...)
	c.calls = append(c.calls, id)
	if c.fail {
		return errors.New("response lost")
	}
	return nil
}

func migrationFixture(t *testing.T, opts ...lmOpts) (*lmFixture, []protocol.DesiredWorker, *migrationCordoner) {
	t.Helper()
	var objects []runtime.Object
	var workers []protocol.DesiredWorker
	for _, opt := range opts {
		objs, w := lmObjects(t, opt)
		objects = append(objects, objs...)
		workers = append(workers, w)
	}
	f := lmCluster(t, false, objects...)
	c := &migrationCordoner{}
	f.m.cordoner = c
	return f, workers, c
}
func migrationHeldOpt(id string) lmOpts {
	return lmOpts{id: id, custody: true, stale: true, layout: "old"}
}
func migrationProviderOpt(id string) lmOpts { return lmOpts{id: id, stale: true, layout: "new"} }

func TestMigrationDrainEntry(t *testing.T) {
	for _, mode := range []string{"busy", "busy-provider", "idle-terminal", "singleton", "partial", "union", "foreign-owner", "foreign-lane", "unknown", "stale", "dead", "draining", "pressure", "maintenance", "undersized-nix", "undersized-nix-busy", "undersized-nix-disabled", "full-size-nix", "ordinary-wins", "third-provider", "reverse-dependency"} {
		t.Run(mode, func(t *testing.T) {
			nixProvider := strings.HasPrefix(mode, "undersized-nix") || mode == "full-size-nix"
			opts := []lmOpts{migrationHeldOpt("a"), migrationHeldOpt("b")}
			if nixProvider {
				// Current, non-held coverage can still disappear to independent
				// /nix maintenance later in this same reconciliation.
				opts[1] = lmOpts{id: "b", layout: "new", promoted: true}
			}
			if mode == "ordinary-wins" || mode == "third-provider" || mode == "reverse-dependency" {
				opts[1] = migrationProviderOpt("b")
			}
			if mode == "singleton" {
				opts = opts[:1]
			}
			if mode == "union" || mode == "third-provider" || mode == "reverse-dependency" {
				opts = append(opts, migrationHeldOpt("c"))
			}
			if mode == "reverse-dependency" {
				opts = append(opts, migrationProviderOpt("d"))
			}
			f, ws, c := migrationFixture(t, opts...)
			ws[0].Busy = mode == "busy"
			want := mode == "busy" || mode == "busy-provider" || mode == "idle-terminal" || mode == "third-provider" || mode == "undersized-nix-disabled" || mode == "full-size-nix"
			if nixProvider {
				pvc, err := f.client.CoreV1().PersistentVolumeClaims(lmNS()).Get(context.Background(), nixPVCName("b"), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				size := "4Gi"
				if mode == "full-size-nix" {
					spec := testSpec(t, "base", "m")
					size = spec.NixSize.String()
				}
				pvc.Spec.Resources.Requests[corev1.ResourceStorage] = *ptrQuantity(size)
				if _, err := f.client.CoreV1().PersistentVolumeClaims(lmNS()).Update(context.Background(), pvc, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				f.m.recycle.Enabled = mode != "undersized-nix-disabled"
				ws[1].Busy = mode == "undersized-nix-busy"
			}
			if mode == "busy-provider" {
				ws[1].Busy = true
			}
			switch mode {
			case "partial", "union":
				ws[0].RollEvidence.SchedulingCapabilities = []string{"x", "y"}
				ws[1].RollEvidence.SchedulingCapabilities = []string{"x"}
				if mode == "union" {
					ws[2].RollEvidence.SchedulingCapabilities = []string{"y"}
				}
			case "foreign-owner":
				ws[1].RollEvidence.OwnerID = "other"
			case "foreign-lane":
				ws[1].Docker = false
			case "unknown":
				ws[1].RollEvidence.LivenessValidUntil = nil
			case "stale":
				expired := m5Now.Add(-time.Minute)
				ws[1].RollEvidence.LivenessValidUntil = &expired
			case "dead":
				p, _ := f.client.CoreV1().Pods(lmNS()).Get(context.Background(), "pod-b", metav1.GetOptions{})
				p.Status.Phase = corev1.PodSucceeded
				_, _ = f.client.CoreV1().Pods(lmNS()).Update(context.Background(), p, metav1.UpdateOptions{})
			case "draining":
				ws[1].DrainingSince, ws[1].DrainingReason = &m5Now, token("image_roll")
			case "pressure":
				ws[1].DiskPressure = true
			case "maintenance":
				ws[1].DindMaintenance = &protocol.DindMaintenance{Phase: "stopping"}
			case "ordinary-wins", "third-provider", "reverse-dependency":
				// b drifts normally; a must remain serving when it is b's only provider.
				if mode == "reverse-dependency" {
					ws[0].RollEvidence.SchedulingCapabilities = []string{"x", "y", "z"}
					ws[1].RollEvidence.SchedulingCapabilities = []string{"x", "y", "z"}
					ws[2].RollEvidence.SchedulingCapabilities = []string{"x", "y"}
					ws[3].RollEvidence.SchedulingCapabilities = []string{"x", "y", "z"}
					_ = f.client.CoreV1().Pods(lmNS()).Delete(context.Background(), "pod-d", metav1.DeleteOptions{})
					// a's existing reservation freezes the narrower drain coverage.
					d, _ := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName("a"), metav1.GetOptions{})
					cov, _ := coverageOf(ws[0], lmNS())
					cov.Scheduling = []string{"docker", "x", "y"}
					raw, _ := json.Marshal(&rollMarker{Version: 1, Hash: strings.Repeat("a", 64), Image: workerImage(d), TargetVersion: "1.0.0", Coverage: cov})
					if _, err := decodeRoll(string(raw)); err != nil {
						t.Fatalf("reverse-dependency marker must decode: %v", err)
					}
					d.Annotations = map[string]string{annotationRoll: string(raw)}
					_, _ = f.client.AppsV1().Deployments(lmNS()).Update(context.Background(), d, metav1.UpdateOptions{})
				}
			default:
				if len(ws) > 1 && !nixProvider {
					ws[1].CustodyHeld = true
				} // another held candidate stays serving this tick
			}
			// Entry proofs use live incarnations, independent of free slots.
			if mode != "ordinary-wins" && mode != "third-provider" && mode != "reverse-dependency" && len(ws) > 1 && !nixProvider {
				ws[1].CustodyHeld = true
			}
			if mode == "third-provider" {
				ws[2].CustodyHeld = true
			}
			if mode == "reverse-dependency" {
				ws[2].CustodyHeld = true
			}
			if err := f.tickErr(t, ws...); err != nil && mode != "maintenance" {
				t.Fatal(err)
			}
			got := false
			for _, id := range c.calls {
				got = got || id == "a"
			}
			if got != want {
				t.Fatalf("drain calls %v; a selected=%v want %v; outcomes %+v", c.calls, got, want, f.m.PacingOutcomes())
			}
			if mode == "ordinary-wins" || mode == "reverse-dependency" {
				if !lmRolled(t, f, "b") || lmAnnotation(t, f, "b") == "" || lmRolled(t, f, "a") || lmAnnotation(t, f, "a") != "" {
					t.Fatal("ordinary admission did not replace b while preserving a")
				}
			}
			if want {
				if !reflect.DeepEqual(c.calls, []string{"a"}) || !reflect.DeepEqual(c.reasons, []string{"image_roll"}) {
					t.Fatalf("requests %v reasons %v", c.calls, c.reasons)
				}
				p := f.m.PacingOutcomes()["a"]
				if p.State != "held" || p.CanaryID != nil || p.BlockingReason == nil || !strings.Contains(*p.BlockingReason, "layout_migration_custody") {
					t.Fatalf("held outcome %+v", p)
				}
				if lmRolled(t, f, "a") || lmAnnotation(t, f, "a") != "" {
					t.Fatal("held worker replaced or reserved")
				}
			}
		})
	}
}

func TestMigrationDrainRequiresUndrainedPoll(t *testing.T) {
	f, ws, c := migrationFixture(t, migrationHeldOpt("a"),
		lmOpts{id: "b", layout: "new", promoted: true})
	ws[1].DrainingSince, ws[1].DrainingReason = &m5Now, token("image_roll")
	f.tickAll(t, ws...)
	if !reflect.DeepEqual(c.clearCalls, []string{"b"}) {
		t.Fatalf("provider drain not cleared: %v", c.clearCalls)
	}
	if len(c.calls) != 0 {
		t.Fatalf("provider drained in original poll covered migration: %v", c.calls)
	}
	// Only an explicit undrained poll makes the same serving proof eligible.
	ws[1].DrainingSince, ws[1].DrainingReason = nil, nil
	f.tickAll(t, ws...)
	if !reflect.DeepEqual(c.calls, []string{"a"}) {
		t.Fatalf("undrained provider did not cover migration: %v", c.calls)
	}
}

// Loop failures must preserve pending write uncertainty. A direct Reconcile test
// alone cannot establish that Poll and Observe failures leave the map untouched.
type migrationPoller struct {
	workers []protocol.DesiredWorker
	err     error
}

func (p *migrationPoller) Poll(context.Context) (protocol.PollResponse, error) {
	return protocol.PollResponse{Workers: p.workers}, p.err
}

type migrationObserver struct {
	*Materializer
	fail     bool
	hiddenID string
}

func (m *migrationObserver) Observe(ctx context.Context) ([]reconcile.ObservedWorker, error) {
	if m.fail {
		return nil, errors.New("observe unavailable")
	}
	observed, err := m.Materializer.Observe(ctx)
	if err != nil {
		return nil, err
	}
	if m.hiddenID == "" {
		return observed, nil
	}
	filtered := make([]reconcile.ObservedWorker, 0, len(observed))
	for _, worker := range observed {
		if worker.ID != m.hiddenID {
			filtered = append(filtered, worker)
		}
	}
	return filtered, nil
}

type migrationReporter struct{ report protocol.StatusReport }

func (r *migrationReporter) Report(_ context.Context, s protocol.StatusReport) error {
	r.report = s
	return nil
}

func TestMigrationDrainPendingLoop(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-commit", true: "after-commit"}[committed], func(t *testing.T) {
			f, ws, c := migrationFixture(t, migrationHeldOpt("a"), migrationHeldOpt("b"), migrationHeldOpt("c"))
			p := &migrationPoller{workers: ws}
			o := &migrationObserver{Materializer: f.m}
			l := reconcile.New(p, o, nil, time.Second, "1.0.0", f.m.log)
			c.fail = true
			if err := l.Tick(context.Background()); err == nil {
				t.Fatal("missing unknown-write error")
			}
			if !reflect.DeepEqual(c.calls, []string{"a"}) {
				t.Fatalf("calls %v", c.calls)
			}
			if committed {
				ws[0].DrainingSince, ws[0].DrainingReason = &m5Now, token("image_roll")
			}
			for i := 0; i < 3; i++ {
				p.err = errors.New("poll unavailable")
				if l.Tick(context.Background()) == nil {
					t.Fatal("poll succeeded")
				}
				p.err = nil
				o.fail = true
				if l.Tick(context.Background()) == nil {
					t.Fatal("observe succeeded")
				}
				o.fail = false
			}
			if len(c.calls) != 1 {
				t.Fatalf("failed cycles retried mutation %v", c.calls)
			}
			// Missing owner is not positive resolution of the old attempt.
			saved := ws[0].RollEvidence
			ws[0].RollEvidence = nil
			if err := l.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(c.calls) != 1 {
				t.Fatalf("missing owner cleared pending: %v", c.calls)
			}
			ws[0].RollEvidence = saved
			// A missing pending worker must still consume the frozen budget,
			// without the missing-owner row independently blocking the lane.
			// Omit its observation too: this models missing evidence, rather
			// than authorizing the existing undesired-worker teardown path.
			p.workers, o.hiddenID = ws[1:], "a"
			if err := l.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(c.calls) != 1 {
				t.Fatalf("absent worker lost pending budget: %v", c.calls)
			}
			if len(deletedSet(f.client)) != 0 {
				t.Fatal("missing evidence deleted worker objects")
			}
			p.workers, o.hiddenID = ws, ""
			c.fail = false
			if err := l.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 2
			if committed {
				want = 1
			}
			if len(c.calls) != want {
				t.Fatalf("fresh resolution calls %v want %d", c.calls, want)
			}
		})
	}
}

func TestMigrationDrainReentryAndBudget(t *testing.T) {
	for _, mode := range []string{"registration", "restart", "changed-coverage", "existing-budget", "image-roll-budget", "legacy-budget", "nonrenderable-budget", "missing-owner-budget", "run-bound-excluded", "coverage-loss", "later-drift"} {
		t.Run(mode, func(t *testing.T) {
			f, ws, c := migrationFixture(t, migrationHeldOpt("a"), migrationHeldOpt("b"), migrationHeldOpt("c"))
			f.tickAll(t, ws...)
			if !reflect.DeepEqual(c.calls, []string{"a"}) {
				t.Fatalf("entry %v", c.calls)
			}
			ws[0].DrainingSince, ws[0].DrainingReason = &m5Now, token("pressure_recycle")
			switch mode {
			case "registration", "restart", "changed-coverage":
				ws[0].DrainingSince, ws[0].DrainingReason = nil, nil
				if mode == "changed-coverage" {
					ws[0].RollEvidence.SchedulingCapabilities = []string{"x"}
				}
			case "image-roll-budget":
				ws[0].DrainingReason = token("image_roll")
			case "legacy-budget":
				ws[0].DrainingReason = nil
			case "run-bound-excluded":
				ws[0].Ephemeral = true
			case "nonrenderable-budget":
				ws[0].Template = "unresolvable"
			case "missing-owner-budget":
				ws[0].RollEvidence = nil
			case "coverage-loss":
				for _, id := range []string{"b", "c"} {
					_ = f.client.CoreV1().Pods(lmNS()).Delete(context.Background(), "pod-"+id, metav1.DeleteOptions{})
				}
			case "later-drift":
				ws[1].CustodyHeld = false
				ws[2].RollEvidence.SchedulingCapabilities = []string{"x"}
				ws[1].RollEvidence.SchedulingCapabilities = []string{"y"}
				ws[0].RollEvidence.SchedulingCapabilities = []string{"y"}
				_ = f.client.CoreV1().Pods(lmNS()).Delete(context.Background(), "pod-c", metav1.DeleteOptions{})
			}
			if mode == "restart" {
				f.m = New(f.client, f.m.cfg, f.m.resolver, c, f.m.drain, f.m.recycle, f.m.log)
				f.m.now = func() time.Time { return m5Now }
			}
			f.tickAll(t, ws...)
			want := []string{"a"}
			if mode == "registration" || mode == "restart" {
				want = []string{"a", "a"}
			}
			if mode == "changed-coverage" || mode == "run-bound-excluded" {
				want = []string{"a", "b"}
			}
			if !reflect.DeepEqual(c.calls, want) {
				t.Fatalf("calls %v want %v", c.calls, want)
			}
			if len(c.clearCalls) != 0 {
				t.Fatalf("custody drain released %v", c.clearCalls)
			}
			if mode == "coverage-loss" {
				dep, err := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName("a"), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if string(dep.UID) != "uid-"+deploymentName("a") || dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 || lmRolled(t, f, "a") || lmAnnotation(t, f, "a") != "" || ws[0].DrainingReason == nil || *ws[0].DrainingReason != "pressure_recycle" {
					t.Fatalf("coverage loss changed retained deployment/drain: %+v", dep)
				}
				if _, err := f.client.CoreV1().Pods(lmNS()).Get(context.Background(), "pod-a", metav1.GetOptions{}); err != nil {
					t.Fatal("retained pod lost: ", err)
				}
				p := f.m.PacingOutcomes()["a"]
				if p.BlockingReason == nil || !strings.Contains(*p.BlockingReason, "retained drain without coverage") {
					t.Fatalf("lost coverage outcome %+v", p)
				}
			}
			if mode == "later-drift" && lmRolled(t, f, "b") {
				t.Fatal("later drift lost availability guard")
			}
		})
	}
}

func TestMigrationDrainLateCommit(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-poll", true: "restart"}[restart], func(t *testing.T) {
			f, ws, c := migrationFixture(t, migrationHeldOpt("a"), migrationHeldOpt("b"), migrationHeldOpt("c"), migrationHeldOpt("0-unrelated"))
			ws[0].RollEvidence.SchedulingCapabilities = []string{"x", "y"}
			ws[1].RollEvidence.SchedulingCapabilities = []string{"x"}
			ws[2].RollEvidence.SchedulingCapabilities = []string{"x", "y"}
			ws[3].RollEvidence.OwnerID = "unrelated"
			c.fail = true
			if f.tickErr(t, ws...) == nil {
				t.Fatal("expected lost response")
			}
			if !reflect.DeepEqual(c.calls, []string{"a"}) {
				t.Fatalf("entry %v", c.calls)
			}
			// The timed-out a POST remains delayed while a fresh poll says undrained.
			_ = f.client.CoreV1().Pods(lmNS()).Delete(context.Background(), "pod-c", metav1.DeleteOptions{})
			if restart {
				f.m = New(f.client, f.m.cfg, f.m.resolver, c, f.m.drain, f.m.recycle, f.m.log)
				f.m.now = func() time.Time { return m5Now }
			}
			c.fail = false
			f.tickAll(t, ws...)
			if !reflect.DeepEqual(c.calls, []string{"a", "b"}) {
				t.Fatalf("accepted late-write schedule %v", c.calls)
			}
			// b commits, then a's old request commits late. No controller fence exists.
			for i := 0; i < 2; i++ {
				ws[i].DrainingSince, ws[i].DrainingReason = &m5Now, token("image_roll")
			}
			if !restart {
				// This unrelated lower-ID row becomes held only during materialization.
				ws[3].CustodyHeld = false
				f.client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
					if a.(k8stesting.PatchAction).GetName() == deploymentName("0-unrelated") && strings.Contains(string(a.(k8stesting.PatchAction).GetPatch()), "\"spec\"") {
						return true, nil, errors.New("late materialization failure")
					}
					return false, nil, nil
				})
			}
			p := &migrationPoller{workers: ws}
			r := &migrationReporter{}
			l := reconcile.New(p, f.m, r, time.Second, "1.0.0", f.m.log)
			if err := l.Tick(context.Background()); (err != nil) == restart {
				t.Fatalf("late error=%v restart=%v", err, restart)
			}
			if len(c.calls) != 2 || len(c.clearCalls) != 0 {
				t.Fatalf("multiplicity caused mutation: %v %v", c.calls, c.clearCalls)
			}
			for _, id := range []string{"a", "b"} {
				if lmRolled(t, f, id) {
					t.Fatal("late drain replaced held source")
				}
			}
			pacing := f.m.PacingOutcomes()["0-unrelated"]
			if pacing == nil || pacing.BlockingReason == nil || !strings.Contains(*pacing.BlockingReason, "fleet: multiple migration-held drains") {
				t.Fatalf("lowest held masks notice: %+v", pacing)
			}
			if !restart && !strings.Contains(*pacing.BlockingReason, "mutation unconfirmed") {
				t.Fatalf("local cause lost: %s", *pacing.BlockingReason)
			}
			if strings.Contains(*pacing.BlockingReason, "held drains=") || len(*pacing.BlockingReason) > 192 {
				t.Fatalf("identity leak or bound: %s", *pacing.BlockingReason)
			}
			// Assert the actual Loop status wire, not just the pacing getter.
			found := false
			for _, row := range r.report.Workers {
				if row.ID == "0-unrelated" && row.Pacing != nil && row.Pacing.BlockingReason != nil {
					found = strings.Contains(*row.Pacing.BlockingReason, "multiple migration-held drains")
				}
			}
			if !found {
				t.Fatalf("notice missing from status wire: %+v", r.report)
			}
		})
	}
}

func TestMigrationDrainNoticeBudget(t *testing.T) {
	const first = "11111111-1111-4111-8111-111111111111"
	const second = "22222222-2222-4222-8222-222222222222"
	const ordinary = "00000000-0000-4000-8000-000000000000"
	const uncertain = "44444444-4444-4444-8444-444444444444"
	for _, count := range []int{2, 10} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			opts := []lmOpts{migrationHeldOpt(first), migrationHeldOpt(second)}
			for i := 2; i < count; i++ {
				opts = append(opts, migrationHeldOpt(fmt.Sprintf("33333333-3333-4333-8333-%012d", i)))
			}
			opts = append(opts, migrationHeldOpt(uncertain),
				lmOpts{id: ordinary, layout: "new", stale: true, marker: "canary"})
			f, ws, cordon := migrationFixture(t, opts...)
			for i := 0; i < count; i++ {
				ws[i].DrainingSince, ws[i].DrainingReason = &m5Now, token("image_roll")
			}
			// The undrained held worker takes the uncertain-evidence branch
			// before the occupied-budget branch. Two-digit counts must not
			// push its sampled UUID beyond the pacing byte budget.
			dep, err := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName(uncertain), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			dep.Annotations = map[string]string{annotationRoll: "malformed"}
			if _, err := f.client.AppsV1().Deployments(lmNS()).Update(context.Background(), dep, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := f.client.CoreV1().Pods(lmNS()).Delete(context.Background(), "pod-"+ordinary, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			observed, err := f.m.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			const originalCause = "confirmed admission suspended for usable coverage"
			if err := f.m.Reconcile(context.Background(), ws, observed); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{first, second, uncertain} {
				p := f.m.PacingOutcomes()[id]
				if p == nil || p.BlockingReason == nil {
					t.Fatalf("missing local notice for %s", id)
				}
				reason := *p.BlockingReason
				wantCause := "retained drain without coverage"
				if id == uncertain {
					wantCause = "uncertain"
				}
				if len(reason) > 192 || !strings.Contains(reason, fmt.Sprintf("held drains=%d; sample=%s", count, first)) ||
					!strings.Contains(reason, wantCause) || !strings.Contains(reason, "layout_migration_custody") {
					t.Fatalf("local notice lost complete sample or cause: %q", reason)
				}
				if strings.Contains(reason, second[:8]) {
					t.Fatalf("partial second UUID: %q", reason)
				}
			}
			p := f.m.PacingOutcomes()[ordinary]
			if p == nil || p.BlockingReason == nil || !strings.Contains(*p.BlockingReason, originalCause) ||
				!strings.Contains(*p.BlockingReason, "fleet: multiple migration-held drains") {
				t.Fatalf("ordinary cause erased by multiplicity: %+v", p)
			}
			if strings.Contains(*p.BlockingReason, first) || strings.Contains(*p.BlockingReason, second) {
				t.Fatalf("sample escaped migration diagnostics: %q", *p.BlockingReason)
			}
			if len(cordon.calls) != 0 || len(cordon.clearCalls) != 0 {
				t.Fatalf("multiplicity caused drain mutations: %v %v", cordon.calls, cordon.clearCalls)
			}
		})
	}
}

func TestMigrationDrainUnknownFences(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "lost"}[lost], func(t *testing.T) {
			f, ws, c := migrationFixture(t, migrationHeldOpt("a"), migrationHeldOpt("b"), migrationProviderOpt("c"), migrationProviderOpt("d"))
			ws[2].RollEvidence.SchedulingCapabilities = []string{"extra"}
			ws[3].RollEvidence.OwnerID = "unrelated"
			c.fail = lost
			err := f.tickErr(t, ws...)
			if (err != nil) != lost {
				t.Fatalf("error %v lost %v", err, lost)
			}
			if !reflect.DeepEqual(c.calls, []string{"a"}) {
				t.Fatalf("calls %v", c.calls)
			}
			if lmRolled(t, f, "c") == lost {
				t.Fatalf("overlapping key crossed uncertainty fence, lost=%v", lost)
			}
			if !lmRolled(t, f, "d") {
				t.Fatal("unrelated owner stalled")
			}
		})
	}
}

// A migration drain has no private marker after custody settles. Ordinary
// admission and reason-owned release must keep their existing predicates.
func TestMigrationDrainCustodyClear(t *testing.T) {
	for _, mode := range []string{"entry", "admitted", "released", "blocked", "unusable", "pressure", "maintenance"} {
		t.Run(mode, func(t *testing.T) {
			f, ws, cordon := migrationFixture(t, migrationHeldOpt("a"), migrationHeldOpt("b"), migrationHeldOpt("c"))
			if mode == "entry" {
				f.tickAll(t, ws...)
				if !reflect.DeepEqual(cordon.calls, []string{"a"}) {
					t.Fatalf("migration entry = %v", cordon.calls)
				}
				return
			}
			// Preseed the poll drain so release/admission is exercised even when
			// migration entry itself is broken.
			ws[0].CustodyHeld = false
			ws[0].DrainingSince, ws[0].DrainingReason = &m5Now, token("image_roll")
			if mode != "admitted" {
				// Another ordinary canary consumes admission, so a may only be
				// released through the existing serving-proof predicates.
				ws[2].CustodyHeld, ws[2].Busy = false, true
				ws[2].DrainingSince, ws[2].DrainingReason = &m5Now, token("image_roll")
				marked := markedDeployment(t, f.m.cfg, ws[2], false)
				marker, err := decodeRoll(marked.Annotations[annotationRoll])
				if err != nil {
					t.Fatal(err)
				}
				marker.Coverage, _ = coverageOf(ws[2], lmNS())
				raw, err := json.Marshal(marker)
				if err != nil {
					t.Fatal(err)
				}
				dep, err := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName("c"), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				dep.Annotations = map[string]string{annotationRoll: string(raw)}
				if _, err := f.client.AppsV1().Deployments(lmNS()).Update(context.Background(), dep, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "blocked":
				dep, err := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName("a"), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				dep.Annotations = map[string]string{annotationRoll: "malformed"}
				if _, err := f.client.AppsV1().Deployments(lmNS()).Update(context.Background(), dep, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "unusable":
				expired := m5Now.Add(-time.Minute)
				ws[0].RollEvidence.LivenessValidUntil = &expired
			case "pressure":
				ws[0].DiskPressure = true
			case "maintenance":
				ws[0].DindMaintenance = &protocol.DindMaintenance{Phase: "requested"}
			}
			if err := f.tickErr(t, ws...); err != nil {
				t.Fatal(err)
			}
			if mode == "admitted" {
				if !lmRolled(t, f, "a") {
					t.Fatalf("custody-cleared worker was not admitted: %+v", f.m.PacingOutcomes()["a"])
				}
			} else if lmRolled(t, f, "a") {
				t.Fatal("unadmitted old worker was replaced")
			}
			aClears := 0
			for _, id := range cordon.clearCalls {
				if id == "a" {
					aClears++
				}
				if id == "b" {
					t.Fatal("custody-held peer drain was cleared")
				}
			}
			wantClears := 0
			if mode == "released" {
				wantClears = 1
			}
			if aClears != wantClears {
				t.Fatalf("clear calls = %v, want %d releases of a", cordon.clearCalls, wantClears)
			}
		})
	}
}

func TestMigrationDrainOrdinaryUnconfirmed(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "unknown"}[fail], func(t *testing.T) {
			f, ws, c := migrationFixture(t, migrationHeldOpt("a"), migrationProviderOpt("b"), migrationHeldOpt("c"))
			ws[1].RollEvidence.SchedulingCapabilities = []string{"extra"}
			if fail {
				f.client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
					if a.(k8stesting.PatchAction).GetName() == deploymentName("b") {
						return true, nil, errors.New("unknown reservation")
					}
					return false, nil, nil
				})
			}
			err := f.tickErr(t, ws...)
			if fail && err == nil {
				t.Fatal("unknown write not reported")
			}
			want := []string{"a"}
			if fail {
				want = nil
			}
			if !reflect.DeepEqual(c.calls, want) {
				t.Fatalf("calls %v want %v", c.calls, want)
			}
			if lmRolled(t, f, "b") == fail || (lmAnnotation(t, f, "b") == "") != fail {
				t.Fatalf("ordinary template/reservation fail=%v pacing=%+v", fail, f.m.PacingOutcomes()["b"])
			}
		})
	}
}

func TestMigrationDrainPendingRemovedWorkerRetirement(t *testing.T) {
	for _, work := range []string{"migration", "ordinary"} {
		t.Run(work, func(t *testing.T) {
			ctx := context.Background()
			f, ws, c := migrationFixture(t, migrationHeldOpt("a"), migrationHeldOpt("b"), migrationHeldOpt("c"))
			p := &migrationPoller{workers: ws}
			o := &migrationObserver{Materializer: f.m}
			l := reconcile.New(p, o, nil, time.Second, "1.0.0", f.m.log)
			c.fail = true
			if err := l.Tick(ctx); err == nil {
				t.Fatal("failed cordon did not report uncertainty")
			}
			if !reflect.DeepEqual(c.calls, []string{"a"}) {
				t.Fatalf("initial cordon = %v", c.calls)
			}
			frozen, pending := f.m.migrationPending["a"]
			if !pending {
				t.Fatal("failed cordon did not retain pending entry")
			}
			c.fail = false
			p.workers = ws[1:]
			if work == "ordinary" {
				ws[1].CustodyHeld = false
			}
			assertRetained := func(stage string) {
				t.Helper()
				if got, ok := f.m.migrationPending["a"]; !ok || !reflect.DeepEqual(got, frozen) {
					t.Fatalf("%s: pending coverage changed: %+v, present=%v", stage, got, ok)
				}
				if !reflect.DeepEqual(c.calls, []string{"a"}) || lmRolled(t, f, "b") {
					t.Fatalf("%s: same owner/lane work crossed fence: drains=%v rolled=%v",
						stage, c.calls, lmRolled(t, f, "b"))
				}
			}
			failTeardown := true
			f.client.PrependReactor("delete", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if failTeardown && a.(k8stesting.DeleteAction).GetName() == deploymentName("a") {
					return true, nil, errors.New("teardown unavailable")
				}
				return false, nil, nil
			})
			if err := l.Tick(ctx); err == nil {
				t.Fatal("failed teardown was not reported")
			}
			assertRetained("failed teardown")
			failTeardown = false
			if err := l.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			assertRetained("successful teardown with remaining pod")
			// Fake deletion, like asynchronous Kubernetes GC, leaves the pod behind.
			// Hide the observation too: the fresh verification must still find it.
			o.hiddenID = "a"
			for _, phase := range []corev1.PodPhase{corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed} {
				pod, err := f.client.CoreV1().Pods(lmNS()).Get(ctx, "pod-a", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				pod.Status.Phase = phase
				if phase == corev1.PodFailed {
					at := metav1.NewTime(m5Now)
					pod.DeletionTimestamp = &at
				}
				if _, err := f.client.CoreV1().Pods(lmNS()).Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				if err := l.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				assertRetained("hidden " + string(phase) + " pod")
			}
			if err := f.client.CoreV1().Pods(lmNS()).Delete(ctx, "pod-a", metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			o.hiddenID = ""
			mode := ""
			pages := 0
			f.client.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.GetNamespace() != lmNS() || mode == "" {
					return false, nil, nil
				}
				options := a.(k8stesting.ListActionImpl).GetListOptions()
				if options.LabelSelector != "" || options.FieldSelector != "" {
					t.Fatal("absence verification selected pods")
				}
				if mode == "error" {
					return true, nil, errors.New("pod list unavailable")
				}
				if options.Limit == 0 {
					return false, nil, nil
				}
				pages++
				page := &corev1.PodList{}
				switch mode {
				case "oversized":
					page.Items = make([]corev1.Pod, maintenancePageSize+1)
				case "repeated":
					page.Continue = "same"
				case "page-limit":
					page.Continue = fmt.Sprint(pages)
				case "late-error":
					if options.Continue != "" {
						return true, nil, errors.New("second page unavailable")
					}
					page.Continue = "next"
				case "late-match":
					if options.Continue == "" {
						page.Continue = "next"
					} else {
						page.Items = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Labels: objectLabels("a")},
							Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}}
					}
				}
				return true, page, nil
			})
			for _, evidence := range []string{"error", "oversized", "repeated", "page-limit", "late-error", "late-match"} {
				mode, pages = evidence, 0
				// Observe deliberately swallows pod-list errors. In that case a
				// is absent without any test-only observation filtering.
				obs, err := f.m.Observe(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, worker := range obs {
					if worker.ID == "a" {
						t.Fatalf("%s: pending worker unexpectedly observed", evidence)
					}
				}
				if err := l.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				assertRetained(evidence)
			}
			// Non-provenance matches must not prevent a complete absence proof.
			mode = ""
			foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: lmNS(),
				Labels: map[string]string{LabelWorkerID: "a"}}}
			if _, err := f.client.CoreV1().Pods(lmNS()).Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := l.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if _, ok := f.m.migrationPending["a"]; ok {
				t.Fatal("complete pod absence did not retire pending entry")
			}
			if work == "migration" && !reflect.DeepEqual(c.calls, []string{"a", "b"}) {
				t.Fatalf("same owner/lane migration did not progress: %v", c.calls)
			}
			if work == "ordinary" && !lmRolled(t, f, "b") {
				t.Fatalf("same owner/lane ordinary roll did not progress: %+v", f.m.PacingOutcomes()["b"])
			}
			if len(c.clearCalls) != 0 {
				t.Fatalf("retirement cleared custody drains: %v", c.clearCalls)
			}
		})
	}
}
