package kube

import (
	"bytes"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestPodEpochRenderProjectionAndRuntime(t *testing.T) {
	cfg := dockerTestConfig()
	worker := desired("w")
	spec := testSpec(t, "base", "m")
	plain := RenderDeployment(cfg, worker, spec)
	cfg.PodEpochRuntime = "containerd-runc"
	plainAfter := RenderDeployment(cfg, worker, spec)
	a, _ := json.Marshal(plain)
	b, _ := json.Marshal(plainAfter)
	if !bytes.Equal(a, b) {
		t.Fatal("runtime changed plain render")
	}
	worker.Isolated = true
	cfg.IsolatedNamespace = "isolated"
	isolatedBefore := RenderDeployment(cfg, worker, spec)
	cfg.PodEpochRuntime = "crio-crun"
	isolatedAfter := RenderDeployment(cfg, worker, spec)
	a, _ = json.Marshal(isolatedBefore)
	b, _ = json.Marshal(isolatedAfter)
	if !bytes.Equal(a, b) {
		t.Fatal("runtime changed isolated render")
	}
	worker.Isolated = false
	cfg.PodEpochRuntime = "containerd-runc"
	worker.Docker = true
	declared := RenderDeployment(cfg, worker, spec)
	if declared.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatal("runtime roll must Recreate")
	}
	var mount *corev1.VolumeMount
	var envs = map[string]string{}
	for _, c := range declared.Spec.Template.Spec.Containers {
		if c.Name == "worker" {
			for _, e := range c.Env {
				envs[e.Name] = e.Value
			}
			for i := range c.VolumeMounts {
				if c.VolumeMounts[i].Name == "pod-epoch" {
					v := c.VolumeMounts[i]
					mount = &v
				}
			}
		}
	}
	if mount == nil || mount.SubPath != "" || mount.MountPath != "/etc/uzi/pod-epoch" || !mount.ReadOnly {
		t.Fatalf("mount=%+v", mount)
	}
	if envs[PodEpochRuntimeEnv] != "containerd-runc" || envs["UZI_POD_EPOCH_EVIDENCE_FILE"] != "/etc/uzi/pod-epoch/evidence" {
		t.Fatalf("env=%v", envs)
	}
	found := false
	for _, v := range declared.Spec.Template.Spec.Volumes {
		if v.Name == "pod-epoch" {
			found = true
			if v.DownwardAPI == nil || len(v.DownwardAPI.Items) != 1 || v.DownwardAPI.Items[0].Path != "evidence" || v.DownwardAPI.Items[0].FieldRef.FieldPath != "metadata.annotations['uzi.dev/pod-epoch-evidence']" {
				t.Fatalf("projection=%+v", v)
			}
		}
	}
	if !found {
		t.Fatal("missing projection")
	}
	cfg.PodEpochRuntime = ""
	off := RenderDeployment(cfg, worker, spec)
	if off.Spec.Template.Annotations[AnnotationSpecHash] == declared.Spec.Template.Annotations[AnnotationSpecHash] {
		t.Fatal("runtime absent from roll hash")
	}
	for _, c := range off.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == PodEpochRuntimeEnv {
				t.Fatal("default-off runtime present")
			}
		}
	}
	for _, v := range off.Spec.Template.Spec.Volumes {
		if v.Name == "pod-epoch" {
			return
		}
	}
	t.Fatal("default-off projection missing")
}
