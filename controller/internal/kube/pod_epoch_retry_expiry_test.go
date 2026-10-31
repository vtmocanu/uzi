package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/watch"
)

func TestPodEpochRetryOtherUIDExpiry(t *testing.T) {
	for _, trigger := range []string{"unregistered", "duplicate", "deliver", "repeat-deliver"} {
		for _, conflicted := range []bool{false, true} {
			name := trigger + "/pending"
			if conflicted {
				name = trigger + "/conflicted"
			}
			t.Run(name, func(t *testing.T) {
				h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
				start := time.Now()
				now := start
				h.session.now = func() time.Time { return now }
				h.event(watch.Added, epochPod(epochB))
				a := h.session.attempts[epochB]
				r := h.session.records[epochB]
				var bDecision *PodEpochDecision
				if conflicted {
					bDecision = h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
					epochConflict(h)
					if !apierrors.IsConflict(h.session.Deliver(context.Background(), bDecision)) {
						t.Fatal("expected B conflict")
					}
					r = h.session.records[epochB]
				}
				wantAttempts := a.attempts
				foreign := epochPod(epochC)
				foreign.Labels[LabelWorkerID] = "other"
				var otherDecision *PodEpochDecision
				if trigger == "duplicate" {
					// A is registered and already observed; this is an exact duplicate.
					foreign = epochTerminal(epochPod(epochA))
				}
				if trigger == "deliver" || trigger == "repeat-deliver" {
					now = start.Add(30 * time.Second)
					if err := h.session.RegisterWorker(PodEpochWorker{ID: "other", Runtime: "containerd-runc"}); err != nil {
						t.Fatal(err)
					}
					// Supply a verified claim snapshot through the resolver seam.
					// With no predecessor for this worker, Handle issues a negative.
					identity := *r.identity
					h.session.resolver = epochResolverFunc(func(context.Context, PodEpochWorker, *corev1.Pod, bool) (PodEpochIdentity, error) {
						return identity, nil
					})
					otherDecision = h.event(watch.Added, foreign)
					_, negative := epochReason(t, otherDecision)
					if negative == nil || negative.Reason != "predecessor_unproven" {
						t.Fatal("missing legitimate second-worker decision")
					}
					if trigger == "repeat-deliver" {
						epochConflict(h)
						if !apierrors.IsConflict(h.session.Deliver(context.Background(), otherDecision)) {
							t.Fatal("expected other worker conflict")
						}
					}
				}
				var logs bytes.Buffer
				h.session.logger = slog.New(slog.NewJSONHandler(&logs, nil))
				charged := h.session.bytes
				payload := r.pendingCost + a.negativeCost + a.issuedCost
				if payload == 0 || a.stopped {
					t.Fatal("B payload not retained before expiry")
				}
				now = start.Add(90 * time.Second)
				if trigger == "deliver" {
					epochConflict(h)
				}
				if otherDecision != nil {
					err := h.session.Deliver(context.Background(), otherDecision)
					if err == nil || (trigger == "deliver" && !apierrors.IsConflict(err)) {
						t.Fatal("expected decision refusal or conflict", err)
					}
				} else if h.event(watch.Added, foreign) != nil {
					t.Fatal("unexpected foreign decision")
				}
				if !a.stopped || a.attempts != wantAttempts || r.pending != nil || r.pendingCost != 0 || a.issued != nil || a.negative != "" {
					t.Fatal("other UID event did not stop/release B without spending an attempt")
				}
				if otherDecision != nil && h.session.attempts[epochC].stopped {
					t.Fatal("later-started delivery expired with B")
				}
				if trigger == "duplicate" && h.session.bytes != charged-payload {
					t.Fatal("duplicate expiry did not refund exactly B's payload")
				}
				decoder := json.NewDecoder(strings.NewReader(logs.String()))
				stops := 0
				for decoder.More() {
					var record map[string]any
					if err := decoder.Decode(&record); err != nil {
						t.Fatal(err)
					}
					if record["msg"] == "pod epoch delivery stopped" {
						stops++
						if record["reason"] != "time_budget" || record["attempts"] != float64(wantAttempts) || record["elapsed"] != float64(90*time.Second) {
							t.Fatal(record)
						}
					}
				}
				if stops != 1 {
					t.Fatalf("expected one expiry stop: %s", logs.String())
				}
				savedLogs := logs.String()
				if h.event(watch.Modified, withRV(epochWithIDs(epochPod(epochB), "containerd://"), "/after-expiry")) != nil {
					t.Fatal("expired B resurrected")
				}
				if bDecision != nil && h.session.Deliver(context.Background(), bDecision) == nil {
					t.Fatal("expired B decision submitted")
				}
				if h.event(watch.Added, foreign) != nil || logs.String() != savedLogs || a.attempts != wantAttempts {
					t.Fatal("subsequent events retried or logged stopped B")
				}
			})
		}
	}
}
