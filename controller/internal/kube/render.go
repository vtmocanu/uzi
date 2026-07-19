// Package kube materializes hosted workers as kubernetes objects (PRD #58 M3).
//
// This file is the PURE half: desired worker + resolved preset -> Secret,
// Deployment, two PVCs. No client, no context, no cluster — so every rendering
// decision below is table-testable, and materializer.go is left with only the
// question of what to call and when.
package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/preset"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/protocol"
)

// Object naming. <id> is the worker's uuid and NOTHING else: the poll query
// deliberately never selects workers.name (arbitrary 200-byte user input), so no
// user text can reach an object name.
const (
	// NamePrefix is what a hosted worker's objects are called. It is ALSO the
	// orphan-detection prefix — an object named like this that we did not stamp is
	// flagged, never adopted, never deleted.
	NamePrefix = "uzi-hw-"

	tokenSuffix = "-token"
	dataSuffix  = "-data"
	nixSuffix   = "-nix"
)

// Labels. The managed-by stamp is PROVENANCE, and provenance is what makes
// teardown safe: it is the only thing separating "a worker we created that the api
// no longer wants" (tear down — Decision 11) from "an object we have never seen
// before" (flag — Decision 9). Defining an orphan by NAME instead would make those
// two sets identical, which is the collision the PRD flagged and this resolves.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelComponent = "app.kubernetes.io/component"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelWorkerID carries the worker uuid. Every observation reads the id from
	// HERE, never by parsing it back out of the object's name.
	LabelWorkerID = "uzi.dev/hosted-worker-id"

	ValueName      = "uzi-hosted-worker"
	ValueComponent = "worker"
	// ValueManagedBy is the provenance stamp — the teardown authority.
	ValueManagedBy = "uzi-controller"
)

// Pod-template annotations: the two drift sources, which are genuinely different
// and neither covers the other.
const (
	// AnnotationGeneration rolls the pod when the join token is ROTATED. It has to
	// live in the pod template or a rotation never rolls anything: the Deployment
	// mounts the Secret by NAME, so a content-only change leaves the template
	// byte-identical, kubelet refreshes the file ~60-90s later, and the worker — which
	// read the token once at boot — 401s forever with the correct token sitting on its
	// own filesystem.
	//
	// INERT IN v1. M2 ships no rotation path and hosted_generation is 0 on every
	// provisioned worker, so nothing can ever change this. It is stamped anyway
	// (M1's contract carries the field, and the first rotation implementation must not
	// also have to discover the roll mechanism), and it is unit-tested by feeding a
	// changed generation to a fake client. NOTHING END-TO-END EXERCISES IT: a green
	// e2e must never be read as "rotation rolls the pod".
	AnnotationGeneration = "uzi.dev/hosted-generation"

	// AnnotationSpecHash rolls the pod on any RENDERING change — a new agent image
	// tag (which the controller resolves from its own config, so the generation does
	// not move), changed preset quantities, changed env. Computed over the pod template
	// AS RENDERED HERE, which sidesteps diffing against server-defaulted state
	// entirely: the standard Helm checksum/config idiom.
	AnnotationSpecHash = "uzi.dev/spec-hash"
)

// Secret keys, and the paths they mount at.
const (
	tokenKey  = "worker_token"
	caCertKey = "ca.crt"

	secretMountPath = "/run/secrets"
	tokenPath       = secretMountPath + "/" + tokenKey
	caCertPath      = secretMountPath + "/" + caCertKey

	dataMountPath = "/data"
	nixMountPath  = "/nix"
	// nixSeedMountPath is where the INIT container mounts the nix PVC — deliberately
	// NOT /nix. Mounting it at /nix would mask the very image content the init
	// container exists to copy: the same masking bug, one level down.
	nixSeedMountPath = "/nix-seed"
)

// The uid/gid pinning. 10001 is PRD #51 M2's `worker` uid; v1 is single-container
// and single-uid (PRD #51's runner container arrives with its k8s phase and is
// additive to this spec).
const (
	workerUID = int64(10001)
	workerGID = int64(10001)
)

// nixSentinel marks a completed seed. A POSITIVE sentinel, written LAST — never an
// emptiness check.
//
// This is not defensive style, it is the difference between a working fleet and a
// stranded one. dev-cluster's storage is hypervisor CSI (storage-class), which formats
// ext4, so a freshly provisioned PVC arrives already containing `lost+found`. An
// `[ -z "$(ls -A /nix-seed)" ]` guard is therefore FALSE on a brand-new volume: the
// store never seeds, /nix mounts empty and masks the image's baked store — nix
// binary included — and devbox then "self-heals" by downloading an unpinned
// nix-installer and escalating via sudo, reintroducing both things
// agent/templates/base/Dockerfile deliberately engineered out.
//
// Writing it last also makes an interrupted copy re-run IN FULL rather than resume
// into a half-seeded store, which mirrors migrate_tree's sentinel idiom already in
// agent/templates/entrypoint.sh.
const nixSentinel = ".uzi-nix-seeded"

// Container names.
const (
	workerContainerName = "worker"
	seedContainerName   = "seed-nix"
	// The docker tier's two extra containers (PRD #83 M3), both native sidecars
	// (initContainers with restartPolicy: Always).
	dindContainerName     = "dind"
	dindInitContainerName = "dind-init"
)

