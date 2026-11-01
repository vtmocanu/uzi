package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// This file deliberately uses only pre-M1 seams, so it can be copied unchanged
// to the base commit for the lead's RED proof.
func TestPodEpochRetryHTTPWatchChurn(t *testing.T) {
	for _, positive := range []bool{false, true} {
		for _, outcome := range []string{"conflict-success", "conflict-limit", "429", "500", "transport", "response-cap"} {
			t.Run(fmt.Sprintf("positive=%v/%s", positive, outcome), func(t *testing.T) {
				var logs bytes.Buffer
				previousLogger := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
				defer slog.SetDefault(previousLogger)
				h := newEpochHarness(t, epochPod(epochA))
				if positive {
					h.event(watch.Modified, epochTerminal(epochPod(epochA)))
				}
				added := epochPod(epochB)
				first := epochWithIDs(added, "containerd://")
				second := withRV(epochWithIDs(added, "cri-o://"), "/changed")
				third := withRV(second, "/third")
				fourth := withRV(third, "/fourth")
				frames := []struct {
					kind watch.EventType
					pod  *corev1.Pod
				}{
					{watch.Added, added}, {watch.Modified, first}, {watch.Modified, first},
					// Late predecessor changes must not re-evaluate the Added verdict.
					{watch.Modified, withRV(epochTerminal(epochPod(epochA)), "/late")},
					{watch.Modified, second}, {watch.Modified, second}, {watch.Modified, third}, {watch.Modified, fourth},
				}
				if !positive {
					// The proof arrives before the first retry opportunity.
					frames = append(frames[:1], append([]struct {
						kind watch.EventType
						pod  *corev1.Pod
					}{
						{watch.Modified, withRV(epochTerminal(epochPod(epochA)), "/late-before-retry")},
					}, frames[1:]...)...)
				}
				var received []map[string]any
				var mu sync.Mutex
				patchCount := func() int { mu.Lock(); defer mu.Unlock(); return len(received) }
				streams := 0
				split := 1
				if positive {
					split = 2
				}
				client := epochHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet {
						streams++
						batch := frames[:split]
						if streams > 1 {
							batch = frames[split-1:]
						}
						for _, f := range batch {
							_ = json.NewEncoder(w).Encode(map[string]any{"type": f.kind, "object": f.pod})
						}
						return
					}
					var patch map[string]any
					if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
						t.Error(err)
					}
					received = append(received, patch)
					switch {
					case outcome == "transport":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
					case outcome == "response-cap":
						_, _ = io.WriteString(w, strings.Repeat("x", (8<<20)+1))
					case outcome == "conflict-limit" || (outcome == "conflict-success" && len(received) == 1):
						w.WriteHeader(409)
						_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Failure", Reason: metav1.StatusReasonConflict, Code: 409})
					case outcome == "429":
						w.Header().Set("Retry-After", "1")
						w.WriteHeader(429)
						_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Failure", Reason: metav1.StatusReasonTooManyRequests, Code: 429})
					case outcome == "500":
						w.WriteHeader(500)
						_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Failure", Reason: metav1.StatusReasonInternalError, Code: 500})
					default:
						p := second.DeepCopy()
						p.TypeMeta = metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"}
						_ = json.NewEncoder(w).Encode(p)
					}
				})
				h.session.client = client
				owner := NewPodEpochWatch(client.CoreV1().RESTClient(), "docker", "containerd-runc", h.session)
				rv := ""
				for pass := 0; pass < 2; pass++ {
					req := client.CoreV1().RESTClient().Get().Namespace("docker").Resource("pods").Param("watch", "true")
					if pass > 0 {
						req.Param("resourceVersion", rv)
					}
					stream, err := req.Stream(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					err = owner.consume(context.Background(), stream, &rv)
					_ = stream.Close()
					if err != io.EOF {
						t.Fatal(err)
					}
					if pass == 0 && (patchCount() != 1 || rv != frames[split-1].pod.ResourceVersion) {
						t.Fatal("first stream did not finish delivery before RV advance")
					}
				}
				mu.Lock()
				patches := append([]map[string]any(nil), received...)
				mu.Unlock()
				want := 1
				if outcome == "conflict-success" {
					want = 2
				}
				if outcome == "conflict-limit" {
					want = 3
				}
				if len(patches) != want {
					t.Fatalf("actual PATCH requests=%d want=%d", len(patches), want)
				}
				var records []map[string]any
				dec := json.NewDecoder(strings.NewReader(logs.String()))
				for dec.More() {
					var record map[string]any
					if err := dec.Decode(&record); err != nil {
						t.Fatal(err)
					}
					records = append(records, record)
				}
				wantReason := map[string]string{"conflict-limit": "conflict", "conflict-success": "conflict", "429": "rate_limited", "500": "server_error", "transport": "transport_error", "response-cap": "response_cap"}[outcome]
				wantFailures := want
				if outcome == "conflict-success" {
					wantFailures = 1
				}
				failures, final := 0, 0
				for _, record := range records {
					switch record["msg"] {
					case "pod epoch delivery request failed":
						failures++
						if record["level"] != "WARN" || record["reason"] != wantReason {
							t.Fatalf("unexpected failure record: %v", record)
						}
					case "pod epoch delivery stopped", "pod epoch delivery succeeded":
						final++
						if record["attempts"] != float64(want) {
							t.Fatal("incorrect attempts in final log", record)
						}
						elapsed, ok := record["elapsed"].(float64)
						if !ok || elapsed < 0 {
							t.Fatal("missing elapsed time", record)
						}
						if outcome == "conflict-success" && (record["conflicts"] != float64(1) || record["msg"] != "pod epoch delivery succeeded") {
							t.Fatal("success counters missing", record)
						}
						if outcome == "conflict-limit" && (record["conflicts"] != float64(3) || record["reason"] != "attempt_limit") {
							t.Fatal("exhaustion counters missing", record)
						}
					}
				}
				if failures != wantFailures || final != 1 || len(records) != failures+final {
					t.Fatalf("unbounded or missing attempt/stop logs: %s", logs.String())
				}
				if rv != fourth.ResourceVersion {
					t.Fatalf("resume RV=%q", rv)
				}
				docs := make([]string, len(patches))
				for i, patch := range patches {
					m := patch["metadata"].(map[string]any)
					if len(patch) != 1 || len(m) != 3 || m["uid"] != epochB {
						t.Fatal("patch target or scope changed")
					}
					docs[i] = m["annotations"].(map[string]any)[PodEpochAnnotation].(string)
				}
				if len(docs) > 1 {
					if !positive {
						for _, doc := range docs[1:] {
							if doc != docs[0] {
								t.Fatal("negative bytes changed")
							}
						}
					} else {
						a, _, err := DecodePodEpochDocument([]byte(docs[0]))
						if err != nil || a == nil {
							t.Fatal("initial positive absent", err)
						}
						b, _, err := DecodePodEpochDocument([]byte(docs[1]))
						if err != nil || b == nil {
							t.Fatal("retry positive absent", err)
						}
						if b.Attestation.Result != "contradicted" || b.SuccessorContainers[0].ContainerID != "cri-o://seed2" {
							t.Fatal("retry did not refresh successor attestation")
						}
						a.SuccessorContainers = nil
						a.Attestation = nil
						b.SuccessorContainers = nil
						b.Attestation = nil
						if !reflect.DeepEqual(a, b) {
							t.Fatal("frozen evidence fields changed")
						}
					}
				}
			})
		}
	}
}
