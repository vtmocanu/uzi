package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/watch"
)

// These behavioral regressions deliberately use only the pre-anchor test seams,
// so the same tests can run against the historical implementation.
func TestPodEpochAnchorAddedIDsWaitForChangedEvent(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	now := time.Now()
	h.session.now = func() time.Time { return now }
	p := epochWithIDs(epochPod(epochB), "containerd://")
	if h.event(watch.Added, p) != nil {
		t.Fatal("Added emitted")
	}
	now = now.Add(120 * time.Second)
	if h.event(watch.Added, p) != nil {
		t.Fatal("duplicate emitted")
	}
	d := h.event(watch.Modified, withRV(p, "/changed"))
	h.assertDecision(d, true)
}

func TestPodEpochAnchorInitOnlyAndNoRenewal(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
			start := time.Now()
			now := start
			h.session.now = func() time.Time { return now }
			var logs bytes.Buffer
			h.session.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			h.event(watch.Added, epochPod(epochB))
			now = start.Add(120 * time.Second)
			p := epochWithIDs(epochPod(epochB), "containerd://")
			p.Status.ContainerStatuses = nil
			p.Status.InitContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{}
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, corev1.ContainerStatus{Name: "later-init", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}})
			if fallback {
				p.Status.InitContainerStatuses[0].ContainerID = strings.Repeat("x", 20000)
			}
			d := h.event(watch.Modified, p)
			if d == nil {
				t.Fatal("init-only IDs did not anchor")
			}
			e, n := epochReason(t, d)
			if fallback {
				if n == nil || n.Reason != "evidence_invalid_or_oversize" {
					t.Fatal("missing fallback")
				}
			} else if e == nil || len(e.SuccessorContainers) != 1 || e.SuccessorContainers[0].Name != "seed" {
				t.Fatal("init-only evidence missing")
			}
			count := epochConflict(h)
			if !apierrors.IsConflict(h.session.Deliver(context.Background(), d)) {
				t.Fatal("expected conflict")
			}
			now = start.Add(209 * time.Second)
			p = withRV(epochWithIDs(epochPod(epochB), "cri-o://"), "/later-init")
			p.Status.ContainerStatuses = nil
			p.Status.InitContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 0}
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, corev1.ContainerStatus{Name: "later-init", ContainerID: "cri-o://later-seed", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
			d = h.event(watch.Modified, p)
			if d == nil {
				t.Fatal("expired before anchor budget")
			}
			if !apierrors.IsConflict(h.session.Deliver(context.Background(), d)) {
				t.Fatal("expected second conflict")
			}
			now = start.Add(210 * time.Second)
			if h.event(watch.Modified, withRV(p, "/expired")) != nil {
				t.Fatal("later IDs renewed budget")
			}
			if h.event(watch.Modified, withRV(p, "/still-expired")) != nil || *count != 2 {
				t.Fatal("expired attempt retried")
			}
			dec := json.NewDecoder(&logs)
			stops := 0
			for dec.More() {
				var r map[string]any
				if err := dec.Decode(&r); err != nil {
					t.Fatal(err)
				}
				if r["msg"] == "pod epoch delivery stopped" {
					stops++
					if r["reason"] != "time_budget" || r["elapsed"] != float64(210*time.Second) || r["elapsed_since_ids"] != float64(90*time.Second) {
						t.Fatal(r)
					}
				}
			}
			if stops != 1 {
				t.Fatal("expiry must log once", stops)
			}
		})
	}
}

func TestPodEpochAnchorLogsBeforeIDs(t *testing.T) {
	for _, negative := range []bool{false, true} {
		t.Run(fmt.Sprint(negative), func(t *testing.T) {
			h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
			if negative {
				h = newEpochHarness(t, epochPod(epochA))
			}
			now := time.Now()
			h.session.now = func() time.Time { return now }
			var logs bytes.Buffer
			h.session.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			h.event(watch.Added, epochPod(epochB))
			now = now.Add(time.Second)
			h.session.StopGeneration(h.generation)
			var record map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
				t.Fatal(err)
			}
			_, present := record["elapsed_since_ids"]
			if present != negative || record["elapsed"] != float64(time.Second) || record["worker_id"] != "w" || record["pod"] != "pod-"+epochB {
				t.Fatal(record)
			}
			if negative && record["elapsed_since_ids"] != float64(time.Second) {
				t.Fatal(record)
			}
		})
	}
}

func TestPodEpochAnchorHTTPStartupBeyondBudget(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	start := time.Now()
	now := start
	h.session.now = func() time.Time { return now }
	var first, later strings.Builder
	emit := func(dst *strings.Builder, kind watch.EventType, p *corev1.Pod) {
		if err := json.NewEncoder(dst).Encode(map[string]any{"type": kind, "object": p}); err != nil {
			t.Fatal(err)
		}
	}
	emit(&first, watch.Added, epochPod(epochB))
	emit(&later, watch.Modified, withRV(epochPod(epochB), "/still-starting"))
	ready := epochWithIDs(epochPod(epochB), "containerd://")
	emit(&later, watch.Modified, ready)
	requests := 0
	client := epochHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, first.String()+later.String())
			return
		}
		requests++
		var body struct {
			Metadata struct {
				UID             string            `json:"uid"`
				ResourceVersion string            `json:"resourceVersion"`
				Annotations     map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Error(err)
		}
		var fields map[string]map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodPatch || r.Header.Get("Content-Type") != "application/merge-patch+json" || len(fields) != 1 || len(fields["metadata"]) != 3 || len(body.Metadata.Annotations) != 1 || body.Metadata.ResourceVersion != ready.ResourceVersion {
			t.Errorf("patch must contain only UID, exact resourceVersion and epoch annotation: %s", payload)
		}
		e, n, err := DecodePodEpochDocument([]byte(body.Metadata.Annotations[PodEpochAnnotation]))
		if err != nil || e == nil || n != nil || body.Metadata.UID != epochB {
			t.Error("missing positive patch", err)
		}
		_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"Pod"}`)
	})
	h.session.client = client
	owner := NewPodEpochWatch(client.CoreV1().RESTClient(), "docker", "containerd-runc", h.session)
	rv := ""
	body, err := client.CoreV1().RESTClient().Get().Namespace("docker").Resource("pods").Param("watch", "true").Stream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := &epochExpiryReader{Reader: body, remaining: first.Len(), expire: func() { now = start.Add(121 * time.Second) }}
	err = owner.consume(context.Background(), reader, &rv)
	_ = body.Close()
	if err != io.EOF {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatal("startup prevented submission", requests)
	}
}