// The docker sidecar's wiring (PRD #83 M3). The socket dir is a shared emptyDir
// carrying ONLY the daemon socket; the data dir is the rootless daemon's own root,
// on a private emptyDir mounted into `dind` alone.
const (
	dindSocketDir  = "/run/dind"
	dindSocketPath = dindSocketDir + "/docker.sock"
	// dockerHostValue is what the ROOTLESS worker's DOCKER_HOST is set to — the k8s
	// branch of the keystone resolver (agent/src/docker-wiring.ts): explicit, never
	// probed. The NON-ROOTLESS posture uses dindLoopbackTCP instead (no shared socket).
	dockerHostValue = "unix://" + dindSocketPath
	// dindLoopbackTCP is the pod-loopback endpoint the NON-ROOTLESS worker reaches the
	// daemon on (PRD #89). Client transport is pod-local loopback TCP, matching coder:
	// loopback ONLY (never the pod IP), so the trust boundary is same-pod containers,
	// identical to the rootless shared unix socket. The dockerd command binds this
	// EXPLICITLY, which SUPPRESSES docker:dind's automatic 0.0.0.0:2375 listener — the
	// primary control keeping the unauthenticated root daemon off the pod IP.
	dindLoopbackTCP = "tcp://127.0.0.1:2375"
	// The ROOTLESS daemon's data root. docker:*-dind-rootless runs as the `rootless`
	// user (uid 1000) and stores its data under this HOME path. The non-rootless posture
	// (PRD #89) uses dindDataDirRoot below; dindContainer picks per posture.
	dindDataDir = "/home/rootless/.local/share/docker"
	// dindDataDirRoot is the NON-ROOTLESS daemon's data root (PRD #89): a root dockerd
	// stores images + build cache under /var/lib/docker, not the rootless HOME path.
	dindDataDirRoot = "/var/lib/docker"

	dindSocketVolume = "dind-sock"
	dindDataVolume   = "dind-data"
)

// The shared run workdir (PRD #89 M-workdir — a Decision-3 amendment). A no-secrets
// emptyDir mounted into BOTH the worker and the dind sidecar at the SAME path, so a
// `docker run -v <src>:/x` / compose bind whose source is the run's checkout resolves
// in the daemon's own filesystem. Without it the daemon mounts none of the worker fs,
// so every bind source is an EMPTY dir — which breaks uzi's own e2e and most real
// docker repos on ANY posture (the daemon never saw the worker's files). It bites
// rootless and non-rootless equally, so it is rendered for both.
//
// The path is the agent's runner-clone root: git.ts sets runnerRoot =
// <UZI_DATA_DIR>/runner and the run's SDK cwd is a clone under it, so the docker bind
// source is /data/runner/... — COUPLED to agent/src/git.ts; if that root moves, this
// must move with it. It is a SEPARATE emptyDir nested under /data (the cache PVC's
// mountpoint), so the clone cache at /data/repos, /nix, and the token/secret mounts
// stay UNSHARED with dind — Decision 3's crown-jewel isolation is preserved.
const (
	dindWorkdirVolume = "run-workdir"
	dindWorkdirDir    = dataMountPath + "/runner"
)

// The rootless dind uid/gid baked into docker:*-dind-rootless. The sidecar MUST run
// as this uid — its /etc/subuid + /etc/subgid ranges are set up for 1000 — so it
// overrides the pod-level runAsUser 10001. The socket-prep helper chowns the shared
// dir to this same uid so the daemon can create its socket there.
const (
	dindUID = int64(1000)
	dindGID = int64(1000)
)

// dindResources / dindInitResources: like seedResources, REQUIRED not decoration —
// a ResourceQuota on requests.* rejects any pod with a container declaring none, and
// native sidecars count toward the pod's request (they run alongside the worker).
// The sidecar budget follows arch §Q4 (~1-2 GiB / ~1 CPU on top of the agent).
var (
	dindResources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
	dindInitResources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
	}
)

// namespaceFor picks a worker's namespace: the dedicated privileged docker tier for
// a docker worker, #58's restricted default otherwise. The two never overlap (the
// controller config refuses equal namespaces), which is what keeps the restricted
// default's blast radius untouched even though the docker tier is privileged
// (Decision 7 / Q-B). A docker worker with DockerNamespace unset resolves to "" and
// is caught by the reconciler, never silently rendered into the restricted default.
func (cfg RenderConfig) namespaceFor(w protocol.DesiredWorker) string {
	if w.Docker {
		return cfg.DockerNamespace
	}
	return cfg.Namespace
}

// seedResources are the init container's requests/limits.
//
// They are REQUIRED, not decoration: a ResourceQuota on requests.* makes admission
// reject any pod with a container that declares none — initContainers included. The
// LimitRange is the backstop, not the source.
//
// They cost nothing as long as they stay under the SMALLEST preset's requests: a
// pod's effective request is max(initContainers), not sum, taken against
// sum(containers). presetRequestsDominateTheSeed pins that property so shrinking a
// preset cannot silently make the seed container the pod's effective request.
var seedResources = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	},
	Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	},
}

