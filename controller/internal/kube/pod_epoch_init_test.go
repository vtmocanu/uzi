package kube

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPodEpochSeedScriptMatchesGolden(t *testing.T) {
	const path = "testdata/pod-epoch-seed.golden"
	got := dockerNixSeedScript()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatal("pod epoch seed golden differs; regenerate with UPDATE_GOLDEN=1")
	}
}

func TestPodEpochInitRender(t *testing.T) {
	for _, p := range dindPostures() {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/split=%v", p.name, split), func(t *testing.T) {
				cfg := p.cfg
				cfg.UIDSplit = split
				pod := RenderDeployment(cfg, desiredDocker("abc"), testSpec(t, "base", "m")).Spec.Template.Spec
				for _, c := range []corev1.Container{
					containerByName(t, pod.Containers, "worker"),
					containerByName(t, pod.InitContainers, seedContainerName),
					containerByName(t, pod.InitContainers, runnerInitContainerName),
				} {
					found := 0
					for _, e := range c.Env {
						if e.Name == "UZI_EPOCH_LOCK" && e.Value == "1" {
							found++
						}
					}
					if found != 1 {
						t.Errorf("%s epoch participation = %d, want one", c.Name, found)
					}
				}
				seed := containerByName(t, pod.InitContainers, seedContainerName)
				if seed.Command[2] != dockerNixSeedScript() {
					t.Fatal("Docker seed lacks protected prefix")
				}
				if len(seed.Env) != 2 || seed.Env[1].Name != "UZI_POD_UID" ||
					seed.Env[1].ValueFrom == nil || seed.Env[1].ValueFrom.FieldRef.FieldPath != "metadata.uid" {
					t.Fatalf("seed downward API env = %+v", seed.Env)
				}
				for _, c := range append(pod.Containers, pod.InitContainers...) {
					if c.Name == "worker" || c.Name == seedContainerName || c.Name == runnerInitContainerName {
						continue
					}
					raw, err := json.Marshal(c)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(raw), "uzi-epoch") || strings.Contains(string(raw), "UZI_EPOCH") {
						t.Errorf("epoch surface exposed to %s", c.Name)
					}
				}
				plain := RenderDeployment(cfg, desired("abc"), testSpec(t, "base", "m")).Spec.Template.Spec
				ps := containerByName(t, plain.InitContainers, seedContainerName)
				if len(ps.Env) != 0 || ps.Command[2] != nixSeedScript(nixMountPath, nixSeedMountPath, toolchainProfileMarker, dataSeedMountPath) {
					t.Fatal("plain seed changed")
				}
				raw, err := json.Marshal(plain)
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"UZI_EPOCH_LOCK", "pod-epoch-init.cjs"} {
					if strings.Contains(string(raw), name) {
						t.Errorf("plain pod gained %s", name)
					}
				}
			})
		}
	}
}

func TestPodEpochInitUsesVolumeRootAndClosesDescriptor(t *testing.T) {
	for name, script := range map[string]string{"seed": dockerNixSeedScript(), "runner": runnerInitScript()} {
		for _, want := range []string{
			"prepare /data-seed", "exec 9</data-seed/.uzi-epoch.lock",
			"hold /data-seed \"$identity\" 9", "exec 9>&-",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		for _, bad := range []string{"prepare /data/runner", "hold /data-seed/runner-shared"} {
			if strings.Contains(script, bad) {
				t.Errorf("%s uses subPath for epoch", name)
			}
		}
	}
}
