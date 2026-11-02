package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	ktesting "k8s.io/client-go/testing"
)

type epochResolverFunc func(context.Context, PodEpochWorker, *corev1.Pod, bool) (PodEpochIdentity, error)

func (f epochResolverFunc) ResolvePodEpoch(ctx context.Context, w PodEpochWorker, p *corev1.Pod, fresh bool) (PodEpochIdentity, error) {
	return f(ctx, w, p, fresh)
}

func epochConflict(h *epochHarness) *int {
	n := new(int)
	h.client.PrependReactor("patch", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		*n++
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "example", errors.New("private request details"))
	})
	return n
}

func TestPodEpochRetryClockBudget(t *testing.T) {
	for _, offset := range []time.Duration{90*time.Second - time.Nanosecond, 90 * time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
			now := time.Now()
			h.session.now = func() time.Time { return now }
			h.event(watch.Added, epochPod(epochB))
			now = now.Add(offset)
			d := h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
			if d == nil || h.session.attempts[epochB].stopped {
				t.Fatal("startup consumed positive delivery budget")
			}
		})
	}
	for _, positive := range []bool{false, true} {
		t.Run(fmt.Sprintf("identity-time-counts/positive=%v", positive), func(t *testing.T) {
			h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
			if !positive {
				h = newEpochHarness(t, epochPod(epochA))
			}
			now := time.Now()
			h.session.now = func() time.Time { return now }
			delegate := h.session.resolver
			h.session.resolver = epochResolverFunc(func(ctx context.Context, w PodEpochWorker, p *corev1.Pod, f bool) (PodEpochIdentity, error) {
				now = now.Add(90 * time.Second)
				return delegate.ResolvePodEpoch(ctx, w, p, f)
			})
			if h.event(watch.Added, epochPod(epochB)) != nil || h.session.attempts[epochB].stopped == positive {
				t.Fatal("incorrect resolver-time budget")
			}
			if positive && h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://")) == nil {
				t.Fatal("positive expired during resolver")
			}
		})
	}
	for _, positive := range []bool{false, true} {
		t.Run(fmt.Sprintf("duplicate-expiry/positive=%v", positive), func(t *testing.T) {
			h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
			now := time.Now()
			h.session.now = func() time.Time { return now }
			if !positive {
				h = newEpochHarness(t, epochPod(epochA))
				h.session.now = func() time.Time { return now }
			}
			p := epochPod(epochB)
			h.event(watch.Added, p)
			now = now.Add(90 * time.Second)
			if h.event(watch.Added, p) != nil || h.session.attempts[epochB].stopped == positive {
				t.Fatal("incorrect duplicate expiry")
			}
			if positive && h.event(watch.Modified, epochWithIDs(p, "containerd://")) == nil {
				t.Fatal("duplicate expired unanchored positive")
			}
		})
	}
	t.Run("deliver-expiry", func(t *testing.T) {
		h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
		now := time.Now()
		h.session.now = func() time.Time { return now }
		var logs bytes.Buffer
		h.session.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		h.event(watch.Added, epochPod(epochB))
		now = now.Add(120 * time.Second)
		d := h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
		epochAssertPendingPositive(t, d)
		now = now.Add(90 * time.Second)
		if h.session.Deliver(context.Background(), d) == nil || len(h.patches) != 0 {
			t.Fatal("expired issued decision submitted")
		}
		_ = h.session.Deliver(context.Background(), d)
		epochAssertBudgetStop(t, &logs)
	})
}

func epochAssertPendingPositive(t *testing.T, d *PodEpochDecision) {
	t.Helper()
	if d == nil {
		t.Fatal("delayed IDs did not issue a decision")
	}
	e, negative := epochReason(t, d)
	if e == nil || negative != nil {
		t.Fatal("delayed IDs did not issue positive evidence")
	}
}

func epochAssertBudgetStop(t *testing.T, logs *bytes.Buffer) {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
		t.Fatalf("expected exactly one stop log: %v: %s", err, logs.String())
	}
	if record["msg"] != "pod epoch delivery stopped" || record["reason"] != "time_budget" || record["attempts"] != float64(0) {
		t.Fatal(record)
	}
}

type epochRetryTransport func(*http.Request) (*http.Response, error)

