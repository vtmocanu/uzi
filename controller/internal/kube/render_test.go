package kube

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

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
		// A ResourceQuota on requests.* rejects any pod with a container declaring none —
		// initContainers included.
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
// PVC at /nix.
func TestNixSeedMountsAtNixSeedNotNix(t *testing.T) {
	dep := RenderDeployment(testConfig(), desired("abc"), testSpec(t, "base", "m"))
	pod := dep.Spec.Template.Spec

	if len(pod.InitContainers) != 1 {
		t.Fatalf("%d init containers, want 1", len(pod.InitContainers))
	}
	init := pod.InitContainers[0]
	if len(init.VolumeMounts) != 1 || init.VolumeMounts[0].Name != "nix" {
		t.Fatalf("init mounts = %v, want the nix volume alone", init.VolumeMounts)
	}
	if got := init.VolumeMounts[0].MountPath; got != "/nix-seed" {
		t.Fatalf("init nix mountPath = %q, want /nix-seed (mounting at /nix masks the store being copied)", got)
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
		if got := nix.Spec.Resources.Requests.Storage().String(); got != "4Gi" {
			t.Errorf("size %q: /nix = %s, want the flat 4Gi", tc.size, got)
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

func TestWorkerEnvPinsTheSingleRunCap(t *testing.T) {
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
		t.Error("every preset pins WORKER_MAX_CONCURRENT_RUNS=1 until PRD #51 lands")
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
	dep := RenderDeployment(dockerTestConfig(), desiredDocker("abc"), testSpec(t, "base", "m"))
	pod := dep.Spec.Template.Spec

	// The volumes (by name) and paths that carry the worker's secrets/state. The dind
	// side must touch none of them, by EITHER name or mount path.
	forbiddenNames := map[string]bool{"token": true, "data": true, "nix": true}
	forbiddenPaths := map[string]bool{secretMountPath: true, dataMountPath: true, nixMountPath: true, nixSeedMountPath: true}

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
					"the dind side mounts ONLY the shared socket dir and its own data root.", c.Name, vm.Name)
			}
			if forbiddenPaths[vm.MountPath] {
				t.Errorf("dind container %q mounts a worker path %q — Decision 3 forbids the dind side seeing "+
					"token/data/nix at all.", c.Name, vm.MountPath)
			}
		}
	}
	if checked != 2 {
		t.Fatalf("expected to check exactly the dind + dind-init containers, checked %d", checked)
	}
}

// The docker sidecars are NATIVE SIDECARS (initContainers with restartPolicy: Always
// + a startupProbe), ordered [seed-nix → dind-init → dind]. This is what makes k8s
// start the daemon before the worker and hold the worker until dockerd is listening
// (the k8s half of the keystone readiness race). Order is load-bearing: dind-init
// (the socket-dir chown) MUST precede dind, because rootless dockerd refuses to start
// without a writable runtime dir.
func TestDockerWorkerRendersNativeSidecarsInOrder(t *testing.T) {
	dep := RenderDeployment(dockerTestConfig(), desiredDocker("abc"), testSpec(t, "base", "m"))
	inits := dep.Spec.Template.Spec.InitContainers

	if len(inits) != 3 {
		t.Fatalf("%d init containers, want 3 (seed-nix, dind-init, dind)", len(inits))
	}
	if inits[0].Name != seedContainerName || inits[1].Name != dindInitContainerName || inits[2].Name != dindContainerName {
		t.Fatalf("init order = [%s %s %s], want [%s %s %s]",
			inits[0].Name, inits[1].Name, inits[2].Name, seedContainerName, dindInitContainerName, dindContainerName)
	}
	for _, c := range []corev1.Container{inits[1], inits[2]} {
		if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			t.Errorf("%q must be a native sidecar (restartPolicy: Always); a plain wait-initContainer would "+
				"deadlock before the sidecar ever starts", c.Name)
		}
		if c.StartupProbe == nil || c.StartupProbe.Exec == nil {
			t.Errorf("%q must carry an exec startupProbe so k8s gates the worker on real readiness", c.Name)
		}
	}
	// The dind gate is the DAEMON being up (`docker info`), not the socket existing.
	if probe := inits[2].StartupProbe; probe == nil || probe.Exec == nil ||
		!strings.Contains(strings.Join(probe.Exec.Command, " "), "info") {
		t.Error("the dind startupProbe must run `docker info` — the socket existing is not the daemon being ready")
	}
}

