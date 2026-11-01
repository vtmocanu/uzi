package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
	anchor := time.Now()
	now := anchor
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
	emit(&first, watch.Modified, epochWithIDs(epochPod(epochB), "containerd://"))
	emit(&later, watch.Modified, withRV(epochWithIDs(epochPod(epochB), "cri-o://"), "/retry"))
	emit(&later, watch.Modified, withRV(epochWithIDs(epochPod(epochB), "containerd://"), "/later"))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			if requests.Add(1) == 1 {
				w.WriteHeader(409)
				_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Failure", Reason: metav1.StatusReasonConflict, Code: 409})
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, first.String()+later.String())
	}))
	t.Cleanup(server.Close)
	patches := 0
	cfg := &rest.Config{Host: server.URL}
	cfg.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return epochRetryTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPatch {
				patches++
				if patches == 2 {
					deadline, ok := r.Context().Deadline()
					left := time.Until(deadline)
					if !ok || left <= 0 || left > 25*time.Millisecond {
						t.Errorf("retry deadline remaining=%s, want positive and <=25ms", left)
					}
				}
			}
			return next.RoundTrip(r)
		})
	})
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.session.client = client
	owner := NewPodEpochWatch(client.CoreV1().RESTClient(), "docker", "containerd-runc", h.session)
	stream, err := client.CoreV1().RESTClient().Get().Namespace("docker").Resource("pods").Param("watch", "true").Stream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rv := ""
	reader := &epochExpiryReader{Reader: stream, remaining: first.Len(), expire: func() {
		if a := h.session.attempts[epochB]; a == nil || a.attempts != 1 || a.conflicts != 1 || a.stopped {
			t.Fatal("first watch batch did not synchronously retain conflict")
		}
		now = anchor.Add(90*time.Second - 25*time.Millisecond)
	}}
	err = owner.consume(context.Background(), reader, &rv)
	_ = stream.Close()
	if err != io.EOF || requests.Load() != 2 || patches != 2 {
		t.Fatalf("requests=%d patches=%d err=%v", requests.Load(), patches, err)
	}
	a := h.session.attempts[epochB]
	if a == nil || !a.stopped || a.attempts != 2 || a.conflicts != 1 {
		t.Fatal("timeout not permanently stopped after conflict and retry")
	}
	decoder := json.NewDecoder(&logs)
	stops := 0
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "pod epoch delivery stopped" {
			stops++
			if record["reason"] != "timeout" || record["attempts"] != float64(2) || record["conflicts"] != float64(1) {
				t.Fatal(record)
			}
		}
	}
	if stops != 1 {
		t.Fatal("timeout must stop exactly once", stops)
	}
}
