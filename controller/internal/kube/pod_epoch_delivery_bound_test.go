package kube

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestPodEpochDeliveryResponseBound(t *testing.T) {
	h := newEpochHarness(t, epochPod(epochA))
	h.event(watch.Modified, epochTerminal(epochPod(epochA)))
	d := h.successor(epochPod(epochB))
	if d == nil {
		t.Fatal("missing successor decision")
	}
	evidence, _, err := DecodePodEpochDocument([]byte(d.Document()))
	if err != nil || evidence == nil {
		t.Fatalf("expected legitimate positive decision: %v", err)
	}

	response := epochPod(epochB)
	response.APIVersion = "v1"
	response.Kind = "Pod"
	response.Annotations = map[string]string{"padding": strings.Repeat("x", (8<<20)+1)}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/namespaces/docker/pods/"+d.name || r.Header.Get("Content-Type") != "application/merge-patch+json" {
			t.Error("unexpected patch request", r.Method, r.URL, r.Header)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Error(err)
		}
		want := map[string]any{"metadata": map[string]any{
			"uid": d.uid, "resourceVersion": d.rv,
			"annotations": map[string]any{PodEpochAnnotation: d.Document()},
		}}
		if !reflect.DeepEqual(got, want) {
			t.Error("patch must contain only the exact UID, resourceVersion and evidence annotation")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // Force chunking without a Content-Length.
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	cfg := &rest.Config{Host: server.URL}
	cfg.Wrap(WrapMaintenanceReads)
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.session.client = client
	err = h.session.Deliver(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "http: request body too large") {
		t.Errorf("expected actual response cap error, got %v", err)
	} else {
		t.Logf("actual response cap error: %v", err)
	}
	if err := h.session.Deliver(context.Background(), d); err == nil || err.Error() != "attempt already submitted" {
		t.Errorf("ambiguous delivery retry must be refused, got %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("request attempts=%d, want exactly one", got)
	}
}
