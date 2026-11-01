package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestPodEpochRetryNegativeBytesAfterLateMaintenance(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	minted, err := h.client.CoreV1().PersistentVolumeClaims("docker").Get(context.Background(), dindDataPVCName("w"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	minted.UID = epochNewDinD
	minted.Spec.VolumeName = "new-dind-pv"
	if _, err = h.client.CoreV1().PersistentVolumeClaims("docker").Update(context.Background(), minted, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	d := h.event(watch.Added, epochPod(epochB))
	_, negative := epochReason(t, d)
	if negative == nil || negative.Reason != "dind_remint_unrecorded" {
		t.Fatal("missing original negative")
	}
	original := d.Document()
	epochConflict(h)
	if !apierrors.IsConflict(h.session.Deliver(context.Background(), d)) {
		t.Fatal("not conflict")
	}
	if err = h.session.RecordMaintenanceRemint(h.generation, "w", PodEpochRemint{OperationID: "example", OldClaimUID: epochDinD, NewClaimUID: epochNewDinD}); err != nil {
		t.Fatal(err)
	}
	d = h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
	if d == nil || d.Document() != original {
		t.Fatal("late receipt changed negative bytes")
	}
}

func TestPodEpochRetryNegativePayloadAndTombstoneBudget(t *testing.T) {
	h := newEpochHarness(t, epochPod(epochA))
	d := h.event(watch.Added, epochPod(epochB))
	a := h.session.attempts[epochB]
	if a == nil || a.negativeCost != len(d.Document()) || a.issued != d {
		t.Fatal("negative/issued payload not retained")
	}
	before := h.session.bytes
	payload := a.negativeCost + a.issuedCost
	if err := h.session.Deliver(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if h.session.bytes != before-payload || !a.stopped || a.negativeCost != 0 || a.negative != "" || a.issued != nil {
		t.Fatal("negative payload not released")
	}
	if h.event(watch.Added, withRV(epochPod(epochB), "/replayed")) != nil || h.session.attempts[epochB] != a || !a.stopped {
		t.Fatal("tombstone evicted or renewed")
	}
	if h.session.bytes <= h.session.workerBytes || len(h.session.attempts) != 1 {
		t.Fatal("bookkeeping not retained")
	}
}

func TestPodEpochRetryBookkeepingAdmission(t *testing.T) {
	// Measure observation storage independently, without a registered worker or
	// any delivery/identity allocation for the new pod.
	control := newEpochHarness(t, epochPod(epochA))
	control.session.workers = nil
	before := control.session.bytes
	control.event(watch.Added, epochPod(epochB))
	observationCost := control.session.bytes - before
	h := newEpochHarness(t, epochPod(epochA))
	calls := 0
	delegate := h.session.resolver
	h.session.resolver = epochResolverFunc(func(ctx context.Context, w PodEpochWorker, p *corev1.Pod, fresh bool) (PodEpochIdentity, error) {
		calls++
		return delegate.ResolvePodEpoch(ctx, w, p, fresh)
	})
	h.session.bytes = PodEpochMaxMetadata - observationCost
	if h.event(watch.Added, epochPod(epochB)) != nil || !h.session.overflow || calls != 0 || len(h.session.attempts) != 0 {
		t.Fatal("retry bookkeeping was not admitted against capacity before resolving")
	}
}

func TestPodEpochRetryGenerationStopFence(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	old := h.generation
	h.event(watch.Added, epochPod(epochB))
	g, err := h.session.Reset(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h.generation = g
	// This fresh generation is independent of the previous watch's exit pass.
	h.event(watch.Added, epochPod(epochC))
	a := h.session.attempts[epochC]
	h.session.StopGeneration(old)
	if a.stopped || h.session.deliveryStopped {
		t.Fatal("old watch stopped replacement generation")
	}
	h.session.StopGeneration(g)
	if !a.stopped {
		t.Fatal("current generation not stopped")
	}
}

// epochExpiryReader advances the test clock only when consume asks for bytes
// after processing the first batch, including its synchronous HTTP PATCH.
type epochExpiryReader struct {
	io.Reader
	remaining int
	expire    func()
}

func (r *epochExpiryReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		r.expire()
		r.remaining = -1
	}
	if r.remaining > 0 && len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.Reader.Read(p)
	if r.remaining > 0 {
		r.remaining -= n
	}
	return n, err
}

func TestPodEpochRetryHTTPBudgetExpiryLogsOnce(t *testing.T) {
	for _, positive := range []bool{false, true} {
		name := "negative"
		if positive {
			name = "positive"
		}
		t.Run(name, func(t *testing.T) {
			h := newEpochHarness(t, epochPod(epochA))
			if positive {
				h.event(watch.Modified, epochTerminal(epochPod(epochA)))
			}
			now := time.Now()
			h.session.now = func() time.Time { return now }
			var logs bytes.Buffer
			h.session.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			var first, later strings.Builder
			emit := func(dst *strings.Builder, kind watch.EventType, p *corev1.Pod) {
				if err := json.NewEncoder(dst).Encode(map[string]any{"type": kind, "object": p}); err != nil {
					t.Fatal(err)
				}
			}
			emit(&first, watch.Added, epochPod(epochB))
			if positive {
				emit(&first, watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
			}
			for i := 0; i < 3; i++ {
				emit(&later, watch.Modified, withRV(epochWithIDs(epochPod(epochB), "cri-o://"), strings.Repeat("/later", i+1)))
			}
			var requests atomic.Int32
			client := epochHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPatch {
					requests.Add(1)
					w.WriteHeader(409)
					_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Failure", Reason: metav1.StatusReasonConflict, Code: 409})
					return
				}
				_, _ = io.WriteString(w, first.String()+later.String())
			})
			h.session.client = client
			owner := NewPodEpochWatch(client.CoreV1().RESTClient(), "docker", "containerd-runc", h.session)
			body, err := client.CoreV1().RESTClient().Get().Namespace("docker").Resource("pods").Param("watch", "true").Stream(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			rv := ""
			reader := &epochExpiryReader{Reader: body, remaining: first.Len(), expire: func() { now = now.Add(90 * time.Second) }}
			err = owner.consume(context.Background(), reader, &rv)
			_ = body.Close()
			if err != io.EOF || requests.Load() != 1 {
				t.Fatalf("requests=%d err=%v", requests.Load(), err)
			}
			a := h.session.attempts[epochB]
			if !a.stopped || a.attempts != 1 || h.session.records[epochB].pending != nil {
				t.Fatal("expiry did not permanently stop/release proof")
			}
			decoder := json.NewDecoder(strings.NewReader(logs.String()))
			failures, stops := 0, 0
			for decoder.More() {
				var record map[string]any
				if err := decoder.Decode(&record); err != nil {
					t.Fatal(err)
				}
				switch record["msg"] {
				case "pod epoch delivery request failed":
					failures++
					if record["reason"] != "conflict" {
						t.Fatal(record)
					}
				case "pod epoch delivery stopped":
					stops++
					if record["reason"] != "time_budget" || record["attempts"] != float64(1) || record["conflicts"] != float64(1) || record["elapsed"] != float64(90*time.Second) {
						t.Fatal(record)
					}
				default:
					t.Fatal("unexpected log", record)
				}
			}
			if failures != 1 || stops != 1 {
				t.Fatalf("expected one failed attempt and one stop: %s", logs.String())
			}
		})
	}
}

func TestPodEpochRetryStoppedStillTracksPredecessor(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	h.event(watch.Added, epochPod(epochB))
	// Expiry stops delivery, but B must still be observed as C's predecessor.
	h.session.attempts[epochB].firstSeen = h.session.now().Add(-podEpochDeliveryBudget)
	h.event(watch.Modified, epochTerminal(epochPod(epochB)))
	if !h.session.attempts[epochB].stopped || h.session.records[epochB].terminal == 0 {
		t.Fatal("stopped delivery lost terminal observation")
	}
	d := h.successor(epochPod(epochC))
	e, negative := epochReason(t, d)
	if negative != nil || e == nil || e.Predecessor != epochB {
		t.Fatal("stopped pod did not remain predecessor")
	}
}
