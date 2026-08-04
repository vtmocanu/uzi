package kube

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/preset"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/protocol"
)

func testConfig() RenderConfig {
	return RenderConfig{
		Namespace:          "uzi-workers",
		ServiceAccountName: "uzi-hosted-worker",
		APIURL:             "https://api.uzi.svc.cluster.local:8443",
		StorageClass:       "storage-class",
	}
}

func testResolver(t *testing.T) preset.Resolver {
	t.Helper()
	r, err := preset.NewResolver("harbor.example.com/uzi", "v1.0.0")
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func testSpec(t *testing.T, template, size string) preset.Spec {
	t.Helper()
	spec, err := testResolver(t).Resolve(template, size)
	if err != nil {
		t.Fatalf("Resolve(%q, %q): %v", template, size, err)
	}
	return spec
}

func desired(id string) protocol.DesiredWorker {
	return protocol.DesiredWorker{ID: id, Template: "base", Size: "m", Generation: 0}
}

// dockerTestConfig is testConfig plus a configured docker tier (PRD #83 M3): the
// separate privileged namespace and the pinned DinD sidecar image.
func dockerTestConfig() RenderConfig {
	cfg := testConfig()
	cfg.DockerNamespace = "uzi-workers-docker"
	cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
	return cfg
}

func desiredDocker(id string) protocol.DesiredWorker {
	return protocol.DesiredWorker{ID: id, Template: "base", Size: "m", Generation: 0, Docker: true}
}

// dockerTestConfigNonRootless is the PRD #89 non-rootless posture: the same docker tier
// with DinDNonRootless set. The image is the non-rootless -dind ref (M2 selects it by
// the flag; M1 render branches only on the posture bool, not the image string).
func dockerTestConfigNonRootless() RenderConfig {
	cfg := dockerTestConfig()
	cfg.DinDNonRootless = true
	cfg.DinDImage = "docker:28-dind@sha256:deadbeef"
	return cfg
}

// dindPosture pairs a posture name with its RenderConfig so the docker render tests can
// assert BOTH postures from one table (PRD #89 M1: parameterize the rootless-pinned
// tests). The security invariant (dind mounts none of token/data/nix) is
// posture-independent and holds for both; only the shape assertions branch.
type dindPosture struct {
	name        string
	cfg         RenderConfig
	nonRootless bool
}

func dindPostures() []dindPosture {
	return []dindPosture{
		{"rootless", dockerTestConfig(), false},
		{"non-rootless", dockerTestConfigNonRootless(), true},
	}
}

// mountPath returns the mount path of the named volume in c, or "" if it is not mounted.
func mountPath(c corev1.Container, volName string) string {
	for _, vm := range c.VolumeMounts {
		if vm.Name == volName {
			return vm.MountPath
		}
	}
	return ""
}

// volumeByName returns the pod volume with the given name (fatals if absent).
func volumeByName(t *testing.T, vols []corev1.Volume, name string) corev1.Volume {
	t.Helper()
	for _, v := range vols {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no volume named %q", name)
	return corev1.Volume{}
}

// initNames lists init-container names in order (for readable order-mismatch failures).
func initNames(cs []corev1.Container) []string {
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name
	}
	return names
}

// The pod posture, asserted field by field. Every line here is a decision that
// fails in a specific, mostly-silent way if it drifts.
func TestRenderedPodPosture(t *testing.T) {
	dep := RenderDeployment(testConfig(), desired("abc"), testSpec(t, "base", "m"))
	pod := dep.Spec.Template.Spec
	sc := pod.SecurityContext

	if *sc.RunAsUser != 10001 || *sc.RunAsGroup != 10001 || !*sc.RunAsNonRoot {
		t.Errorf("uid/gid = %d/%d, nonRoot=%v; want 10001/10001 non-root", *sc.RunAsUser, *sc.RunAsGroup, *sc.RunAsNonRoot)
	}
	// fsGroup is load-bearing twice: the RWO PVC write AND the token read. Without it
	// the Secret volume is root:root and the worker cannot read its own join token.
	if sc.FSGroup == nil || *sc.FSGroup != 10001 {
		t.Error("fsGroup must be 10001: an RWO PVC mounts root:root 0755 and a Secret volume is owned root:<fsGroup>")
	}
	// The default (Always) recursively chowns a ~1.7GB store on EVERY pod start, and
	// Decision 9 rolls every worker on every release.
	if sc.FSGroupChangePolicy == nil || *sc.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Error("fsGroupChangePolicy must be OnRootMismatch")
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccompProfile must be RuntimeDefault (PodSecurity restricted)")
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be false: the worker gets zero kube access")
	}
	if pod.ServiceAccountName != "uzi-hosted-worker" {
		t.Errorf("serviceAccountName = %q", pod.ServiceAccountName)
	}
	// RollingUpdate would Multi-Attach-deadlock against the RWO PVCs.
	if dep.Spec.Strategy.Type != "Recreate" {
		t.Errorf("strategy = %q, want Recreate", dep.Spec.Strategy.Type)
	}
	// A Deployment's selector is IMMUTABLE — anything extra in it is unpatchable
	// forever. This is a one-way door.
	if len(dep.Spec.Selector.MatchLabels) != 1 || dep.Spec.Selector.MatchLabels[LabelWorkerID] != "abc" {
		t.Errorf("selector = %v, want the worker-id label ALONE", dep.Spec.Selector.MatchLabels)
	}

	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		if c.SecurityContext == nil || *c.SecurityContext.AllowPrivilegeEscalation {
			t.Errorf("container %q: allowPrivilegeEscalation must be false", c.Name)
		}
		if len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
			t.Errorf("container %q: must drop ALL capabilities", c.Name)
		}
		// A ResourceQuota on requests.CPU or requests.MEMORY rejects any pod with a
		// container declaring none — initContainers included. Those two resources and no
		// others: the quota evaluator's validationSet is hardcoded to them (issue #224
		// corrected "requests.*" here), which is exactly why the ephemeral-storage
		// request asserted further down needs its own test rather than this one.
		if c.Resources.Requests.Cpu().IsZero() || c.Resources.Requests.Memory().IsZero() {
			t.Errorf("container %q: requests are required or the ResourceQuota rejects the pod", c.Name)
		}
		if c.Resources.Limits.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
			t.Errorf("container %q: limits are required", c.Name)
		}
	}
}

// Overriding the image's ENTRYPOINT/CMD from the pod spec is the Decision Log's
// explicitly REJECTED option (c): it silently bypasses PRD #51's security wrapper by
// duplicating the image's CMD in YAML, and drifts invisibly whenever that entrypoint
// changes.
func TestWorkerContainerNeverOverridesTheImageEntrypoint(t *testing.T) {
	dep := RenderDeployment(testConfig(), desired("abc"), testSpec(t, "base", "m"))
	for _, c := range dep.Spec.Template.Spec.Containers {
		if len(c.Command) > 0 || len(c.Args) > 0 {
			t.Errorf("worker container sets command=%v args=%v; both must be inherited from the image", c.Command, c.Args)
		}
	}
}

// The token volume's mode. 0400 leaves the file owned by ROOT (a Secret volume is
// root:<fsGroup>), so the worker cannot read its own join token — and nothing else
// fixes it up: the compose entrypoint's chmod is on the root branch and is skipped
// on a non-root start.
func TestTokenVolumeIs0440NotThe0400ThatWouldBeUnreadable(t *testing.T) {
	dep := RenderDeployment(testConfig(), desired("abc"), testSpec(t, "base", "m"))
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name != "token" {
			continue
		}
		if v.Secret == nil {
			t.Fatal("the token volume must be a secret volume")
		}
		if v.Secret.DefaultMode == nil || *v.Secret.DefaultMode != 0o440 {
			t.Fatalf("token defaultMode = %v, want 0440 (files are root:fsGroup, so 0400 is unreadable by uid 10001)", v.Secret.DefaultMode)
		}
		if v.Secret.SecretName != "uzi-hw-abc-token" {
			t.Errorf("secretName = %q", v.Secret.SecretName)
		}
		return
	}
	t.Fatal("no token volume rendered")
}

// The init container must mount the nix PVC at /nix-seed, NOT /nix — mounting it at
// /nix masks the very image content it exists to copy. The worker mounts the same
// PVC at /nix. PRD #92 M2: it ALSO mounts the data PVC at /data-seed (NOT /data, same
// masking rationale) so a reseed can clear the dangling shared provisioning state.
func TestNixSeedMountsAtNixSeedNotNix(t *testing.T) {
	dep := RenderDeployment(testConfig(), desired("abc"), testSpec(t, "base", "m"))
	pod := dep.Spec.Template.Spec

	if len(pod.InitContainers) != 1 {
		t.Fatalf("%d init containers, want 1", len(pod.InitContainers))
	}
	init := pod.InitContainers[0]
	initMounts := map[string]string{}
	for _, m := range init.VolumeMounts {
		initMounts[m.Name] = m.MountPath
	}
	if got := initMounts["nix"]; got != "/nix-seed" {
		t.Fatalf("init nix mountPath = %q, want /nix-seed (mounting at /nix masks the store being copied)", got)
	}
	// The data PVC at a NON-/data path: needed to clear the dangling devbox/nix
	// provisioning state on a reseed, and at /data-seed (not /data) for the same
	// masking reason as the nix mount.
	if got := initMounts["data"]; got != "/data-seed" {
		t.Fatalf("init data mountPath = %q, want /data-seed (the reseed clears dangling /data provisioning state)", got)
	}
	if len(init.VolumeMounts) != 2 {
		t.Fatalf("init mounts = %v, want exactly the nix (/nix-seed) and data (/data-seed) volumes", init.VolumeMounts)
	}
	// The SAME per-template image as the worker, so the store it copies is the one
	// that image was built with.
	if init.Image != pod.Containers[0].Image {
		t.Errorf("init image %q != worker image %q", init.Image, pod.Containers[0].Image)
	}

	mounts := map[string]string{}
	for _, m := range pod.Containers[0].VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if mounts["nix"] != "/nix" || mounts["data"] != "/data" || mounts["token"] != "/run/secrets" {
		t.Errorf("worker mounts = %v", mounts)
	}
}

// A pod's effective request is max(initContainers) against sum(containers), so the
// seed container is free ONLY while it stays under the smallest preset's requests.
// Pin it: shrinking a preset must not silently make the seed the pod's request.
func TestSeedContainerRequestsStayUnderEveryPreset(t *testing.T) {
	r := testResolver(t)
	for _, size := range preset.SizeNames() {
		spec, err := r.Resolve("base", size)
		if err != nil {
			t.Fatalf("Resolve(base, %q): %v", size, err)
		}
		if seedResources.Requests.Cpu().Cmp(spec.Size.CPURequest) > 0 {
			t.Errorf("preset %q: seed cpu request %s exceeds the worker's %s, so the seed becomes the pod's effective request",
				size, seedResources.Requests.Cpu(), spec.Size.CPURequest.String())
		}
		if seedResources.Requests.Memory().Cmp(spec.Size.MemoryRequest) > 0 {
			t.Errorf("preset %q: seed memory request %s exceeds the worker's %s",
				size, seedResources.Requests.Memory(), spec.Size.MemoryRequest.String())
		}
	}
}

