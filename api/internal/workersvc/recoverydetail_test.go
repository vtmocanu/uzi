package workersvc

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRecoveryDetail(t *testing.T) {
	adoption := strPtr(recoveryCauseRetainedRecoveryAdoptionFailed)
	req := func(state string, cause *string, gen *int64, detail string) StateRequest {
		return StateRequest{State: state, RecoveryCause: cause, ClaimGeneration: gen, RecoveryDetail: strPtr(detail)}
	}
	if err := validateRecoveryDetail(StateRequest{State: "recovery_wait"}); err != nil {
		t.Fatalf("absent detail must validate: %v", err)
	}
	for _, d := range []string{RecoveryDetailSecretRemediationActive, RecoveryDetailRemediationProofUnknown} {
		if err := validateRecoveryDetail(req("recovery_wait", adoption, i64Ptr(3), d)); err != nil {
			t.Fatalf("valid detail %q refused: %v", d, err)
		}
	}
	refusals := map[string]StateRequest{
		"non-enum":          req("recovery_wait", adoption, i64Ptr(3), "other"),
		"empty":             req("recovery_wait", adoption, i64Ptr(3), ""),
		"wrong case":        req("recovery_wait", adoption, i64Ptr(3), "Secret_Remediation_Active"),
		"exhausted cause":   req("recovery_wait", strPtr(recoveryCauseRetainedRecoveryExhausted), i64Ptr(3), RecoveryDetailSecretRemediationActive),
		"unwitnessed cause": req("recovery_wait", strPtr(recoveryCauseRetainedContinuationUnwitnessed), i64Ptr(3), RecoveryDetailSecretRemediationActive),
		"no cause":          req("recovery_wait", nil, i64Ptr(3), RecoveryDetailSecretRemediationActive),
		"running state":     req("running", adoption, i64Ptr(3), RecoveryDetailSecretRemediationActive),
		"limit_wait state":  req("limit_wait", adoption, i64Ptr(3), RecoveryDetailSecretRemediationActive),
		"nil generation":    req("recovery_wait", adoption, nil, RecoveryDetailSecretRemediationActive),
		"zero generation":   req("recovery_wait", adoption, i64Ptr(0), RecoveryDetailSecretRemediationActive),
		"negative gen":      req("recovery_wait", adoption, i64Ptr(-1), RecoveryDetailRemediationProofUnknown),
	}
	for name, r := range refusals {
		t.Run(name, func(t *testing.T) {
			err := validateRecoveryDetail(r)
			if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), "recovery_detail") {
				t.Fatalf("err = %v, want ErrInvalidState naming recovery_detail", err)
			}
		})
	}
}
