package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// Structural assertions are separate from the historical behavioral suite.
func TestPodEpochAnchorStateAndNameAccounting(t *testing.T) {
	p := epochPod(epochB)
	p.Name = "captured-name"
	control := newEpochHarness(t, epochPod(epochA))
	control.session.workers = nil
	before := control.session.bytes
	control.event(watch.Added, p)
	observation := control.session.bytes - before

	h := newEpochHarness(t, epochPod(epochA))
	before = h.session.bytes
	delegate := h.session.resolver
	h.session.resolver = epochResolverFunc(func(ctx context.Context, w PodEpochWorker, pod *corev1.Pod, fresh bool) (PodEpochIdentity, error) {
		if got, want := h.session.bytes-before, observation+256+len(epochB)+len(w.ID)+len(p.Name); got != want {
			t.Fatalf("allocation=%d want=%d", got, want)
		}
		return delegate.ResolvePodEpoch(ctx, w, pod, fresh)
	})
	now := time.Time{}
	h.session.now = func() time.Time { return now }
	h.event(watch.Added, p)
	a := h.session.attempts[epochB]
	if !a.hasAnchor || !a.anchor.IsZero() || !a.firstSeen.IsZero() || a.pod != p.Name {
		t.Fatal("zero-time negative anchor or name lost")
	}
	var logs bytes.Buffer
	h.session.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	p = withRV(p.DeepCopy(), "/renamed")
	p.Name = "later-name"
	h.event(watch.Modified, p)
	calls := 0
	h.session.now = func() time.Time { calls++; return now.Add(time.Duration(calls) * time.Second) }
	h.session.StopGeneration(h.generation)
	var r map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &r); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || r["pod"] != "captured-name" || r["elapsed"] != float64(time.Second) || r["elapsed_since_ids"] != r["elapsed"] {
		t.Fatal("record clock/name mismatch", calls, r)
	}
}
