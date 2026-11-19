package kube

import (
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// RefusedError is the typed refusal for a desired worker whose size cannot be rendered
// on this cluster (PRD #4213): its integers are invalid, or a CPU/memory limit or a PVC
// request exceeds the tier's ceiling. Like preset.UnknownError it means "skip RENDERING
// this worker, keep it desired, touch nothing it already owns" and never a teardown.
type RefusedError struct {
	// Reason is a short human sentence naming the field, the value and the ceiling. It is
	// built from integers and operator config only, never from apiserver text.
	Reason string
}

func (e *RefusedError) Error() string { return e.Reason }

// IsRefused reports whether err is a size refusal.
func IsRefused(err error) bool {
	var r *RefusedError
	return errors.As(err, &r)
}

// recordRefusal remembers a per-worker refusal/rejection for this tick's fleet report.
func (m *Materializer) recordRefusal(id, kind, reason string) {
	m.refusalMu.Lock()
	defer m.refusalMu.Unlock()
	if m.refusals == nil {
		m.refusals = map[string]reconcile.Refusal{}
	}
	m.refusals[id] = reconcile.Refusal{Kind: kind, Reason: reason}
}

// resetRefusals starts a tick with no refusals; a worker that renders cleanly next tick
// therefore clears on its own.
func (m *Materializer) resetRefusals() {
	m.refusalMu.Lock()
	defer m.refusalMu.Unlock()
	m.refusals = nil
}

// SizeRefusals returns a copy of this tick's per-worker size refusals and PVC-create
// rejections, keyed by worker ID. Replaced on every Reconcile; display-only.
func (m *Materializer) SizeRefusals() map[string]reconcile.Refusal {
	m.refusalMu.Lock()
	defer m.refusalMu.Unlock()
	out := make(map[string]reconcile.Refusal, len(m.refusals))
	for id, r := range m.refusals {
		out[id] = r
	}
	return out
}

// checkSizeCeilings applies the tier ceilings to a resolved spec. Unset ceilings skip
// their check; an unparsable one refuses (config validated it at boot, so that is a
// programming or test-wiring error and must not silently pass).
func (m *Materializer) checkSizeCeilings(w protocol.DesiredWorker, spec preset.Spec) error {
	cfg := m.cfg
	maxCPU, maxMem, maxPVC := cfg.MaxCPU, cfg.MaxMemory, cfg.MaxPVCStorage
	tier := "restricted"
	switch {
	case w.Isolated:
		tier, maxCPU, maxMem, maxPVC = "isolated", cfg.IsolatedMaxCPU, cfg.IsolatedMaxMemory, ""
	case w.Docker:
		tier, maxCPU, maxMem, maxPVC = "docker", cfg.DockerMaxCPU, cfg.DockerMaxMemory, cfg.DockerMaxPVCStorage
	}
	checks := []struct {
		what  string
		got   resource.Quantity
		max   string
		apply bool
	}{
		{"cpu limit", spec.Size.CPULimit, maxCPU, true},
		{"memory limit", spec.Size.MemoryLimit, maxMem, true},
		// Ephemeral (run-bound) /data is the flat cluster size, not the preset's.
		{"data size", spec.Size.DataSize, maxPVC, !w.Ephemeral},
		// The cluster-wide DinD override wins over the preset, so it is not the size's.
		{"dind data size", spec.Size.DinDDataSize, maxPVC, withDinD(w) && cfg.DinDDataSize == ""},
	}
	for _, c := range checks {
		if !c.apply || c.max == "" {
			continue
		}
		limit, err := resource.ParseQuantity(c.max)
		if err != nil {
			return &RefusedError{Reason: fmt.Sprintf("size refused: %s tier max %q is not a valid quantity", tier, c.max)}
		}
		if c.got.Cmp(limit) > 0 {
			return &RefusedError{Reason: fmt.Sprintf("size refused: %s %s exceeds tier max %s", c.what, c.got.String(), limit.String())}
		}
	}
	return nil
}

// recordPVCRejection records a failed claim create as pvc_rejected, only when the cluster
// refused the claim at admission (Forbidden, Invalid, RequestEntityTooLarge); transient
// errors (timeouts, 429, 5xx) record nothing. The reason is the apiserver's StatusReason
// plus which claim, never the raw apiserver message, which can carry quota arithmetic and
// object names. For an admission-webhook denial the StatusReason can be webhook-authored
// text, so the api truncates and sanitizes it before display.
func (m *Materializer) recordPVCRejection(id, pvcName string, err error) {
	if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) && !apierrors.IsRequestEntityTooLargeError(err) {
		return
	}
	claim := "data"
	switch pvcName {
	case dindDataPVCName(id):
		claim = "dind"
	case nixPVCName(id):
		claim = "nix"
	}
	reason := string(apierrors.ReasonForError(err))
	if reason == "" {
		reason = "Unknown"
	}
	m.recordRefusal(id, protocol.BlockingKindPVCRejected, fmt.Sprintf("%s creating %s claim", reason, claim))
}
