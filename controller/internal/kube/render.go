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
)

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
			Namespace: cfg.Namespace,
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
	return []*corev1.PersistentVolumeClaim{
		renderPVC(cfg, w.ID, dataPVCName(w.ID), spec.Size.DataSize),
		renderPVC(cfg, w.ID, nixPVCName(w.ID), spec.NixSize),
	}
}

func renderPVC(cfg RenderConfig, id, name string, size resource.Quantity) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
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
func nixSeedScript(src, dst string) string {
	return fmt.Sprintf(`set -eu
if [ -f %[2]s/%[3]s ]; then
  echo "uzi-seed-nix: store already seeded; nothing to do"
  exit 0
fi
echo "uzi-seed-nix: seeding the nix store from the image into an empty volume"
tar -cf - -C %[1]s . | tar -xpf - -C %[2]s
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
			Namespace: cfg.Namespace,
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

	containerSecurity := &corev1.SecurityContext{
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
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
			InitContainers: []corev1.Container{{
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
			}},
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
				VolumeMounts: []corev1.VolumeMount{
					{Name: "token", MountPath: secretMountPath, ReadOnly: true},
					{Name: "data", MountPath: dataMountPath},
					{Name: "nix", MountPath: nixMountPath},
				},
			}},
			Volumes: []corev1.Volume{
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
			},
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
