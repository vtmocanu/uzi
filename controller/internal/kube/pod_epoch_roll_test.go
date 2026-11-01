package kube

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestPodEpochRuntimeRoll(t *testing.T) {
	for _, previous := range []string{"", "crio-crun"} {
		for _, runtimeClass := range []bool{false, true} {
			name := previous
			if name == "" {
				name = "unset"
			}
			if runtimeClass {
				name += "/runtimeclass"
			}
			t.Run(name, func(t *testing.T) {
				x := epochPod(epochA)
				if previous == "" {
					x.Spec.Containers[0].Env = nil
				} else {
					x.Spec.Containers[0].Env[0].Value = previous
				}
				if runtimeClass {
					v := "other"
					x.Spec.RuntimeClassName = &v
				}
				h := newEpochHarness(t, x)
				h.event(watch.Modified, epochTerminal(x))
				e := h.assertDecision(h.successor(epochPod(epochB)), !runtimeClass)
				if runtimeClass {
					return
				}
				if e.Predecessor != epochA || e.Successor != epochB || e.Runtime != "containerd-runc" {
					t.Fatalf("runtime roll identities: %+v", e)
				}
				if e.Data != (PodEpochClaim{Name: dataPVCName("w"), UID: epochData, VolumeName: "data-pv"}) ||
					e.DinD != (PodEpochClaim{Name: dindDataPVCName("w"), UID: epochDinD, VolumeName: "dind-pv"}) {
					t.Fatalf("runtime roll claims: %+v / %+v", e.Data, e.DinD)
				}
				if !reflect.DeepEqual(e.Containers, []PodEpochContainer{{Name: "worker", ContainerID: "containerd://worker"}}) ||
					!reflect.DeepEqual(e.InitContainers, []PodEpochContainer{{Name: "seed", ContainerID: "containerd://seed"}}) {
					t.Fatalf("runtime roll terminal proof: %+v / %+v", e.Containers, e.InitContainers)
				}
			})
		}
	}
}

// Keep the actual successor declaration mandatory during a declaration roll.
func TestPodEpochRuntimeRollSuccessorDeclaration(t *testing.T) {
	x := epochPod(epochA)
	x.Spec.Containers[0].Env = nil
	for _, name := range []string{"unset", "previous", "duplicate", "valueFrom"} {
		t.Run(name, func(t *testing.T) {
			h := newEpochHarness(t, x)
			h.event(watch.Modified, epochTerminal(x))
			s := epochPod(epochB)
			switch name {
			case "unset":
				s.Spec.Containers[0].Env = nil
			case "previous":
				s.Spec.Containers[0].Env[0].Value = "crio-crun"
			case "duplicate":
				s.Spec.Containers[0].Env = append(s.Spec.Containers[0].Env, s.Spec.Containers[0].Env[0])
			case "valueFrom":
				s.Spec.Containers[0].Env[0].ValueFrom = &corev1.EnvVarSource{}
			}
			h.assertDecision(h.event(watch.Added, s), false)
		})
	}
}
