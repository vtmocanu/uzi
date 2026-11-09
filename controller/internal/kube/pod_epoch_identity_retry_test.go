package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// These helpers and HTTP regressions use seams present at 4a52cf87.
func epochFailIdentityOnce(h *epochHarness, advance func()) *int {
	calls := new(int)
	delegate := h.session.resolver
	h.session.resolver = epochResolverFunc(func(ctx context.Context, w PodEpochWorker, p *corev1.Pod, fresh bool) (PodEpochIdentity, error) {
		*calls++
		if *calls == 1 {
			if advance != nil {
				advance()
			}
			return PodEpochIdentity{}, errors.New("identity read unavailable")
		}
		return delegate.ResolvePodEpoch(ctx, w, p, fresh)
	})
	return calls
}

type epochIdentityHTTP struct {
	t         *testing.T
	h         *epochHarness
	owner     *PodEpochWatch
	mu        sync.Mutex
	patches   []map[string]any
	conflicts int
}

func epochIdentityHTTPSetup(t *testing.T, h *epochHarness, conflicts int) *epochIdentityHTTP {
	t.Helper()
	x := &epochIdentityHTTP{t: t, h: h, conflicts: conflicts}
	client := epochHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		defer x.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/namespaces/docker/pods/"+epochPod(epochB).Name {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			t.Error(err)
			return
		}
		x.patches = append(x.patches, patch)
		if len(x.patches) <= x.conflicts {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Failure", Reason: metav1.StatusReasonConflict, Code: 409})
			return
		}
		_ = json.NewEncoder(w).Encode(corev1.Pod{TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"}})
	})
	h.session.client = client
	x.owner = NewPodEpochWatch(client.CoreV1().RESTClient(), "docker", "containerd-runc", h.session)
	return x
}

func (x *epochIdentityHTTP) consume(kind watch.EventType, p *corev1.Pod, want int) {
	x.t.Helper()
	x.consumeContext(context.Background(), kind, p, want)
}

func (x *epochIdentityHTTP) consumeContext(ctx context.Context, kind watch.EventType, p *corev1.Pod, want int) {
	x.t.Helper()
	var frame bytes.Buffer
	if err := json.NewEncoder(&frame).Encode(map[string]any{"type": kind, "object": p}); err != nil {
		x.t.Fatal(err)
	}
	rv := ""
	err := x.owner.consume(ctx, &frame, &rv)
	if err != io.EOF || rv != p.ResourceVersion {
		x.t.Fatalf("consume err=%v rv=%q", err, rv)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.patches) != want {
		x.t.Fatalf("PATCH requests=%d want=%d", len(x.patches), want)
	}
}

func (x *epochIdentityHTTP) assertPatches(rvs ...string) {
	x.t.Helper()
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.patches) != len(rvs) {
		x.t.Fatalf("patches=%d want=%d", len(x.patches), len(rvs))
	}
	const negative = `{"handoff":"none","reason":"identity_unverified"}`
	for i, patch := range x.patches {
		metadata, ok := patch["metadata"].(map[string]any)
		if !ok || len(patch) != 1 || len(metadata) != 3 || metadata["uid"] != epochB || metadata["resourceVersion"] != rvs[i] {
			x.t.Fatalf("patch %d target/shape: %v", i, patch)
		}
		annotations, ok := metadata["annotations"].(map[string]any)
		if !ok || len(annotations) != 1 || annotations[PodEpochAnnotation] != negative {
			x.t.Fatalf("patch %d changed frozen negative: %v", i, annotations)
		}
	}
}

func TestPodEpochIdentityNegativeHTTPRetry(t *testing.T) {
	for _, predecessor := range []string{"absent", "proven"} {
		t.Run(predecessor, func(t *testing.T) {
			h := newEpochHarness(t)
			if predecessor == "proven" {
				h = epochReadyHarness(t, epochTerminal(epochPod(epochA)))
			}
			calls := epochFailIdentityOnce(h, nil)
			x := epochIdentityHTTPSetup(t, h, 1)
			p := epochPod(epochB)
			x.consume(watch.Added, p, 1)
			x.consume(watch.Added, p, 1) // replay across consume calls
			x.consume(watch.Modified, p, 1)
			changed := epochWithIDs(p, "containerd://")
			x.consume(watch.Modified, changed, 2)
			x.consume(watch.Modified, withRV(changed, "/after-success"), 2)
			x.assertPatches(p.ResourceVersion, changed.ResourceVersion)
			if *calls != 1 {
				t.Fatalf("resolver calls=%d want=1", *calls)
			}
		})
	}
}

