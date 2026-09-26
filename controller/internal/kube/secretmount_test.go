package kube

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Issue #1761: OpenShift's CRI-O shadows a volume mounted at /run/secrets, so the
// join-token Secret mount must be relocatable. These tests pin the override in both
// directions: the default render is untouched, and a custom path moves the mount AND
// both env vars that point into it.

func workerEnv(pod corev1.PodSpec) map[string]string {
	env := map[string]string{}
	for _, c := range pod.Containers {
		if c.Name != "worker" {
			continue
		}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
	}
	return env
}

func workerContainer(t *testing.T, pod corev1.PodSpec) corev1.Container {
	t.Helper()
	for _, c := range pod.Containers {
		if c.Name == "worker" {
			return c
		}
	}
	t.Fatal("no worker container")
	return corev1.Container{}
}

func TestSecretMountPathDefaultIsByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  RenderConfig
	}{
		{"plain", testConfig()},
		{"docker", dockerTestConfig()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := desired("abc")
			if tc.name == "docker" {
				w = desiredDocker("abc")
			}
			tc.cfg.APICAPEM = []byte("ca")
			unset := RenderDeployment(tc.cfg, w, testSpec(t, "base", "m"))
			explicit := tc.cfg
			explicit.SecretMountPath = "/run/secrets"
			set := RenderDeployment(explicit, w, testSpec(t, "base", "m"))
			if !reflect.DeepEqual(unset, set) {
				t.Fatal("SecretMountPath=/run/secrets must render identically to the unset default")
			}
			pod := unset.Spec.Template.Spec
			if got := mountPath(workerContainer(t, pod), "token"); got != "/run/secrets" {
				t.Errorf("default token mount = %q, want /run/secrets", got)
			}
			env := workerEnv(pod)
			if env["UZI_WORKER_TOKEN_FILE"] != "/run/secrets/worker_token" || env["NODE_EXTRA_CA_CERTS"] != "/run/secrets/ca.crt" {
				t.Errorf("default env = token %q ca %q", env["UZI_WORKER_TOKEN_FILE"], env["NODE_EXTRA_CA_CERTS"])
			}
		})
	}
}

func TestSecretMountPathOverrideMovesMountAndBothEnvs(t *testing.T) {
	for _, p := range append([]dindPosture{{"plain", testConfig(), false}}, dindPostures()...) {
		t.Run(p.name, func(t *testing.T) {
			cfg := p.cfg
			cfg.APICAPEM = []byte("ca")
			cfg.SecretMountPath = "/run/uzi-secrets"
			w := desired("abc")
			if p.name != "plain" {
				w = desiredDocker("abc")
			}
			pod := RenderDeployment(cfg, w, testSpec(t, "base", "m")).Spec.Template.Spec
			if got := mountPath(workerContainer(t, pod), "token"); got != "/run/uzi-secrets" {
				t.Errorf("token mount = %q, want /run/uzi-secrets", got)
			}
			env := workerEnv(pod)
			if env["UZI_WORKER_TOKEN_FILE"] != "/run/uzi-secrets/worker_token" {
				t.Errorf("UZI_WORKER_TOKEN_FILE = %q", env["UZI_WORKER_TOKEN_FILE"])
			}
			if env["NODE_EXTRA_CA_CERTS"] != "/run/uzi-secrets/ca.crt" {
				t.Errorf("NODE_EXTRA_CA_CERTS = %q", env["NODE_EXTRA_CA_CERTS"])
			}
			// The dind side must never see the relocated token either (the
			// posture-independent containment TestDindContainersMountNoneOfTheWorkersVolumes
			// pins for the default path).
			for _, c := range append(pod.InitContainers, pod.Containers...) {
				if c.Name == "worker" {
					continue
				}
				for _, m := range c.VolumeMounts {
					if m.Name == "token" || pathsOverlap(m.MountPath, "/run/uzi-secrets") {
						t.Errorf("container %q mounts %q at %q: only the worker may see the token", c.Name, m.Name, m.MountPath)
					}
				}
			}
		})
	}
}

func TestValidateSecretMountPath(t *testing.T) {
	valid := []string{"", "/run/secrets", "/run/uzi-secrets", "/run/uzi.secrets_1"}
	for _, p := range valid {
		if err := ValidateSecretMountPath(RenderConfig{SecretMountPath: p}); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	invalid := []string{
		"/", "run/uzi", "/run", "/run/", "/run/uzi-secrets/", "/run/a/b", "/run/../etc",
		"/run/Uzi", "/data/secrets", "/tmp/secrets", "/etc/uzi", "/run/secrets/uzi",
		dindSocketDir, // /run/dind: shape-valid, but the dind socket volume lives there
	}
	for _, p := range invalid {
		if err := ValidateSecretMountPath(RenderConfig{SecretMountPath: p}); err == nil {
			t.Errorf("%q accepted, want a refusal", p)
		}
	}
}

// TestKnownWorkerMountPathsCoverRenderedPods keeps knownWorkerMountPaths honest: every
// mount any container renders, in every posture, other than the token itself, must be
// in the list the overlap check consults. A new volume that is not added there fails
// here instead of silently becoming a place a custom secret path could collide with.
func TestKnownWorkerMountPathsCoverRenderedPods(t *testing.T) {
	known := map[string]bool{}
	for _, m := range knownWorkerMountPaths {
		known[m] = true
	}
	postures := append([]dindPosture{{"plain", testConfig(), false}}, dindPostures()...)
	for _, uidSplit := range []bool{false, true} {
		for _, p := range postures {
			cfg := p.cfg
			cfg.UIDSplit = uidSplit
			w := desired("abc")
			if p.name != "plain" {
				w = desiredDocker("abc")
			}
			pod := RenderDeployment(cfg, w, testSpec(t, "base", "m")).Spec.Template.Spec
			for _, c := range append(pod.InitContainers, pod.Containers...) {
				for _, m := range c.VolumeMounts {
					if m.Name == "token" {
						continue
					}
					if !known[m.MountPath] {
						t.Errorf("%s uidSplit=%v: container %q mounts %q at %q, which knownWorkerMountPaths does not name", p.name, uidSplit, c.Name, m.Name, m.MountPath)
					}
				}
			}
		}
	}
}