func TestRenderedResourcesComeFromThePreset(t *testing.T) {
	dep := RenderDeployment(testConfig(), protocol.DesiredWorker{ID: "abc", Template: "jvm", Size: "l"}, testSpec(t, "jvm", "l"))
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != "harbor.example.com/uzi/agent-jvm:v1.0.0" {
		t.Errorf("image = %q", c.Image)
	}
	if c.Resources.Requests.Cpu().String() != "1" || c.Resources.Limits.Cpu().String() != "4" {
		t.Errorf("cpu = %s/%s, want 1/4", c.Resources.Requests.Cpu(), c.Resources.Limits.Cpu())
	}
	if c.Resources.Requests.Memory().String() != "4Gi" || c.Resources.Limits.Memory().String() != "8Gi" {
		t.Errorf("memory = %s/%s, want 4Gi/8Gi", c.Resources.Requests.Memory(), c.Resources.Limits.Memory())
	}
}

func TestPVCsSizeFromThePresetAndNixIsFlat(t *testing.T) {
	for _, tc := range []struct{ size, data string }{{"s", "5Gi"}, {"m", "10Gi"}, {"l", "20Gi"}} {
		pvcs := RenderPVCs(testConfig(), desired("abc"), testSpec(t, "base", tc.size))
		if len(pvcs) != 2 {
			t.Fatalf("%d pvcs, want 2", len(pvcs))
		}
		data, nix := pvcs[0], pvcs[1]
		if data.Name != "uzi-hw-abc-data" || nix.Name != "uzi-hw-abc-nix" {
			t.Fatalf("names = %q / %q", data.Name, nix.Name)
		}
		if got := data.Spec.Resources.Requests.Storage().String(); got != tc.data {
			t.Errorf("size %q: /data = %s, want %s", tc.size, got, tc.data)
		}
		if got := nix.Spec.Resources.Requests.Storage().String(); got != "20Gi" {
			t.Errorf("size %q: /nix = %s, want the flat 20Gi", tc.size, got)
		}
		for _, p := range pvcs {
			if p.Spec.AccessModes[0] != corev1.ReadWriteOnce {
				t.Errorf("%s: must be RWO", p.Name)
			}
			if p.Spec.StorageClassName == nil || *p.Spec.StorageClassName != "storage-class" {
				t.Errorf("%s: storageClass not applied", p.Name)
			}
		}
	}
}

// Issue #224 M-b: the worker container declares requests.ephemeral-storage, on the
// WORKER container and nowhere else, at the per-tier value.
//
// Declaring nothing is what the issue is about: kubelet ranks a pod that requested 0
// as exceeding its request by everything it uses, so a worker was the first thing
// evicted under node disk pressure — and an evicted worker's in-flight run loses its
// entire working tree, silently.
//
// The "worker container and nowhere else" half is the part that reads as an
// oversight and is the actual design. Quota and the scheduler sum containers PLUS
// restartable initContainers; the eviction ranker uses max(sum(Containers),
// max(InitContainers)) with no sidecar case. .spec.containers holds exactly one
// entry, so the whole budget on `worker` makes all three views agree on one number.
// Splitting it across containers "for symmetry" is charged in full by quota and
// credited only partially by the ranker, i.e. it silently halves the threshold this
// exists to raise.
func TestWorkerDeclaresTheWholeEphemeralBudgetAndNoOtherContainerDoes(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  RenderConfig
		w    protocol.DesiredWorker
		want string
	}{
		{"plain", testConfig(), desired("abc"), "512Mi"},
		{"docker rootless", dockerTestConfig(), desiredDocker("abc"), "4Gi"},
		{"docker non-rootless", dockerTestConfigNonRootless(), desiredDocker("abc"), "4Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := RenderDeployment(tc.cfg, tc.w, testSpec(t, "base", "m")).Spec.Template.Spec

			worker := containerByName(t, pod.Containers, workerContainerName)
			got, ok := worker.Resources.Requests[corev1.ResourceEphemeralStorage]
			if !ok {
				t.Fatalf("the worker container declares NO requests.ephemeral-storage. A pod that declares "+
					"none is ranked as exceeding its request by its whole usage and is evicted FIRST under node "+
					"disk pressure, which destroys the in-flight run's working tree (issue #224). requests = %v",
					worker.Resources.Requests)
			}
			if want := resource.MustParse(tc.want); got.Cmp(want) != 0 {
				t.Errorf("worker requests.ephemeral-storage = %s, want %s", got.String(), tc.want)
			}

			// EVERY other container: none of them declares one. This is the half a future
			// "make the sidecar declare its share too" edit breaks.
			for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
				if c.Name == workerContainerName {
					continue
				}
				if q, ok := c.Resources.Requests[corev1.ResourceEphemeralStorage]; ok {
					t.Errorf("container %q also declares requests.ephemeral-storage (%s). The whole pod budget "+
						"belongs on %q alone: quota and the scheduler sum containers plus restartable init "+
						"containers, while the eviction ranker uses max(sum(Containers), max(InitContainers)) "+
						"with no sidecar case, so a split is charged in full and credited only in part.",
						c.Name, q.String(), workerContainerName)
				}
			}
		})
	}
}

// 🔴 THE INVARIANT TEST. No container in .spec.containers OR .spec.initContainers may
// declare limits.ephemeral-storage, in either posture, docker or plain.
//
// This is not defensive tidiness — a limit is strictly WORSE than declaring nothing,
// and it is what the next person adds "for symmetry with cpu and memory":
//
//  1. podEphemeralStorageLimitEviction fires as soon as ANY container declares a
//     limit, and compares the SUM of declared limits against pod usage that INCLUDES
//     the emptyDirs. One limit on `worker` therefore creates a pod-level ceiling that
//     run-workdir — the run's own checkout — counts against. That is a DETERMINISTIC
//     eviction with no node pressure at all: this issue's failure, on schedule.
//  2. A limit on `dind` is inert anyway (containerEphemeralStorageLimitEviction reads
//     pod.Spec.Containers only, and dind is a restartable initContainer), so it buys
//     nothing while still arming (1).
//  3. All three localStorageEviction arms evict at gracePeriodOverride 0, so a limit
//     would also disarm any SIGTERM-time work-preservation the shutdown path grows.
//
// A REQUEST adds no eviction path at all — every arm is limit- or sizeLimit-gated —
// which is why the request above is safe and this must stay empty.
func TestNoContainerDeclaresAnEphemeralStorageLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  RenderConfig
		w    protocol.DesiredWorker
	}{
		{"plain", testConfig(), desired("abc")},
		{"docker rootless", dockerTestConfig(), desiredDocker("abc")},
		{"docker non-rootless", dockerTestConfigNonRootless(), desiredDocker("abc")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := RenderDeployment(tc.cfg, tc.w, testSpec(t, "base", "m")).Spec.Template.Spec

			// Both lists, explicitly. Checking only .spec.containers would miss the whole
			// docker tier, whose sidecars are initContainers — and trap (1) fires on a limit
			// declared by ANY container, sidecar included.
			seen := 0
			for _, group := range []struct {
				what string
				cs   []corev1.Container
			}{
				{".spec.initContainers", pod.InitContainers},
				{".spec.containers", pod.Containers},
			} {
				for _, c := range group.cs {
					seen++
					if q, ok := c.Resources.Limits[corev1.ResourceEphemeralStorage]; ok {
						t.Errorf("%s container %q declares limits.ephemeral-storage = %s. NEVER declare one: a limit "+
							"on ANY container makes kubelet enforce the SUM of declared limits against pod usage that "+
							"includes the emptyDirs, creating a deterministic eviction that fires with no node pressure "+
							"— the exact failure issue #224 exists to reduce. Read ephemeralRequest's header in "+
							"render.go before reverting this.", group.what, c.Name, q.String())
					}
				}
			}
			// The test iterated something. Without this it would pass vacuously if the
			// render ever stopped producing containers at all.
			if seen < 2 {
				t.Fatalf("only %d containers in the rendered pod; the invariant was asserted over almost nothing", seen)
			}
		})
	}
}

// The per-tier values are overridable per cluster (the right number is a property of
// the CLUSTER'S NODES, not of the product — the same argument that made dindResources
// overridable), docker REPLACES plain rather than adding to it, and either override
// rolls its own tier and only its own.
func TestEphemeralRequestOverridesArePerTierAndRollOnlyThatTier(t *testing.T) {
	ephemeralOf := func(cfg RenderConfig, w protocol.DesiredWorker) string {
		t.Helper()
		pod := RenderDeployment(cfg, w, testSpec(t, "base", "m")).Spec.Template.Spec
		q := containerByName(t, pod.Containers, workerContainerName).Resources.Requests[corev1.ResourceEphemeralStorage]
		return q.String()
	}

	cfg := dockerTestConfig()
	cfg.EphemeralRequest = "1Gi"
	cfg.DockerEphemeralRequest = "8Gi"

	if got := ephemeralOf(cfg, desired("abc")); got != "1Gi" {
		t.Errorf("overridden plain request = %s, want 1Gi", got)
	}
	// REPLACES, not adds: 8Gi exactly, never 9Gi.
	if got := ephemeralOf(cfg, desiredDocker("abc")); got != "8Gi" {
		t.Errorf("overridden docker request = %s, want exactly 8Gi — the docker value REPLACES the plain one", got)
	}

	// A partial override leaves the other tier on its default, in both directions.
	plainOnly := dockerTestConfig()
	plainOnly.EphemeralRequest = "1Gi"
	if got := ephemeralOf(plainOnly, desiredDocker("abc")); got != "4Gi" {
		t.Errorf("docker request with only the plain override set = %s, want the 4Gi default", got)
	}
	dockerOnly := dockerTestConfig()
	dockerOnly.DockerEphemeralRequest = "8Gi"
	if got := ephemeralOf(dockerOnly, desired("abc")); got != "512Mi" {
		t.Errorf("plain request with only the docker override set = %s, want the 512Mi default", got)
	}

	// Each override rolls its own tier (it changes the rendered pod, so a bump must
	// actually reach the fleet) and leaves the other tier's hash alone.
	base := dockerTestConfig()
	if SpecHashOf(base, desired("abc"), testSpec(t, "base", "m")) ==
		SpecHashOf(plainOnly, desired("abc"), testSpec(t, "base", "m")) {
		t.Error("raising the plain ephemeral request did not change a plain worker's spec hash, so the bump would never roll it")
	}
	if SpecHashOf(base, desiredDocker("abc"), testSpec(t, "base", "m")) !=
		SpecHashOf(plainOnly, desiredDocker("abc"), testSpec(t, "base", "m")) {
		t.Error("the PLAIN override changed a DOCKER worker's spec hash; the two tiers must roll independently")
	}
	if SpecHashOf(base, desired("abc"), testSpec(t, "base", "m")) !=
		SpecHashOf(dockerOnly, desired("abc"), testSpec(t, "base", "m")) {
		t.Error("the DOCKER override changed a PLAIN worker's spec hash; a docker-only knob must never roll the plain fleet")
	}
}

