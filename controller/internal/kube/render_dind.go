package kube

import (
	"fmt"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
//     userns (dev-cluster). RunAsNonRoot MUST be false at CONTAINER scope: the
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
		Resources:       cfg.dindResources(),
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
