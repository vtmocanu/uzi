package kube

import (
	"context"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	corev1 "k8s.io/api/core/v1"
)

type podEpochMaintenanceKey struct{}
type podEpochMaintenanceDelete struct {
	generation                 PodEpochGeneration
	operationID, nonce, oldUID string
}

// SetPodEpochSession is configured before either the watch or materializer runs.
func (m *Materializer) SetPodEpochSession(s *PodEpochSession) { m.epoch = s }
func (m *Materializer) recordEpochDelete(g PodEpochGeneration, worker string, op protocol.DindMaintenance) {
	if m.epoch == nil || g != m.epoch.CurrentGeneration() || op.ID == "" || op.Nonce == "" {
		return
	}
	// Only the serial materializer accesses this map. Purge receipts on generation
	// changes and cap entries; siblings continue even if the evidence budget is full.
	for id, r := range m.epochDeletes {
		if r.generation != g {
			delete(m.epochDeletes, id)
		}
	}
	if m.epochDeletes == nil {
		m.epochDeletes = map[string]podEpochMaintenanceDelete{}
	}
	if len(m.epochDeletes) >= PodEpochMaxIdentities {
		return
	}
	m.epochDeletes[worker] = podEpochMaintenanceDelete{g, op.ID, op.Nonce, op.PVCUID}
}
func (m *Materializer) recordEpochMint(ctx context.Context, worker string, pvc *corev1.PersistentVolumeClaim) {
	if m.epoch == nil || pvc == nil {
		return
	}
	r, ok := m.epochDeletes[worker]
	op, bound := ctx.Value(podEpochMaintenanceKey{}).(protocol.DindMaintenance)
	if !ok || !bound || r.generation != m.epoch.CurrentGeneration() || r.operationID != op.ID || r.nonce != op.Nonce || r.oldUID != op.PVCUID {
		return
	}
	// Only a successful Create response supplies the new identity. Neither an API
	// maintenance intent nor an AlreadyExists response imports restart authority.
	_ = m.epoch.RecordMaintenanceRemint(r.generation, worker, PodEpochRemint{OperationID: r.operationID, OldClaimUID: r.oldUID, NewClaimUID: string(pvc.UID)})
	delete(m.epochDeletes, worker)
}