// The shipped defaults must fit the fleet the chart's own quotas permit, or a worker
// provisions and never appears — silently, because a quota on this resource cannot
// bind (the evaluator enforces cpu and memory alone) and so cannot warn anybody.
//
// The node figure is dev-cluster's measured ephemeral-storage ALLOCATABLE, identical
// on all four worker nodes. It is a constant in this test rather than config on
// purpose: it is a fact about one cluster, and the assertion it supports is "the
// SHIPPED DEFAULTS are sane for a real node", not "this controller knows your nodes".
// A cluster with smaller nodes lowers the values through the chart.
func TestShippedEphemeralDefaultsFitAWholeFleetOnRealNodes(t *testing.T) {
	// 17.55 GiB allocatable x 4 worker nodes.
	const nodeAllocatable = 17.55
	const workerNodes = 4
	// The tiers' own `count/deployments.apps` quotas: 10 docker + 20 restricted.
	const dockerWorkers, plainWorkers = 10, 20

	gib := func(s string) float64 {
		t.Helper()
		q := resource.MustParse(s)
		return float64(q.Value()) / (1 << 30)
	}
	plain, docker := gib(workerDefaultEphemeralRequest), gib(workerDefaultDockerEphemeralRequest)

	// A single worker must fit on ONE node with room for the node's own eviction
	// threshold (15%) and for everything else already running there.
	if docker > nodeAllocatable*0.85 {
		t.Errorf("the docker default (%s = %.2f GiB) exceeds 85%% of a node's allocatable (%.2f GiB): "+
			"the pod would be permanently unschedulable, which presents as a worker that provisions and never appears",
			workerDefaultDockerEphemeralRequest, docker, nodeAllocatable)
	}

	// And the whole fleet the quotas permit must fit across the pool. This is the
	// assertion that would have caught the 20Gi-per-worker figure an early draft
	// considered: 1.14x a whole node, each.
	fleet := float64(dockerWorkers)*docker + float64(plainWorkers)*plain
	pool := nodeAllocatable * workerNodes
	if fleet > pool {
		t.Errorf("a full fleet (%d docker x %s + %d plain x %s = %.1f GiB) exceeds the worker pool's "+
			"ephemeral-storage allocatable (%d x %.2f = %.1f GiB); workers would silently stop scheduling",
			dockerWorkers, workerDefaultDockerEphemeralRequest, plainWorkers, workerDefaultEphemeralRequest,
			fleet, workerNodes, nodeAllocatable, pool)
	}
}

// Issue #224 M-a: the DinD daemon's data root is a THIRD PVC, and ONLY for a docker
// worker. Both halves matter and they fail differently.
//
// The docker half is the fix: as an emptyDir, the daemon's image + build cache was
// charged to the POD's ephemeral-storage usage, so a big pull made this pod the one
// kubelet evicted under node disk pressure — and an evicted worker's in-flight run
// loses its whole working tree, silently.
//
// The plain half is the blast radius. render.go builds the docker init containers,
// mounts and volumes as slices precisely so a non-docker render stays byte-identical;
// if the third PVC or its volume leaked into a plain worker's render, shipping a
// DOCKER-only change would roll the ENTIRE plain fleet — every one of which is the
// exact data-loss event this issue is about.
func TestDinDDataPVCIsRenderedForDockerWorkersOnly(t *testing.T) {
	for _, p := range dindPostures() {
		// Docker: three claims, the third being the daemon's data root.
		pvcs := RenderPVCs(p.cfg, desiredDocker("abc"), testSpec(t, "base", "m"))
		if len(pvcs) != 3 {
			t.Fatalf("[%s] docker worker rendered %d pvcs, want 3 (data, nix, dind-data)", p.name, len(pvcs))
		}
		dind := pvcs[2]
		if dind.Name != "uzi-hw-abc-dind-data" {
			t.Errorf("[%s] third pvc = %q, want uzi-hw-abc-dind-data", p.name, dind.Name)
		}
		if got := dind.Spec.Resources.Requests.Storage().String(); got != "20Gi" {
			t.Errorf("[%s] dind-data size = %s, want the flat 20Gi default", p.name, got)
		}
		if dind.Spec.AccessModes[0] != corev1.ReadWriteOnce {
			t.Errorf("[%s] dind-data must be RWO", p.name)
		}
		if dind.Spec.StorageClassName == nil || *dind.Spec.StorageClassName != "storage-class" {
			t.Errorf("[%s] dind-data: storageClass not applied", p.name)
		}
		if dind.Namespace != "uzi-workers-docker" {
			t.Errorf("[%s] dind-data namespace = %q, want the docker tier's", p.name, dind.Namespace)
		}

		// Plain: two claims, and NO object by the dind name anywhere in the set.
		plain := RenderPVCs(p.cfg, desired("abc"), testSpec(t, "base", "m"))
		if len(plain) != 2 {
			t.Fatalf("[%s] plain worker rendered %d pvcs, want 2 — a docker-only volume must never reach the restricted tier", p.name, len(plain))
		}
		for _, q := range plain {
			if q.Name == dindDataPVCName("abc") {
				t.Errorf("[%s] a plain worker rendered %q", p.name, q.Name)
			}
		}

		// ...and no such VOLUME in a plain worker's pod, which is the half that would
		// roll the fleet rather than merely waste a claim.
		for _, v := range RenderDeployment(p.cfg, desired("abc"), testSpec(t, "base", "m")).Spec.Template.Spec.Volumes {
			if v.Name == dindDataVolume {
				t.Errorf("[%s] a plain worker's pod carries the %q volume", p.name, dindDataVolume)
			}
		}
	}
}

// The volume SOURCE, asserted positively against a control. "No EmptyDir" alone would
// pass on a volume that is neither (an unset VolumeSource renders as an empty
// {} and the apiserver rejects it), so assert the claim IS a PVC and names the right
// claim — and keep run-workdir in the same test as the control that proves the
// assertion discriminates: it is still an emptyDir and MUST stay one (it holds the
// run's checkout, so bounding or persisting it is the opposite of this fix).
func TestDinDDataVolumeIsAPVCAndRunWorkdirIsStillAnEmptyDir(t *testing.T) {
	for _, p := range dindPostures() {
		vols := RenderDeployment(p.cfg, desiredDocker("abc"), testSpec(t, "base", "m")).Spec.Template.Spec.Volumes

		data := volumeByName(t, vols, dindDataVolume)
		if data.PersistentVolumeClaim == nil {
			t.Fatalf("[%s] %s is not a PVC (source = %+v). As an emptyDir its bytes count toward the pod's "+
				"ephemeral-storage usage, which is what got workers evicted (issue #224).", p.name, dindDataVolume, data.VolumeSource)
		}
		if got := data.PersistentVolumeClaim.ClaimName; got != "uzi-hw-abc-dind-data" {
			t.Errorf("[%s] %s claimName = %q, want uzi-hw-abc-dind-data", p.name, dindDataVolume, got)
		}
		if data.EmptyDir != nil {
			t.Errorf("[%s] %s carries BOTH a PVC and an emptyDir source", p.name, dindDataVolume)
		}

		// The control.
		work := volumeByName(t, vols, dindWorkdirVolume)
		if work.EmptyDir == nil {
			t.Errorf("[%s] %s must stay an emptyDir: it holds the run's working tree, and this issue exists "+
				"to stop that tree being destroyed, not to bound it", p.name, dindWorkdirVolume)
		}
	}
}

// The size is flat, chart-overridable, and — deliberately — NOT part of the pod's
// spec hash.
//
// That last property is easy to read as a bug and is the correct behaviour: the pod
// template only NAMES the claim, so raising the size cannot roll a worker, and it must
// not pretend to. PVCs are never patched here (RenderPVCs' own header), so a size
// change applies to newly-provisioned workers and existing ones need delete +
// reprovision — exactly what /data and /nix already do. A roll would produce a pod
// that restarts and then mounts the SAME old claim, i.e. a disruption that changes
// nothing.
func TestDinDDataSizeDefaultsOverridesAndDoesNotRollThePod(t *testing.T) {
	dindPVC := func(cfg RenderConfig) *corev1.PersistentVolumeClaim {
		t.Helper()
		pvcs := RenderPVCs(cfg, desiredDocker("abc"), testSpec(t, "base", "m"))
		return pvcs[len(pvcs)-1]
	}

	if got := dindPVC(dockerTestConfig()).Spec.Resources.Requests.Storage().String(); got != "20Gi" {
		t.Errorf("default dind-data size = %s, want 20Gi", got)
	}

	// 10Gi, and it is deliberately BELOW the chart's limitRange.maxPVCStorage (20Gi)
	// rather than a round number picked for contrast. A fixture is read as an example:
	// this used to be 40Gi, which is double the ceiling, so it demonstrated a value that
	// renders clean, boots clean and then has its PVC rejected at admission — the worker
	// provisions and never appears. worker-invariants.yaml now refuses to render that
	// pair, so a 40Gi fixture would also contradict the chart's own guard.
	over := dockerTestConfig()
	over.DinDDataSize = "10Gi"
	if got := dindPVC(over).Spec.Resources.Requests.Storage().String(); got != "10Gi" {
		t.Errorf("overridden dind-data size = %s, want 10Gi", got)
	}

	// The override does not touch the pod template, in EITHER direction.
	if a, b := SpecHashOf(dockerTestConfig(), desiredDocker("abc"), testSpec(t, "base", "m")),
		SpecHashOf(over, desiredDocker("abc"), testSpec(t, "base", "m")); a != b {
		t.Error("changing the dind-data PVC size changed the docker worker's spec hash; it must not — " +
			"the pod names the claim, never its size, and rolling the pod would remount the same old claim")
	}
	// And a plain worker's hash is untouched by the setting existing at all — the same
	// guard TestDockerFlagRollsThePodButAbsentDockerIsInert applies to the docker tier
	// config as a whole, restated for the one knob added here.
	if a, b := SpecHashOf(testConfig(), desired("abc"), testSpec(t, "base", "m")),
		SpecHashOf(over, desired("abc"), testSpec(t, "base", "m")); a != b {
		t.Error("a plain worker's spec hash depends on the dind-data size; a docker-only knob must never roll the plain fleet")
	}
}

