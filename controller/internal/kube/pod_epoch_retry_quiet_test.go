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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestPodEpochRetryQuietWatchStopJoinsAfterRelease(t *testing.T) {
	for _, issued := range []bool{false, true} {
		name := "pending"
		if issued {
			name = "conflicted"
		}
		t.Run(name, func(t *testing.T) {
			h := newEpochHarness(t)
			var logs bytes.Buffer
			h.session.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			resolved := make(chan struct{})
			patched := make(chan struct{})
			delegate := h.session.resolver
			h.session.resolver = epochResolverFunc(func(ctx context.Context, w PodEpochWorker, p *corev1.Pod, f bool) (PodEpochIdentity, error) {
				id, err := delegate.ResolvePodEpoch(ctx, w, p, f)
				if string(p.UID) == epochB {
					close(resolved)
				}
				return id, err
			})
			client := epochHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPatch {
					w.WriteHeader(409)
					_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Failure", Reason: metav1.StatusReasonConflict, Code: 409})
					close(patched)
					return
				}
				if r.URL.Query().Get("watch") != "true" {
					_ = json.NewEncoder(w).Encode(corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "baseline"}, Items: []corev1.Pod{*epochTerminal(epochPod(epochA))}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"type": watch.Added, "object": epochPod(epochB)})
				if issued {
					_ = json.NewEncoder(w).Encode(map[string]any{"type": watch.Modified, "object": epochWithIDs(epochPod(epochB), "containerd://")})
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			h.session.client = client
			owner := NewPodEpochWatch(client.CoreV1().RESTClient(), "docker", "containerd-runc", h.session)
			owner.Start(context.Background())
			defer owner.Stop()
			epochAwait(t, resolved)
			h.session.events.Lock()
			h.session.mu.Lock()
			observed := h.session.records[epochB] != nil && h.session.records[epochB].pending != nil
			h.session.mu.Unlock()
			h.session.events.Unlock()
			if !observed {
				t.Fatal("Added proof not retained before quiet cancellation")
			}
			if issued {
				epochAwait(t, patched)
				// The server receipt precedes client-side outcome classification.
				// Join that lock-held Deliver before cancelling the quiet stream.
				h.session.mu.Lock()
				a := h.session.attempts[epochB]
				conflicted := a != nil && a.conflicts == 1 && !a.stopped
				h.session.mu.Unlock()
				if !conflicted {
					t.Fatal("conflict was not retained before quiet cancellation")
				}
			}
			owner.Stop()
			h.generation = h.session.CurrentGeneration()
			h.session.mu.Lock()
			a := h.session.attempts[epochB]
			r := h.session.records[epochB]
			stopped := a != nil && a.stopped && a.issued == nil && r != nil && r.pending == nil && r.pendingCost == 0
			h.session.mu.Unlock()
			if !stopped {
				t.Fatal("Stop joined before stopping/releasing quiet delivery")
			}
			logged := logs.String()
			if strings.Count(logged, `"msg":"pod epoch delivery stopped"`) != 1 || !strings.Contains(logged, `"reason":"watch_stopped"`) {
				t.Fatalf("missing or duplicate shutdown stop: %s", logged)
			}
			if h.event(watch.Modified, withRV(epochWithIDs(epochPod(epochB), "containerd://"), "/fresh-context")) != nil {
				t.Fatal("quiet cancellation resurrected UID")
			}
			if logs.String() != logged {
				t.Fatal("stopped watch logged on fresh-context replay")
			}
		})
	}
}

func TestPodEpochRetryHTTPTimeoutNeverRetriesLaterEvent(t *testing.T) {
	h := epochReadyHarness(t, epochTerminal(epochPod(epochA)))
	now := time.Now()
	h.session.now = func() time.Time { return now }
	h.event(watch.Added, epochPod(epochB))
	decision := h.event(watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
	now = now.Add(90*time.Second - 25*time.Millisecond)
	var requests atomic.Int32
	client := epochHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			requests.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		for _, frame := range []struct {
			kind watch.EventType
			p    *corev1.Pod
		}{
			{watch.Added, epochPod(epochB)},
			{watch.Modified, epochWithIDs(epochPod(epochB), "containerd://")},
			{watch.Modified, withRV(epochWithIDs(epochPod(epochB), "cri-o://"), "/later")},
		} {
			_ = json.NewEncoder(w).Encode(map[string]any{"type": frame.kind, "object": frame.p})
		}
	})
	h.session.client = client
	if err := h.session.Deliver(context.Background(), decision); err == nil {
		t.Fatal("expected timeout")
	}
	owner := NewPodEpochWatch(client.CoreV1().RESTClient(), "docker", "containerd-runc", h.session)
	stream, err := client.CoreV1().RESTClient().Get().Namespace("docker").Resource("pods").Param("watch", "true").Stream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rv := ""
	err = owner.consume(context.Background(), stream, &rv)
	_ = stream.Close()
	if err != io.EOF || requests.Load() != 1 {
		t.Fatalf("requests=%d err=%v", requests.Load(), err)
	}
	a := h.session.attempts[epochB]
	if !a.stopped || a.attempts != 1 {
		t.Fatal("timeout not permanently stopped")
	}
}
