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

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
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
	// dindDataSuffix names the DinD daemon's data-root claim (issue #224 M-a).
	// DOCKER workers only — a plain worker renders no such PVC.
	dindDataSuffix = "-dind-data"
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
	// dataSeedMountPath is where the seed init container mounts the DATA PVC —
	// deliberately NOT /data, for the same masking rationale as nixSeedMountPath:
	// mounting at /data would mask the image path and, more to the point, the seed
	// only needs to REACH the persisted devbox/nix provisioning state to clear it on
	// a reseed, not to run as /data.
	dataSeedMountPath = "/data-seed"

	// toolchainProfileMarker is the image-baked toolchain-identity file (PRD #92 M1):
	// `readlink -f /opt/uzi-toolchain`, the realized nix store profile path — the
	// "store hash". Keying the reseed on this (NOT the image appVersion) fires a
	// reseed only when the toolchain actually changed, preserving the warm-start cache
	// of per-run provisioned packages the volume exists to hold.
	toolchainProfileMarker = "/etc/uzi-toolchain-profile"
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
// stranded one. dev-cluster's storage is a CSI driver, which formats
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

// nixToolchainMarker records, alongside the sentinel, the toolchain identity
// (toolchainProfileMarker's value = the store-hash profile path) the seeded store
// was built from. The reseed is version-aware: it skips ONLY when the sentinel AND
// this recorded marker both exist AND match the image's current marker. A legacy
// sentinel with NO recorded marker is therefore a MISMATCH and reseeds — which is
// exactly how the currently-broken live worker heals on its next roll (PRD #92). An
// "absent marker = skip" reading would silently fail that primary DoD.
const nixToolchainMarker = ".uzi-nix-toolchain"

// Container names.
const (
	workerContainerName = "worker"
	seedContainerName   = "seed-nix"
	// The docker tier's two extra containers (PRD #83 M3), both native sidecars
	// (initContainers with restartPolicy: Always).
	dindContainerName     = "dind"
	dindInitContainerName = "dind-init"
)