// The CA relay: hosted workers are in a different namespace and cannot mount the
// api's TLS Secret, so the CA rides the per-worker Secret this controller already
// creates. No new RBAC verb — Decision 1's Secrets line stays verbatim.
func TestSecretCarriesTheTokenAndRelaysTheCA(t *testing.T) {
	cfg := testConfig()
	cfg.APICAPEM = []byte("-----BEGIN CERTIFICATE-----\nnot-a-real-ca\n-----END CERTIFICATE-----\n")
	s := RenderSecret(cfg, desired("abc"), "uzw_the-token")

	if s.Name != "uzi-hw-abc-token" || s.Namespace != "uzi-workers" {
		t.Fatalf("secret = %s/%s", s.Namespace, s.Name)
	}
	if string(s.Data["worker_token"]) != "uzw_the-token" {
		t.Error("the join token must be delivered as a FILE key, never a secretKeyRef env var")
	}
	if string(s.Data["ca.crt"]) != string(cfg.APICAPEM) {
		t.Error("the CA must ride the worker's own Secret")
	}

	// No CA configured => no key, and no NODE_EXTRA_CA_CERTS pointing at a file that
	// does not exist.
	plain := RenderSecret(testConfig(), desired("abc"), "uzw_the-token")
	if _, ok := plain.Data["ca.crt"]; ok {
		t.Error("no CA configured: the key must be absent")
	}
	dep := RenderDeployment(testConfig(), desired("abc"), testSpec(t, "base", "m"))
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "NODE_EXTRA_CA_CERTS" {
			t.Error("NODE_EXTRA_CA_CERTS set with no CA relayed: it would point at a missing file")
		}
	}
	withCA := RenderDeployment(cfg, desired("abc"), testSpec(t, "base", "m"))
	var found string
	for _, e := range withCA.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "NODE_EXTRA_CA_CERTS" {
			found = e.Value
		}
	}
	if found != "/run/secrets/ca.crt" {
		t.Errorf("NODE_EXTRA_CA_CERTS = %q, want the relayed CA's path", found)
	}
}

func TestWorkerEnvDefaultsToSingleRunCap(t *testing.T) {
	// testConfig() leaves MaxConcurrentRuns at its zero value, which must still render
	// "1" — the default cap, so any RenderConfig{} built in a test keeps working. The
	// cap is now operator-configurable (see TestWorkerEnvPropagatesTheConfiguredCap).
	dep := RenderDeployment(testConfig(), desired("abc"), testSpec(t, "base", "m"))
	env := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
		// The token is a FILE. A secretKeyRef would reopen the /proc/<pid>/environ leak
		// docs/proc-hardening.md closed.
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			t.Errorf("env %q is a secretKeyRef: the join token must be file-mounted", e.Name)
		}
	}
	if env["WORKER_MAX_CONCURRENT_RUNS"] != "1" {
		t.Errorf("WORKER_MAX_CONCURRENT_RUNS = %q, want the default 1 when unset", env["WORKER_MAX_CONCURRENT_RUNS"])
	}
	// Short Service names do not resolve cross-namespace; the worker can only reach the
	// api by FQDN.
	if env["UZI_API_URL"] != "https://api.uzi.svc.cluster.local:8443" {
		t.Errorf("UZI_API_URL = %q", env["UZI_API_URL"])
	}
	if env["UZI_WORKER_TOKEN_FILE"] != "/run/secrets/worker_token" {
		t.Errorf("UZI_WORKER_TOKEN_FILE = %q", env["UZI_WORKER_TOKEN_FILE"])
	}
}

// A configured cap propagates to the pod env, and — because the env is part of the
// hashed pod template — changing it rolls the pod (the spec hash differs from the
// default's). That roll-on-change is what makes an operator raising the cap take
// effect on a worker's next roll rather than silently only on brand-new workers.
func TestWorkerEnvPropagatesTheConfiguredCap(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConcurrentRuns = 3
	dep := RenderDeployment(cfg, desired("abc"), testSpec(t, "base", "m"))
	var got string
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "WORKER_MAX_CONCURRENT_RUNS" {
			got = e.Value
		}
	}
	if got != "3" {
		t.Errorf("WORKER_MAX_CONCURRENT_RUNS = %q, want the configured 3", got)
	}
	// The cap is in the hashed pod template, so a different cap is a different spec hash
	// (roll-on-change): the same worker rolls onto the new cap when it next rolls.
	if h1, h3 := SpecHashOf(testConfig(), desired("abc"), testSpec(t, "base", "m")),
		SpecHashOf(cfg, desired("abc"), testSpec(t, "base", "m")); h1 == h3 {
		t.Error("spec hash must differ between cap 1 and cap 3 so raising the cap rolls the pod")
	}
}

// Every created object carries the provenance stamp. It IS the teardown authority:
// an object without it is an orphan we must never touch.
func TestEveryRenderedObjectIsStamped(t *testing.T) {
	cfg := testConfig()
	w := desired("abc")
	spec := testSpec(t, "base", "m")

	objs := map[string]map[string]string{
		"secret":     RenderSecret(cfg, w, "t").Labels,
		"deployment": RenderDeployment(cfg, w, spec).Labels,
		"pod":        RenderDeployment(cfg, w, spec).Spec.Template.Labels,
		"data-pvc":   RenderPVCs(cfg, w, spec)[0].Labels,
		"nix-pvc":    RenderPVCs(cfg, w, spec)[1].Labels,
	}
	for name, labels := range objs {
		if labels[LabelManagedBy] != ValueManagedBy {
			t.Errorf("%s: missing the managed-by provenance stamp", name)
		}
		if labels[LabelWorkerID] != "abc" {
			t.Errorf("%s: missing the worker-id label", name)
		}
		if id, ours := IsOurs(labels); !ours || id != "abc" {
			t.Errorf("%s: IsOurs = %q/%v", name, id, ours)
		}
	}
}

func TestIsOursNeedsBothHalvesOfTheStamp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{"nothing", map[string]string{}},
		{"stamp without an id", map[string]string{LabelManagedBy: ValueManagedBy}},
		{"id without a stamp", map[string]string{LabelWorkerID: "abc"}},
		{"someone else's stamp", map[string]string{LabelManagedBy: "Helm", LabelWorkerID: "abc"}},
	} {
		if _, ours := IsOurs(tc.labels); ours {
			t.Errorf("%s: IsOurs = true, want false — we may only ever tear down what we stamped", tc.name)
		}
	}
}

// The spec hash must move when the RENDERING changes (a new image tag is the case
// that matters: the controller resolves it from its own config, so the generation
// never moves for it) and must NOT move for an unrelated worker.
func TestSpecHashTracksTheRenderingNotTheGeneration(t *testing.T) {
	cfg := testConfig()
	w := desired("abc")

	base := SpecHashOf(cfg, w, testSpec(t, "base", "m"))

	// Same everything => same hash. If this is unstable, every reconcile rolls every
	// pod forever.
	if again := SpecHashOf(cfg, w, testSpec(t, "base", "m")); again != base {
		t.Fatal("the spec hash is not stable across renders: every reconcile would roll every pod")
	}

	// A new release's image tag.
	newTag, err := preset.NewResolver("harbor.example.com/uzi", "v2.0.0")
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	spec2, err := newTag.Resolve("base", "m")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if SpecHashOf(cfg, w, spec2) == base {
		t.Error("a new agent image tag must change the spec hash: nothing else would ever roll the worker onto a new release")
	}

	// A bigger preset.
	if SpecHashOf(cfg, w, testSpec(t, "base", "l")) == base {
		t.Error("changed preset quantities must change the spec hash")
	}

	// The generation is a SEPARATE signal and must not bleed into the hash — keeping
	// them independent is what lets each mean one thing.
	rotated := w
	rotated.Generation = 7
	if SpecHashOf(cfg, rotated, testSpec(t, "base", "m")) != base {
		t.Error("the generation must not feed the spec hash: they are independent drift signals")
	}
}

// The generation lands in the POD TEMPLATE's annotations. Anywhere else and a
// rotation never rolls the pod: the Deployment mounts the Secret by name, so a
// content-only change leaves the template byte-identical.
//
// INERT IN v1 — M2 ships no rotation path and hosted_generation is 0 on every
// provisioned worker, so nothing can change it. This unit test is the ONLY thing
// that exercises the roll. A green e2e must never be read as "rotation rolls".
func TestGenerationIsStampedOnThePodTemplate(t *testing.T) {
	dep := RenderDeployment(testConfig(), desired("abc"), testSpec(t, "base", "m"))
	if got := dep.Spec.Template.Annotations[AnnotationGeneration]; got != "0" {
		t.Errorf("generation annotation = %q, want \"0\" (v1 provisions every worker at 0)", got)
	}
	if dep.Spec.Template.Annotations[AnnotationSpecHash] == "" {
		t.Error("the spec-hash annotation must be stamped on the pod template")
	}

	rotated := desired("abc")
	rotated.Generation = 3
	rolled := RenderDeployment(testConfig(), rotated, testSpec(t, "base", "m"))
	if got := rolled.Spec.Template.Annotations[AnnotationGeneration]; got != "3" {
		t.Errorf("generation annotation = %q, want \"3\"", got)
	}
}

// --- docker-capable workers (PRD #83 M3) -----------------------------------

// THE ONE SECURITY INVARIANT on a wired worker (Decision 3): the DinD containers
// mount NONE of the worker's token/`/data`/`/nix` volumes, so a `docker run -v
// <anything>:/x` binds a filesystem that holds none of them. This is what closes the
// docker `-v` vector on k8s; the in-container guardrail only denies "no daemon", it
// is NOT containment. A failing assertion here is a real security regression, not a
// style nit — do not "fix" it by loosening the check.
func TestDindContainersMountNoneOfTheWorkersVolumes(t *testing.T) {
	// The volumes (by name) and paths that carry the worker's secrets/state. The dind
	// side must touch none of them, by EITHER name or mount path — POSTURE-INDEPENDENT.
	// (The shared run workdir at dindWorkdirDir is a SEPARATE no-secrets emptyDir; it is
	// deliberately not in this set — M-workdir shares only it, never token/data/nix.)
	forbiddenNames := map[string]bool{"token": true, "data": true, "nix": true}
	forbiddenPaths := map[string]bool{secretMountPath: true, dataMountPath: true, nixMountPath: true, nixSeedMountPath: true}

	for _, p := range dindPostures() {
		t.Run(p.name, func(t *testing.T) {
			dep := RenderDeployment(p.cfg, desiredDocker("abc"), testSpec(t, "base", "m"))
			pod := dep.Spec.Template.Spec

			var checked int
			for _, c := range pod.InitContainers {
				if c.Name != dindContainerName && c.Name != dindInitContainerName {
					continue
				}
				checked++
				for _, vm := range c.VolumeMounts {
					if forbiddenNames[vm.Name] {
						t.Errorf("dind container %q mounts the worker volume %q — this reopens the docker -v vector "+
							"(a `docker run -v` into this container would expose the worker's secret/data/nix). Decision 3: "+
							"the dind side mounts ONLY its own data root, the shared workdir, and (rootless) the socket dir.", c.Name, vm.Name)
					}
					if forbiddenPaths[vm.MountPath] {
						t.Errorf("dind container %q mounts a worker path %q — Decision 3 forbids the dind side seeing "+
							"token/data/nix at all.", c.Name, vm.MountPath)
					}
				}
			}
			// Rootless renders dind + dind-init (2); non-rootless drops dind-init (1).
			wantChecked := 2
			if p.nonRootless {
				wantChecked = 1
			}
			if checked != wantChecked {
				t.Fatalf("expected to check %d dind container(s), checked %d", wantChecked, checked)
			}
		})
	}
}

