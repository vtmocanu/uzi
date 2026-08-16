package kube

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// ceilingProbeID names the throwaway worker this check renders to enumerate the PVCs.
// It never reaches the apiserver — RenderPVCs builds objects in memory and this function
// only reads their sizes — and it exists so the claim names in the failure message are
// stripped back to their suffixes (-data, -nix, -dind-data).
const ceilingProbeID = "ceiling-probe"

// ValidatePVCCeilings refuses to boot when any PVC this controller would create is
// larger than the LimitRange ceiling of the namespace it lands in (issue #224).
//
// WHY THIS LIVES HERE AND NOT IN THE CHART, since it looks at first like a chart
// concern. Three things claim a PVC per worker — `nixSize`, each preset's `DataSize`
// and the dind data root — and only ONE of them comes from the chart. The other two
// are Go constants a Helm template cannot read, and putting them into values.yaml was
// rejected deliberately: it would make the chart a fourth place preset quantities
// appear and the only one with no golden gating it, which is the ungated-skew class
// preset.go's own header exists to prevent. This controller is the only place in the
// system where all of them exist at once, because it is what constructs the claims.
// The ceiling is therefore a MISSING INPUT rather than a layering violation — the same
// kind of fact as StorageClass, which it already takes from the chart.
//
// WHAT IT PREVENTS, and why refusing to boot is the proportionate response. An
// oversized claim is rejected by the limitranger admission plugin; the materializer
// returns before the Deployment is created; the reconcile loop logs and retries every
// tick. Nothing is reported to the api. The worker provisions and NEVER APPEARS, for
// as long as anyone leaves it, with the cause in one log line. Failing at boot turns
// a silent permanent stall into a CrashLoopBackOff naming the two numbers — the same
// trade `parseQuantityEnv` already makes for a typo'd quantity, and the same posture
// as secretbox refusing to start on a placeholder key.
//
// THE CHART GUARD STAYS AND IS NOT REDUNDANT. `helm install` failing is earlier and
// cheaper than a CrashLoopBackOff, and it covers the one claimant the chart owns. They
// are complementary nets, not alternatives.
//
// 🔴 THIS CHECK'S VALUE DEPENDS ON THE CONTROLLER DEPLOYMENT'S `strategy: Recreate`,
// WHICH LIVES IN ANOTHER FILE IN ANOTHER LANGUAGE. Refusing to boot only helps if the
// refusing pod is the ONLY one. Under `RollingUpdate` a new pod that EXITS AT BOOT never
// becomes Available, so the old pod is never retired — it keeps reconciling
// indefinitely, holding the OLD ceiling, against the NEW LimitRange helm has already
// applied. That is precisely the mismatch this check exists to catch, with the check
// firing and being DEFEATED, and with a crash-looping pod beside it that reads like the
// guard working.
//
// That holds at ANY replicaCount and with or WITHOUT probes — a crash-looping container
// is never Ready either way. An earlier version of this paragraph reasoned from
// "replicaCount 1 and no probes, SO ...", which reads as if adding a readinessProbe
// would address it. It would not, and the gate would stay green.
//
// `Recreate` deletes the old pod FIRST, so the failure is fail-STOPPED (no controller
// runs at all) rather than fail-STALE (an old one keeps going on stale config).
// deploy/chart/templates/controller-deployment.yaml carries the reciprocal note, and
// scripts/assert-controller-strategy.sh fails the render if it ever changes — a comment
// alone did not hold, which is how this coupling came to be undocumented in the first
// place.
//
// THE INVARIANT IS A PER-TIER MINIMUM, NOT AN EQUALITY. The two tiers are separate
// namespaces with separate LimitRanges; they carry the same 20Gi today and nothing
// requires that to continue. Each tier's claimants are checked against THAT tier's
// ceiling, so a future divergence is handled by construction. Where one value ever
// serves both tiers, the invariant is `claimant <= min(restricted, docker)` — the min
// is the fallback shape, and the per-tier check is the general one.
//
// AN ABSENT CEILING SKIPS THAT TIER, AND THE REASON IS STRUCTURAL RATHER THAN
// PRUDENTIAL. Each env var renders under EXACTLY its tier's LimitRange condition, so
// absent env <=> no LimitRange <=> there is no ceiling in existence. Skipping is not
// "declining to check", it is "there is no operand": a default here would invent a
// constraint the cluster does not have.
//
// That distinction is load-bearing because the weaker argument invites the wrong fix.
// This comment used to say only that a guessed value too low would refuse to boot a
// healthy fleet — true, but an argument against DEFAULTING, not an argument FOR
// skipping, and it reads as a pragmatic compromise someone could improve on. The
// structural form forecloses that.
//
// One sub-case where the equivalence is imperfect, reachable through a SUPPORTED chart
// value rather than an out-of-band edit: setting `limitRange.enabled: false` while a
// LimitRange from a previous install is still in the cluster. The env then goes
// uninjected and this check skips a tier that still has a live ceiling. Under uzi's own
// ArgoCD app `prune: true` deletes the object, so it does not arise here; an
// installation that syncs manually should delete the LimitRange with the flag.
func ValidatePVCCeilings(cfg RenderConfig, resolver preset.Resolver) error {
	tiers := []struct {
		what   string // human name, for the message
		key    string // the env var an operator would edit
		max    string // "" = no LimitRange on this tier, so no operand; see below
		docker bool   // which tier's worker shape to render
	}{
		{"restricted", "UZI_WORKER_MAX_PVC_STORAGE", cfg.MaxPVCStorage, false},
		{"docker", "UZI_WORKER_DOCKER_MAX_PVC_STORAGE", cfg.DockerMaxPVCStorage, true},
	}

	var problems []string
	for _, tier := range tiers {
		if tier.max == "" {
			continue
		}
		// Config validated this as a quantity at boot, before this runs.
		max, err := resource.ParseQuantity(tier.max)
		if err != nil {
			return fmt.Errorf("%s=%q is not a resource quantity: %w", tier.key, tier.max, err)
		}

		// The largest claim seen per PVC, and the preset that produced it.
		type worst struct {
			size   resource.Quantity
			preset string
		}
		largest := map[string]worst{}

		// EVERY (template, size) pair, because DataSize varies by size and a template
		// that resolves against a different table later must not slip past.
		for _, template := range preset.TemplateNames() {
			for _, size := range preset.SizeNames() {
				spec, err := resolver.Resolve(template, size)
				if err != nil {
					return fmt.Errorf("resolving preset %q/%q to check PVC ceilings: %w", template, size, err)
				}
				w := protocol.DesiredWorker{ID: ceilingProbeID, Template: template, Size: size, Docker: tier.docker}
				for _, pvc := range RenderPVCs(cfg, w, spec) {
					got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
					name := strings.TrimPrefix(pvc.Name, NamePrefix+ceilingProbeID)
					if cur, seen := largest[name]; !seen || got.Cmp(cur.size) > 0 {
						largest[name] = worst{size: got, preset: size}
					}
				}
			}
		}

		names := make([]string, 0, len(largest))
		for name := range largest {
			names = append(names, name)
		}
		sort.Strings(names) // deterministic message ordering
		for _, name := range names {
			w := largest[name]
			if w.size.Cmp(max) > 0 {
				problems = append(problems, fmt.Sprintf(
					"%s tier: the %s claim is %s at preset %q, which exceeds %s = %s",
					tier.what, name, w.size.String(), w.preset, tier.key, max.String()))
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	// The claim is named by its PVC SUFFIX because that is what RenderPVCs produced and
	// what an operator sees in `kubectl get pvc`. The remediation sentence names where the
	// sizes come from as a whole rather than per claim, deliberately: a per-claim mapping
	// would be a second hand-maintained list, which is the defect this function's
	// enumeration was just rewritten to remove.
	return fmt.Errorf("hosted-worker PVC sizes exceed their namespace's LimitRange ceiling, so those claims would be "+
		"REJECTED at admission and every affected worker would provision and never appear (the Deployment is never "+
		"created and the reconcile loop retries forever with the reason only in this log):\n  %s\n"+
		"Raise the tier's limitRange.maxPVCStorage in deploy/chart/values.yaml — and its quota.requestsStorage with "+
		"it, or the tier runs out of storage budget instead — or lower the offending size (-data and -nix come from "+
		"the preset table in controller/internal/preset; -dind-data from workers.docker.dindDataSize, or the "+
		"controller's built-in default when that is unset)",
		strings.Join(problems, "\n  "))
}