// RenderConfig is everything the renderer needs that does not come from the api.
// Every field here is operator config — the api sends names, this side owns every
// mapping to a concrete pod-spec value.
type RenderConfig struct {
	// Namespace is the dedicated worker namespace: it contains nothing but hosted
	// workers, which is what bounds a compromised controller.
	Namespace string
	// DockerNamespace is the SEPARATE `enforce: privileged` namespace docker workers
	// (PRD #83 M3) render into. It is distinct from Namespace precisely so the
	// privileged DinD sidecar's blast radius never touches #58's restricted default —
	// Decision 7's real goal, preserved even though this tier is privileged not
	// baseline (Q-B). Empty means this controller renders no docker workers; a docker
	// worker in the poll is then SKIPPED (never rendered into the restricted default).
	DockerNamespace string
	// DinDImage is the fully-pinned DinD sidecar image, of the posture DinDNonRootless
	// selects: rootless (docker:<tag>-dind-rootless@sha256:...) or non-rootless
	// (docker:<tag>-dind@sha256:...). The chart picks the matching pinned digest
	// (uzi.workerDinDImage) and a chart fail-guard rejects an image/posture mismatch, so
	// it always agrees with DinDNonRootless. Shared with the compose track's / CI's pins.
	// In the ROOTLESS posture this one ref also serves the tiny root socket-prep helper
	// (dindInitContainer — that image ships /bin/sh + chown/chmod); the non-rootless
	// posture renders no such helper (loopback transport, no shared socket dir). Empty
	// exactly when DockerNamespace is (config validates the pair).
	DinDImage string
	// DinDNonRootless selects the NON-ROOTLESS privileged DinD posture (PRD #89). Its
	// ZERO VALUE (false) is ROOTLESS — the safe default — so an unset posture renders
	// exactly PRD #83's rootless sidecar and no caller that omits it can accidentally
	// get node root. Set true ONLY for clusters whose nodes cannot run rootless dockerd
	// (no unprivileged userns — dev-cluster/Linux): the daemon then runs as real root
	// (uid 0) in the already-privileged, already-fenced docker namespace and the worker
	// reaches it over pod-loopback TCP (dindLoopbackTCP) instead of a shared unix
	// socket. Pure operator config gated by workers.docker.rootless + the
	// acknowledgeNonRootlessNodeRoot fail-guard (M2); it never comes off the wire and is
	// kept out of a plain worker's rendered spec/hash.
	DinDNonRootless bool
	// ServiceAccountName is the workers' own zero-permission SA. It carries the
	// imagePullSecrets (so the controller need not know about Harbor) and its token is
	// never automounted.
	ServiceAccountName string
	// APIURL is what the WORKER dials — necessarily the FQDN
	// (https://api.<ns>.svc.cluster.local:8443), since short Service names do not
	// resolve cross-namespace. It is not this controller's own api URL, which may be
	// the short name.
	APIURL string
	// StorageClass is optional; empty means the cluster default.
	StorageClass string
	// APICAPEM is the api's CA, relayed to workers through the per-worker Secret this
	// controller already creates — no new RBAC verb, so Decision 1's Secrets line
	// stays verbatim. Empty means the worker verifies against the system roots.
	APICAPEM []byte
}

// names for one worker's objects.
func deploymentName(id string) string { return NamePrefix + id }
func secretName(id string) string     { return NamePrefix + id + tokenSuffix }
func dataPVCName(id string) string    { return NamePrefix + id + dataSuffix }
func nixPVCName(id string) string     { return NamePrefix + id + nixSuffix }

// objectLabels is the stamp every created object carries.
func objectLabels(id string) map[string]string {
	return map[string]string{
		LabelName:      ValueName,
		LabelComponent: ValueComponent,
		LabelManagedBy: ValueManagedBy,
		LabelWorkerID:  id,
	}
}

// IsOurs reports whether we created an object — the provenance check that gives
// this controller teardown authority over it. Both halves are required: the
// managed-by stamp AND a worker id we can act on.
func IsOurs(labels map[string]string) (string, bool) {
	if labels[LabelManagedBy] != ValueManagedBy {
		return "", false
	}
	id := labels[LabelWorkerID]
	if id == "" {
		return "", false
	}
	return id, true
}