// The docker sidecars are NATIVE SIDECARS (initContainers with restartPolicy: Always
// + a startupProbe), ordered [seed-nix → dind-init → dind]. This is what makes k8s
// start the daemon before the worker and hold the worker until dockerd is listening
// (the k8s half of the keystone readiness race). Order is load-bearing: dind-init
// (the socket-dir chown) MUST precede dind, because rootless dockerd refuses to start
// without a writable runtime dir.
func TestDockerWorkerRendersNativeSidecarsInOrder(t *testing.T) {
	for _, p := range dindPostures() {
		t.Run(p.name, func(t *testing.T) {
			dep := RenderDeployment(p.cfg, desiredDocker("abc"), testSpec(t, "base", "m"))
			inits := dep.Spec.Template.Spec.InitContainers

			// Rootless: [seed-nix, dind-init, dind]. Non-rootless: [seed-nix, dind] — no
			// dind-init, since there is no shared socket dir to chown (loopback transport).
			wantOrder := []string{seedContainerName, dindInitContainerName, dindContainerName}
			if p.nonRootless {
				wantOrder = []string{seedContainerName, dindContainerName}
			}
			if len(inits) != len(wantOrder) {
				t.Fatalf("init containers = %v, want %v", initNames(inits), wantOrder)
			}
			for i, name := range wantOrder {
				if inits[i].Name != name {
					t.Fatalf("init order = %v, want %v", initNames(inits), wantOrder)
				}
			}
			// Every dind sidecar (dind, and rootless' dind-init) is native + carries a probe.
			for _, c := range inits {
				if c.Name == seedContainerName {
					continue
				}
				if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
					t.Errorf("%q must be a native sidecar (restartPolicy: Always); a plain wait-initContainer would "+
						"deadlock before the sidecar ever starts", c.Name)
				}
				if c.StartupProbe == nil || c.StartupProbe.Exec == nil {
					t.Errorf("%q must carry an exec startupProbe so k8s gates the worker on real readiness", c.Name)
				}
			}
			// The dind gate is the DAEMON being up (`docker info`), not the socket existing.
			dind := containerByName(t, inits, dindContainerName)
			if probe := dind.StartupProbe; probe == nil || probe.Exec == nil ||
				!strings.Contains(strings.Join(probe.Exec.Command, " "), "info") {
				t.Error("the dind startupProbe must run `docker info` — the socket existing is not the daemon being ready")
			}
		})
	}
}

// The worker container keeps #58's posture VERBATIM even on a docker pod — only the
// sidecar is privileged. And the dind sidecar is privileged + runs as the rootless
// uid 1000 (not the pod's 10001), which is the actual security property (a breakout
// lands as a userns-mapped unprivileged host uid).
func TestDockerWorkerKeepsWorkerPostureAndPrivilegesOnlyTheSidecar(t *testing.T) {
	for _, p := range dindPostures() {
		t.Run(p.name, func(t *testing.T) {
			dep := RenderDeployment(p.cfg, desiredDocker("abc"), testSpec(t, "base", "m"))
			pod := dep.Spec.Template.Spec

			// Pod-level posture unchanged in BOTH postures: the worker still runs as 10001
			// non-root; only the dind sidecar's identity differs.
			if *pod.SecurityContext.RunAsUser != 10001 || !*pod.SecurityContext.RunAsNonRoot {
				t.Errorf("pod runAsUser/nonRoot = %d/%v, want the #58 posture 10001/true", *pod.SecurityContext.RunAsUser, *pod.SecurityContext.RunAsNonRoot)
			}
			worker := containerByName(t, pod.Containers, workerContainerName)
			if worker.SecurityContext == nil || *worker.SecurityContext.AllowPrivilegeEscalation {
				t.Error("the worker container must keep allowPrivilegeEscalation:false — only the sidecar is privileged")
			}
			if worker.SecurityContext.Privileged != nil && *worker.SecurityContext.Privileged {
				t.Error("the WORKER container must never be privileged")
			}
			if len(worker.SecurityContext.Capabilities.Drop) != 1 || worker.SecurityContext.Capabilities.Drop[0] != "ALL" {
				t.Error("the worker container must keep drop ALL")
			}

			dind := containerByName(t, pod.InitContainers, dindContainerName)
			if dind.SecurityContext == nil || dind.SecurityContext.Privileged == nil || !*dind.SecurityContext.Privileged {
				t.Fatal("the dind sidecar must be privileged:true in the privileged-tier namespace (both postures)")
			}
			if dind.SecurityContext.SeccompProfile == nil || dind.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
				t.Error("the dind sidecar needs seccomp Unconfined so the nested daemon can profile its children")
			}

			if p.nonRootless {
				// Real root, and RunAsNonRoot MUST be false at CONTAINER scope or the pod's
				// RunAsNonRoot:true rejects the uid-0 container at admission.
				if dind.SecurityContext.RunAsUser == nil || *dind.SecurityContext.RunAsUser != 0 {
					t.Error("non-rootless dind must run as real root (uid 0)")
				}
				if dind.SecurityContext.RunAsNonRoot == nil || *dind.SecurityContext.RunAsNonRoot {
					t.Error("non-rootless dind must set RunAsNonRoot:false at container scope (the pod is RunAsNonRoot:true, so a uid-0 container is rejected without it)")
				}
				// No dind-init in the non-rootless posture (no shared socket dir to prep).
				for _, c := range pod.InitContainers {
					if c.Name == dindInitContainerName {
						t.Error("non-rootless posture must NOT render dind-init")
					}
				}
				return
			}

			// Rootless: uid 1000 (its subuid/subgid ranges), overriding the pod's 10001.
			if dind.SecurityContext.RunAsUser == nil || *dind.SecurityContext.RunAsUser != 1000 {
				t.Error("rootless dind must run as the rootless uid 1000, overriding the pod's 10001")
			}
			// dind-init runs as root but with a TIGHT cap set: drop ALL, add back only CHOWN +
			// FOWNER (the two it uses to chown/chmod the socket dir). Not the full root cap set,
			// so a compromise of this tiny helper widens the pod by exactly those two.
			dindInit := containerByName(t, pod.InitContainers, dindInitContainerName)
			if dindInit.SecurityContext == nil || dindInit.SecurityContext.RunAsUser == nil || *dindInit.SecurityContext.RunAsUser != 0 {
				t.Fatal("dind-init must run as root (uid 0) to chown/chmod the socket dir")
			}
			caps := dindInit.SecurityContext.Capabilities
			// FATAL, not Error: a nil capability set means every assertion below is
			// vacuous, and reading caps.Add past a non-fatal Error is a real nil
			// dereference — the test would PANIC rather than report the security
			// regression it exists to catch.
			if caps == nil {
				t.Fatal("dind-init declares no capabilities at all; it must drop ALL and add back exactly CHOWN + FOWNER")
			}
			if len(caps.Drop) != 1 || caps.Drop[0] != "ALL" {
				t.Error("dind-init must drop ALL capabilities")
			}
			gotAdd := map[corev1.Capability]bool{}
			for _, c := range caps.Add {
				gotAdd[c] = true
			}
			if len(caps.Add) != 2 || !gotAdd["CHOWN"] || !gotAdd["FOWNER"] {
				t.Errorf("dind-init must add exactly [CHOWN, FOWNER] and nothing else, got %v", caps.Add)
			}
		})
	}
}

// The k8s branch of the keystone resolver: DOCKER_HOST is set EXPLICITLY on the worker
// (never probed). Rootless points at the shared socket; non-rootless at pod-loopback TCP
// (no shared socket mount). A non-docker worker gets no DOCKER_HOST at all.
func TestDockerWorkerSetsExplicitDockerHostAndSharesOnlyTheSocket(t *testing.T) {
	for _, p := range dindPostures() {
		t.Run(p.name, func(t *testing.T) {
			dep := RenderDeployment(p.cfg, desiredDocker("abc"), testSpec(t, "base", "m"))
			worker := containerByName(t, dep.Spec.Template.Spec.Containers, workerContainerName)

			var dockerHost string
			for _, e := range worker.Env {
				if e.Name == "DOCKER_HOST" {
					dockerHost = e.Value
				}
			}
			socketPath := mountPath(worker, dindSocketVolume)

			if p.nonRootless {
				// Non-rootless: pod-loopback TCP, and NO shared socket mount (loopback transport).
				if dockerHost != dindLoopbackTCP {
					t.Errorf("DOCKER_HOST = %q, want the pod-loopback TCP endpoint %q", dockerHost, dindLoopbackTCP)
				}
				if socketPath != "" {
					t.Errorf("non-rootless worker must NOT mount the shared socket dir (it reaches the daemon over loopback), got %q", socketPath)
				}
				return
			}
			// Rootless: the shared unix socket, mounted at the socket dir.
			if dockerHost != "unix:///run/dind/docker.sock" {
				t.Errorf("DOCKER_HOST = %q, want the explicit shared-socket path", dockerHost)
			}
			if socketPath != dindSocketDir {
				t.Errorf("rootless worker must mount the shared socket dir at %q, got %q", dindSocketDir, socketPath)
			}
		})
	}

	// A plain worker gets neither — the docker path is fully gated on w.Docker.
	plain := RenderDeployment(dockerTestConfig(), desired("abc"), testSpec(t, "base", "m"))
	pw := containerByName(t, plain.Spec.Template.Spec.Containers, workerContainerName)
	for _, e := range pw.Env {
		if e.Name == "DOCKER_HOST" {
			t.Error("a non-docker worker must not carry DOCKER_HOST")
		}
	}
	if len(plain.Spec.Template.Spec.InitContainers) != 1 {
		t.Errorf("a non-docker worker must render exactly the seed init container, got %d", len(plain.Spec.Template.Spec.InitContainers))
	}
}