// The worker's requests.ephemeral-storage (issue #224 M-b). Docker REPLACES plain;
// they do not add.
//
// 🔴 THE WHOLE POD BUDGET GOES ON THE `worker` CONTAINER, AND NOT BECAUSE IT IS
// TIDIER. Quota, the scheduler and kubelet's eviction ranker use three different
// formulas over the same pod, and only one placement makes all three agree:
//
//   - PodRequests (quota + scheduler) sums .spec.containers PLUS restartable
//     initContainers, so a budget on `dind` is charged in full;
//   - GetResourceRequestQuantity, which the ranker's exceedDiskRequests uses, is
//     max(sum(Containers), max(InitContainers)) with NO native-sidecar case, so the
//     same budget on `dind` is only partially credited. Worker 1Gi + dind 8Gi is
//     charged 9Gi and credited 8Gi.
//
// .spec.containers has exactly ONE entry, so putting the number there makes all
// three views equal to it, with no double count. SPLITTING IT ACROSS CONTAINERS FOR
// SYMMETRY SILENTLY HALVES THE RANKING THRESHOLD — that is the edit the next reader
// makes, and this paragraph is here to stop it.
//
// 🔴 AND NEVER A `limits.ephemeral-storage`, ON ANY CONTAINER. A limit is strictly
// worse than declaring nothing, three ways, any one of them sufficient:
//
//  1. podEphemeralStorageLimitEviction fires as soon as ANY container declares a
//     limit, and compares the SUM of declared limits against pod usage that INCLUDES
//     the emptyDirs. So one limit on `worker` creates a pod-level ceiling that
//     run-workdir counts against: a new DETERMINISTIC eviction path that fires with
//     no node pressure at all, which is this issue's failure arriving on schedule
//     instead of by luck.
//  2. A limit on `dind` is inert anyway: containerEphemeralStorageLimitEviction
//     builds its map from pod.Spec.Containers only, and `dind` is a restartable
//     initContainer. It also measures rootfs+logs, so emptyDirs are excluded from
//     container-level enforcement entirely.
//  3. A REQUEST adds no eviction path at all: every arm of localStorageEviction is
//     limit- or sizeLimit-gated. It buys placement plus exceedDiskRequests ranking,
//     over a podDiskUsage that DOES include emptyDirs.
//
// The LimitRange is the other half of the same rule: an ephemeral `max` would need a
// `default` beside it, and that `default` would hand trap (1) to every pod in the
// namespace. Neither tier declares one; do not add one.
// TestNoContainerDeclaresAnEphemeralStorageLimit is what actually enforces this.
//
// preset.go:44-46 already makes this argument for memory ("kubelet evicts pods whose
// usage exceeds their REQUESTS first"). This is the same argument for disk.
//
// WHAT THE NUMBERS BUY, stated honestly because the shipped values are small: this
// is a RANKING mitigation, not a CAPACITY guarantee. Nothing else in this cluster
// declares the resource — a node measured 51-52% full reports `ephemeral-storage
// 0 (0%)` requested — so the scheduler's view of it is empty and the undeclared
// usage stays invisible however much we declare. The capacity answer is bigger node
// disks, and image accumulation is its own defect (issue #225).
//
// "CONSERVATIVE" HERE MEANS ERR *LOW*, which inverts the usual instinct and is the
// sentence most worth keeping. Too low is today's behaviour: probabilistic,
// recoverable, and visible in a message we now know how to read. Too high is
// UNSCHEDULABLE: total, deterministic and SILENT, presenting as preset.go:14-17's
// worker that provisions and never appears, and the docker tier's quota cannot warn
// you because it cannot bind on this resource (see values.yaml's ephemeralStorage
// comment).
const (
	// Plain worker: the working tree is NOT on ephemeral storage. render.go mounts
	// run-workdir only when w.Docker, so /data/runner falls through to the `data`
	// PVC and what is left on ephemeral is container logs plus the writable layer.
	// Measured 45 KiB; kubelet's default log rotation ceiling is 10Mi x 5 per
	// container. 512Mi is ~10x that ceiling.
	workerDefaultEphemeralRequest = "512Mi"
	// Docker worker: run-workdir IS an emptyDir, and it holds the run's entire
	// working tree — one clone per run, multiplied by WORKER_MAX_CONCURRENT_RUNS,
	// with $TMPDIR pointed at the same volume. This repo alone is a ~170 MiB runner
	// clone before a single dependency install. 4Gi covers ~2 concurrent heavy runs.
	//
	// It is 4Gi and not the 6Gi an earlier draft carried BECAUSE of M-a: the daemon's
	// image cache used to be an emptyDir on this same budget and is now a PVC. Do not
	// raise this without re-deriving what is actually left on ephemeral storage.
	workerDefaultDockerEphemeralRequest = "4Gi"
)

