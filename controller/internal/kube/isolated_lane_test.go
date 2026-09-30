package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// --- the isolated research lane (PRD #1906 M5) -------------------------------

const (
	laneNS         = "uzi-workers-isolated"
	laneFetcherURL = "https://uzi-fetcher.uzi.svc.cluster.local:8443"
)

var laneFetcherCA = []byte("-----BEGIN CERTIFICATE-----\nnot-a-real-fetcher-ca\n-----END CERTIFICATE-----\n")

// laneTestConfig is the docker-tier test config plus a configured isolated lane, so every
// test below has all three namespaces available and can prove a lane worker picks the
// lane, not either other tier.
func laneTestConfig() RenderConfig {
	cfg := dockerTestConfig()
	cfg.IsolatedNamespace = laneNS
	cfg.FetcherURL = laneFetcherURL
	cfg.FetcherCAPEM = laneFetcherCA
	return cfg
}

func desiredIsolated(id string) protocol.DesiredWorker {
	return protocol.DesiredWorker{ID: id, Template: "base", Size: "m", Isolated: true, Ephemeral: true}
}

func envMap(c corev1.Container) map[string]string {
	out := map[string]string{}
	for _, e := range c.Env {
		out[e.Name] = e.Value
	}
	return out
}

func laneWorkerContainer(t *testing.T, cfg RenderConfig, w protocol.DesiredWorker) corev1.Container {
	t.Helper()
	pod := RenderDeployment(cfg, w, testSpec(t, "base", "m")).Spec.Template.Spec
	return containerByName(t, pod.Containers, workerContainerName)
}

// Every object of an isolated worker lands in the lane namespace, never the restricted
// default and never the docker tier, even with both of those configured.
func TestIsolatedWorkerObjectsGoToTheLaneNamespace(t *testing.T) {
	cfg := laneTestConfig()
	w := desiredIsolated("iso")
	spec := testSpec(t, "base", "m")

	if ns, err := cfg.Placement(w); err != nil || ns != laneNS {
		t.Fatalf("Placement = %q, %v; want %q", ns, err, laneNS)
	}
	if got := RenderDeployment(cfg, w, spec).Namespace; got != laneNS {
		t.Errorf("deployment namespace = %q, want %q", got, laneNS)
	}
	if got := RenderSecret(cfg, w, "uzw_t").Namespace; got != laneNS {
		t.Errorf("secret namespace = %q, want %q", got, laneNS)
	}
	for _, p := range RenderPVCs(cfg, w, spec) {
		if p.Namespace != laneNS {
			t.Errorf("pvc %q namespace = %q, want %q", p.Name, p.Namespace, laneNS)
		}
	}
}

// An isolated pod carries the two env names the agent reads (agent/src/config.ts), and
// the CA file they name is a key of the worker's own Secret, which the token volume mounts.
func TestIsolatedWorkerGetsTheFetcherEnvAndCA(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mountPath string
		wantCA    string
	}{
		{"default secret mount", "", "/run/secrets/fetcher-ca.crt"},
		{"relocated secret mount", "/run/uzi-secrets", "/run/uzi-secrets/fetcher-ca.crt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := laneTestConfig()
			cfg.SecretMountPath = tc.mountPath
			w := desiredIsolated("iso")

			c := laneWorkerContainer(t, cfg, w)
			env := envMap(c)
			if env["UZI_FETCHER_URL"] != laneFetcherURL {
				t.Errorf("UZI_FETCHER_URL = %q, want %q", env["UZI_FETCHER_URL"], laneFetcherURL)
			}
			if env["UZI_FETCHER_CA_FILE"] != tc.wantCA {
				t.Errorf("UZI_FETCHER_CA_FILE = %q, want %q", env["UZI_FETCHER_CA_FILE"], tc.wantCA)
			}
			// The file behind UZI_FETCHER_CA_FILE: the Secret key, mounted at the token mount.
			if got := mountPath(c, "token"); got+"/"+fetcherCACertKey != tc.wantCA {
				t.Errorf("token volume mounts at %q, so %q is not where the CA lands", got, tc.wantCA)
			}
			s := RenderSecret(cfg, w, "uzw_t")
			if string(s.Data[fetcherCACertKey]) != string(laneFetcherCA) {
				t.Errorf("secret key %q = %q, want the relayed fetcher CA", fetcherCACertKey, s.Data[fetcherCACertKey])
			}
		})
	}
}