// A docker worker renders into the SEPARATE privileged namespace; a plain worker
// stays in the restricted default. The blast-radius separation (Decision 7 / Q-B) is
// only real if the objects actually land in different namespaces.
func TestDockerWorkerObjectsGoToTheDockerNamespace(t *testing.T) {
	cfg := dockerTestConfig()
	dw := desiredDocker("abc")

	if got := RenderDeployment(cfg, dw, testSpec(t, "base", "m")).Namespace; got != "uzi-workers-docker" {
		t.Errorf("docker deployment namespace = %q, want uzi-workers-docker", got)
	}
	if got := RenderSecret(cfg, dw, "uzw_t").Namespace; got != "uzi-workers-docker" {
		t.Errorf("docker secret namespace = %q, want uzi-workers-docker", got)
	}
	for _, p := range RenderPVCs(cfg, dw, testSpec(t, "base", "m")) {
		if p.Namespace != "uzi-workers-docker" {
			t.Errorf("docker pvc %q namespace = %q, want uzi-workers-docker", p.Name, p.Namespace)
		}
	}
	// The plain worker is untouched: restricted default.
	if got := RenderDeployment(cfg, desired("abc"), testSpec(t, "base", "m")).Namespace; got != "uzi-workers" {
		t.Errorf("plain deployment namespace = %q, want the restricted uzi-workers", got)
	}
}

// The docker toggle rolls the pod (specHash covers the new containers), so flipping
// it is a real re-render — but a plain worker's hash is UNCHANGED from the pre-#83
// render (the docker path is fully gated), so shipping docker never rolls the
// existing fleet.
func TestDockerFlagRollsThePodButAbsentDockerIsInert(t *testing.T) {
	for _, p := range dindPostures() {
		dockerHash := SpecHashOf(p.cfg, desiredDocker("abc"), testSpec(t, "base", "m"))
		plain := SpecHashOf(p.cfg, desired("abc"), testSpec(t, "base", "m"))
		if plain == dockerHash {
			t.Errorf("[%s] a docker worker must hash differently from a plain one, or enabling docker would never roll the pod", p.name)
		}
		// With docker off, the docker config (namespace/image/POSTURE) must not bleed into
		// a plain worker's hash: a controller that merely CAN render docker (in either
		// posture) must not roll every plain worker.
		if noCfg := SpecHashOf(testConfig(), desired("abc"), testSpec(t, "base", "m")); noCfg != plain {
			t.Errorf("[%s] a plain worker's spec hash must not depend on the docker tier config", p.name)
		}
	}

	// The POSTURE itself rolls a docker worker: rootless and non-rootless render
	// different uid/transport/containers, so their spec hashes must differ (flipping
	// workers.docker.rootless is a real re-render, not a no-op).
	rootless := SpecHashOf(dockerTestConfig(), desiredDocker("abc"), testSpec(t, "base", "m"))
	nonRootless := SpecHashOf(dockerTestConfigNonRootless(), desiredDocker("abc"), testSpec(t, "base", "m"))
	if rootless == nonRootless {
		t.Error("flipping the posture (rootless -> non-rootless) must change the docker worker's spec hash")
	}
}

// --- non-rootless posture (PRD #89) ----------------------------------------

// THE loopback-bind invariant (PRD #89 M1, mitigation #1). This is a POSITIVE
// assertion, NOT a "no 0.0.0.0 string" negative: dropping the command override
// entirely would pass a negative test while docker:dind's image entrypoint restores
// the 0.0.0.0:2375 listener, putting the unauthenticated ROOT daemon on the pod IP.
// So we assert the command is PRESENT and its ONLY --host value is the pod-loopback
// TCP endpoint. A failing assertion here is a real security regression (node-root
// daemon reachable off-pod), not a style nit.
//
// It binds NO unix socket (0.8.1 fix): the non-rootless posture drops dind-init and the
// shared dind-sock volume, so /run/dind does not exist and a `--host=unix://...` makes
// dockerd exit 1 (live-proven on dev-cluster). The worker uses the loopback TCP endpoint
// only.
func TestNonRootlessDindBindsOnlyLoopbackTCPViaExplicitCommand(t *testing.T) {
	dep := RenderDeployment(dockerTestConfigNonRootless(), desiredDocker("abc"), testSpec(t, "base", "m"))
	dind := containerByName(t, dep.Spec.Template.Spec.InitContainers, dindContainerName)

	if len(dind.Command) == 0 {
		t.Fatal("non-rootless dind MUST override the command to bind loopback explicitly — without the " +
			"override docker:dind's entrypoint restores the 0.0.0.0:2375 listener, exposing the root daemon on the pod IP")
	}
	if dind.Command[0] != "dockerd" {
		t.Errorf("dind command[0] = %q, want dockerd", dind.Command[0])
	}

	var hosts []string
	tlsOff := false
	for _, arg := range dind.Command[1:] {
		if strings.HasPrefix(arg, "--host=") {
			hosts = append(hosts, strings.TrimPrefix(arg, "--host="))
		}
		if arg == "--tls=false" {
			tlsOff = true
		}
	}
	want := map[string]bool{dindLoopbackTCP: true}
	if len(hosts) != len(want) {
		t.Fatalf("dind --host args = %v, want EXACTLY the loopback TCP endpoint (%v) and nothing else — "+
			"binding the unix socket crashes dockerd (the socket dir is dropped in this posture), and any "+
			"other bind (esp. 0.0.0.0) exposes the unauthenticated root daemon on the pod network", hosts, want)
	}
	for _, h := range hosts {
		if !want[h] {
			t.Errorf("dind binds --host=%q, which is not the loopback TCP endpoint — the unix socket crashes "+
				"dockerd here, and any other bind (esp. 0.0.0.0) exposes the unauthenticated root daemon on the pod network", h)
		}
	}
	if !tlsOff {
		t.Error("non-rootless dind must pass --tls=false (the loopback endpoint is unencrypted; the trust boundary is the pod netns)")
	}

	// The startup probe must reach the daemon over the loopback endpoint (not a socket path).
	if probe := dind.StartupProbe; probe == nil || probe.Exec == nil ||
		!strings.Contains(strings.Join(probe.Exec.Command, " "), dindLoopbackTCP) {
		t.Error("non-rootless dind startupProbe must `docker info` over the loopback TCP endpoint")
	}

	// The ROOTLESS posture must NOT carry a command override — its image entrypoint sets
	// up the daemon, and duplicating it here would be the same drift trap the worker
	// container avoids.
	rootless := RenderDeployment(dockerTestConfig(), desiredDocker("abc"), testSpec(t, "base", "m"))
	rl := containerByName(t, rootless.Spec.Template.Spec.InitContainers, dindContainerName)
	if len(rl.Command) != 0 {
		t.Errorf("rootless dind must inherit its image entrypoint, got command=%v", rl.Command)
	}
}

// M-workdir (PRD #89, a Decision-3 amendment): a no-secrets emptyDir mounted into BOTH
// the worker and the dind sidecar at the SAME path, so a `docker run -v` bind source
// under the run's checkout resolves in the daemon. Applies to BOTH postures. Secrets +
// the /data cache + /nix stay unshared (the Decision-3 invariant above still holds).
func TestDockerWorkerSharesTheRunWorkdirWithDindButNotSecrets(t *testing.T) {
	for _, p := range dindPostures() {
		t.Run(p.name, func(t *testing.T) {
			dep := RenderDeployment(p.cfg, desiredDocker("abc"), testSpec(t, "base", "m"))
			pod := dep.Spec.Template.Spec
			worker := containerByName(t, pod.Containers, workerContainerName)
			dind := containerByName(t, pod.InitContainers, dindContainerName)

			wPath := mountPath(worker, dindWorkdirVolume)
			dPath := mountPath(dind, dindWorkdirVolume)
			if wPath == "" || dPath == "" {
				t.Fatalf("both worker and dind must mount the shared workdir %q; worker=%q dind=%q", dindWorkdirVolume, wPath, dPath)
			}
			if wPath != dPath {
				t.Errorf("the shared workdir must mount at the SAME path in worker and dind: worker=%q dind=%q — "+
					"a differing path means a `docker run -v <src>` source resolves to an empty dir in the daemon", wPath, dPath)
			}
			if wPath != dindWorkdirDir {
				t.Errorf("shared workdir path = %q, want %q (the agent's runner-clone root, coupled to agent/src/git.ts)", wPath, dindWorkdirDir)
			}

			// It is a no-secrets emptyDir (never a secret volume or a PVC).
			v := volumeByName(t, pod.Volumes, dindWorkdirVolume)
			if v.EmptyDir == nil {
				t.Error("the shared workdir must be an emptyDir (no secret, torn down with the pod)")
			}
			if v.Secret != nil || v.PersistentVolumeClaim != nil {
				t.Error("the shared workdir must carry no secret and no PVC")
			}

			// A PLAIN worker never gets the shared workdir (docker-only; keeps its hash stable).
			plain := RenderDeployment(p.cfg, desired("abc"), testSpec(t, "base", "m"))
			if mountPath(containerByName(t, plain.Spec.Template.Spec.Containers, workerContainerName), dindWorkdirVolume) != "" {
				t.Error("a plain worker must not mount the shared run workdir")
			}
			for _, vol := range plain.Spec.Template.Spec.Volumes {
				if vol.Name == dindWorkdirVolume {
					t.Error("a plain worker must not render the shared run workdir volume")
				}
			}
		})
	}
}

// TMPDIR under the shared run workdir (PRD #89 0.8.1), docker workers ONLY. A tool that
// stages a `docker run -v <src>` bind source under $TMPDIR (uzi's own e2e defaults its
// run dir to ${TMPDIR:-/tmp}) needs <src> to resolve in the DAEMON's filesystem; the
// daemon shares only dindWorkdirDir with the worker, so tmp must live under it. A plain
// worker gets no TMPDIR (docker-only, so its spec/hash is untouched).
func TestDockerWorkerTMPDIRUnderSharedWorkdirAndPlainHasNone(t *testing.T) {
	envValue := func(c corev1.Container, name string) (string, bool) {
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value, true
			}
		}
		return "", false
	}
	for _, p := range dindPostures() {
		t.Run(p.name, func(t *testing.T) {
			dep := RenderDeployment(p.cfg, desiredDocker("abc"), testSpec(t, "base", "m"))
			worker := containerByName(t, dep.Spec.Template.Spec.Containers, workerContainerName)

			tmp, ok := envValue(worker, "TMPDIR")
			if !ok {
				t.Fatal("a docker worker must set TMPDIR so tmp-staged bind sources land in the dind-visible shared workdir")
			}
			if tmp != dindWorkdirDir {
				t.Errorf("TMPDIR = %q, want %q (the shared run workdir the dind sidecar also mounts)", tmp, dindWorkdirDir)
			}
			// TMPDIR must resolve inside the shared workdir mount, or a bind source under it
			// is invisible to the daemon.
			if mountPath(worker, dindWorkdirVolume) != dindWorkdirDir {
				t.Errorf("TMPDIR %q is not covered by the shared workdir mount %q", tmp, dindWorkdirDir)
			}
		})
	}

	// A PLAIN worker never gets TMPDIR (docker-only; keeps its hash stable).
	plain := RenderDeployment(dockerTestConfig(), desired("abc"), testSpec(t, "base", "m"))
	pw := containerByName(t, plain.Spec.Template.Spec.Containers, workerContainerName)
	if _, ok := envValue(pw, "TMPDIR"); ok {
		t.Error("a plain worker must not set TMPDIR (docker-only env; setting it would change the plain worker's spec hash)")
	}
}

