package kube

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// render_dind.go carries the Docker-in-Docker worker seam split out of render.go
// (PRD #1048): the dind consts, sizes, resources and the sidecar/init/anti-affinity constructors.

// The docker sidecar's wiring (PRD #83 M3). The socket dir is a shared emptyDir
// carrying ONLY the daemon socket; the data dir is the daemon's own root, on a
// private PVC mounted into `dind` alone (issue #224 M-a — it was an emptyDir until
// then; see dindDataDefaultSize).
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
// a ResourceQuota on requests.CPU OR requests.MEMORY rejects any pod with a container
// declaring none, and native sidecars count toward the pod's request (they run
// alongside the worker). The sidecar budget follows arch §Q4 (~1-2 GiB / ~1 CPU on
// top of the agent).
//
// "cpu or memory", NOT "requests.*", and the difference is not pedantry (issue #224).
// The quota evaluator intersects a quota's tracked resources against a hardcoded
// validationSet that holds cpu and memory ALONE — upstream calls that enforcement a
// frozen mistake and says in so many words not to extend it. So the rule justifying
// these constants holds EXACTLY, and it does not generalise: a quota on
// requests.ephemeral-storage tracks the resource and can never force a pod to declare
// it, which is why the docker tier's 200Gi key sat at used=0 while the nodes filled
// and a worker was evicted. Do not read the corrected sentence as a reason to relax
// anything here.
//
// The dind requests+limits are the DEFAULT; a cluster overrides them per-cluster (PRD #89
// 0.8.1, RenderConfig.DinDRequest*/DinDLimit*) because in-daemon image builds OOM the 2Gi
// default — uzi's own e2e builds 5 images inside the daemon. BOTH the requests and the
// limits are overridable: raising only the limit lets the scheduler place the pod on a
// node without the memory and OOM it under contention, so a cluster that raises the memory
// limit raises the request in lockstep. See dindResources() below.
const (
	dindDefaultRequestCPU    = "250m"
	dindDefaultRequestMemory = "256Mi"
	dindDefaultLimitCPU      = "2"
	dindDefaultLimitMemory   = "2Gi"
)

// dindDataDefaultSize is the DinD daemon's data-root PVC — the images + build cache
// (issue #224 M-a). It was an emptyDir until then, and moving it off node ephemeral
// storage is the whole point: an emptyDir's bytes are charged to the POD's
// ephemeral-storage usage, so a runaway image pull made the worker the pod kubelet
// evicted under node disk pressure, which destroys the run's working tree. On a PVC
// the daemon simply runs out of space: the docker step fails with ENOSPC, the pod
// survives, and the tree survives with it. Backpressure instead of eviction.
//
// FLAT, and chart-overridable per cluster (UZI_WORKER_DIND_DATA_SIZE), for exactly
// nixSize's reasons: the size tracks the user's repo images, not the worker's
// CPU/RAM preset, so a per-size table here would be one repeated number.
//
// 20Gi is the top of the chart's own documented dind band, AND it is the ceiling:
// the worker namespaces' LimitRange caps a PVC at maxPVCStorage (20Gi), and the
// limitranger validates PVC creates, so a larger value is rejected at admission
// rather than merely being expensive. Raising this means raising that too.
//
// THIS CONSTANT IS DUPLICATED IN deploy/chart/templates/worker-invariants.yaml, which
// substitutes it when workers.docker.dindDataSize is unset — otherwise the guard there
// would skip the default path entirely and a lowered maxPVCStorage would render clean
// while every docker worker's third claim was rejected at admission (audit A18.2).
// Helm cannot read a Go constant, so the two are tied by comment in both directions
// plus TestDinDDataDefaultFitsTheChartsLimitRangeMax, which fails if this outgrows the
// chart's ceiling. Move them together.
//
// RESIDUAL, documented rather than built, and it is the identical one nixSize
// carries for /nix: the cache now PERSISTS across pod rolls and nothing garbage-
// collects it, so a long-lived docker worker's image cache only grows into this
// fixed volume. The emptyDir at least died with the pod. `docker system prune` is
// the remedy and an agent can run it; v1's fallback is the same as everywhere else
// here — delete + reprovision.
const dindDataDefaultSize = "20Gi"

// dindDataSize is the DinD data-root PVC size: the cluster's override when set,
// dindDataDefaultSize otherwise. Config validated any override string as a k8s
// quantity at boot, so MustParse here is safe — the same contract dindResources
// relies on.
func (cfg RenderConfig) dindDataSize() resource.Quantity {
	if cfg.DinDDataSize != "" {
		return resource.MustParse(cfg.DinDDataSize)
	}
	return resource.MustParse(dindDataDefaultSize)
}

// dindResources builds the DinD sidecar's requests+limits from cfg, falling back to the
// dindDefault* constants for any field a cluster did not override (config validated every
// override string as a k8s quantity at boot, so MustParse here is safe).
func (cfg RenderConfig) dindResources() corev1.ResourceRequirements {
	pick := func(override, def string) string {
		if override != "" {
			return override
		}
		return def
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(pick(cfg.DinDRequestCPU, dindDefaultRequestCPU)),
			corev1.ResourceMemory: resource.MustParse(pick(cfg.DinDRequestMemory, dindDefaultRequestMemory)),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(pick(cfg.DinDLimitCPU, dindDefaultLimitCPU)),
			corev1.ResourceMemory: resource.MustParse(pick(cfg.DinDLimitMemory, dindDefaultLimitMemory)),
		},
	}
}

var (
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
