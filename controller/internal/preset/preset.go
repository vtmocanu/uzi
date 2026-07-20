// Package preset is the controller's authority on what a hosted worker's pod spec
// actually contains (PRD #58 Decisions 1 and 7).
//
// The api sends NAMES — a template ("base") and a size ("m") — and never a
// quantity, an image, or anything else a pod spec is made of. That is not a
// stylistic split: the api holds zero kube-apiserver access, and the moment it
// ships resolved values it becomes the authority on a pod spec it is not allowed to
// know anything about. So every mapping from a name to a concrete value lives here,
// in the component that already holds the cluster credential.
//
// TWO TABLES, TWO GOLDENS, ONE FAILURE MODE. Both `sizes` and `templateImages` are
// resolved against names the api independently validates, so both skew identically:
// add a name to the api's registry without an entry here and the api accepts a
// provision this controller can never render — the worker provisions, no pod is
// built, and the row sits pending until its token expires, visible only as a worker
// that never comes online. preset_contract_test.go reads the api's goldens across
// the tree so that is a red build rather than a silent strand.
//
// The goldens catch OUR drift. They cannot catch DEPLOYMENT SKEW: api and
// controller are separately-built images, so even under Model B's version pinning a
// rollout has a window where an old controller polls a new api. Resolve therefore
// returns a typed miss (UnknownError) rather than a zero Spec, and the caller logs
// and skips RENDERING that one worker — never removes it from desired state. See
// kube.Materializer.Reconcile: getting that backwards tears down every worker
// carrying the new name.
package preset

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Size is one preset's resolved quantities.
//
// QoS is deliberately BURSTABLE (requests < limits), not Guaranteed. The PRD's own
// fleet arithmetic ("10 users x quota 2 x M => 10 cores / 20Gi RAM") is only
// coherent as REQUESTS — it is impossible as a limit, since a measured real Claude
// Agent SDK run peaks at 676 MiB while compose grants 4 GiB. Guaranteed would also
// strand 4Gi per IDLE worker (measured idle: 148 MiB) on a shared cluster.
//
// The requests deliberately sit ABOVE the measured peak: kubelet evicts pods whose
// usage exceeds their REQUESTS first, so a request below real usage would make
// hosted workers the first thing evicted under node pressure.
type Size struct {
	CPURequest    resource.Quantity
	CPULimit      resource.Quantity
	MemoryRequest resource.Quantity
	MemoryLimit   resource.Quantity
	// DataSize is the /data PVC: the bare-clone cache, worktrees and per-run
	// workspaces. The only volume that varies by size.
	DataSize resource.Quantity
}

// Spec is a fully resolved (template, size) pair — everything the renderer needs
// and nothing it has to look up elsewhere.
type Spec struct {
	// Image is the fully-qualified agent image reference for the template.
	Image string
	Size  Size
	// NixSize is flat across every size — see nixSize.
	NixSize resource.Quantity
}

// UnknownError is the typed miss: a name this controller's tables do not carry.
// Returned instead of a zero Spec so a caller cannot confuse "unknown" with
// "resolved to nothing", and typed so the reconcile loop can tell a skew skip
// (expected, log at info, keep the worker desired) from a real failure.
type UnknownError struct {
	// Field is "template" or "size".
	Field string
	// Value is the unresolvable name. It is a server-validated registry name, never
	// free user text, so it is safe to log.
	Value string
}

func (e *UnknownError) Error() string {
	return fmt.Sprintf("unknown %s %q: this controller's preset table does not carry it "+
		"(expected during a rollout where an older controller polls a newer api; "+
		"otherwise the api's registry and this table have drifted)", e.Field, e.Value)
}

// IsUnknown reports whether err is a preset miss rather than a real failure.
func IsUnknown(err error) bool {
	var u *UnknownError
	return errors.As(err, &u)
}

