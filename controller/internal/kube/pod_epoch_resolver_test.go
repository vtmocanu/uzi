package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func TestPodEpochResolverTransport(t *testing.T) {
	for _, mode := range []string{"paginated", "actual-page-bytes", "actual-total-bytes"} {
		t.Run(mode, func(t *testing.T) {
			h := newEpochHarness(t)
			ctx := context.Background()
			rs, err := h.client.AppsV1().ReplicaSets("docker").Get(ctx, "rs", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			dep, err := h.client.AppsV1().Deployments("docker").Get(ctx, deploymentName("w"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.Query().Get("limit") != "100" || r.Header.Get("Accept") != "application/json" {
					t.Error("not a bounded JSON list", r.URL, r.Header)
				}
				w.Header().Set("Content-Type", "application/json")
				var list any
				switch {
				case strings.HasSuffix(r.URL.Path, "/replicasets"):
					if r.URL.Query().Get("fieldSelector") != "metadata.name=rs" {
						t.Error("missing RS selector")
					}
					l := &appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{*rs}}
					if mode == "paginated" && r.URL.Query().Get("continue") == "" {
						l.Items[0].Name = "foreign"
						l.Continue = "opaque-next"
					}
					list = l
				case strings.HasSuffix(r.URL.Path, "/deployments"):
					list = &appsv1.DeploymentList{Items: []appsv1.Deployment{*dep}}
				case strings.HasSuffix(r.URL.Path, "/persistentvolumeclaims"):
					name := strings.TrimPrefix(r.URL.Query().Get("fieldSelector"), "metadata.name=")
					uid := epochData
					pv := "data-pv"
					if name == dindDataPVCName("w") {
						uid = epochDinD
						pv = "dind-pv"
					}
					list = &corev1.PersistentVolumeClaimList{Items: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "docker", UID: types.UID(uid)}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pv}}}}
				default:
					t.Error("unexpected endpoint", r.URL)
				}
				if mode == "actual-page-bytes" {
					_, _ = fmt.Fprint(w, strings.Repeat(" ", (8<<20)+1))
				}
				if mode == "actual-total-bytes" {
					_, _ = fmt.Fprint(w, strings.Repeat(" ", 6<<20))
				}
				_ = json.NewEncoder(w).Encode(list)
			}))
			defer server.Close()
			cfg := &rest.Config{Host: server.URL}
			cfg.Wrap(WrapMaintenanceReads)
			client, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			result, err := (PodEpochKubeResolver{Client: client}).ResolvePodEpoch(ctx, h.worker, epochPod(epochB), true)
			if mode == "paginated" {
				if err != nil || result.Data.UID != epochData || calls != 5 {
					t.Fatal("paginated resolution", result, calls, err)
				}
			} else if err == nil {
				t.Fatal("actual byte overflow accepted")
			} else if mode == "actual-total-bytes" && calls != 3 {
				t.Fatalf("total byte budget calls=%d", calls)
			}
		})
	}
}

func TestPodEpochResolverPaginationRefusals(t *testing.T) {
	for _, mode := range []string{"duplicate-next-page", "repeat-token", "oversized-page", "item-budget"} {
		t.Run(mode, func(t *testing.T) {
			h := newEpochHarness(t)
			calls := 0
			h.client.PrependReactor("list", "replicasets", func(a ktesting.Action) (bool, runtime.Object, error) {
				calls++
				rs := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "docker", UID: "rs-uid"}}
				l := &appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{rs}}
				switch mode {
				case "duplicate-next-page":
					if calls == 1 {
						l.Continue = "next"
					}
				case "repeat-token":
					l.Items = nil
					l.Continue = "same"
				case "oversized-page":
					l.Items = make([]appsv1.ReplicaSet, 101)
				case "item-budget":
					l.Items = make([]appsv1.ReplicaSet, 100)
					l.Continue = fmt.Sprint(calls)
				}
				return true, l, nil
			})
			if _, err := (PodEpochKubeResolver{Client: h.client}).ResolvePodEpoch(context.Background(), h.worker, epochPod(epochB), true); err == nil {
				t.Fatal("ambiguous or unbounded list accepted")
			}
			if mode == "item-budget" && calls != 41 {
				t.Fatalf("item cap calls=%d", calls)
			}
		})
	}
}