// RenderSecret builds a worker's token Secret.
//
// Only ever called with a NON-NIL token: a null join_token means "write no Secret
// for this worker", never "this worker has no token" — see Materializer.Reconcile,
// where that decision is made and explained.
//
// The CA rides along because hosted workers live in a different namespace and
// cannot mount the api's TLS Secret. This Secret is written ONCE, at creation, and
// never updated (the controller cannot read Secrets back, and delete+recreate would
// destroy the join token — which after delivery is the only copy in existence). So
// a CA ROOT rotation strands existing workers, and the remedy is v1's answer to
// everything: delete + reprovision. The chart's caDuration is 5 years precisely
// because re-trusting a new root means touching every worker at once.
func RenderSecret(cfg RenderConfig, w protocol.DesiredWorker, token string) *corev1.Secret {
	data := map[string][]byte{tokenKey: []byte(token)}
	if len(cfg.APICAPEM) > 0 {
		data[caCertKey] = cfg.APICAPEM
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName(w.ID),
			Namespace: cfg.namespaceFor(w),
			Labels:    objectLabels(w.ID),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// RenderPVCs builds the /data and /nix claims.
//
// Two volumes, pointing opposite ways for opposite reasons. /data is the clone
// cache + per-run workspaces and varies by size. /nix is FLAT (4Gi) and persists
// because the store is an expensive INTERNET fetch (measured: 209 MB baked -> 1,703
// MB provisioned in 10m53s), and Decision 9 rolls every worker on every release —
// so not persisting it would pay that cost per worker per release.
//
// Never patched. PVC specs are near-immutable and a size change is delete +
// reprovision.
func RenderPVCs(cfg RenderConfig, w protocol.DesiredWorker, spec preset.Spec) []*corev1.PersistentVolumeClaim {
	ns := cfg.namespaceFor(w)
	return []*corev1.PersistentVolumeClaim{
		renderPVC(cfg, ns, w.ID, dataPVCName(w.ID), spec.Size.DataSize),
		renderPVC(cfg, ns, w.ID, nixPVCName(w.ID), spec.NixSize),
	}
}

func renderPVC(cfg RenderConfig, ns, id, name string, size resource.Quantity) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    objectLabels(id),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	if cfg.StorageClass != "" {
		sc := cfg.StorageClass
		pvc.Spec.StorageClassName = &sc
	}
	return pvc
}

// nixSeedScript is the init container's program.
//
// Parameterised only so the tests can run THIS script against a temp directory; in
// production both paths are the compile-time constants below, so nothing user- or
// api-supplied is ever interpolated into a shell.
//
// `tar`, NOT `cp -a`, and that is not a preference:
//   - nix store paths are mode 0555 READ-ONLY directories. A recursive copy that
//     recreates a 0555 directory and then writes children into it fails EACCES — the
//     owner's write bit is enforced for uid 10001 exactly as for anyone else, and
//     PodSecurity `restricted` forbids the root/CAP_DAC_OVERRIDE escape. GNU tar
//     defers directory permissions to a final pass.
//   - `cp` in this image is BUSYBOX (verified: /bin/cp -> /bin/busybox; the image
//     installs git bash tini curl xz tar setpriv and NO coreutils), so its -a
//     behaviour on read-only dirs and hardlinks is exactly what must not be assumed.
//     GNU tar 1.35 is present in both templates and preserves hardlinks, which the
//     store relies on.
//
// TWO THINGS BELOW LOOK LIKE FUSS AND ARE NOT. Both were found by running this on a
// real kubelet against a real fsGroup volume, and each one CrashLooped the pod:
//
//  1. It archives the TOP-LEVEL ENTRIES (`$(ls -A)`), never `.`. A `tar -C /nix .`
//     puts a `./` member in the archive, so extraction ends by applying the image's
//     /nix metadata to the DESTINATION ROOT — which is the kubelet's mount point,
//     owned root:<fsGroup>. uid 10001 does not own it, so the chmod/utime EPERMs,
//     tar exits non-zero, the sentinel is never written, and the pod CrashLoops
//     forever having copied the store perfectly. The mount root's mode is kubelet's
//     business, not the image's; we have no business restoring it.
//
//  2. It WIPES before extracting, scoped BELOW the root (-mindepth 1). Reaching here
//     means the sentinel is absent, so any content present is a partial copy from an
//     interrupted attempt — and tar will not overwrite into the 0555 dirs a previous
//     pass already sealed ("Cannot open: File exists"), so a re-run would CrashLoop
//     forever and strand the worker behind a manual PVC deletion. The store is a
//     cache, so starting clean is correct and cheap. `-mindepth 1` is what makes it
//     work at all: busybox `chmod -R` ABORTS on the un-chmodable root and never
//     reaches the children.
//
// `set -o pipefail` IS THE SENTINEL'S HONESTY, not shell hygiene. Without it only
// the EXTRACTING tar's status gates the write, so a producer that fails while still
// emitting a well-formed short archive writes "seeded" over a partial store — and
// every later boot then SKIPS seeding, forever, on a store missing paths. Measured
// in this image rather than reasoned: a failing producer piped to a succeeding
// consumer exits 0 today. That is the one failure mode this design must never have,
// because unlike the two CrashLoops above it is SILENT. Verified that /bin/sh here
// (busybox ash) both accepts pipefail and propagates it; a shell that lacked it
// would abort under `set -e`, which fails loudly and in the safe direction.
func nixSeedScript(src, dst string) string {
	return fmt.Sprintf(`set -eu
set -o pipefail
if [ -f %[2]s/%[3]s ]; then
  echo "uzi-seed-nix: store already seeded; nothing to do"
  exit 0
fi
echo "uzi-seed-nix: seeding the nix store from the image"
find %[2]s -mindepth 1 -type d -exec chmod u+w {} + 2>/dev/null || true
find %[2]s -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true
cd %[1]s
tar -cf - -- $(ls -A) | tar -xpf - -C %[2]s
: > %[2]s/%[3]s
echo "uzi-seed-nix: seeded"
`, src, dst, nixSentinel)
}

// RenderDeployment builds a worker's Deployment, with the spec hash stamped in.
func RenderDeployment(cfg RenderConfig, w protocol.DesiredWorker, spec preset.Spec) *appsv1.Deployment {
	tmpl := podTemplate(cfg, w, spec)
	// Hash BEFORE stamping the hash (it cannot cover itself), and with the generation
	// removed too, so the two annotations stay INDEPENDENT drift signals: spec-hash
	// means "the rendering changed", generation means "the token rotated". Either one
	// moving changes the pod template and therefore rolls the pod, which is the whole
	// job.
	hash := specHash(tmpl)
	tmpl.Annotations[AnnotationSpecHash] = hash

	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName(w.ID),
			Namespace: cfg.namespaceFor(w),
			Labels:    objectLabels(w.ID),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			// The worker id label ALONE, and this is a one-way door: a Deployment's
			// selector is IMMUTABLE, so anything else in here becomes unpatchable forever.
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{LabelWorkerID: w.ID},
			},
			// Recreate, never RollingUpdate: the surge pod would Multi-Attach-deadlock
			// against the RWO PVCs the old pod still holds.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: tmpl,
		},
	}
}

