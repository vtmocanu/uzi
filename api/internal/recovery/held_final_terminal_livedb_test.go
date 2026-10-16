package recovery

import (
	"github.com/vtmocanu/uzi/api/internal/capability"
	"testing"
)

func TestHeldFinalTerminalV2LiveDB(t *testing.T) {
	for _, status := range []string{"failed", "cancelled", "completed"} {
		for _, gen := range []int{1, 2} {
			t.Run(status+map[int]string{1: "/current", 2: "/older"}[gen], func(t *testing.T) {
				e := newHeldEnv(t)
				e.w.ProtocolCapabilities = []string{capability.RecoveryInventoryV1, capability.RecoveryHeldPublicationV1, capability.RecoveryHeldPublicationV2}
				e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.w.ID, e.w.ProtocolCapabilities)
				e.exec("UPDATE runs SET status=$2,claim_generation=$3,worker_id=NULL WHERE id=$1", e.run, status, gen)
				e.mustRelease()
				// Immutable FINAL receipt remains replayable after capabilities and switch disappear.
				e.exec("UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", e.w.ID)
				e.w.ProtocolCapabilities = nil
				e.svc.SetHeldPublication(false, nil)
				e.mustRelease()
			})
		}
	}
}