// ephemeralRequest is the worker container's requests.ephemeral-storage: the docker
// value for a docker worker, the plain value otherwise, each overridable per cluster.
// Config validated any override string as a quantity at boot, so MustParse is safe.
func (cfg RenderConfig) ephemeralRequest(w protocol.DesiredWorker) resource.Quantity {
	if w.Docker {
		if cfg.DockerEphemeralRequest != "" {
			return resource.MustParse(cfg.DockerEphemeralRequest)
		}
		return resource.MustParse(workerDefaultDockerEphemeralRequest)
	}
	if cfg.EphemeralRequest != "" {
		return resource.MustParse(cfg.EphemeralRequest)
	}
	return resource.MustParse(workerDefaultEphemeralRequest)
}

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
// They are REQUIRED, not decoration: a ResourceQuota on requests.CPU or
// requests.MEMORY makes admission reject any pod with a container that declares none
// — initContainers included. The LimitRange is the backstop, not the source.
//
// "cpu or memory" rather than "requests.*", corrected in issue #224: the quota
// evaluator's validationSet is those two and nothing else, permanently and by
// upstream's explicit design. That makes the rule above EXACT here (cpu and memory
// are what these constants declare) and makes it false as a generalisation — see the
// longer note on dindResources.
//
// They cost nothing as long as they stay under the SMALLEST preset's requests: a
// pod's effective request is max(initContainers), not sum, taken against
// sum(containers). TestSeedContainerRequestsStayUnderEveryPreset (render_test.go)
// pins that property so shrinking a preset cannot silently make the seed container
// the pod's effective request. It named `presetRequestsDominateTheSeed` until issue
// #224; no such symbol has ever existed in this tree.
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
	// (no unprivileged userns — dev-cluster): the daemon then runs as real root
	// (uid 0) in the already-privileged, already-fenced docker namespace and the worker
	// reaches it over pod-loopback TCP (dindLoopbackTCP) instead of a shared unix
	// socket. Pure operator config gated by workers.docker.rootless + the
	// acknowledgeNonRootlessNodeRoot fail-guard (M2); it never comes off the wire and is
	// kept out of a plain worker's rendered spec/hash.
	DinDNonRootless bool
	// DinDRequest*/DinDLimit* override the DinD sidecar's resource requests+limits (PRD #89
	// 0.8.1). Empty ⇒ dindDefault* ("250m"/"256Mi" requests, "2"/"2Gi" limits). A cluster
	// raises them because in-daemon image builds OOM the 2Gi default (uzi's own e2e builds
	// 5 images inside the daemon); the REQUEST is raised alongside the limit so the
	// scheduler reserves the memory rather than placing the pod on a node that OOMs it under
	// contention. Quantity strings (e.g. "4" / "6Gi"), validated at the controller's boot so
	// the render side can MustParse them. Docker-only, so they never touch a plain worker's
	// spec/hash.
	DinDRequestCPU    string
	DinDRequestMemory string
	DinDLimitCPU      string
	DinDLimitMemory   string
	// EphemeralRequest / DockerEphemeralRequest override the WORKER container's
	// requests.ephemeral-storage (issue #224 M-b). Docker REPLACES plain rather than
	// adding to it. Empty ⇒ workerDefault{,Docker}EphemeralRequest ("512Mi" / "4Gi").
	// Quantity strings, validated at the controller's boot so the render side can
	// MustParse them. The right value is a property of the CLUSTER'S NODES rather than
	// of the product, which is the same argument that made dindResources overridable.
	// Read ephemeralRequest's header before touching either: the budget belongs on the
	// worker container alone, and NEITHER may ever become a limit.
	EphemeralRequest       string
	DockerEphemeralRequest string
	// MaxPVCStorage / DockerMaxPVCStorage are each tier's LimitRange maxPVCStorage
	// (issue #224). They render NOTHING — they are the admission ceiling every PVC
	// this controller creates must fit, checked once at boot by ValidatePVCCeilings so
	// an oversized claim is a refusal to start rather than a fleet of workers that
	// provision and never appear. Empty means that tier has no LimitRange, so there is
	// no ceiling and its check is skipped. Per-tier because the two are separate
	// namespaces with separate LimitRanges; see ValidatePVCCeilings for why the
	// invariant is a per-tier minimum rather than an equality.
	MaxPVCStorage       string
	DockerMaxPVCStorage string
	// DinDDataSize overrides the DinD daemon's data-root PVC size (issue #224 M-a).
	// Empty ⇒ dindDataDefaultSize ("20Gi"). A quantity string, validated at the
	// controller's boot so the render side can MustParse it. Docker-only, so it never
	// touches a plain worker's spec/hash. See dindDataDefaultSize for the 20Gi
	// admission ceiling and the no-GC residual.
	DinDDataSize string
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
	// MaxConcurrentRuns is the per-worker slot cap rendered into the pod's
	// WORKER_MAX_CONCURRENT_RUNS. Zero (a RenderConfig{} built in a test, or a caller
	// that never set it) renders "1", so the default stays 1 everywhere; see config.go,
	// which validates and defaults it. Raising it opts into the intra-user concurrency
	// residuals documented in docs/worker-setup.md (PRD #58 Decision 7).
	MaxConcurrentRuns int
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