func podTemplate(cfg RenderConfig, w protocol.DesiredWorker, spec preset.Spec) corev1.PodTemplateSpec {
	automount := false
	runAsNonRoot := true
	uid, gid := workerUID, workerGID
	fsGroup := workerGID
	// OnRootMismatch, not the default Always. Always recursively chowns the WHOLE
	// volume on EVERY mount; against a provisioned /nix (measured 1,703 MB / 1,205
	// store paths) that is a full recursive walk on every pod start — i.e. on every
	// release, since Decision 9 rolls every worker on every release.
	fsGroupPolicy := corev1.FSGroupChangeOnRootMismatch
	allowPrivilegeEscalation := false

	env := []corev1.EnvVar{
		{Name: "UZI_API_URL", Value: cfg.APIURL},
		{Name: "UZI_WORKER_TOKEN_FILE", Value: tokenPath},
		{Name: "UZI_DATA_DIR", Value: dataMountPath},
		// Pinned at 1 for every preset (Decision 7): a size buys headroom for ONE run,
		// never parallelism. Raising it would server-provision the intra-user
		// concurrency residuals documented in docs/worker-setup.md.
		{Name: "WORKER_MAX_CONCURRENT_RUNS", Value: "1"},
	}
	if len(cfg.APICAPEM) > 0 {
		// Node reads this path before startup and agent/src/client.ts uses plain fetch
		// with no custom dispatcher, so trusting the cluster CA is pure pod spec —
		// nothing in agent/ parses a CA today and nothing needs to.
		env = append(env, corev1.EnvVar{Name: "NODE_EXTRA_CA_CERTS", Value: caCertPath})
	}
	if w.Docker {
		// The k8s branch of the keystone resolver (agent/src/docker-wiring.ts): set
		// DOCKER_HOST EXPLICITLY, never probe. It is BOTH the socket target AND the
		// resolver's "sidecar expected" signal — which turns on the bounded readiness
		// RETRY, so a daemon that is briefly slow or a socket briefly at 0660 does not
		// cost the capability. It does NOT itself hold the worker: what gates the worker
		// container's start on dockerd actually listening is the dind native-sidecar
		// startupProbe (pod ordering), and a probe that never succeeds degrades to
		// unwired regardless of this env. The value carries no secret (a socket path),
		// is present only when the sidecar is rendered, and cannot be overwritten by a
		// provisioned toolEnv (the SDK env fold writes only allowlisted nix keys).
		//
		// Rootless: the shared unix socket. Non-rootless (PRD #89): pod-loopback TCP,
		// since there is no shared socket volume — the worker reaches the root daemon over
		// the pod's loopback netns. parseDockerTarget already handles tcp:// unchanged.
		host := dockerHostValue
		if cfg.DinDNonRootless {
			host = dindLoopbackTCP
		}
		env = append(env, corev1.EnvVar{Name: "DOCKER_HOST", Value: host})
	}

	containerSecurity := &corev1.SecurityContext{
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	// Init containers, worker volume mounts and pod volumes all grow (only) for a
	// docker worker. Built as slices so the NON-docker render stays byte-identical to
	// #58 — same spec hash, so enabling docker in the product never rolls an existing
	// plain worker.
	initContainers := []corev1.Container{{
		Name: seedContainerName,
		// The SAME per-template agent image as the worker, so the store it copies is
		// the one that image was built with.
		Image:           spec.Image,
		Command:         []string{"/bin/sh", "-c", nixSeedScript(nixMountPath, nixSeedMountPath)},
		Resources:       seedResources,
		SecurityContext: containerSecurity,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "nix", MountPath: nixSeedMountPath},
		},
	}}
	workerMounts := []corev1.VolumeMount{
		{Name: "token", MountPath: secretMountPath, ReadOnly: true},
		{Name: "data", MountPath: dataMountPath},
		{Name: "nix", MountPath: nixMountPath},
	}
	volumes := []corev1.Volume{
		{
			Name: "token",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secretName(w.ID),
					// 0440, NOT 0400. A Secret volume's files are owned root:<fsGroup>, not
					// by runAsUser — so at 0400 the OWNER is root and uid 10001 cannot read
					// its own join token. At 0440 it reads it via group 10001. Nothing else
					// fixes this up in k8s: the compose entrypoint's chmod is on the ROOT
					// branch and is skipped on a non-root start. And nothing needs to — v1 is
					// single-uid, so there is no runner uid to fence out here.
					DefaultMode: ptr(int32(0o440)),
				},
			},
		},
		{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: dataPVCName(w.ID),
				},
			},
		},
		{
			Name: "nix",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: nixPVCName(w.ID),
				},
			},
		},
	}
	if w.Docker {
		// Native-sidecar ordering (reviewer race, k8s half): the DinD daemon is an
		// initContainer with restartPolicy: Always, so k8s starts it BEFORE the worker
		// and — via the dind startupProbe — holds the worker until dockerd is actually
		// listening. A plain wait-for-dind initContainer would DEADLOCK (it runs before
		// regular-container sidecars ever start); native sidecars are the clean fix and
		// are GA on k8s 1.29 (dev-cluster).
		//
		// Rootless (PRD #83): [seed-nix] → [dind-init: chown the socket dir to the
		// rootless uid so dockerd can start] → [dind]. dind-init MUST precede dind
		// because rootless dockerd REFUSES to start without a writable XDG_RUNTIME_DIR,
		// and it also holds the socket at 0666 for the agent's own uid after the daemon
		// creates it.
		//
		// Non-rootless (PRD #89): [seed-nix] → [dind]. There is NO shared socket dir to
		// prepare — the worker reaches the root daemon over pod-loopback TCP, not a
		// shared volume — so dind-init and the dind-sock socket volume+mounts are dropped
		// entirely.
		if cfg.DinDNonRootless {
			initContainers = append(initContainers, dindContainer(cfg))
		} else {
			initContainers = append(initContainers, dindInitContainer(cfg), dindContainer(cfg))
			// Rootless only: the worker reaches the daemon through the shared socket dir —
			// and NOTHING else transits it (Decision 3).
			workerMounts = append(workerMounts, corev1.VolumeMount{Name: dindSocketVolume, MountPath: dindSocketDir})
		}
		// The shared no-secrets run workdir (M-workdir), BOTH postures: mounted into the
		// worker AND the dind sidecar at the SAME path so a `docker run -v` bind source
		// under the run's checkout resolves in the daemon. Secrets + the /data cache +
		// /nix stay UNSHARED (Decision 3): the invariant test pins that dind mounts none
		// of token/data/nix.
		workerMounts = append(workerMounts, corev1.VolumeMount{Name: dindWorkdirVolume, MountPath: dindWorkdirDir})
		volumes = append(volumes,
			// The daemon's data root, mounted into `dind` ALONE (rootless HOME path or
			// /var/lib/docker per posture). emptyDir in v1 (a PVC is the durable/size
			// alternative, arch §Q4); this is the images + build cache the daemon writes,
			// none of it worker state.
			corev1.Volume{Name: dindDataVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			// The shared run workdir (M-workdir). emptyDir, so it is torn down with the pod
			// and is never a persistence surface; carries the run's checkout, NO secrets.
			corev1.Volume{Name: dindWorkdirVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		)
		if !cfg.DinDNonRootless {
			// Rootless only: the socket-only shared dir. emptyDir, torn down with the pod;
			// carries ONLY the socket (Decision 3).
			volumes = append(volumes, corev1.Volume{Name: dindSocketVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		}
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: objectLabels(w.ID),
			Annotations: map[string]string{
				AnnotationGeneration: strconv.FormatInt(w.Generation, 10),
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:           cfg.ServiceAccountName,
			AutomountServiceAccountToken: &automount,
			// Soft anti-affinity for docker workers ONLY (nil for a plain worker, so its
			// spec/hash is untouched). Keeps the pod off nodes running the crown-jewel
			// pods — see dockerNodeAntiAffinity.
			Affinity: dockerNodeAntiAffinity(w),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &runAsNonRoot,
				RunAsUser:    &uid,
				RunAsGroup:   &gid,
				// fsGroup is load-bearing TWICE. (a) an RWO PVC mounts root:root 0755, so
				// uid 10001 cannot write it, and the usual initContainer-chown escape needs
				// root, which PodSecurity `restricted` forbids. (b) a Secret volume's files
				// are owned root:<fsGroup>, so without it the token would be root:root and
				// unreadable — see the 0440 note on the volume below.
				FSGroup:             &fsGroup,
				FSGroupChangePolicy: &fsGroupPolicy,
				SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			// Built above: [seed-nix] for a plain worker, [seed-nix, dind-init, dind]
			// for a docker one. The docker sidecars are native (restartPolicy: Always),
			// so k8s orders and gates them ahead of the worker container.
			InitContainers: initContainers,
			Containers: []corev1.Container{{
				Name:  workerContainerName,
				Image: spec.Image,
				// NO command: / args:. Both templates carry
				// ENTRYPOINT ["/usr/local/sbin/uzi-entrypoint"] + CMD ["npm","run","start"],
				// and overriding them from the pod spec is the Decision Log's explicitly
				// REJECTED option (c): it silently bypasses a security wrapper by duplicating
				// the image's CMD in YAML, and drifts invisibly whenever PRD #51 touches the
				// entrypoint. The entrypoint detects the non-root start and runs single-uid.
				Env: env,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    spec.Size.CPURequest,
						corev1.ResourceMemory: spec.Size.MemoryRequest,
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    spec.Size.CPULimit,
						corev1.ResourceMemory: spec.Size.MemoryLimit,
					},
				},
				SecurityContext: containerSecurity,
				// Built above: [token, data, nix] plus, for a docker worker, the shared
				// run workdir (M-workdir) and — rootless only — the shared socket dir.
				VolumeMounts: workerMounts,
			}},
			// Built above: [token, data, nix] plus, for a docker worker, the daemon's
			// private data root + the shared run workdir emptyDirs (and, rootless only,
			// the shared socket-dir emptyDir).
			Volumes: volumes,
		},
	}
}

// dindContainer is the Docker-in-Docker daemon, modelled as a NATIVE SIDECAR (an
// initContainer with restartPolicy: Always) so k8s starts it before the worker and
// holds the worker container until its startupProbe (`docker info`) passes — the k8s
// half of the keystone readiness race fix. It is `privileged: true` in the dedicated
// `enforce: privileged` namespace (Q-B) in BOTH postures.
//
// The posture (cfg.DinDNonRootless) picks the daemon's identity and transport:
//
//   - ROOTLESS (PRD #83, the default): runs as the image's rootless uid 1000
//     (overriding the pod's 10001, whose subuid/subgid ranges the rootless setup
//     needs). The security property is the USERNS REMAP inside the rootless image — a
//     breakout lands as an unprivileged, userns-mapped host uid. The socket lands on
//     the shared dind-sock volume ($XDG_RUNTIME_DIR); the worker reaches it there.
//
//   - NON-ROOTLESS (PRD #89): runs as REAL ROOT (uid 0), no userns, so a breakout
//     lands as node root — the owner-accepted residual on nodes without unprivileged
//     userns (dev-cluster/Linux). RunAsNonRoot MUST be false at CONTAINER scope: the
//     pod is RunAsNonRoot:true, so a uid-0 container is rejected at admission without
//     this override (the same override dindInitContainer uses). The dockerd command is
//     OVERRIDDEN to bind ONLY dindLoopbackTCP (`--tls=false`) — THE control that
//     suppresses docker:dind's automatic 0.0.0.0:2375 listener, so the unauthenticated
//     root daemon is never on the pod IP. It binds NO unix socket: this posture drops
//     dind-init AND the shared socket volume, so /run/dind does not exist and a unix
//     --host would make dockerd exit 1 (live-proven, fixed in 0.8.1). The worker reaches
//     the daemon over the pod's loopback netns.
//
// Both keep seccomp Unconfined (the nested daemon profiles its children) and mount the
// shared run workdir (M-workdir).
//
// DECISION 3 — the one security invariant on a wired worker: it mounts ONLY its OWN
// data root, the shared run workdir (M-workdir, no secrets) and — rootless only — the
// shared socket dir; NONE of the worker's token/`/data`(cache)/`/nix`. A `docker run -v
// <anything>:/x` then binds this container's fs, which holds none of them. render_test.go
// pins this.
func dindContainer(cfg RenderConfig) corev1.Container {
	always := corev1.ContainerRestartPolicyAlways
	privileged := true

	// The shared run workdir is mounted in BOTH postures (M-workdir).
	mounts := []corev1.VolumeMount{{Name: dindWorkdirVolume, MountPath: dindWorkdirDir}}

	var sc *corev1.SecurityContext
	var env []corev1.EnvVar
	var command []string
	var probeCmd string
	var dataDir string

	if cfg.DinDNonRootless {
		root := int64(0)
		runAsNonRoot := false // real root: the pod's RunAsNonRoot:true rejects it without this
		dataDir = dindDataDirRoot
		sc = &corev1.SecurityContext{
			// allowPrivilegeEscalation left nil: the apiserver rejects privileged:true
			// alongside allowPrivilegeEscalation:false, and privileged already grants all.
			Privileged:     &privileged,
			RunAsNonRoot:   &runAsNonRoot,
			RunAsUser:      &root,
			RunAsGroup:     &root,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		}
		// THE loopback-bind control (PRD #89 M1): bind ONLY the pod-loopback TCP endpoint
		// and nothing else, which suppresses docker:dind's automatic 0.0.0.0:2375
		// listener. --tls=false because the loopback endpoint is plain (the trust boundary
		// is the pod netns, same as a same-pod unix socket).
		//
		// It must NOT also bind the unix socket: the non-rootless posture drops dind-init
		// AND the shared dind-sock volume, so /run/dind does not exist and a
		// `--host=unix:///run/dind/docker.sock` makes dockerd exit 1 ("can't create unix
		// socket ...: bind: no such file or directory") — live-proven on dev-cluster in
		// 0.8.0 and fixed here (0.8.1). Nothing needs the socket: the worker's DOCKER_HOST
		// is dindLoopbackTCP and the probe is `docker -H tcp://127.0.0.1:2375 info`.
		command = []string{"dockerd", "--host=" + dindLoopbackTCP, "--tls=false"}
		// `docker info` over the loopback endpoint: the daemon answering there is the real
		// readiness signal (the socket existing is not the daemon being up).
		probeCmd = "docker -H " + dindLoopbackTCP + " info >/dev/null 2>&1"
	} else {
		uid, gid := dindUID, dindGID
		runAsNonRoot := true // uid 1000 is non-root
		dataDir = dindDataDir
		// rootless dockerd writes its API socket to $XDG_RUNTIME_DIR/docker.sock; point it
		// at the shared volume so the socket lands at the path DOCKER_HOST names.
		env = []corev1.EnvVar{{Name: "XDG_RUNTIME_DIR", Value: dindSocketDir}}
		sc = &corev1.SecurityContext{
			Privileged:     &privileged,
			RunAsNonRoot:   &runAsNonRoot,
			RunAsUser:      &uid,
			RunAsGroup:     &gid,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		}
		// `docker info` connects as uid 1000 (the socket's owner), so it succeeds even
		// before dind-init has relaxed the socket to 0666.
		probeCmd = "docker -H unix://" + dindSocketPath + " info >/dev/null 2>&1"
		// Rootless only: the worker reaches the daemon through the shared socket dir.
		mounts = append(mounts, corev1.VolumeMount{Name: dindSocketVolume, MountPath: dindSocketDir})
	}
	mounts = append(mounts, corev1.VolumeMount{Name: dindDataVolume, MountPath: dataDir})

	return corev1.Container{
		Name:            dindContainerName,
		Image:           cfg.DinDImage,
		RestartPolicy:   &always,
		Command:         command, // nil for rootless (image entrypoint), set for non-rootless
		Env:             env,
		SecurityContext: sc,
		Resources:       dindResources,
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", probeCmd}},
			},
			PeriodSeconds:    2,
			FailureThreshold: 30, // ~60s bring-up worst case
		},
		VolumeMounts: mounts,
	}
}

