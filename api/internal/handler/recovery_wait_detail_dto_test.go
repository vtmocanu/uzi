package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRecoveryWaitDetailDTOGate proves the detail reaches the wire only for a recovery_wait run
// parked at retained_recovery_adoption_failed with a closed-set value (issue #4177).
func TestRecoveryWaitDetailDTOGate(t *testing.T) {
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	cases := []struct {
		name string
		run  store.Run
		want string // "" = absent
	}{
		{"active", store.Run{Status: "recovery_wait", RecoveryWaitCause: txt("retained_recovery_adoption_failed"), RecoveryWaitDetail: txt("secret_remediation_active")}, "secret_remediation_active"},
		{"unknown proof", store.Run{Status: "recovery_wait", RecoveryWaitCause: txt("retained_recovery_adoption_failed"), RecoveryWaitDetail: txt("remediation_proof_unknown")}, "remediation_proof_unknown"},
		{"null detail", store.Run{Status: "recovery_wait", RecoveryWaitCause: txt("retained_recovery_adoption_failed")}, ""},
		{"non-enum detail", store.Run{Status: "recovery_wait", RecoveryWaitCause: txt("retained_recovery_adoption_failed"), RecoveryWaitDetail: txt("leak-me")}, ""},
		{"other cause", store.Run{Status: "recovery_wait", RecoveryWaitCause: txt("retained_recovery_exhausted"), RecoveryWaitDetail: txt("secret_remediation_active")}, ""},
		{"no cause", store.Run{Status: "recovery_wait", RecoveryWaitDetail: txt("secret_remediation_active")}, ""},
		{"stale on queued run", store.Run{Status: "queued", RecoveryWaitCause: txt("retained_recovery_adoption_failed"), RecoveryWaitDetail: txt("secret_remediation_active")}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dto := runToDTO(tc.run, "normal", 0, 0, 0, dtoTestNow, 3)
			raw, err := json.Marshal(dto)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if dto.RecoveryWaitDetail != nil || strings.Contains(string(raw), "recovery_wait_detail") || strings.Contains(string(raw), "leak-me") {
					t.Fatalf("detail leaked: %s", raw)
				}
				return
			}
			if dto.RecoveryWaitDetail == nil || *dto.RecoveryWaitDetail != tc.want {
				t.Fatalf("detail = %v, want %q", dto.RecoveryWaitDetail, tc.want)
			}
		})
	}
}
