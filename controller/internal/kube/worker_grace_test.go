package kube

import (
	"os"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

type graceCase struct {
	name string
	cfg  RenderConfig
	w    protocol.DesiredWorker
	// baseHash is specHash(podTemplate(...)) for this exact fixed config at commit
	// f943b2590f760ea186aa39355b362f20e66fd21e, before the grace period was set.
	baseHash string
}

func graceCases() []graceCase {
	ephemeral := desired("w1")
	ephemeral.Ephemeral = true
	return []graceCase{
		{"plain", testConfig(), desired("w1"), "62873a66420e91e389056f04007450d8d5340e9f8ca8b8e86bd0c56339922d46"},
		{"docker", dockerTestConfig(), desiredDocker("w1"), "405f77daac7790c3fc992cef8ff19eb7bae2df7d165c5339266560ae26b1c9c7"},
		{"ephemeral", testConfig(), ephemeral, "62873a66420e91e389056f04007450d8d5340e9f8ca8b8e86bd0c56339922d46"},
	}
}

func TestWorkerPodsRenderTheShutdownGracePeriod(t *testing.T) {
	spec := testSpec(t, "base", "m")
	for _, tc := range graceCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := podTemplate(tc.cfg, tc.w, spec).Spec.TerminationGracePeriodSeconds
			if got == nil || *got != 40 {
				t.Fatalf("TerminationGracePeriodSeconds = %v, want 40", got)
			}
		})
	}
}

func TestGracePeriodIsTheOnlyChangeToTheSpecHash(t *testing.T) {
	spec := testSpec(t, "base", "m")
	for _, tc := range graceCases() {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := podTemplate(tc.cfg, tc.w, spec)
			if specHash(tmpl) == tc.baseHash {
				t.Fatal("spec hash unchanged: the grace period must roll existing workers")
			}
			tmpl.Spec.TerminationGracePeriodSeconds = nil
			if got := specHash(tmpl); got != tc.baseHash {
				t.Fatalf("hash without grace = %s, want base %s: something besides the grace period changed", got, tc.baseHash)
			}
		})
	}
}

func TestComposeAgentStopGracePeriod(t *testing.T) {
	type composeFile struct {
		Services map[string]struct {
			StopGracePeriod string `json:"stop_grace_period"`
		}
	}
	for path, want := range map[string]string{
		"../../../docker-compose.yml":          "40s",
		"../../../deploy/compose/compose.yaml": "40s",
		"../../../e2e/docker-compose.e2e.yml":  "",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var c composeFile
		if err := yaml.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got := c.Services["agent"].StopGracePeriod; got != want {
			t.Errorf("%s agent stop_grace_period = %q, want %q", path, got, want)
		}
	}
}