// dockerNodeAntiAffinity keeps a docker worker's pod OFF nodes running the crown-jewel
// pods — the api (which holds UZI_SECRET_KEY, the master key that decrypts every user's
// forge PAT + Anthropic token) and CNPG. It is a cheap mitigation for the non-rootless
// node-root residual (PRD #89 mitigation #3): with kubelet NodeRestriction, node root
// reads only the secrets of pods bound to THAT node, so keeping the crown jewels off
// docker-worker nodes directly shrinks the worst outcome.
//
// PREFERRED (soft) on purpose — it must never wedge scheduling — and best-effort, not a
// full fix. It is CROSS-NAMESPACE (the crown jewels are in the release namespace, the
// worker in the docker namespace), so it uses an empty namespaceSelector (all
// namespaces) plus a label match: the api pod (app.kubernetes.io/component=api) and any
// CNPG instance pod (the cnpg.io/cluster label). Rendered for docker workers only; a
// plain worker gets nil, so its spec/hash is untouched.
func dockerNodeAntiAffinity(w protocol.DesiredWorker) *corev1.Affinity {
	if !w.Docker {
		return nil
	}
	const hostnameTopology = "kubernetes.io/hostname"
	term := func(sel *metav1.LabelSelector) corev1.WeightedPodAffinityTerm {
		return corev1.WeightedPodAffinityTerm{
			Weight: 100,
			PodAffinityTerm: corev1.PodAffinityTerm{
				TopologyKey:       hostnameTopology,
				NamespaceSelector: &metav1.LabelSelector{}, // empty => all namespaces
				LabelSelector:     sel,
			},
		}
	}
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				// The uzi api pod.
				term(&metav1.LabelSelector{MatchLabels: map[string]string{LabelComponent: "api"}}),
				// Any CNPG instance pod (the operator stamps cnpg.io/cluster on each).
				term(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "cnpg.io/cluster", Operator: metav1.LabelSelectorOpExists},
				}}),
			},
		},
	}
}