// The worker container keeps #58's posture VERBATIM even on a docker pod — only the
// sidecar is privileged. And the dind sidecar is privileged + runs as the rootless
// uid 1000 (not the pod's 10001), which is the actual security property (a breakout
// lands as a userns-mapped unprivileged host uid).
func TestDockerWorkerKeepsWorkerPostureAndPrivilegesOnlyTheSidecar(t *testing.T) {
	dep := RenderDeployment(dockerTestConfig(), desiredDocker("abc"), testSpec(t, "base", "m"))
	pod := dep.Spec.Template.Spec

	// Pod-level posture unchanged: the worker still runs as 10001 non-root.
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
		t.Fatal("the dind sidecar must be privileged:true in the privileged-tier namespace")
	}
	if dind.SecurityContext.RunAsUser == nil || *dind.SecurityContext.RunAsUser != 1000 {
		t.Error("the dind sidecar must run as the rootless uid 1000 (its subuid/subgid ranges), overriding the pod's 10001")
	}
	if dind.SecurityContext.SeccompProfile == nil || dind.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined {
		t.Error("the dind sidecar needs seccomp Unconfined so the nested daemon can profile its children")
	}
}

// The k8s branch of the keystone resolver: DOCKER_HOST is set EXPLICITLY on the
// worker (never probed), pointing at the shared socket. A non-docker worker gets no
// DOCKER_HOST at all.
func TestDockerWorkerSetsExplicitDockerHostAndSharesOnlyTheSocket(t *testing.T) {
	dep := RenderDeployment(dockerTestConfig(), desiredDocker("abc"), testSpec(t, "base", "m"))
	worker := containerByName(t, dep.Spec.Template.Spec.Containers, workerContainerName)

	var dockerHost string
	socketMounted := false
	for _, e := range worker.Env {
		if e.Name == "DOCKER_HOST" {
			dockerHost = e.Value
		}
	}
	for _, vm := range worker.VolumeMounts {
		if vm.Name == dindSocketVolume {
			socketMounted = true
			if vm.MountPath != dindSocketDir {
				t.Errorf("worker mounts the socket dir at %q, want %q", vm.MountPath, dindSocketDir)
			}
		}
	}
	if dockerHost != "unix:///run/dind/docker.sock" {
		t.Errorf("DOCKER_HOST = %q, want the explicit shared-socket path", dockerHost)
	}
	if !socketMounted {
		t.Error("the worker must mount the shared socket dir to reach the daemon")
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
	cfg := dockerTestConfig()
	plain := SpecHashOf(cfg, desired("abc"), testSpec(t, "base", "m"))
	dockerHash := SpecHashOf(cfg, desiredDocker("abc"), testSpec(t, "base", "m"))
	if plain == dockerHash {
		t.Error("a docker worker must hash differently from a plain one, or enabling docker would never roll the pod")
	}
	// With docker off, the docker namespace/image config must not bleed into the hash:
	// a controller that merely CAN render docker must not roll every plain worker.
	if noCfg := SpecHashOf(testConfig(), desired("abc"), testSpec(t, "base", "m")); noCfg != plain {
		t.Error("a plain worker's spec hash must not depend on whether the docker tier is configured")
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
func runSeed(t *testing.T, src, dst string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", nixSeedScript(src, dst))
	out, err := cmd.CombinedOutput()
	return string(out), err
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
	if out, err := runSeed(t, seedSource(t), dst); err != nil {
		t.Fatalf("seed failed: %v\n%s", err, out)
	}
	if !seeded(t, dst) {
		t.Fatal("the store was not copied")
	}
	if _, err := os.Stat(filepath.Join(dst, nixSentinel)); err != nil {
		t.Fatal("the sentinel must be written after a successful copy")
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
	out, err := runSeed(t, seedSource(t), dst)
	if err != nil {
		t.Fatalf("seed failed: %v\n%s", err, out)
	}
	if !seeded(t, dst) {
		t.Fatal("a volume containing only lost+found is a FRESH volume and must still seed. " +
			"This is the ext4 trap: an emptiness check passes on kind's local-path provisioner " +
			"and strands every worker on dev-cluster's hypervisor CSI.")
	}
}

func TestSeedScriptSkipsOnTheSentinel(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, nixSentinel), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runSeed(t, seedSource(t), dst)
	if err != nil {
		t.Fatalf("seed failed: %v\n%s", err, out)
	}
	if seeded(t, dst) {
		t.Fatal("a seeded volume must never be overwritten: it holds a provisioned store")
	}
	if !strings.Contains(out, "already seeded") {
		t.Errorf("expected a skip, got:\n%s", out)
	}
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

	out, err := runSeed(t, seedSource(t), dst)
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
	script := nixSeedScript("/nix", "/nix-seed")
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
	if !strings.Contains(nixSeedScript("/nix", "/nix-seed"), "set -o pipefail") {
		t.Fatal("the seed pipeline must set pipefail: without it a producer failure that still yields a " +
			"well-formed short archive writes the 'seeded' sentinel over a partial store, and every later " +
			"boot skips seeding forever")
	}

	// Behavioural half: a source that cannot be read must NOT leave a sentinel behind.
	dst := t.TempDir()
	out, err := runSeed(t, filepath.Join(t.TempDir(), "does-not-exist"), dst)
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
	script := nixSeedScript("/nix", "/nix-seed")
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
	if !strings.Contains(script, "if [ -f /nix-seed/"+nixSentinel+" ]") {
		t.Error("the skip must be decided by the sentinel file")
	}
}
