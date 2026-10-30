package kube

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestDindDNSRootlessInheritsEntrypointWithExactArgs(t *testing.T) {
	dep := RenderDeployment(dockerTestConfig(), desiredDocker("abc"), testSpec(t, "base", "m"))
	dind := containerByName(t, dep.Spec.Template.Spec.InitContainers, "dind")
	if dind.Command != nil {
		t.Fatalf("rootless Command = %q, want nil to inherit image entrypoint", dind.Command)
	}
	want := []string{"--dns-search=.", "--dns-opt=ndots:0"}
	if !reflect.DeepEqual(dind.Args, want) {
		t.Fatalf("rootless Args = %q, want %q", dind.Args, want)
	}
}

func TestDindDNSRootfulSuccessAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "success"
		if fallback {
			name = "fallback"
		}
		t.Run(name, func(t *testing.T) {
			dind := renderedRootfulDind(t)
			if dind.Args != nil {
				t.Fatalf("rootful Args = %q, want nil", dind.Args)
			}
			netDir := dindMTUFixture(t, []dindMTUInterface{{"eth0", "1400\n"}})
			if fallback {
				netDir = filepath.Join(netDir, "absent")
			}
			launch, stderr, err := runRenderedDind(t, dind, netDir, dindLaunchOptions{expectDiscoveryFailure: fallback})
			if err != nil {
				t.Fatalf("launch: %v; stderr=%q", err, stderr)
			}
			want := []string{"--host=tcp://127.0.0.1:2375", "--tls=false", "--dns-search=.", "--dns-opt=ndots:0"}
			if !fallback {
				want = append(want, "--mtu=1400", "--config-file="+launch.configPath)
			}
			if !reflect.DeepEqual(launch.argv, want) {
				t.Fatalf("dockerd argv = %q, want %q", launch.argv, want)
			}
			if fallback {
				if launch.config != nil || launch.configPath != "" {
					t.Fatalf("fallback created config: %#v", launch)
				}
				wantDiagnostic := "dind MTU warning: network interface directory unavailable: " + netDir +
					"; starting dockerd with unchanged MTU defaults\n"
				if stderr != wantDiagnostic {
					t.Fatalf("fallback stderr = %q, want %q", stderr, wantDiagnostic)
				}
			} else {
				if stderr != "" {
					t.Fatalf("success stderr = %q, want empty", stderr)
				}
				wantConfig := map[string]any{"default-network-opts": map[string]any{
					"bridge": map[string]any{"com.docker.network.driver.mtu": "1400"},
				}}
				if !reflect.DeepEqual(launch.config, wantConfig) {
					t.Fatalf("daemon config = %#v, want %#v", launch.config, wantConfig)
				}
			}
		})
	}
}

func TestDindDNSDefaultsStayOutOfPodWorkerAndHelpers(t *testing.T) {
	for _, p := range dindPostures() {
		t.Run(p.name, func(t *testing.T) {
			p.cfg.APICAPEM = []byte("test CA")
			spec := testSpec(t, "base", "m")
			dockerPod := RenderDeployment(p.cfg, desiredDocker("abc"), spec).Spec.Template.Spec
			plainPod := RenderDeployment(p.cfg, desired("abc"), spec).Spec.Template.Spec
			for _, item := range []struct {
				name string
				pod  corev1.PodSpec
			}{{"docker", dockerPod}, {"plain", plainPod}} {
				t.Run(item.name, func(t *testing.T) {
					pod := item.pod
					if pod.DNSPolicy != "" || pod.DNSConfig != nil {
						t.Fatalf("pod DNSPolicy/DNSConfig = %q/%+v, want unset", pod.DNSPolicy, pod.DNSConfig)
					}
					if pod.HostNetwork || len(pod.HostAliases) != 0 || pod.Hostname != "" || pod.Subdomain != "" {
						t.Fatal("pod must preserve default network and DNS identity fields")
					}
					worker := containerByName(t, pod.Containers, "worker")
					if worker.Command != nil || worker.Args != nil {
						t.Fatalf("worker must inherit image command/args, got %q/%q", worker.Command, worker.Args)
					}
					var apiURL, caFile string
					for _, env := range worker.Env {
						switch env.Name {
						case "UZI_API_URL":
							apiURL = env.Value
						case "NODE_EXTRA_CA_CERTS":
							caFile = env.Value
						case "RES_OPTIONS", "LOCALDOMAIN", "HOSTALIASES":
							t.Errorf("worker must not override resolver via %s", env.Name)
						}
					}
					if apiURL != "https://api.uzi.svc.cluster.local:8443" || caFile != "/run/secrets/ca.crt" {
						t.Fatalf("worker API URL/CA = %q/%q, want preserved cluster URL and CA path", apiURL, caFile)
					}
					for _, c := range append(append([]corev1.Container(nil), pod.InitContainers...), pod.Containers...) {
						if c.Name == "dind" {
							if item.name == "plain" {
								t.Error("plain worker must not render DinD")
							}
							continue
						}
						if item.name == "plain" && (c.Name == "dind-init" || c.Name == "dind-meter") {
							t.Errorf("plain worker unexpectedly renders %s", c.Name)
						}
						for _, arg := range append(append([]string(nil), c.Command...), c.Args...) {
							if strings.Contains(arg, "--dns") {
								t.Errorf("DNS daemon flags leaked into %s: %q", c.Name, arg)
							}
						}
						if c.Name == "dind-init" || c.Name == "dind-meter" {
							if c.Args != nil {
								t.Errorf("%s Args = %q, want nil", c.Name, c.Args)
							}
						}
					}
				})
			}
		})
	}
}
