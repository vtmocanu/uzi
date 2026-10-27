package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func epochHTTPClient(t *testing.T, handler http.HandlerFunc) *kubernetes.Clientset {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := &rest.Config{Host: server.URL}
	cfg.Wrap(WrapMaintenanceReads)
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func epochAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		var v T
		return v
	}
}
func TestPodEpochWatchHTTPSerialResumeAnd410(t *testing.T) {
	h := newEpochHarness(t)
	ctx := context.Background()
	deps, _ := h.client.AppsV1().Deployments("docker").List(ctx, metav1.ListOptions{})
	sets, _ := h.client.AppsV1().ReplicaSets("docker").List(ctx, metav1.ListOptions{})
	claims, _ := h.client.CoreV1().PersistentVolumeClaims("docker").List(ctx, metav1.ListOptions{})
	old := epochPod(epochA)
	next := epochPod(epochB)
	next.ResourceVersion = "rv/not-a-number"
	patches := make(chan map[string]any, 4)
	watches := make(chan string, 4)
	release := make(chan struct{})
	releaseSecond := make(chan struct{})
	var lists, watchCount atomic.Int32
	var mu sync.Mutex
	var wireErrors []string
	bad := func(s string) { mu.Lock(); wireErrors = append(wireErrors, s); mu.Unlock() }
	client := epochHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(r.URL.Path, "/namespaces/docker/") {
			bad(r.URL.Path)
		}
		if r.Method == http.MethodPatch {
			var patch map[string]any
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				bad(err.Error())
			}
			patches <- patch
			response := next.DeepCopy()
			response.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/persistentvolumeclaims"):
			_ = json.NewEncoder(w).Encode(claims)
		case strings.HasSuffix(r.URL.Path, "/replicasets"):
			_ = json.NewEncoder(w).Encode(sets)
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			_ = json.NewEncoder(w).Encode(deps)
		case strings.HasSuffix(r.URL.Path, "/pods"):
			if r.URL.Query().Get("labelSelector") != "" || r.URL.Query().Get("fieldSelector") != "" {
				bad("filtered pods")
			}
			if r.URL.Query().Get("watch") != "true" {
				lists.Add(1)
				if r.URL.Query().Get("limit") != "100" {
					bad("wrong page size")
				}
				_ = json.NewEncoder(w).Encode(corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "opaque/list"}, Items: []corev1.Pod{*old}})
				return
			}
			n := watchCount.Add(1)
			watches <- r.URL.Query().Get("resourceVersion")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			emit := func(kind string, obj any) {
				_ = json.NewEncoder(w).Encode(map[string]any{"type": kind, "object": obj})
				w.(http.Flusher).Flush()
			}
			if n == 1 {
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				emit("MODIFIED", epochTerminal(old))
				emit("ADDED", next)
				emit("ADDED", next)
				return
			}
			if n == 2 {
				select {
				case <-releaseSecond:
				case <-r.Context().Done():
					return
				}
				emit("ERROR", metav1.Status{Code: 410, Reason: metav1.StatusReasonExpired})
				return
			}
			<-r.Context().Done()
		default:
			bad(r.URL.Path)
			http.NotFound(w, r)
		}
	})
	session := NewPodEpochSession(client, PodEpochKubeResolver{Client: client}, nil)
	owner := NewPodEpochWatch(client.CoreV1().RESTClient(), "docker", "containerd-runc", session)
	owner.Start(ctx)
	defer owner.Stop()
	if !owner.WaitForSync(ctx, time.After(5*time.Second)) {
		t.Fatal("not synced")
	}
	initial := session.CurrentGeneration()
	if rv := epochAwait(t, watches); rv != "opaque/list" {
		t.Fatal(rv)
	}
	close(release)
	patch := epochAwait(t, patches)
	metadata := patch["metadata"].(map[string]any)
	if len(patch) != 1 || len(metadata) != 3 || metadata["uid"] != epochB || metadata["resourceVersion"] != next.ResourceVersion {
		t.Fatalf("patch=%v", patch)
	}
	annotations := metadata["annotations"].(map[string]any)
	if len(annotations) != 1 {
		t.Fatal(annotations)
	}
	evidence, _, err := DecodePodEpochDocument([]byte(annotations[PodEpochAnnotation].(string)))
	if err != nil || evidence == nil {
		t.Fatalf("positive evidence absent: %v %v", annotations, err)
	}
	if rv := epochAwait(t, watches); rv != next.ResourceVersion {
		t.Fatalf("resume rv=%q", rv)
	}
	if session.CurrentGeneration() != initial {
		t.Fatal("resume reset session")
	}
	close(releaseSecond)
	_ = epochAwait(t, watches)
	if session.CurrentGeneration() == initial || lists.Load() != 2 {
		t.Fatal("410 did not relist")
	}
	select {
	case <-patches:
		t.Fatal("duplicate patch")
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	if len(wireErrors) > 0 {
		t.Fatal(wireErrors)
	}
}
func TestPodEpochWatchHTTPResetAndJoin(t *testing.T) {
	for _, mode := range []string{"http-410", "decode", "unknown", "oversize", "transport"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			observed := make(chan string, 3)
			release := make(chan struct{})
			client := epochHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Query().Get("watch") != "true" {
					_ = json.NewEncoder(w).Encode(corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "opaque/base"}})
					return
				}
				n := calls.Add(1)
				if n == 1 {
					observed <- r.URL.Query().Get("resourceVersion")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					// HTTP status rejection is tested on the reconnect below.
					switch mode {
					case "decode":
						_, _ = io.WriteString(w, "invalid\n")
					case "unknown":
						_, _ = io.WriteString(w, "{\"type\":\"UNKNOWN\",\"object\":{}}\n")
					case "oversize":
						_, _ = io.WriteString(w, strings.Repeat("x", (2<<20)+1)+"\n")
					}
					return
				}
				if n == 2 && mode == "http-410" {
					w.WriteHeader(http.StatusGone)
					_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Code: 410, Reason: metav1.StatusReasonGone})
					return
				}
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				observed <- r.URL.Query().Get("resourceVersion")
				<-r.Context().Done()
			})
			session := NewPodEpochSession(client, nil, nil)
			owner := NewPodEpochWatch(client.CoreV1().RESTClient(), "docker", "", session)
			owner.Start(context.Background())
			defer owner.Stop()
			_ = epochAwait(t, observed)
			first := session.CurrentGeneration()
			close(release)
			if rv := epochAwait(t, observed); rv != "opaque/base" {
				t.Fatal(rv)
			}
			changed := session.CurrentGeneration() != first
			if changed != (mode != "transport") {
				t.Fatalf("mode=%s generation changed=%v", mode, changed)
			}
			// Stop cancels a quiet accepted HTTP stream and joins its sole goroutine.
			owner.Stop()
		})
	}
}

