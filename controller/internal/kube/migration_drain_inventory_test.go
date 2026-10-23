package kube

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// A drained source must consume availability and the lane budget even when the
// first Observe missed its pod and the planning LIST subsequently discovers it.
func TestMigrationDrainRefreshedInventory(t *testing.T) {
	for _, ordinary := range []bool{false, true} {
		t.Run(map[bool]string{false: "migration-budget", true: "ordinary-availability"}[ordinary], func(t *testing.T) {
			f, ws, c := migrationFixture(t, migrationHeldOpt("a"), migrationHeldOpt("b"), migrationHeldOpt("c"))
			ws[0].DrainingSince, ws[0].DrainingReason = &m5Now, token("image_roll")
			if ordinary {
				ws[1].CustodyHeld = false
				ws[1].RollEvidence.SchedulingCapabilities = []string{"unique"}
				ws[0].RollEvidence.SchedulingCapabilities = []string{"unique"}
				if err := f.client.CoreV1().Pods(lmNS()).Delete(context.Background(), "pod-c", metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			observed, err := f.m.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for i := range observed {
				if observed[i].ID == "a" {
					observed[i].RunnerLayout = reconcile.RunnerLayoutNone
				}
			}
			if err := f.m.Reconcile(context.Background(), ws, observed); err != nil {
				t.Fatal(err)
			}
			if len(c.calls) != 0 || lmRolled(t, f, "a") || lmRolled(t, f, "b") {
				t.Fatalf("refreshed drained source allowed extra drain/replacement: %v", c.calls)
			}
			// Positive migration action keeps the compound regression meaningful on the
			// pre-M1 baseline as well as isolating the stale-accounting failure on M1.
			ws[0].DrainingSince, ws[0].DrainingReason = nil, nil
			ws[1].CustodyHeld = true
			ws[1].RollEvidence.SchedulingCapabilities = ws[0].RollEvidence.SchedulingCapabilities
			ws[2].RollEvidence.SchedulingCapabilities = ws[0].RollEvidence.SchedulingCapabilities
			f.tickAll(t, ws...)
			if !reflect.DeepEqual(c.calls, []string{"a"}) {
				t.Fatalf("fresh undrained selection: %v", c.calls)
			}
		})
	}
}

func TestMigrationDrainInventoryNotice(t *testing.T) {
	const first = "11111111-1111-4111-8111-111111111111"
	const second = "22222222-2222-4222-8222-222222222222"
	for _, mode := range []string{"unresolved", "unresolved-no-phase", "nil-scheduling", "nil-protocol", "separate-owner", "separate-lane", "unknown-owner"} {
		t.Run(mode, func(t *testing.T) {
			f, ws, c := migrationFixture(t, migrationHeldOpt(first), migrationHeldOpt(second))
			for i := range ws {
				ws[i].DrainingSince, ws[i].DrainingReason = &m5Now, token("image_roll")
				switch mode {
				case "nil-scheduling":
					ws[i].RollEvidence.SchedulingCapabilities = nil
				case "nil-protocol":
					ws[i].RollEvidence.ProtocolCapabilities = nil
				default:
					ws[i].Template = "unresolvable"
				}
			}
			if mode == "separate-owner" {
				ws[1].RollEvidence.OwnerID = "other"
			}
			if mode == "separate-lane" {
				// The non-Docker physical lane cannot join a Docker migration group.
				ws[1].Docker = false
			}
			if mode == "unknown-owner" {
				ws[1].RollEvidence.OwnerID = ""
			}
			o := &migrationPhaseObserver{Materializer: f.m, clearPhase: mode == "unresolved-no-phase"}
			r := &migrationReporter{}
			l := reconcile.New(&migrationPoller{workers: ws}, o, r, time.Second, "1.0.0", f.m.log)
			if err := l.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := mode != "separate-owner" && mode != "separate-lane" && mode != "unknown-owner"
			found := map[string]bool{}
			for _, row := range r.report.Workers {
				if row.Pacing == nil {
					continue
				}
				p := row.Pacing
				if !want {
					t.Fatalf("unrelated identities produced diagnostic pacing: %+v", p)
				}
				if p.State != "held" || p.CanaryID != nil || p.BlockingReason == nil {
					t.Fatalf("held diagnostic missing: %+v", p)
				}
				reason := *p.BlockingReason
				if len(reason) > 192 || !strings.Contains(reason, "fleet: multiple migration-held drains") || !strings.Contains(reason, "held drains=2; sample="+first) || !strings.Contains(reason, "retained drain") {
					t.Fatalf("lost local diagnostic: %q", reason)
				}
				if strings.HasPrefix(mode, "unresolved") && (p.TargetImage != "" || p.TargetVersion != "") {
					t.Fatalf("invented desired target: %+v", p)
				}
				if mode == "unresolved-no-phase" && row.Phase != protocol.PhaseRolling {
					t.Fatalf("missing Loop fallback: %+v", row)
				}
				found[row.ID] = true
			}
			if want && (!found[first] || !found[second]) {
				t.Fatalf("missing full-inventory status wire: %+v", r.report)
			}
			if len(c.calls) != 0 || len(c.clearCalls) != 0 || lmAnnotation(t, f, first) != "" || lmAnnotation(t, f, second) != "" || lmRolled(t, f, first) || lmRolled(t, f, second) {
				t.Fatal("display granted mutation authority")
			}
		})
	}
}

// An uncertain Docker migration must leave ordinary work in another physical
// lane free to progress. Owner isolation is exercised separately in the same lane.
func TestMigrationDrainIndependentLanes(t *testing.T) {
	for _, lane := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner", true: "physical-lane"}[lane], func(t *testing.T) {
			var objects []runtime.Object
			var ws []protocol.DesiredWorker
			for _, opt := range []lmOpts{migrationHeldOpt("a"), migrationHeldOpt("b"), migrationProviderOpt("c")} {
				objs, w := lmObjects(t, opt)
				if w.ID == "c" {
					if lane {
						w.Docker = false
						for _, obj := range objs {
							obj.(metav1.Object).SetNamespace(testConfig().Namespace)
						}
					} else {
						w.RollEvidence.OwnerID = "other"
					}
				}
				objects = append(objects, objs...)
				ws = append(ws, w)
			}
			f := lmCluster(t, false, objects...)
			c := &migrationCordoner{fail: true}
			f.m.cordoner = c
			if f.tickErr(t, ws...) == nil {
				t.Fatal("uncertain migration not reported")
			}
			if !reflect.DeepEqual(c.calls, []string{"a"}) {
				t.Fatalf("migration request: %v", c.calls)
			}
			ns := lmNS()
			if lane {
				ns = testConfig().Namespace
			}
			d, err := f.client.AppsV1().Deployments(ns).Get(context.Background(), deploymentName("c"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if d.Spec.Template.Annotations[AnnotationSpecHash] == lmStaleHash || d.Annotations[annotationRoll] == "" {
				t.Fatal("independent ordinary admission stalled")
			}
		})
	}
}

func TestMigrationDrainIndependentOwners(t *testing.T) {
	f, ws, c := migrationFixture(t, migrationHeldOpt("a"), migrationHeldOpt("b"), migrationHeldOpt("c"), migrationHeldOpt("d"))
	ws[2].RollEvidence.OwnerID = "other"
	ws[3].RollEvidence.OwnerID = "other"
	f.tickAll(t, ws...)
	if !reflect.DeepEqual(c.calls, []string{"a", "c"}) {
		t.Fatalf("owner selection budgets: %v", c.calls)
	}
	for _, i := range []int{0, 2} {
		ws[i].DrainingSince, ws[i].DrainingReason = &m5Now, token("image_roll")
	}
	f.tickAll(t, ws...)
	if !reflect.DeepEqual(c.calls, []string{"a", "c"}) {
		t.Fatalf("owner retained budgets: %v", c.calls)
	}
}

type migrationPhaseObserver struct {
	*Materializer
	clearPhase bool
}

func (o *migrationPhaseObserver) Observe(ctx context.Context) ([]reconcile.ObservedWorker, error) {
	rows, err := o.Materializer.Observe(ctx)
	if o.clearPhase {
		for i := range rows {
			rows[i].Roll = reconcile.RollHealth{}
		}
	}
	return rows, err
}

func TestMigrationDrainSuccessfulPollRetries(t *testing.T) {
	f, ws, c := migrationFixture(t, migrationHeldOpt("a"), migrationHeldOpt("b"), migrationHeldOpt("c"))
	c.fail = true
	l := reconcile.New(&migrationPoller{workers: ws}, f.m, nil, time.Second, "1.0.0", f.m.log)
	for i := 0; i < 3; i++ {
		if l.Tick(context.Background()) == nil {
			t.Fatal("lost cordon response not reported")
		}
		want := make([]string, i+1)
		for j := range want {
			want[j] = "a"
		}
		if !reflect.DeepEqual(c.calls, want) {
			t.Fatalf("successful poll retried sibling: %v", c.calls)
		}
	}
	c.fail = false
	f.tickAll(t, ws...)
	if !reflect.DeepEqual(c.calls, []string{"a", "a", "a", "a"}) {
		t.Fatalf("resolved retry: %v", c.calls)
	}
}

func TestMigrationDrainBlockedCandidate(t *testing.T) {
	for _, mode := range []string{"snapshot", "malformed", "withdrawal", "run-bound"} {
		t.Run(mode, func(t *testing.T) {
			positive, pw, pc := migrationFixture(t, migrationHeldOpt("a"), migrationHeldOpt("b"))
			positive.tickAll(t, pw...)
			if !reflect.DeepEqual(pc.calls, []string{"a"}) {
				t.Fatalf("migration entry: %v", pc.calls)
			}
			a := migrationHeldOpt("a")
			if mode == "withdrawal" {
				a.marker = "canary"
			}
			f, ws, c := migrationFixture(t, a, migrationHeldOpt("b"), migrationHeldOpt("c"))
			switch mode {
			case "snapshot":
				f.client.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("snapshot unavailable")
				})
			case "malformed":
				d, _ := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName("a"), metav1.GetOptions{})
				d.Annotations = map[string]string{annotationRoll: "malformed"}
				if _, err := f.client.AppsV1().Deployments(lmNS()).Update(context.Background(), d, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "withdrawal":
				f.client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
					if a.(k8stesting.PatchAction).GetName() == deploymentName("a") {
						return true, nil, errors.New("withdrawal unavailable")
					}
					return false, nil, nil
				})
			case "run-bound":
				ws[0].Ephemeral = true
			}
			_ = f.tickErr(t, ws...)
			if mode == "run-bound" {
				if !reflect.DeepEqual(c.calls, []string{"b"}) {
					t.Fatalf("run-bound candidate selected or persistent action missing: %v", c.calls)
				}
			} else if len(c.calls) != 0 {
				t.Fatalf("uncertain evidence authorized drain: %v", c.calls)
			}
		})
	}
}