// Candidate-only: these tests establish identity-negative retries are live
// before exercising the limits, so the old one-attempt bug cannot mask a fence.
func TestPodEpochIdentityNegativeLimits(t *testing.T) {
	for _, mode := range []string{"three-conflicts", "first-seen-budget", "resolver-time"} {
		t.Run(mode, func(t *testing.T) {
			h := newEpochHarness(t)
			now := time.Now()
			start := now
			h.session.now = func() time.Time { return now }
			calls := epochFailIdentityOnce(h, func() {
				if mode == "resolver-time" {
					now = now.Add(89 * time.Second)
				}
			})
			x := epochIdentityHTTPSetup(t, h, 10)
			p := epochPod(epochB)
			x.consume(watch.Added, p, 1)
			now = start.Add(89 * time.Second)
			second := withRV(p, "/second")
			x.consume(watch.Modified, second, 2)
			want := 2
			if mode == "three-conflicts" {
				x.consume(watch.Modified, withRV(p, "/third"), 3)
				want = 3
			} else {
				now = start.Add(90 * time.Second)
			}
			x.consume(watch.Modified, withRV(p, "/stopped"), want)
			now = start // clock rollback cannot resurrect a tombstone
			x.consume(watch.Added, withRV(p, "/replay"), want)
			if !h.session.attempts[epochB].stopped || *calls != 1 {
				t.Fatal("limit renewed identity delivery")
			}
			if want == 3 {
				x.assertPatches(p.ResourceVersion, second.ResourceVersion, withRV(p, "/third").ResourceVersion)
			} else {
				x.assertPatches(p.ResourceVersion, second.ResourceVersion)
			}
		})
	}
}

func TestPodEpochIdentityNegativeResolverExpiry(t *testing.T) {
	h := newEpochHarness(t)
	now := time.Now()
	h.session.now = func() time.Time { return now }
	calls := epochFailIdentityOnce(h, func() { now = now.Add(90 * time.Second) })
	x := epochIdentityHTTPSetup(t, h, 0)
	x.consume(watch.Added, epochPod(epochB), 0)
	now = now.Add(-90 * time.Second)
	x.consume(watch.Modified, withRV(epochPod(epochB), "/later"), 0)
	if *calls != 1 || !h.session.attempts[epochB].stopped {
		t.Fatal("resolver time did not exhaust firstSeen budget")
	}
}

func TestPodEpochIdentityNegativeHTTPFences(t *testing.T) {
	for _, mode := range []string{"label", "mount", "deleted", "terminal", "failed-unaudited", "superseded", "reset", "generation", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			h := newEpochHarness(t)
			epochFailIdentityOnce(h, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			x := epochIdentityHTTPSetup(t, h, 10)
			p := epochPod(epochB)
			x.consumeContext(ctx, watch.Added, p, 1)
			second := withRV(p, "/retry")
			x.consume(watch.Modified, second, 2)
			attempt := h.session.attempts[epochB]
			if attempt == nil || attempt.stopped {
				t.Fatal("retry not live before fence")
			}

			fence := withRV(p, "/fence")
			switch mode {
			case "label":
				fence.Labels = nil
				x.consume(watch.Modified, fence, 2)
			case "mount":
				fence.Spec.Volumes = nil
				x.consume(watch.Modified, fence, 2)
			case "deleted":
				x.consume(watch.Deleted, fence, 2)
			case "terminal":
				x.consume(watch.Modified, epochTerminal(fence), 2)
			case "failed-unaudited":
				fence.Status.Phase = corev1.PodFailed
				fence.Status.Reason = "Evicted"
				x.consume(watch.Modified, fence, 2)
			case "superseded":
				// Observe C without delivering its decision: HTTP counts target B.
				if _, err := h.session.Handle(context.Background(), h.generation, watch.Added, epochPod(epochC)); err != nil {
					t.Fatal(err)
				}
			case "reset":
				// B is a baseline predecessor in the new subscription.
				var err error
				h.generation, err = h.session.Reset(context.Background(), []corev1.Pod{*p})
				if err != nil {
					t.Fatal(err)
				}
			case "generation":
				h.session.StopGeneration(h.generation)
			case "cancel":
				// Later consume calls use a fresh context; the original one fences B.
				cancel()
			}
			x.consume(watch.Modified, withRV(p, "/restored"), 2)
			x.consume(watch.Added, withRV(p, "/replayed"), 2)
			if !attempt.stopped {
				t.Fatal("fenced identity negative resurrected")
			}
			if current := h.session.attempts[epochB]; current != nil && !current.stopped {
				t.Fatal("fenced UID acquired a live attempt")
			}
			x.assertPatches(p.ResourceVersion, second.ResourceVersion)
		})
	}
}