// sizes is the preset table (USER DECISION, 2026-07-17; measured basis in PRD #58's
// M6 bullet — a real SDK run peaks at 676 MiB, idle 148 MiB, and compose grants
// 2 CPU / 4 GiB for 1 run slot + 1 chat session).
//
// `m` is compose parity at the limit and is the default. The default cap is 1
// (WORKER_MAX_CONCURRENT_RUNS), so a size still buys headroom for ONE run. That cap
// is now operator-configurable (chart workers.maxConcurrentRuns → the controller's
// UZI_WORKER_MAX_CONCURRENT_RUNS), and an operator raising it must pick a size that
// fits that many concurrent runs.
//
// MustParse is right here: these are compile-time constants, so a typo is a
// programmer error that must not be papered over into a zero quantity — and it
// panics at package init, which the contract test below reaches on every run.
var sizes = map[string]Size{
	"s": {
		CPURequest:    resource.MustParse("250m"),
		CPULimit:      resource.MustParse("1"),
		MemoryRequest: resource.MustParse("1Gi"),
		MemoryLimit:   resource.MustParse("2Gi"),
		DataSize:      resource.MustParse("5Gi"),
	},
	"m": {
		CPURequest:    resource.MustParse("500m"),
		CPULimit:      resource.MustParse("2"),
		MemoryRequest: resource.MustParse("2Gi"),
		MemoryLimit:   resource.MustParse("4Gi"),
		DataSize:      resource.MustParse("10Gi"),
	},
	"l": {
		CPURequest:    resource.MustParse("1"),
		CPULimit:      resource.MustParse("4"),
		MemoryRequest: resource.MustParse("4Gi"),
		MemoryLimit:   resource.MustParse("8Gi"),
		DataSize:      resource.MustParse("20Gi"),
	},
}

// nixSize is FLAT across every size and every template, which is why it sits
// outside the table (Decision 7's own later correction). Measured: the image's
// baked store is byte-identical between `base` and `jvm` (209 MB / 74 store paths —
// the JDK ships via apk to /usr/lib/jvm, never through nix), and provisioning the
// full tier-1 allowlist grows it to 1,703 MB / 1,205 store paths worst case. 4Gi is
// ~2.4x headroom over that; 8Gi would be mostly waste, and storage is the binding
// fleet constraint (20 x `m` = 10 CPU / 40Gi requested but 280Gi of PVC).
//
// Residual, documented rather than built: nix has no auto-GC, so a long-lived
// worker's store only grows into this fixed volume. `nix store gc` lives in agent/,
// so v1's remedy is delete + reprovision.
var nixSize = resource.MustParse("4Gi")

// templateImages maps a worker template to its published image's NAME component.
// The full reference is <repo>/<name>:<tag>, where repo and tag come from the
// controller's own config (Decision 9's text says the chart injects the tag "into
// the api config" — that is wrong and could not be implemented: the api knows no
// image tag and M1's wire carries no image field. Corrected in the PRD).
//
// Spelled out per template rather than computed as "agent-"+template ON PURPOSE. A
// computed name would resolve ANY template, which makes both this table and the
// golden gate vacuous — a new template would render a pod pulling an image CI never
// published, and the failure would move from a red test to an ImagePullBackOff on a
// user's worker. The explicit entry is the thing M6's CI build list has to agree
// with.
var templateImages = map[string]string{
	"base": "agent-base",
	"jvm":  "agent-jvm",
}

// Resolver turns the api's names into a pod spec's values. It is constructed from
// config (the image repo + tag) so the tables above stay pure data.
type Resolver struct {
	repo string
	tag  string
}

// NewResolver builds a Resolver for a given image repository prefix and tag.
//
// Both are required and neither has a defensible default: an empty repo would
// silently mean Docker Hub, and defaulting the tag to something like "latest" would
// render a pod pulling an unpinned image — the two failures are an ImagePullBackOff
// and a fleet running an unknown release, and both are worse than refusing to boot.
func NewResolver(repo, tag string) (Resolver, error) {
	repo = strings.TrimSuffix(strings.TrimSpace(repo), "/")
	tag = strings.TrimSpace(tag)
	if repo == "" {
		return Resolver{}, errors.New("worker image repository is required")
	}
	if tag == "" {
		return Resolver{}, errors.New("worker image tag is required")
	}
	return Resolver{repo: repo, tag: tag}, nil
}

// Resolve maps a desired worker's (template, size) names onto its pod spec's
// values. An unknown name on EITHER field is an UnknownError, never a partial Spec.
func (r Resolver) Resolve(template, size string) (Spec, error) {
	image, ok := templateImages[template]
	if !ok {
		return Spec{}, &UnknownError{Field: "template", Value: template}
	}
	sz, ok := sizes[size]
	if !ok {
		return Spec{}, &UnknownError{Field: "size", Value: size}
	}
	return Spec{
		Image:   fmt.Sprintf("%s/%s:%s", r.repo, image, r.tag),
		Size:    sz,
		NixSize: nixSize,
	}, nil
}

// SizeNames and TemplateNames report what the tables carry, for the contract tests
// and for a startup log line. Order is not meaningful (they are map keys); callers
// that care sort.
func SizeNames() []string     { return keys(sizes) }
func TemplateNames() []string { return keys(templateImages) }

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
