package workersvc

import (
	"errors"
	"testing"
)

func TestIdleStallReportValidationAndTransaction(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		generation           *int64
		progress, productive *int32
		invalid              bool
	}{
		{"missing", nil, nil, nil, true}, {"zero", i64Ptr(0), nil, nil, true},
		{"negative", i64Ptr(-1), nil, nil, true}, {"negative progress", i64Ptr(1), i32Ptr(-1), nil, true},
		{"productive forbidden", i64Ptr(1), nil, i32Ptr(1), true},
		{"zero progress", i64Ptr(1), i32Ptr(0), nil, false}, {"positive progress", i64Ptr(1), i32Ptr(2), nil, false},
		{"no progress", i64Ptr(1), nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := recoveryRun(0)
			fs, svc, w := limitParkFixture(t, run)
			_, applied, err := svc.SetState(t.Context(), w, run.ID, StateRequest{
				State: "recovery_wait", RecoveryCause: strPtr("codex_idle_stall"),
				ClaimGeneration: tc.generation, ProviderProgressTurns: tc.progress, ProductiveTurns: tc.productive,
			})
			if applied || err == nil || fs.setRecoveryWait != nil {
				t.Fatalf("applied=%v err=%v write=%v", applied, err, fs.setRecoveryWait)
			}
			if errors.Is(err, ErrInvalidState) != tc.invalid {
				t.Fatalf("invalid=%v err=%v", tc.invalid, err)
			}
		})
	}
}
