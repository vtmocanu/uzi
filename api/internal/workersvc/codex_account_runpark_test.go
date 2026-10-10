package workersvc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

func TestSetStateCodexAccountParkMissingGeneration(t *testing.T) {
	for _, caps := range []struct {
		name        string
		values      []string
		wantMessage string
	}{
		{"account-park", []string{capability.CodexAccountParkV1}, "account park requires claim_generation"},
		{"account-park-and-credential-switch", []string{capability.CodexAccountParkV1, capability.CredentialSwitchV1}, "account park requires claim_generation"},
		{"credential-switch-only", []string{capability.CredentialSwitchV1}, "codex account park capability required"},
	} {
		t.Run(caps.name, func(t *testing.T) {
			run := runningRun(false)
			fs, svc, wkr := limitParkFixture(t, run)
			gate := &accountGateStore{fakeStore: fs}
			svc.q = gate
			wkr.ProtocolCapabilities = caps.values
			before := fs.runOwned
			_, applied, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
				State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseCodexAccountUnavailable),
			})
			if !errors.Is(err, ErrInvalidState) || !strings.Contains(err.Error(), caps.wantMessage) || applied ||
				gate.reads != 0 || fs.setRecoveryWait != nil || fs.setFailed != nil || !reflect.DeepEqual(before, fs.runOwned) {
				t.Fatalf("missing generation: applied=%v err=%v ownership reads=%d writes=%v/%v", applied, err, gate.reads, fs.setRecoveryWait, fs.setFailed)
			}
		})
	}
}