// dindDataPVCName is the DinD daemon's data root (issue #224 M-a). Rendered for
// docker workers only, so a plain worker never has an object by this name.
func dindDataPVCName(id string) string { return NamePrefix + id + dindDataSuffix }

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

// RenderPVCs builds the /data and /nix claims, plus — for a DOCKER worker only —
// the DinD daemon's data root.
//
// The first two point opposite ways for opposite reasons. /data is the clone
// cache + per-run workspaces and varies by size. /nix is FLAT (20Gi, PRD #87 bump
// for the prebaked Chromium closure) and persists because the store is an expensive
// INTERNET fetch (measured: 209 MB baked pre-#87 -> ~2.6 GiB baked with Chromium),
// and Decision 9 rolls every worker on every release —
// so not persisting it would pay that cost per worker per release.
//
// The third is issue #224 M-a: the daemon's image + build cache, moved off the pod's
// ephemeral storage so a runaway pull produces ENOSPC in the daemon instead of a
// kubelet eviction that destroys the run's working tree. Flat, and rendered ONLY
// when w.Docker — a plain worker's object set is unchanged, which is the same
// property the docker branch of podTemplate preserves for its spec hash.
//
// Never patched. PVC specs are near-immutable and a size change is delete +
// reprovision.
func RenderPVCs(cfg RenderConfig, w protocol.DesiredWorker, spec preset.Spec) []*corev1.PersistentVolumeClaim {
	ns := cfg.namespaceFor(w)
	pvcs := []*corev1.PersistentVolumeClaim{
		renderPVC(cfg, ns, w.ID, dataPVCName(w.ID), spec.Size.DataSize),
		renderPVC(cfg, ns, w.ID, nixPVCName(w.ID), spec.NixSize),
	}
	if w.Docker {
		pvcs = append(pvcs, renderPVC(cfg, ns, w.ID, dindDataPVCName(w.ID), cfg.dindDataSize()))
	}
	return pvcs
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
//
// VERSION-AWARE SKIP (PRD #92 M2). The once-only sentinel stranded every worker
// rolled onto a toolchain-changing image: the seeded PVC kept the OLD store while
// PATH pointed at the new profile hash, so go/python3/gcc/pip became 127. The skip
// is now keyed on the image's baked toolchain-identity marker (markerFile =
// /etc/uzi-toolchain-profile, the store-hash profile path): we skip ONLY when the
// sentinel exists AND a recorded marker exists AND the image marker is non-empty AND
// it equals the recorded one. Fresh (no sentinel) seeds; a changed marker reseeds;
// and a LEGACY sentinel with no recorded marker reseeds — that last case is how the
// live broken worker heals on its next roll, so "absent marker = skip" is forbidden.
//
// A reseed WIPES the store, so any store paths per-run provisioning realized at
// runtime vanish — while the devbox/nix state under the data volume (profiles, GC
// roots, generation links, HOME-derived under /data/agent-home; the E2E stub's
// /data/provision root) survives and would dangle into the wiped store, so the next
// `devbox install` can fail to re-realize. On the reseed body ONLY (never the skip
// path) we therefore best-effort clear that SHARED provisioning state under dataDir
// — never the whole data volume, never per-run SDK homes. Best-effort (|| true) so a
// /data permission hiccup cannot strand the worker behind a failed seed; the M3 boot
// preflight and per-run re-install are the backstop.
func nixSeedScript(src, dst, markerFile, dataDir string) string {
	// dataDir is compile-time in production and empty in the seed tests that don't
	// exercise it; emit the clear only when it's set, so an empty dataDir never
	// produces bogus /agent-home paths.
	dataClear := ""
	if dataDir != "" {
		dataClear = fmt.Sprintf(`echo "uzi-seed-nix: clearing dangling shared devbox/nix provisioning state under %[1]s (reseed)"
rm -rf %[1]s/agent-home/.local/share/devbox %[1]s/agent-home/.local/state/nix %[1]s/agent-home/.local/share/nix %[1]s/provision 2>/dev/null || true
`, dataDir)
	}
	return fmt.Sprintf(`set -eu
set -o pipefail
WANT="$(cat %[3]s 2>/dev/null || true)"
HAVE="$(cat %[2]s/%[5]s 2>/dev/null || true)"
if [ -f %[2]s/%[4]s ] && [ -f %[2]s/%[5]s ] && [ -n "$WANT" ] && [ "$WANT" = "$HAVE" ]; then
  echo "uzi-seed-nix: store already seeded (toolchain $WANT matches); nothing to do"
  exit 0
fi
echo "uzi-seed-nix: seeding the nix store from the image (want=$WANT have=$HAVE)"
find %[2]s -mindepth 1 -type d -exec chmod u+w {} + 2>/dev/null || true
find %[2]s -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true
%[6]scd %[1]s
tar -cf - -- $(ls -A) | tar -xpf - -C %[2]s
printf '%%s' "$WANT" > %[2]s/%[5]s
: > %[2]s/%[4]s
echo "uzi-seed-nix: seeded"
`, src, dst, markerFile, nixSentinel, nixToolchainMarker, dataClear)
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
			// Bounds superseded-RS retention so wedged init pods from old agent-image rolls
			// are GC'd (issue #360). This is a DeploymentSpec-level field, not part of the
			// pod Template, so it does not affect the spec-hash and cannot cause a spurious
			// roll.
			RevisionHistoryLimit: ptr(int32(1)),
			Template:             tmpl,
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

	// Default 1, operator-configurable via UZI_WORKER_MAX_CONCURRENT_RUNS (chart
	// workers.maxConcurrentRuns). A zero-value cfg (e.g. a RenderConfig{} in a test)
	// still renders "1", so the default holds everywhere. Raising it opts into the
	// intra-user concurrency residuals documented in docs/worker-setup.md (Decision 7),
	// so a preset must be sized to hold that many concurrent runs.
	maxConcurrentRuns := cfg.MaxConcurrentRuns
	if maxConcurrentRuns < 1 {
		maxConcurrentRuns = 1
	}

	env := []corev1.EnvVar{
		{Name: "UZI_API_URL", Value: cfg.APIURL},
		{Name: "UZI_WORKER_TOKEN_FILE", Value: tokenPath},
		{Name: "UZI_DATA_DIR", Value: dataMountPath},
		{Name: "WORKER_MAX_CONCURRENT_RUNS", Value: strconv.Itoa(maxConcurrentRuns)},
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

		// TMPDIR into the shared run workdir (M-workdir), docker workers ONLY. A tool that
		// stages a `docker run -v <src>:/x` / compose bind source under $TMPDIR (uzi's own
		// e2e defaults its run dir to ${TMPDIR:-/tmp}/uzi-e2e-$$) needs <src> to resolve in
		// the DAEMON's filesystem, not the worker's — otherwise the bind source is an empty
		// dir in the daemon and the run silently sees nothing. The daemon shares only
		// dindWorkdirDir with the worker, so tmp MUST live under it. Point it at the mount
		// root directly: the emptyDir mount point always exists and is writable by fsGroup
		// 10001 (no subdir to create, no agent/entrypoint change).
		//
		// NO secret transits here (Decision 3 holds): the PAT is env-scoped not on disk,
		// per-run secrets have their own volume, and the join token is its own mount — none
		// of them route through $TMPDIR. The daemon can already read this dir (it is shared
		// by design), so tmp being dind-visible adds no exposure beyond the checkout that
		// M-workdir already shares.
		//
		// The agent honours this: on the current non-root k8s start entrypoint.sh takes the
		// single-uid branch (it unsets UZI_RUNNER_TMPDIR and does NOT set its own TMPDIR), so
		// this pod-spec TMPDIR survives and runnerTmpdir() (= UZI_RUNNER_TMPDIR || TMPDIR)
		// returns it — reaching both the worker and the repo's docker/compose. Render-only,
		// verified against agent/templates/entrypoint.sh + agent/src/runner-uid.ts.
		// FUTURE COUPLING: PRD #51 M4's runner-uid split (root-started A1 path, not yet on
		// k8s) makes the entrypoint export its OWN tmpdirs UNDER /tmp — worker TMPDIR
		// /tmp/uzi-worker (overriding this value) and UZI_RUNNER_TMPDIR /tmp/uzi-runner — both
		// OUTSIDE /data/runner, so repo bind sources would land back outside the dind-shared
		// workdir. When that split lands on k8s, both tmpdirs must be relocated under
		// dindWorkdirDir. Not a 0.8.1 concern (the split does not run on the non-root start).
		env = append(env, corev1.EnvVar{Name: "TMPDIR", Value: dindWorkdirDir})
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
		Command:         []string{"/bin/sh", "-c", nixSeedScript(nixMountPath, nixSeedMountPath, toolchainProfileMarker, dataSeedMountPath)},
		Resources:       seedResources,
		SecurityContext: containerSecurity,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "nix", MountPath: nixSeedMountPath},
			// The DATA PVC too, at a NON-/data path: a reseed must clear the shared
			// devbox/nix provisioning state persisted here or it dangles into the wiped
			// store (PRD #92 M2). Mounted at dataSeedMountPath for the same masking
			// rationale as the nix mount above.
			// FUTURE COUPLING: PRD #51 M4's runner-uid split (root-started A1 path, not yet
			// on k8s) makes /data shared across the worker and a distinct `runner` uid. This
			// init runs as the pod runAsUser; today (single-uid) that IS the uid that owns the
			// devbox/nix state, so the best-effort `rm -rf` succeeds. Once the split lands on
			// k8s, this container's uid + the cleared paths' ownership must be re-derived —
			// otherwise the clear EACCESes and, being best-effort (`|| true`), silently
			// no-ops, regressing the dangling-store heal onto M4's "devbox install re-realizes"
			// fallback. Re-derive alongside the TMPDIR relocation noted above.
			{Name: "data", MountPath: dataSeedMountPath},
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
			// /var/lib/docker per posture). Its OWN PVC since issue #224 M-a — it was the
			// emptyDir arch §Q4 flagged, and an emptyDir's bytes count toward the pod's
			// ephemeral-storage usage, which is what made a big image pull get the worker
			// EVICTED and its in-flight run's tree destroyed. This is the images + build
			// cache the daemon writes, none of it worker state, and Decision 3 is
			// untouched: it stays mounted into `dind` alone.
			corev1.Volume{Name: dindDataVolume, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: dindDataPVCName(w.ID),
				},
			}},
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
						// The WHOLE pod's ephemeral-storage budget, on this one container, with
						// NO matching limit. Both of those are load-bearing and neither is
						// obvious — read ephemeralRequest's header before changing either.
						// Adding a limit "for symmetry with cpu/memory" creates a deterministic
						// eviction path that fires with no node pressure at all.
						corev1.ResourceEphemeralStorage: cfg.ephemeralRequest(w),
					},
					// cpu and memory ONLY. See above: there is no ephemeral-storage limit here
					// on purpose, and TestNoContainerDeclaresAnEphemeralStorageLimit enforces
					// it across every container and initContainer in the pod.
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
			// private data-root PVC (issue #224 M-a) + the shared run workdir emptyDir
			// (and, rootless only, the shared socket-dir emptyDir).
			Volumes: volumes,
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