func (f epochRetryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPodEpochRetryDeadlineAndLastMoment(t *testing.T) {
	for _, remaining := range []time.Duration{30 * time.Second, 2 * time.Second} {
		t.Run(remaining.String(), func(t *testing.T) {
			h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
			now := time.Now()
			h.session.now = func() time.Time { return now }
			h.event(watch.Added, epochPod(epochB))
			now = now.Add(120 * time.Second)
			d := h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
			epochAssertPendingPositive(t, d)
			now = now.Add(90*time.Second - remaining)
			requests := 0
			client, err := kubernetes.NewForConfig(&rest.Config{Host: "http://example.invalid", Transport: epochRetryTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				deadline, ok := r.Context().Deadline()
				left := time.Until(deadline)
				want := min(remaining, 10*time.Second)
				if !ok || left > want || left < want-time.Second {
					t.Errorf("deadline remaining=%s want <=%s", left, want)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader("{\"kind\":\"Pod\",\"apiVersion\":\"v1\"}"))}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			h.session.client = client
			if err := h.session.Deliver(context.Background(), d); err != nil || requests != 1 {
				t.Fatalf("requests=%d err=%v", requests, err)
			}
		})
	}
	t.Run("last-moment", func(t *testing.T) {
		h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
		start := time.Now()
		now := start
		h.session.now = func() time.Time { return now }
		var logs bytes.Buffer
		h.session.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		h.event(watch.Added, epochPod(epochB))
		anchor := start.Add(120 * time.Second)
		now = anchor
		d := h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
		epochAssertPendingPositive(t, d)
		checks := 0
		h.session.now = func() time.Time {
			checks++
			if checks >= 3 {
				return anchor.Add(90 * time.Second)
			}
			return anchor.Add(90*time.Second - time.Nanosecond)
		}
		if h.session.Deliver(context.Background(), d) == nil || len(h.patches) != 0 || h.session.attempts[epochB].attempts != 0 {
			t.Fatal("last-moment expiry submitted")
		}
		_ = h.session.Deliver(context.Background(), d)
		epochAssertBudgetStop(t, &logs)
	})
}

func TestPodEpochRetryFrozenResolverAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
			delegate := h.session.resolver
			calls := 0
			h.session.resolver = epochResolverFunc(func(ctx context.Context, w PodEpochWorker, p *corev1.Pod, f bool) (PodEpochIdentity, error) {
				calls++
				return delegate.ResolvePodEpoch(ctx, w, p, f)
			})
			h.event(watch.Added, epochPod(epochB))
			p := epochWithIDs(epochPod(epochB), "containerd://")
			if fallback {
				p.Status.ContainerStatuses[0].ContainerID = strings.Repeat("x", 20000)
			}
			d := h.event(watch.Modified, p)
			if d == nil {
				t.Fatal("no decision")
			}
			original := d.Document()
			n := epochConflict(h)
			if !apierrors.IsConflict(h.session.Deliver(context.Background(), d)) {
				t.Fatal("not conflict")
			}
			d = h.event(watch.Modified, withRV(epochWithIDs(epochPod(epochB), "cri-o://"), "/retry"))
			if d == nil || calls != 1 {
				t.Fatalf("decision=%v resolver calls=%d", d, calls)
			}
			if fallback {
				_, negative, err := DecodePodEpochDocument([]byte(original))
				if err != nil || negative == nil || d.Document() != original {
					t.Fatal("fallback upgraded or changed bytes")
				}
				if h.session.records[epochB].pending != nil {
					t.Fatal("fallback retained positive")
				}
			}
			if !apierrors.IsConflict(h.session.Deliver(context.Background(), d)) || *n != 2 {
				t.Fatal("no later retry")
			}
		})
	}
}

func TestPodEpochRetryIdentityUnverifiedLaterEvent(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	h.session.resolver = nil
	d := h.event(watch.Added, epochPod(epochB))
	_, negative := epochReason(t, d)
	if negative == nil || negative.Reason != "identity_unverified" {
		t.Fatal("missing explicit negative")
	}
	n := epochConflict(h)
	if !apierrors.IsConflict(h.session.Deliver(context.Background(), d)) || *n != 1 {
		t.Fatal("initial negative not delivered once")
	}
	retry := h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
	if retry == nil || retry.Document() != d.Document() || h.session.Deliver(context.Background(), d) == nil || *n != 1 {
		t.Fatal("frozen negative missing or old decision resubmitted")
	}
	if !apierrors.IsConflict(h.session.Deliver(context.Background(), retry)) || *n != 2 {
		t.Fatal("later negative did not retry")
	}
}

