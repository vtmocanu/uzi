package handler

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4205 M1: both DTO builders pass HibernationState into the Go intake
// mirror, so a non-awake worker reports effective_cordoned.
func TestWorkerDTOEffectiveCordonedHibernation(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		state string
		want  bool
	}{
		{"none", false}, {"preparing", true}, {"hibernated", true}, {"waking", true},
	} {
		t.Run(tc.state, func(t *testing.T) {
			w := store.Worker{HibernationState: tc.state, MaintenancePhase: "none"}
			if got := workerDTOFromWorker(w, 0, false, "", "", "", now, now).EffectiveCordoned; got != tc.want {
				t.Errorf("workerDTOFromWorker EffectiveCordoned=%v want %v", got, tc.want)
			}
			row := store.ListWorkersByUserRow{HibernationState: tc.state, MaintenancePhase: "none"}
			if got := workerDTOFromRow(row, "", "", now, now).EffectiveCordoned; got != tc.want {
				t.Errorf("workerDTOFromRow EffectiveCordoned=%v want %v", got, tc.want)
			}
		})
	}
}