// Only lane pods learn the fetcher: a plain or docker worker on a lane-configured
// controller renders neither env var and no fetcher CA key.
func TestNonIsolatedWorkersLackTheFetcherEnvAndCA(t *testing.T) {
	cfg := laneTestConfig()
	nonRootless := laneTestConfig()
	nonRootless.DinDNonRootless = true
	for _, tc := range []struct {
		name string
		cfg  RenderConfig
		w    protocol.DesiredWorker
	}{
		{"plain", cfg, desired("p")},
		{"plain ephemeral", cfg, protocol.DesiredWorker{ID: "e", Template: "base", Size: "m", Ephemeral: true}},
		{"docker rootless", cfg, desiredDocker("d")},
		{"docker non-rootless", nonRootless, desiredDocker("d")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := RenderDeployment(tc.cfg, tc.w, testSpec(t, "base", "m")).Spec.Template.Spec
			all := append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...)
			for _, c := range all {
				for _, e := range c.Env {
					if strings.HasPrefix(e.Name, "UZI_FETCHER_") {
						t.Errorf("container %s of a non-isolated worker carries %s", c.Name, e.Name)
					}
				}
			}
			if _, ok := RenderSecret(tc.cfg, tc.w, "uzw_t").Data[fetcherCACertKey]; ok {
				t.Errorf("a non-isolated worker's Secret carries %q", fetcherCACertKey)
			}
		})
	}
}

// The render gate: an isolated worker with the lane unconfigured, or one that is also
// docker, is refused, and namespaceFor never falls through to another tier for it.
func TestPlacementRefusesAnUnplaceableIsolatedWorker(t *testing.T) {
	noLane := dockerTestConfig()
	iso := desiredIsolated("iso")
	isoDocker := iso
	isoDocker.Docker = true

	for _, tc := range []struct {
		name string
		cfg  RenderConfig
		w    protocol.DesiredWorker
		want error
	}{
		{"lane unconfigured", noLane, iso, ErrIsolatedLaneUnconfigured},
		{"lane unconfigured, docker also unconfigured", testConfig(), iso, ErrIsolatedLaneUnconfigured},
		{"isolated and docker, lane configured", laneTestConfig(), isoDocker, ErrIsolatedDocker},
		{"isolated and docker, lane unconfigured", noLane, isoDocker, ErrIsolatedDocker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, err := tc.cfg.Placement(tc.w)
			if !errors.Is(err, tc.want) || ns != "" {
				t.Fatalf("Placement = %q, %v; want \"\", %v", ns, err, tc.want)
			}
			if got := tc.cfg.namespaceFor(tc.w); got != "" {
				t.Errorf("namespaceFor = %q; an unplaceable isolated worker must resolve to no namespace", got)
			}
		})
	}
	// The existing docker refusal still holds, and ordinary workers still place.
	if _, err := testConfig().Placement(desiredDocker("d")); !errors.Is(err, ErrDockerTierUnconfigured) {
		t.Errorf("docker worker with no docker namespace: err = %v, want ErrDockerTierUnconfigured", err)
	}
	if ns, err := laneTestConfig().Placement(desired("p")); err != nil || ns != "uzi-workers" {
		t.Errorf("plain worker: Placement = %q, %v", ns, err)
	}
	if ns, err := laneTestConfig().Placement(desiredDocker("d")); err != nil || ns != "uzi-workers-docker" {
		t.Errorf("docker worker: Placement = %q, %v", ns, err)
	}
}

