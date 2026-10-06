package kube

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestShippedLargeWorkerFleetFitsRequestQuotas(t *testing.T) {
	type quota struct {
		CPU         string `json:"requestsCPU"`
		Memory      string `json:"requestsMemory"`
		Deployments string `json:"deployments"`
	}
	type limits struct {
		Max struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		} `json:"max"`
	}
	type budget struct {
		Quota quota  `json:"quota"`
		Range limits `json:"limitRange"`
		DinD  struct {
			Requests struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"requests"`
			Limits struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"limits"`
		} `json:"dindResources"`
	}
	var values struct {
		Workers struct {
			Quota    quota  `json:"quota"`
			Range    limits `json:"limitRange"`
			Docker   budget `json:"docker"`
			Isolated budget `json:"isolatedLane"`
		} `json:"workers"`
	}
	path := filepath.Join("..", "..", "..", "deploy", "chart", "values.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: repository-controlled chart fixture
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	for _, tier := range []struct {
		name     string
		docker   bool
		isolated bool
		budget   budget
	}{
		{name: "plain", budget: budget{Quota: values.Workers.Quota, Range: values.Workers.Range}},
		{name: "docker", docker: true, budget: values.Workers.Docker},
		{name: "isolated", isolated: true, budget: values.Workers.Isolated},
	} {
		t.Run(tier.name, func(t *testing.T) {
			count, err := strconv.Atoi(tier.budget.Quota.Deployments)
			if err != nil || count <= 0 {
				t.Fatalf("invalid deployments %q: %v", tier.budget.Quota.Deployments, err)
			}
			cfg := testConfig()
			if tier.docker {
				cfg = dockerTestConfig()
				cfg.DinDRequestCPU = tier.budget.DinD.Requests.CPU
				cfg.DinDRequestMemory = tier.budget.DinD.Requests.Memory
				cfg.DinDLimitCPU = tier.budget.DinD.Limits.CPU
				cfg.DinDLimitMemory = tier.budget.DinD.Limits.Memory
			}
			cfg.IsolatedNamespace = "isolated-test"
			cfg.FetcherURL = "https://fetcher.example.com"
			for _, template := range preset.TemplateNames() {
				worker := protocol.DesiredWorker{ID: "quota-test", Template: template, Size: "l", Docker: tier.docker, Isolated: tier.isolated}
				pod := RenderDeployment(cfg, worker, testSpec(t, template, "l")).Spec.Template.Spec
				for _, dimension := range []struct {
					name         corev1.ResourceName
					ceiling      string
					containerMax string
				}{
					{corev1.ResourceCPU, tier.budget.Quota.CPU, tier.budget.Range.Max.CPU},
					{corev1.ResourceMemory, tier.budget.Quota.Memory, tier.budget.Range.Max.Memory},
				} {
					regular := resource.Quantity{}
					for _, container := range pod.Containers {
						regular.Add(container.Resources.Requests[dimension.name])
					}
					all := append(append([]corev1.Container{}, pod.Containers...), pod.InitContainers...)
					max := chartQuantity(t, path, "limitRange.max", dimension.containerMax)
					for _, container := range all {
						limit := container.Resources.Limits[dimension.name]
						if limit.Cmp(max) > 0 {
							t.Errorf("%s/%s container %s %s limit %s exceeds max %s", tier.name, template, container.Name, dimension.name, limit.String(), max.String())
						}
					}
					// Restartable init sidecars overlap later init containers and the apps.
					// Admission uses max(apps + sidecars, each init + preceding sidecars).
					sidecars := resource.Quantity{}
					initPeak := resource.Quantity{}
					for _, container := range pod.InitContainers {
						request := container.Resources.Requests[dimension.name]
						candidate := sidecars.DeepCopy()
						if container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways {
							sidecars.Add(request)
						}
						candidate.Add(request)
						if candidate.Cmp(initPeak) > 0 {
							initPeak = candidate
						}
					}
					perPod := regular.DeepCopy()
					perPod.Add(sidecars)
					if initPeak.Cmp(perPod) > 0 {
						perPod = initPeak
					}
					perPod.Add(pod.Overhead[dimension.name])
					if perPod.Sign() <= 0 {
						t.Fatal("zero pod request would make the quota check vacuous")
					}
					fleet := resource.Quantity{}
					for i := 0; i < count; i++ {
						fleet.Add(perPod)
					}
					ceiling := chartQuantity(t, path, "quota.requests", dimension.ceiling)
					t.Logf("%s/%s fleet %d x %s %s = %s; quota %s", tier.name, template, count, dimension.name, perPod.String(), fleet.String(), ceiling.String())
					if fleet.Cmp(ceiling) > 0 {
						t.Errorf("%s/%s fleet %d x %s %s = %s exceeds quota %s", tier.name, template, count, dimension.name, perPod.String(), fleet.String(), ceiling.String())
					}
				}
			}
		})
	}
}