func TestPodEpochIdentityNegativeFences(t *testing.T) {
	for _, mode := range []string{"label", "mount", "deleted", "terminal", "failed-unaudited", "superseded", "reset", "generation", "cancel"} {
		for _, afterConflict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/after-conflict=%v", mode, afterConflict), func(t *testing.T) {
				h := newEpochHarness(t)
				epochFailIdentityOnce(h, nil)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				d, err := h.session.Handle(ctx, h.generation, watch.Added, epochPod(epochB))
				if err != nil || d == nil {
					t.Fatal("missing issued negative", err)
				}
				n := epochConflict(h)
				if afterConflict {
					_ = h.session.Deliver(context.Background(), d)
					d = h.event(watch.Modified, withRV(epochPod(epochB), "/retry"))
					if d == nil || *n != 1 {
						t.Fatal("retry not live before fence")
					}
				}
				p := withRV(epochPod(epochB), "/fence")
				switch mode {
				case "label":
					p.Labels = nil
					h.event(watch.Modified, p)
				case "mount":
					p.Spec.Volumes = nil
					h.event(watch.Modified, p)
				case "deleted":
					h.event(watch.Deleted, p)
				case "terminal":
					h.event(watch.Modified, epochTerminal(p))
				case "failed-unaudited":
					p.Status.Phase = corev1.PodFailed
					p.Status.Reason = "Evicted"
					h.event(watch.Modified, p)
				case "superseded":
					h.event(watch.Added, epochPod(epochC))
				case "reset":
					h.generation, err = h.session.Reset(context.Background(), nil)
					if err != nil {
						t.Fatal(err)
					}
				case "generation":
					h.session.StopGeneration(h.generation)
				case "cancel":
					cancel()
				}
				want := 0
				if afterConflict {
					want = 1
				}
				if h.session.Deliver(context.Background(), d) == nil || *n != want {
					t.Fatal("fenced issued decision submitted")
				}
				if h.event(watch.Modified, withRV(epochPod(epochB), "/restored")) != nil || *n != want {
					t.Fatal("fenced UID resurrected")
				}
			})
		}
	}
}

// Mutation assertion: a later structural event must not rewrite the frozen
// classification, even though it stops delivery before any PATCH was submitted.
func TestPodEpochIdentityNegativeProvenance(t *testing.T) {
	h := newEpochHarness(t)
	epochFailIdentityOnce(h, nil)
	d := h.event(watch.Added, epochPod(epochB))
	p := withRV(epochPod(epochB), "/label")
	p.Labels = nil
	h.event(watch.Modified, p)
	if !h.session.attempts[epochB].identityUnverified {
		t.Fatal("later event cleared frozen identityUnverified")
	}
	if !h.session.records[epochB].tainted {
		t.Fatal("identity/event taint lost to proof readers")
	}
	if h.session.Deliver(context.Background(), d) == nil || len(h.patches) != 0 {
		t.Fatal("event taint allowed initial submission")
	}
}