// An isolated pod never carries the DinD sidecar set or any privilege, and keeps the
// restricted-PSS posture even when the fleet runs the uid split and even if a caller
// rendered an isolated+docker worker without going through Placement.
func TestIsolatedWorkerNeverGetsDinDOrPrivilege(t *testing.T) {
	uidSplit := laneTestConfig()
	uidSplit.UIDSplit = true
	iso := desiredIsolated("iso")
	isoDocker := iso
	isoDocker.Docker = true

	for _, tc := range []struct {
		name string
		cfg  RenderConfig
		w    protocol.DesiredWorker
	}{
		{"isolated", laneTestConfig(), iso},
		{"isolated, uid split on", uidSplit, iso},
		{"isolated+docker rendered directly", laneTestConfig(), isoDocker},
		{"isolated+docker rendered directly, uid split on", uidSplit, isoDocker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec(t, "base", "m")
			pod := RenderDeployment(tc.cfg, tc.w, spec).Spec.Template.Spec

			if names := initNames(pod.InitContainers); len(names) != 1 || names[0] != seedContainerName {
				t.Errorf("init containers = %v, want only [%s]", names, seedContainerName)
			}
			if len(pod.Containers) != 1 || pod.Containers[0].Name != workerContainerName {
				t.Errorf("containers = %v, want only [%s]", initNames(pod.Containers), workerContainerName)
			}
			if pod.Affinity != nil {
				t.Error("an isolated worker must not carry the docker anti-affinity")
			}
			for _, v := range pod.Volumes {
				if strings.HasPrefix(v.Name, "dind") {
					t.Errorf("an isolated worker renders the DinD volume %q", v.Name)
				}
			}
			for _, p := range RenderPVCs(tc.cfg, tc.w, spec) {
				if p.Name == dindDataPVCName(tc.w.ID) {
					t.Error("an isolated worker renders the DinD data PVC")
				}
			}
			env := envMap(pod.Containers[0])
			for _, k := range []string{"DOCKER_HOST", "TMPDIR", "UZI_DIND_PRUNE_ENABLED"} {
				if _, ok := env[k]; ok {
					t.Errorf("an isolated worker carries the docker env %s", k)
				}
			}

			// Restricted Pod Security: pod-level 10001 non-root with RuntimeDefault seccomp,
			// and every container drops ALL, adds nothing, never escalates, never privileged.
			psc := pod.SecurityContext
			if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot ||
				psc.RunAsUser == nil || *psc.RunAsUser != workerUID {
				t.Fatalf("pod security = %+v, want runAsNonRoot:true runAsUser:%d", psc, workerUID)
			}
			if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Error("an isolated pod must keep the RuntimeDefault seccomp profile")
			}
			all := append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...)
			for _, c := range all {
				sc := c.SecurityContext
				if sc == nil {
					t.Fatalf("%s: nil SecurityContext", c.Name)
				}
				if sc.Privileged != nil && *sc.Privileged {
					t.Errorf("%s: privileged", c.Name)
				}
				if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
					t.Errorf("%s: allowPrivilegeEscalation must be false", c.Name)
				}
				if sc.RunAsUser != nil && *sc.RunAsUser == 0 {
					t.Errorf("%s: runs as root", c.Name)
				}
				if sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 ||
					len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
					t.Errorf("%s: capabilities = %+v, want drop [ALL] and add nothing", c.Name, sc.Capabilities)
				}
			}
		})
	}
}

// The lane dimension is inert for every other worker: a plain or docker worker renders
// byte-identically whether or not the lane is configured, so enabling the lane rolls
// nothing already running.
func TestLaneConfigIsInertForNonIsolatedWorkers(t *testing.T) {
	spec := testSpec(t, "base", "m")
	for _, w := range []protocol.DesiredWorker{desired("p"), desiredDocker("d")} {
		if SpecHashOf(dockerTestConfig(), w, spec) != SpecHashOf(laneTestConfig(), w, spec) {
			t.Errorf("worker %s: configuring the lane changed its spec hash", w.ID)
		}
	}
}

// --- materializer -------------------------------------------------------------

func newLaneMat(t *testing.T, cfg RenderConfig, logw io.Writer, objs ...runtime.Object) (*Materializer, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset(objs...)
	m := New(client, cfg, testResolver(t), nil, DrainPolicy{}, RecyclePolicy{Enabled: true}, slog.New(slog.NewTextHandler(logw, nil)))
	return m, client
}

func assertNoWrites(t *testing.T, client *fake.Clientset, why string) {
	t.Helper()
	for _, a := range client.Actions() {
		if v := a.GetVerb(); v == "create" || v == "delete" || v == "patch" || v == "update" {
			t.Fatalf("a %q on %s in namespace %q reached the apiserver: %s", v, a.GetResource().Resource, a.GetNamespace(), why)
		}
	}
}