func TestPodEpochWatchHTTPStartupTimeoutFence(t *testing.T) {
	h := newEpochHarness(t)
	ctx := context.Background()
	old, unsynced, next := epochPod(epochA), epochPod(epochB), epochPod(epochC)
	firstWatch := make(chan struct{})
	release := make(chan struct{})
	freshWatch := make(chan struct{})
	proceed := make(chan struct{})
	patches := make(chan string, 8)
	h.client.PrependReactor("patch", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		var patch struct {
			Metadata struct {
				UID         string            `json:"uid"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(a.(ktesting.PatchAction).GetPatch(), &patch); err != nil {
			return true, nil, err
		}
		e, _, err := DecodePodEpochDocument([]byte(patch.Metadata.Annotations[PodEpochAnnotation]))
		if e != nil {
			patches <- patch.Metadata.UID
		}
		return true, &corev1.Pod{}, err
	})
	var lists, watches atomic.Int32
	client := epochHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") != "true" {
			items, rv := []corev1.Pod{*old}, "before"
			if lists.Add(1) > 1 {
				items, rv = []corev1.Pod{*unsynced}, "after"
			}
			_ = json.NewEncoder(w).Encode(corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: rv}, Items: items})
			return
		}
		if watches.Add(1) == 1 {
			close(firstWatch)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			// Headers and both queued events arrive only after the startup timeout.
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "MODIFIED", "object": epochTerminal(old)})
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "ADDED", "object": unsynced})
			w.(http.Flusher).Flush()
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if r.URL.Query().Get("resourceVersion") != "after" {
			t.Error("fresh watch did not use fresh baseline")
		}
		close(freshWatch)
		select {
		case <-proceed:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "MODIFIED", "object": epochTerminal(unsynced)})
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "ADDED", "object": next})
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	owner := NewPodEpochWatch(client.CoreV1().RESTClient(), "docker", "containerd-runc", h.session)
	owner.Start(ctx)
	defer owner.Stop()
	epochAwait(t, firstWatch)
	generation := h.session.CurrentGeneration()
	timeout := make(chan time.Time, 1)
	timeout <- time.Time{}
	if owner.WaitForSync(ctx, timeout) {
		t.Fatal("unsynced watch accepted")
	}
	close(release)
	epochAwait(t, freshWatch)
	if lists.Load() != 2 || h.session.CurrentGeneration() == generation {
		t.Fatal("timeout did not invalidate and relist")
	}
	if !owner.WaitForSync(ctx, nil) {
		t.Fatal("fresh subscription did not signal readiness")
	}
	select {
	case uid := <-patches:
		t.Fatalf("fenced successor received positive patch: %s", uid)
	default:
	}
	close(proceed)
	if uid := epochAwait(t, patches); uid != epochC {
		t.Fatalf("positive successor = %s, want legitimate T", uid)
	}
}

func TestPodEpochWatchStartupTimeout(t *testing.T) {
	owner := NewPodEpochWatch(nil, "docker", "", nil)
	timeout := make(chan time.Time, 1)
	timeout <- time.Time{}
	if owner.WaitForSync(context.Background(), timeout) {
		t.Fatal("unsynced timeout accepted")
	}
	close(owner.synced)
	if !owner.WaitForSync(context.Background(), nil) {
		t.Fatal("established watch not accepted")
	}
}
func TestPodEpochWatchBoundedList(t *testing.T) {
	for _, mode := range []string{"pagination", "oversize", "too-many", "malformed", "namespace", "repeated", "rv-change", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			client := epochHTTPClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Query().Get("limit") != "100" || r.URL.Query().Get("labelSelector") != "" || r.URL.Query().Get("fieldSelector") != "" {
					t.Error("LIST must be unfiltered and bounded")
				}
				if mode == "oversize" {
					_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"v"},"items":[],"padding":"`+strings.Repeat("x", 8<<20)+`"}`)
					return
				}
				if mode == "malformed" {
					_, _ = io.WriteString(w, "{")
					return
				}
				list := corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "opaque/a"}}
				switch mode {
				case "pagination":
					if calls == 1 {
						list.Continue = "token"
					} else if r.URL.Query().Get("continue") != "token" {
						t.Error("lost token")
					}
				case "too-many":
					list.Items = make([]corev1.Pod, 101)
				case "namespace":
					list.Items = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Namespace: "other"}}}
				case "repeated":
					list.Continue = "same"
				case "rv-change":
					if calls == 1 {
						list.Continue = "token"
					} else {
						list.ResourceVersion = "opaque/b"
					}
				case "overflow":
					list.Items = make([]corev1.Pod, 100)
					for i := range list.Items {
						list.Items[i] = corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "docker", UID: "foreign"}}
					}
					list.Continue = fmt.Sprint(calls)
				}
				_ = json.NewEncoder(w).Encode(list)
			})
			session := NewPodEpochSession(client, nil, nil)
			owner := NewPodEpochWatch(client.CoreV1().RESTClient(), "docker", "", session)
			rv, err := owner.baseline(context.Background())
			if mode == "pagination" {
				if err != nil || rv != "opaque/a" || calls != 2 {
					t.Fatalf("rv=%s calls=%d err=%v", rv, calls, err)
				}
			} else if err == nil {
				t.Fatal("invalid LIST accepted")
			}
		})
	}
}
func TestPodEpochWatchFrameBeforeDecode(t *testing.T) {
	for _, raw := range []string{
		`{"type":"ADDED","object":{"metadata":{"namespace":"docker","uid":"foreign","resourceVersion":"new"}},"padding":"` + strings.Repeat("x", 2<<20) + `\"}` + "\n",
		"{bad}\n",
		"{\"type\":\"BOOKMARK\",\"object\":{}}\n",
		"{\"type\":\"ERROR\",\"object\":{\"code\":410}}\n",
		"{\"type\":\"ERROR\",\"object\":{}}\n",
		"{\"type\":\"ADDED\",\"object\":{\"metadata\":{\"namespace\":\"other\",\"resourceVersion\":\"x\"}}}\n",
	} {
		h := newEpochHarness(t)
		owner := NewPodEpochWatch(nil, "docker", "containerd-runc", h.session)
		rv := "last/delivered"
		if err := owner.consume(context.Background(), strings.NewReader(raw), &rv); err != errPodEpochReset {
			t.Fatalf("error=%v", err)
		}
		if rv != "last/delivered" {
			t.Fatal("failed frame advanced RV")
		}
	}
}
