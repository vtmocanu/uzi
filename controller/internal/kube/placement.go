package kube

import (
	"context"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// placementTickKey carries the per-tick placement decision (worker id -> stamped) in the
// Reconcile context.
type placementTickKey struct{}

// placementClaimChanged detects fresh evidence that the tick's counted claim is
// no longer the same placement input. Size and resourceVersion changes do not
// affect placement; identity, presence and the create-time stamp do.
func placementClaimChanged(before, after *reconcile.ClaimObservation) bool {
	if before == nil || after == nil {
		return before != after
	}
	return before.UID != after.UID || before.PlacementNew != after.PlacementNew
}

// newPlacementStamped reports whether a worker renders the new placement: newPlacement is
// configured and every observed PVC (Terminating included) carries the stamp. A worker with
// no PVCs counts as stamped, because every PVC it creates this tick is stamped.
func newPlacementStamped(cfg RenderConfig, obs reconcile.ObservedWorker) bool {
	if !cfg.NewPlacementSet() {
		return false
	}
	for _, c := range []struct {
		has   bool
		claim *reconcile.ClaimObservation
	}{
		{obs.HasDataPVC, obs.DataClaim},
		{obs.HasNixPVC, obs.NixClaim},
		{obs.HasDinDDataPVC, obs.DinDClaim},
	} {
		if c.has && c.claim == nil {
			return false // present but unreadable: not proof of a stamp
		}
		if c.claim != nil && !c.claim.PlacementNew {
			return false
		}
	}
	return true
}

// placementDecisions computes the stamped decision for every DESIRED worker once per
// tick, from that tick's observation. Observe emits only workers that already have
// objects, so a brand-new or ephemeral worker reads as an empty observation (stamped).
// Freezing it here keeps the Deployment render, roll pacing, maintenance and hibernation
// paths on one decision even as PVCs are created or deleted mid-tick.
func (m *Materializer) placementDecisions(desired []protocol.DesiredWorker, observed map[string]reconcile.ObservedWorker) map[string]bool {
	p := make(map[string]bool, len(desired))
	for _, w := range desired {
		p[w.ID] = newPlacementStamped(m.cfg, observed[w.ID])
	}
	return p
}

// placementStampedFor returns the tick's frozen decision for w, or derives it from obs
// when called outside Reconcile.
func (m *Materializer) placementStampedFor(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker) bool {
	if p, ok := ctx.Value(placementTickKey{}).(map[string]bool); ok {
		if v, ok := p[w.ID]; ok {
			return v
		}
	}
	return newPlacementStamped(m.cfg, obs)
}

// renderCfgFor is the config every render of w's Deployment uses, so all hash consumers
// agree on the placement and on whether the worker's Secret carries the Codex CA key.
func (m *Materializer) renderCfgFor(ctx context.Context, w protocol.DesiredWorker, obs reconcile.ObservedWorker) RenderConfig {
	return m.cfg.withPlacement(m.placementStampedFor(ctx, w, obs)).withCodexCA(m.codexCAInSecretFor(ctx, w, obs))
}