func TestIsolatedWorkerReconcilesIntoTheLane(t *testing.T) {
	m, client := newLaneMat(t, laneTestConfig(), io.Discard)
	ctx := context.Background()
	w := desiredIsolated("iso")
	w.JoinToken = token("uzw_i")

	if err := m.Reconcile(ctx, []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	dep, err := client.AppsV1().Deployments(laneNS).Get(ctx, "uzi-hw-iso", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("isolated worker deployment not in the lane: %v", err)
	}
	if env := envMap(dep.Spec.Template.Spec.Containers[0]); env["UZI_FETCHER_URL"] != laneFetcherURL {
		t.Errorf("rendered UZI_FETCHER_URL = %q", env["UZI_FETCHER_URL"])
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" && a.GetNamespace() != laneNS {
			t.Errorf("a create of %s went to %q; every object of a lane worker belongs in %q",
				a.GetResource().Resource, a.GetNamespace(), laneNS)
		}
	}
	var created []string
	for _, a := range client.Actions() {
		if c, ok := a.(k8stesting.CreateAction); ok {
			created = append(created, c.GetResource().Resource)
		}
	}
	if len(created) != 4 { // secret, data PVC, nix PVC, deployment
		t.Errorf("created %v, want secret + two PVCs + deployment", created)
	}
	assertNoSecretReads(t, client)
}

// Fail closed: with the lane unconfigured an isolated worker is SKIPPED and logged. It is
// never rendered into the ordinary worker namespace (nor the docker one), and Reconcile
// does not fail the tick over it.
func TestIsolatedWorkerSkippedWhenTheLaneIsUnconfigured(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  RenderConfig
	}{
		{"restricted tier only", testConfig()},
		{"restricted and docker tiers", dockerTestConfig()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			m, client := newLaneMat(t, tc.cfg, &logs)
			w := desiredIsolated("iso")
			w.JoinToken = token("uzw_i")

			if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
				t.Fatalf("Reconcile must tolerate an unplaceable isolated worker, got: %v", err)
			}
			assertNoWrites(t, client, "an isolated worker with no lane configured must be skipped, never rendered into another namespace")
			if !strings.Contains(logs.String(), "iso") || !strings.Contains(logs.String(), "no isolated lane configured") {
				t.Errorf("the skip must be logged with the worker id and the reason; got:\n%s", logs.String())
			}
		})
	}
}

// Isolated together with Docker is refused: never rendered into any namespace.
func TestIsolatedDockerWorkerIsRefused(t *testing.T) {
	var logs strings.Builder
	m, client := newLaneMat(t, laneTestConfig(), &logs)
	w := desiredIsolated("iso")
	w.Docker = true
	w.JoinToken = token("uzw_i")

	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertNoWrites(t, client, "an isolated+docker worker must be refused, never rendered")
	if !strings.Contains(logs.String(), "both isolated and docker") {
		t.Errorf("the refusal must be logged; got:\n%s", logs.String())
	}
}

// Observe lists the lane namespace: a lane worker is observed there, torn down there
// when the api drops it, and an unstamped uzi-hw-* object there is flagged, not touched.
func TestObserveCoversTheLaneNamespace(t *testing.T) {
	orphan := deployedWorkerNS("handmade", laneNS, 0, "h")
	orphan.Labels = map[string]string{"app": "someone-elses"}
	var logs strings.Builder
	m, client := newLaneMat(t, laneTestConfig(), &logs,
		deployedWorkerNS("iso", laneNS, 0, "h"),
		pvcForNS("iso", "data", laneNS),
		pvcForNS("iso", "nix", laneNS),
		orphan,
	)
	ctx := context.Background()

	observed, err := m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(observed) != 1 || observed[0].ID != "iso" || observed[0].Namespace != laneNS {
		t.Fatalf("observed = %+v, want the one lane worker stamped with %q", observed, laneNS)
	}
	if !strings.Contains(logs.String(), "orphan") || !strings.Contains(logs.String(), laneNS) {
		t.Errorf("an orphan in the lane must be flagged with that namespace; got:\n%s", logs.String())
	}

	if err := m.Reconcile(ctx, nil, observed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var deleted int
	for _, a := range client.Actions() {
		d, ok := a.(k8stesting.DeleteAction)
		if !ok {
			continue
		}
		deleted++
		if d.GetNamespace() != laneNS {
			t.Errorf("a delete targeted %q, want %q", d.GetNamespace(), laneNS)
		}
		if strings.Contains(d.GetName(), "handmade") {
			t.Error("the lane orphan was deleted; it must only be flagged")
		}
	}
	if deleted == 0 {
		t.Fatal("nothing was torn down for a dropped lane worker")
	}
}
