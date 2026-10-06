package kube

import (
	"context"
	"errors"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"testing"
)

func TestDinDLaterPodPagesKeepStopGateClosed(t *testing.T) {
	for _, failed := range []bool{false, true} {
		cfg := dockerTestConfig()
		m, client := newMat(t)
		m.cfg = cfg
		f := &maintenanceFake{op: &protocol.DindMaintenance{ID: "operation", Nonce: "nonce", RegisterNonce: "registration", DeploymentUID: "old", PVCUID: "old-pvc", Phase: "stopping", Fenced: true}}
		m.cordoner = f
		w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true, DindMaintenance: f.op}
		pages := 0
		client.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
			options := a.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
			pages++
			if pages == 1 {
				if options.Continue != "" || options.Limit != 256 {
					t.Fatalf("first page options %+v", options)
				}
				return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next"}, Items: make([]corev1.Pod, 256)}, nil
			}
			if pages != 2 || options.Continue != "next" {
				t.Fatalf("page/options %d %+v", pages, options)
			}
			if failed {
				return true, nil, errors.New("late page failed")
			}
			pod := corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "docker", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dindDataPVCName(w.ID)}}}}}}
			return true, &corev1.PodList{Items: []corev1.Pod{pod}}, nil
		})
		err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil)
		if (err != nil) != failed || pages != 2 {
			t.Fatalf("failed=%v pages=%d err=%v", failed, pages, err)
		}
		if f.op.Phase != "stopping" || len(f.calls) != 0 {
			t.Fatal("late page authorized a transition")
		}
		for _, a := range client.Actions() {
			if a.GetVerb() == "delete" || a.GetVerb() == "create" {
				t.Fatal("late page permitted resource mutation")
			}
		}
	}
}