// dind sidecar resources (PRD #89 0.8.1): BOTH requests and limits are configurable per
// cluster (in-daemon image builds OOM the 2Gi default, and the request must rise with the
// limit or the scheduler under-reserves). Empty overrides keep the built-in default; set
// overrides win; a partial override leaves the untouched field at its default.
func TestDindResourceRequestsAndLimitsDefaultAndOverride(t *testing.T) {
	dindOf := func(cfg RenderConfig) corev1.Container {
		dep := RenderDeployment(cfg, desiredDocker("abc"), testSpec(t, "base", "m"))
		return containerByName(t, dep.Spec.Template.Spec.InitContainers, dindContainerName)
	}
	eq := func(t *testing.T, got resource.Quantity, want string, what string) {
		t.Helper()
		if w := resource.MustParse(want); got.Cmp(w) != 0 {
			t.Errorf("dind %s = %s, want %s", what, got.String(), want)
		}
	}

	// Default: requests 250m / 256Mi, limits 2 / 2Gi.
	def := dindOf(dockerTestConfigNonRootless())
	eq(t, def.Resources.Requests[corev1.ResourceCPU], "250m", "default CPU request")
	eq(t, def.Resources.Requests[corev1.ResourceMemory], "256Mi", "default memory request")
	eq(t, def.Resources.Limits[corev1.ResourceCPU], "2", "default CPU limit")
	eq(t, def.Resources.Limits[corev1.ResourceMemory], "2Gi", "default memory limit")

	// Full override raises requests AND limits (dev-cluster's shape).
	cfg := dockerTestConfigNonRootless()
	cfg.DinDRequestCPU = "500m"
	cfg.DinDRequestMemory = "2Gi"
	cfg.DinDLimitCPU = "4"
	cfg.DinDLimitMemory = "6Gi"
	ov := dindOf(cfg)
	eq(t, ov.Resources.Requests[corev1.ResourceCPU], "500m", "overridden CPU request")
	eq(t, ov.Resources.Requests[corev1.ResourceMemory], "2Gi", "overridden memory request")
	eq(t, ov.Resources.Limits[corev1.ResourceCPU], "4", "overridden CPU limit")
	eq(t, ov.Resources.Limits[corev1.ResourceMemory], "6Gi", "overridden memory limit")

	// A PARTIAL override leaves the untouched fields at their default (per-field fallback).
	part := dockerTestConfigNonRootless()
	part.DinDLimitMemory = "6Gi" // only the memory limit
	pc := dindOf(part)
	eq(t, pc.Resources.Limits[corev1.ResourceMemory], "6Gi", "partial: overridden memory limit")
	eq(t, pc.Resources.Limits[corev1.ResourceCPU], "2", "partial: CPU limit stays default")
	eq(t, pc.Resources.Requests[corev1.ResourceMemory], "256Mi", "partial: memory request stays default")

	// The override rolls the docker worker's spec hash (it changes the rendered pod).
	if SpecHashOf(dockerTestConfigNonRootless(), desiredDocker("abc"), testSpec(t, "base", "m")) ==
		SpecHashOf(cfg, desiredDocker("abc"), testSpec(t, "base", "m")) {
		t.Error("raising the dind resources must change the docker worker's spec hash (else a bump never rolls the pod)")
	}
}

// Soft anti-affinity (PRD #89 mitigation #3): a docker worker prefers to avoid nodes
// running the crown-jewel pods (the api holding UZI_SECRET_KEY + CNPG). PREFERRED, not
// required (it must never wedge scheduling), node-scoped, cross-namespace. A plain
// worker gets none (so its spec/hash is untouched).
func TestDockerWorkerGetsSoftAntiAffinityAndPlainDoesNot(t *testing.T) {
	dep := RenderDeployment(dockerTestConfigNonRootless(), desiredDocker("abc"), testSpec(t, "base", "m"))
	aff := dep.Spec.Template.Spec.Affinity
	if aff == nil || aff.PodAntiAffinity == nil || len(aff.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
		t.Fatal("a docker worker must get PREFERRED pod anti-affinity keeping it off crown-jewel nodes")
	}
	if len(aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0 {
		t.Error("the anti-affinity must be SOFT (preferred) — a required term could wedge scheduling on a full cluster")
	}
	for _, term := range aff.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		if term.PodAffinityTerm.TopologyKey != "kubernetes.io/hostname" {
			t.Errorf("anti-affinity topologyKey = %q, want kubernetes.io/hostname (node-scoped)", term.PodAffinityTerm.TopologyKey)
		}
		if term.PodAffinityTerm.NamespaceSelector == nil {
			t.Error("anti-affinity must set an (empty=all) namespaceSelector: the crown jewels are in another namespace")
		}
	}

	// A plain worker carries no affinity at all.
	plain := RenderDeployment(dockerTestConfigNonRootless(), desired("abc"), testSpec(t, "base", "m"))
	if plain.Spec.Template.Spec.Affinity != nil {
		t.Error("a plain worker must not carry the docker anti-affinity (keeps its spec hash stable)")
	}
}

func containerByName(t *testing.T, cs []corev1.Container, name string) corev1.Container {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container named %q", name)
	return corev1.Container{}
}

// --- the seed script -------------------------------------------------------
//
// These close, locally and with no cluster, the one thing the kind run CANNOT
// prove: kind's local-path-provisioner makes a plain directory on the node
// filesystem, so a fresh PVC there really IS empty — while dev-cluster's hypervisor
// CSI formats ext4 and a fresh PVC arrives carrying `lost+found`. An emptiness
// check therefore passes on kind and strands every worker on dev-cluster. It is a
// property of the check, so it needs no kubelet.

// runSeed executes the REAL script (same text, different paths) against temp dirs.
// markerFile is the image toolchain-identity marker (the /etc/uzi-toolchain-profile
// analogue); dataDir is the reseed's data-clear target ("" to not exercise it).
func runSeed(t *testing.T, src, dst, markerFile, dataDir string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", nixSeedScript(src, dst, markerFile, dataDir))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// markerFileWith writes an image toolchain-identity marker file holding value (the
// store-hash profile path in production) and returns its path.
func markerFileWith(t *testing.T, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "uzi-toolchain-profile")
	if err := os.WriteFile(p, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// recordedMarker reads the toolchain identity the seed recorded in the PVC.
func recordedMarker(t *testing.T, dst string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dst, nixToolchainMarker))
	if err != nil {
		return ""
	}
	return string(b)
}

func seedSource(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "store", "abc-pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "store", "abc-pkg", "bin"), []byte("payload"), 0o555); err != nil {
		t.Fatal(err)
	}
	return src
}

func seeded(t *testing.T, dst string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dst, "store", "abc-pkg", "bin"))
	return err == nil
}

func TestSeedScriptSeedsAnEmptyVolume(t *testing.T) {
	dst := t.TempDir()
	if out, err := runSeed(t, seedSource(t), dst, markerFileWith(t, "store-hash-A"), ""); err != nil {
		t.Fatalf("seed failed: %v\n%s", err, out)
	}
	if !seeded(t, dst) {
		t.Fatal("the store was not copied")
	}
	if _, err := os.Stat(filepath.Join(dst, nixSentinel)); err != nil {
		t.Fatal("the sentinel must be written after a successful copy")
	}
	// A fresh seed must ALSO record the image's toolchain identity, or the very next
	// boot would read an absent marker as a mismatch and reseed on every start.
	if got := recordedMarker(t, dst); got != "store-hash-A" {
		t.Fatalf("the recorded toolchain marker must be written on a fresh seed, got %q", got)
	}
}

// THE TRAP. A fresh ext4 PVC is NOT empty — it carries lost+found. An `ls -A` guard
// reads that as "already seeded", skips forever, and the worker boots with a masked
// empty /nix: no nix binary on PATH, and devbox then downloads an unpinned
// nix-installer and escalates via sudo. kind cannot catch this (local-path makes a
// plain directory), which is exactly why this test exists here.
func TestSeedScriptStillSeedsAVolumeThatCameWithLostAndFound(t *testing.T) {
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := runSeed(t, seedSource(t), dst, markerFileWith(t, "store-hash-A"), "")
	if err != nil {
		t.Fatalf("seed failed: %v\n%s", err, out)
	}
	if !seeded(t, dst) {
		t.Fatal("a volume containing only lost+found is a FRESH volume and must still seed. " +
			"This is the ext4 trap: an emptiness check passes on kind's local-path provisioner " +
			"and strands every worker on dev-cluster's hypervisor CSI.")
	}
}

// The skip is version-aware now: a bare sentinel is NO LONGER enough. Skip fires only
// when the sentinel AND the recorded toolchain marker both exist AND match the image
// marker — that is the store the running image expects, so it is never overwritten.
func TestSeedScriptSkipsWhenTheRecordedMarkerMatches(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, nixSentinel), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, nixToolchainMarker), []byte("store-hash-A"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runSeed(t, seedSource(t), dst, markerFileWith(t, "store-hash-A"), "")
	if err != nil {
		t.Fatalf("seed failed: %v\n%s", err, out)
	}
	if seeded(t, dst) {
		t.Fatal("a matching-marker volume must never be overwritten: it holds a provisioned store the image expects")
	}
	if !strings.Contains(out, "already seeded") {
		t.Errorf("expected a skip, got:\n%s", out)
	}
}

