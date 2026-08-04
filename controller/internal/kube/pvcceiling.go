package kube

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/preset"
)

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
// THE INVARIANT IS A PER-TIER MINIMUM, NOT AN EQUALITY. The two tiers are separate
// namespaces with separate LimitRanges; they carry the same 20Gi today and nothing
// requires that to continue. Each tier's claimants are checked against THAT tier's
// ceiling, so a future divergence is handled by construction. Where one value ever
// serves both tiers, the invariant is `claimant <= min(restricted, docker)` — the min
// is the fallback shape, and the per-tier check is the general one.
//
// AN ABSENT CEILING SKIPS THAT TIER, DELIBERATELY. Empty means the chart rendered no
// LimitRange, so there is nothing to violate. Defaulting a ceiling would be worse than
// not checking: a guessed value that is too low refuses to boot a healthy fleet, which
// is a self-inflicted outage in a function whose whole purpose is preventing one.
func ValidatePVCCeilings(cfg RenderConfig, resolver preset.Resolver) error {
	tiers := []struct {
		what      string   // human name, for the message
		key       string   // the env var an operator would edit
		ceiling   string   // "" = no LimitRange on this tier, skip
		claimants []string // extra claim names beyond data+nix
	}{
		{"restricted", "UZI_WORKER_MAX_PVC_STORAGE", cfg.MaxPVCStorage, nil},
		{"docker", "UZI_WORKER_DOCKER_MAX_PVC_STORAGE", cfg.DockerMaxPVCStorage, []string{"dind-data"}},
	}

	var problems []string
	for _, tier := range tiers {
		if tier.ceiling == "" {
			continue
		}
		// Config validated this as a quantity at boot, before this runs.
		max, err := resource.ParseQuantity(tier.ceiling)
		if err != nil {
			return fmt.Errorf("%s=%q is not a resource quantity: %w", tier.key, tier.ceiling, err)
		}

		claims := map[string]resource.Quantity{}
		// Every (template, size) pair, because DataSize varies by size and a template
		// that resolves to a different table later must not slip past this.
		for _, template := range preset.TemplateNames() {
			for _, size := range preset.SizeNames() {
				spec, err := resolver.Resolve(template, size)
				if err != nil {
					return fmt.Errorf("resolving preset %q/%q to check PVC ceilings: %w", template, size, err)
				}
				claims[fmt.Sprintf("preset %q /data (DataSize)", size)] = spec.Size.DataSize
				claims["/nix (nixSize, flat across every preset)"] = spec.NixSize
			}
		}
		for _, extra := range tier.claimants {
			if extra == "dind-data" {
				// The EFFECTIVE size: the cluster's override when set, the controller's
				// own default otherwise. Checking only the override would leave the
				// default path unguarded, which is exactly the hole the chart guard had.
				claims["dind data root (workers.docker.dindDataSize, or the built-in default when unset)"] = cfg.dindDataSize()
			}
		}

		names := make([]string, 0, len(claims))
		for name := range claims {
			names = append(names, name)
		}
		sort.Strings(names) // deterministic message ordering
		for _, name := range names {
			size := claims[name]
			if size.Cmp(max) > 0 {
				problems = append(problems, fmt.Sprintf(
					"%s tier: %s = %s exceeds %s = %s",
					tier.what, name, size.String(), tier.key, max.String()))
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("hosted-worker PVC sizes exceed their namespace's LimitRange ceiling, so those claims would be "+
		"REJECTED at admission and every affected worker would provision and never appear (the Deployment is never "+
		"created and the reconcile loop retries forever with the reason only in this log):\n  %s\n"+
		"Raise the tier's limitRange.maxPVCStorage in deploy/chart/values.yaml — and its quota.requestsStorage with "+
		"it, or the tier runs out of storage budget instead — or lower the offending size",
		strings.Join(problems, "\n  "))
}