// dindInitContainer prepares the shared socket dir and then holds the socket
// readable across uids. Also a NATIVE SIDECAR, ordered BEFORE dind: rootless dockerd
// refuses to start without a writable XDG_RUNTIME_DIR (M2 established this live), so
// the chown must land before the daemon, and the 0666 loop must keep running to
// re-apply after a dind restart's new socket inode — the exact shape of the compose
// dind-init.
//
// It runs as ROOT (uid 0) to chown/chmod a dir the rootless daemon then owns —
// admissible only because the namespace is `enforce: privileged`. 0711 on the dir is
// a SECURITY perm not a lax one: others get traverse-only (x), so the worker reaches
// the socket but cannot squat a rogue file in the dir. 0666 on the socket is what
// lets the agent — which on the #58 single-uid k8s path runs as the worker's own uid
// 10001, NOT the rootless owner — reach it; the socket's gid is a userns-remapped,
// host-specific value (M2), so group alignment is unusable and world-rw is the
// portable answer. mount-ns (Decision 3), never the socket mode, is the containment.
//
// Like dind, it mounts ONLY the shared socket dir — never token/`/data`/`/nix`.
func dindInitContainer(cfg RenderConfig) corev1.Container {
	always := corev1.ContainerRestartPolicyAlways
	root := int64(0)
	runAsNonRoot := false
	script := fmt.Sprintf(`set -eu
chown %[1]d:%[2]d %[3]s
chmod 0711 %[3]s
touch /tmp/dind-dir-ready
while :; do
  [ -S %[4]s ] && chmod 0666 %[4]s
  sleep 2
done
`, dindUID, dindGID, dindSocketDir, dindSocketPath)
	return corev1.Container{
		Name:          dindInitContainerName,
		Image:         cfg.DinDImage,
		RestartPolicy: &always,
		Command:       []string{"/bin/sh", "-c", script},
		SecurityContext: &corev1.SecurityContext{
			// Root, for chown/chmod — nothing more. Drop ALL and add back ONLY the two
			// caps it actually uses: CHOWN (chown the socket dir to the rootless uid) and
			// FOWNER (chmod a dir/socket it does not own once the daemon owns it). Not
			// privileged, and not the full root cap set — so a compromise of this tiny
			// helper widens the pod by exactly those two caps, nothing more.
			RunAsNonRoot: &runAsNonRoot,
			RunAsUser:    &root,
			RunAsGroup:   &root,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"CHOWN", "FOWNER"},
			},
		},
		Resources: dindInitResources,
		StartupProbe: &corev1.Probe{
			// "dir prepared" — the signal that gates dind's start. NOT the socket
			// existing (the socket only appears once dind runs, which is after this).
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "test -f /tmp/dind-dir-ready"}},
			},
			PeriodSeconds:    1,
			FailureThreshold: 30,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: dindSocketVolume, MountPath: dindSocketDir},
		},
	}
}