// A toolchain-changing image roll: the recorded marker no longer matches the image's
// baked identity, so the stale store MUST be reseeded (this is the #92 bug fix). The
// recorded marker is advanced to the new value so the next boot skips.
func TestSeedScriptReseedsWhenTheMarkerMismatches(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, nixSentinel), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A stale store path from the OLD image, plus the OLD recorded marker.
	if err := os.MkdirAll(filepath.Join(dst, "store", "stale-pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, nixToolchainMarker), []byte("store-hash-OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runSeed(t, seedSource(t), dst, markerFileWith(t, "store-hash-NEW"), "")
	if err != nil {
		t.Fatalf("reseed failed: %v\n%s", err, out)
	}
	if !seeded(t, dst) {
		t.Fatal("a changed toolchain marker must reseed the store from the new image")
	}
	if _, err := os.Stat(filepath.Join(dst, "store", "stale-pkg")); err == nil {
		t.Fatal("the stale store payload must be wiped on a reseed, not merged around")
	}
	if got := recordedMarker(t, dst); got != "store-hash-NEW" {
		t.Fatalf("the recorded marker must advance to the new toolchain identity, got %q", got)
	}
}

// THE #92 HEAL. The live broken worker was seeded by a pre-M2 image: it has a
// sentinel but NO recorded marker. That MUST count as a mismatch and reseed — an
// "absent marker = skip" reading would leave it stranded with a dead PATH forever.
func TestSeedScriptReseedsALegacySentinelWithoutAMarker(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, nixSentinel), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "store", "stale-pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runSeed(t, seedSource(t), dst, markerFileWith(t, "store-hash-NEW"), "")
	if err != nil {
		t.Fatalf("reseed failed: %v\n%s", err, out)
	}
	if !seeded(t, dst) {
		t.Fatal("a legacy sentinel with no recorded marker must reseed — that is how the live broken worker heals")
	}
	if got := recordedMarker(t, dst); got != "store-hash-NEW" {
		t.Fatalf("the reseed must record the image's toolchain identity, got %q", got)
	}
}

// A reseed wipes the store, so the SHARED devbox/nix provisioning state persisted
// under the data volume (HOME-derived under agent-home; the E2E stub's provision
// root) would dangle into the wiped store and break the next `devbox install`. The
// reseed clears exactly that state — and the SKIP path leaves it untouched.
func TestSeedScriptClearsDanglingDataStateOnReseedOnly(t *testing.T) {
	// The subpaths verified from agent/src (sdkHomeRoot=/data/agent-home,
	// buildProvisionEnv HOME=that; executor.ts provisionRoot=/data/provision).
	danglers := []string{
		"agent-home/.local/share/devbox",
		"agent-home/.local/state/nix",
		"agent-home/.local/share/nix",
		"provision",
	}
	seedDangling := func(dataDir string) {
		for _, d := range danglers {
			if err := os.MkdirAll(filepath.Join(dataDir, d, "child"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}

	// RESEED (legacy sentinel, no marker): the dangling state is cleared.
	t.Run("reseed clears", func(t *testing.T) {
		dst := t.TempDir()
		dataDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dst, nixSentinel), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		seedDangling(dataDir)
		// A sibling under agent-home that is NOT provisioning state must SURVIVE.
		if err := os.MkdirAll(filepath.Join(dataDir, "agent-home", "some-run-id"), 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := runSeed(t, seedSource(t), dst, markerFileWith(t, "store-hash-NEW"), dataDir)
		if err != nil {
			t.Fatalf("reseed failed: %v\n%s", err, out)
		}
		for _, d := range danglers {
			if _, err := os.Stat(filepath.Join(dataDir, d)); !os.IsNotExist(err) {
				t.Fatalf("reseed must clear dangling provisioning state %q (err=%v)", d, err)
			}
		}
		if _, err := os.Stat(filepath.Join(dataDir, "agent-home", "some-run-id")); err != nil {
			t.Fatal("the reseed must NOT wipe per-run SDK homes, only the shared provisioning state")
		}
	})

	// SKIP (marker matches): the data state is left entirely alone.
	t.Run("skip leaves data untouched", func(t *testing.T) {
		dst := t.TempDir()
		dataDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dst, nixSentinel), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, nixToolchainMarker), []byte("store-hash-A"), 0o644); err != nil {
			t.Fatal(err)
		}
		seedDangling(dataDir)
		out, err := runSeed(t, seedSource(t), dst, markerFileWith(t, "store-hash-A"), dataDir)
		if err != nil {
			t.Fatalf("seed failed: %v\n%s", err, out)
		}
		for _, d := range danglers {
			if _, err := os.Stat(filepath.Join(dataDir, d)); err != nil {
				t.Fatalf("the SKIP path must NOT touch data state, but %q is gone (err=%v)", d, err)
			}
		}
	})
}

// The sentinel is written LAST, so an interrupted copy re-runs IN FULL rather than
// resuming into a half-seeded store — the same property migrate_tree's sentinel
// idiom already has in agent/templates/entrypoint.sh.
//
// The 0555 dirs are the point, not decoration: tar will NOT extract over a
// read-only directory a previous pass already sealed ("Cannot open: File exists"),
// so without the wipe this re-run CrashLoops forever and strands the worker behind
// a manual PVC deletion. Found by running it on a real kubelet, reproduced here.
func TestSeedScriptReRunsInFullOverAnInterruptedCopysReadOnlyDirs(t *testing.T) {
	dst := t.TempDir()
	// A previous attempt that got as far as sealing a store path read-only.
	sealed := filepath.Join(dst, "store", "abc-pkg")
	if err := os.MkdirAll(sealed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sealed, "bin"), []byte("stale"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) }) // so t.TempDir can clean up

	out, err := runSeed(t, seedSource(t), dst, markerFileWith(t, "store-hash-A"), "")
	if err != nil {
		t.Fatalf("an interrupted seed must re-run in full over its own read-only dirs: %v\n%s", err, out)
	}
	if !seeded(t, dst) {
		t.Fatal("the store was not re-seeded")
	}
	// The stale content must be REPLACED, not merged around: a half-written store
	// path is corrupt, and the store is content-addressed.
	got, err := os.ReadFile(filepath.Join(dst, "store", "abc-pkg", "bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Errorf("stale content survived the re-seed: %q", got)
	}
}

// The destination root is the KUBELET'S MOUNT POINT, owned root:<fsGroup>. uid
// 10001 does not own it, so any attempt to restore metadata onto it EPERMs, tar
// exits non-zero, the sentinel is never written, and the pod CrashLoops forever
// having copied the store perfectly. Archiving `.` is what causes that; archiving
// the top-level entries is what avoids it.
//
// This is the failure the kind run actually produced, and no fake client could have.
func TestSeedScriptNeverRestoresMetadataOntoTheMountRoot(t *testing.T) {
	script := nixSeedScript("/nix", "/nix-seed", toolchainProfileMarker, dataSeedMountPath)
	if strings.Contains(script, "tar -cf - -C /nix .") {
		t.Error("archiving `.` puts a `./` member in the archive, so extraction ends by chmod/utime-ing " +
			"the destination ROOT — the kubelet's mount point, which uid 10001 does not own. It EPERMs, " +
			"tar exits non-zero, and the pod CrashLoops with a perfectly copied store.")
	}
	if !strings.Contains(script, "$(ls -A)") {
		t.Error("the archive must enumerate the top-level entries so `.` never enters it")
	}
	// -mindepth 1 is load-bearing twice: it keeps the wipe off the mount root, and
	// busybox chmod -R ABORTS on the un-chmodable root without ever reaching the
	// children.
	if !strings.Contains(script, "-mindepth 1") {
		t.Error("the wipe must be scoped below the mount root: busybox chmod -R aborts on the root it cannot chmod")
	}
}

// THE SENTINEL MUST NEVER LIE, and this is the only failure in the whole seed that
// would be SILENT rather than a CrashLoop.
//
// The pipeline is producer|consumer, and without pipefail only the CONSUMER's status
// gates the sentinel write. A producer that dies while still emitting a well-formed
// short archive therefore writes "seeded" over a PARTIAL store — and every later boot
// skips seeding forever, on a store missing paths. Verified in the real image: a
// failing producer piped to a succeeding consumer exits 0.
func TestSeedScriptNeverWritesTheSentinelOnAProducerFailure(t *testing.T) {
	if !strings.Contains(nixSeedScript("/nix", "/nix-seed", toolchainProfileMarker, dataSeedMountPath), "set -o pipefail") {
		t.Fatal("the seed pipeline must set pipefail: without it a producer failure that still yields a " +
			"well-formed short archive writes the 'seeded' sentinel over a partial store, and every later " +
			"boot skips seeding forever")
	}

	// Behavioural half: a source that cannot be read must NOT leave a sentinel behind.
	dst := t.TempDir()
	out, err := runSeed(t, filepath.Join(t.TempDir(), "does-not-exist"), dst, markerFileWith(t, "store-hash-A"), "")
	if err == nil {
		t.Fatalf("seeding from a missing source must fail loudly, got success:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(dst, nixSentinel)); statErr == nil {
		t.Fatal("a failed seed wrote the sentinel — every later boot would skip seeding a store that was never copied")
	}
}

// tar, never cp. Nix store paths are 0555 read-only dirs, and `cp` in this image is
// BUSYBOX (the image installs no coreutils), so its -a behaviour on read-only dirs
// and hardlinks is exactly what must not be assumed.
func TestSeedScriptUsesTarNotCp(t *testing.T) {
	script := nixSeedScript("/nix", "/nix-seed", toolchainProfileMarker, dataSeedMountPath)
	if !strings.Contains(script, "tar -cf -") {
		t.Error("the seed must stream through tar")
	}
	if strings.Contains(script, "cp -a") || strings.Contains(script, "cp -r") {
		t.Error("cp is BusyBox in this image and fails on the store's 0555 dirs; use tar")
	}
	// The SKIP DECISION must be the sentinel and nothing else. An emptiness check is
	// the ext4 trap — and note `ls -A` legitimately appears in this script to
	// enumerate the archive's top-level entries, so the thing to forbid is testing
	// emptiness, not the command. TestSeedScriptStillSeedsAVolumeThatCameWithLostAndFound
	// is the behavioural half of this; this half stops the shape coming back.
	if strings.Contains(script, `-z "$(ls -A`) || strings.Contains(script, `-n "$(ls -A`) {
		t.Error("idempotence must be a POSITIVE sentinel, never an emptiness check: a fresh ext4 PVC carries lost+found, " +
			"so an emptiness check passes on kind's local-path provisioner and strands every worker on dev-cluster")
	}
	// The skip gate is version-aware now (PRD #92 M2): it is decided by the sentinel
	// AND a recorded toolchain marker that MATCHES the image marker — never a bare
	// sentinel and never an emptiness check. Assert the new gate shape.
	if !strings.Contains(script, "if [ -f /nix-seed/"+nixSentinel+" ] && [ -f /nix-seed/"+nixToolchainMarker+" ]") {
		t.Error("the skip must require BOTH the sentinel and the recorded toolchain marker")
	}
	if !strings.Contains(script, `[ "$WANT" = "$HAVE" ]`) {
		t.Error("the skip must additionally require the recorded marker to MATCH the image marker, or a stale store is never reseeded")
	}
}