func TestPodEpochRetryIssuedStopAndNoResurrection(t *testing.T) {
	for _, mode := range []string{"terminal", "failed-unaudited", "deleted", "tainted", "label-removed", "superseded", "replacement", "reset", "generation-stop", "cancel"} {
		for _, afterConflict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/after-conflict=%v", mode, afterConflict), func(t *testing.T) {
				h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if d, err := h.session.Handle(ctx, h.generation, watch.Added, epochPod(epochB)); d != nil || err != nil {
					t.Fatal("unexpected Added", err)
				}
				d := h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
				if afterConflict {
					epochConflict(h)
					if !apierrors.IsConflict(h.session.Deliver(context.Background(), d)) {
						t.Fatal("expected first conflict")
					}
				}
				switch mode {
				case "terminal":
					h.event(watch.Modified, epochTerminal(epochPod(epochB)))
				case "failed-unaudited":
					p := withRV(epochPod(epochB), "/failed")
					p.Status.Phase = corev1.PodFailed
					p.Status.Reason = "Evicted"
					h.event(watch.Modified, p)
				case "deleted":
					h.event(watch.Deleted, epochPod(epochB))
				case "tainted":
					p := withRV(epochPod(epochB), "/mounts")
					p.Spec.Volumes = nil
					h.event(watch.Modified, p)
				case "label-removed":
					p := withRV(epochPod(epochB), "/label")
					p.Labels = nil
					h.event(watch.Modified, p)
				case "superseded":
					h.event(watch.Added, epochPod(epochC))
				case "replacement":
					p := epochPod(epochC)
					p.Name = epochPod(epochB).Name
					h.event(watch.Added, p)
				case "reset":
					g, err := h.session.Reset(context.Background(), []corev1.Pod{*epochPod(epochB)})
					if err != nil {
						t.Fatal(err)
					}
					h.generation = g
				case "generation-stop":
					h.session.StopGeneration(h.generation)
				case "cancel":
					cancel()
				}
				if h.session.Deliver(context.Background(), d) == nil || len(h.patches) != 0 {
					t.Fatal("stale issued decision submitted")
				}
				if h.event(watch.Modified, withRV(epochWithIDs(epochPod(epochB), "containerd://"), "/later")) != nil {
					t.Fatal("stopped UID resurrected")
				}
				r := h.session.records[epochB]
				if r == nil || r.pending != nil || r.pendingCost != 0 {
					t.Fatal("observation lost or payload retained")
				}
			})
		}
	}
}

func TestPodEpochRetryLogsAndAccounting(t *testing.T) {
	for _, outcome := range []string{"success", "conflicts", "maintenance-overflow", "reset"} {
		t.Run(outcome, func(t *testing.T) {
			h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
			var logs bytes.Buffer
			h.session.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			d := h.successor(epochPod(epochB))
			a := h.session.attempts[epochB]
			r := h.session.records[epochB]
			charged := h.session.bytes
			payload := r.pendingCost + a.issuedCost
			if payload == 0 {
				t.Fatal("uncharged payload")
			}
			switch outcome {
			case "success":
				if err := h.session.Deliver(context.Background(), d); err != nil {
					t.Fatal(err)
				}
				if h.session.bytes != charged-payload {
					t.Fatal("success payload accounting")
				}
			case "conflicts":
				epochConflict(h)
				for i := 0; i < 3; i++ {
					if !apierrors.IsConflict(h.session.Deliver(context.Background(), d)) {
						t.Fatal("not conflict")
					}
					if i < 2 {
						d = h.event(watch.Modified, withRV(epochWithIDs(epochPod(epochB), "containerd://"), fmt.Sprint(i)))
					}
				}
			case "maintenance-overflow":
				h.session.bytes = PodEpochMaxMetadata - 1
				if h.session.RecordMaintenanceRemint(h.generation, "w", PodEpochRemint{OperationID: "example", OldClaimUID: epochDinD, NewClaimUID: epochNewDinD}) == nil || !h.session.overflow || h.session.bytes != 0 {
					t.Fatal("receipt overflow not invalidated")
				}
			case "reset":
				_, err := h.session.Reset(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if outcome == "conflicts" {
				r = h.session.records[epochB]
			}
			if !a.stopped || r.pending != nil || r.pendingCost != 0 || a.negative != "" || a.issued != nil {
				t.Fatal("terminal payload retained")
			}
			text := logs.String()
			var records []map[string]any
			dec := json.NewDecoder(strings.NewReader(text))
			for dec.More() {
				var record map[string]any
				if err := dec.Decode(&record); err != nil {
					t.Fatal(err)
				}
				records = append(records, record)
			}
			requests, stops, success := 0, 0, 0
			for _, record := range records {
				switch record["msg"] {
				case "pod epoch delivery request failed":
					requests++
					if record["level"] != "WARN" || record["reason"] != "conflict" {
						t.Fatal(record)
					}
				case "pod epoch delivery stopped":
					stops++
				case "pod epoch delivery succeeded":
					success++
					if record["level"] != "INFO" {
						t.Fatal(record)
					}
				}
			}
			wantRequests := 0
			if outcome == "conflicts" {
				wantRequests = 3
			}
			if requests != wantRequests || stops+success != 1 || (outcome == "success" && success != 1) {
				t.Fatalf("unexpected lifecycle records %s", text)
			}
			for _, record := range records {
				if record["worker_id"] != "w" || record["pod"] != "pod-"+epochB || record["elapsed"] == nil || record["elapsed_since_ids"] == nil {
					t.Fatal("missing delivery context", record)
				}
				for _, value := range record {
					if value == epochB || value == "containerd://worker2" || value == "containerd://seed2" {
						t.Fatal("identifier leaked", record)
					}
				}
			}
			if strings.Contains(text, "private request") {
				t.Fatal("raw error leaked")
			}
			_ = h.session.Deliver(context.Background(), d)
			if outcome != "reset" && outcome != "maintenance-overflow" {
				h.event(watch.Modified, withRV(epochWithIDs(epochPod(epochB), "containerd://"), "/silent"))
			}
			if logs.String() != text {
				t.Fatal("stopped delivery logged again")
			}
		})
	}
}