// specHash is a sha256 over the rendered pod template with BOTH drift annotations
// removed — the spec hash cannot cover itself, and excluding the generation keeps
// the two signals independent.
//
// Hashing what WE render, rather than diffing against the live object, sidesteps
// server-defaulted fields entirely: the apiserver fills in dozens of them, so a
// naive comparison would report permanent drift and roll the pod forever.
func specHash(tmpl corev1.PodTemplateSpec) string {
	// Copy the annotations so the caller's map is untouched.
	clean := tmpl
	clean.Annotations = map[string]string{}
	for k, v := range tmpl.Annotations {
		if k == AnnotationSpecHash || k == AnnotationGeneration {
			continue
		}
		clean.Annotations[k] = v
	}
	// encoding/json sorts map keys, so this is stable across runs and processes.
	raw, err := json.Marshal(clean)
	if err != nil {
		// PodTemplateSpec is plain data; a marshal failure is not a runtime condition.
		panic(fmt.Sprintf("render: marshal pod template for hashing: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SpecHashOf exposes the hash of what we WOULD render, so the reconciler can
// compare it against the annotation read back off the live Deployment.
func SpecHashOf(cfg RenderConfig, w protocol.DesiredWorker, spec preset.Spec) string {
	return specHash(podTemplate(cfg, w, spec))
}

func ptr[T any](v T) *T { return &v }
