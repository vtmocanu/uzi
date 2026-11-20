package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

type switchStampRow struct{ requestedAt, opID, target string }

// switchStamp reads the run's switch stamp columns as text ("" when NULL).
func switchStamp(t *testing.T, f applyFx, run uuid.UUID) switchStampRow {
	t.Helper()
	var s switchStampRow
	if err := f.env.pool.QueryRow(f.env.ctx, "SELECT COALESCE(credential_switch_requested_at::text,''), COALESCE(credential_switch_op_id::text,''), COALESCE(codex_switch_target_secret_id::text,'') FROM runs WHERE id=$1", run).Scan(&s.requestedAt, &s.opID, &s.target); err != nil {
		t.Fatal(err)
	}
	return s
}

// An owner Codex switch request on a worker that cannot apply it is refused as
// unsupported and leaves no switch stamp; the same request on a supported worker succeeds.
func TestOwnerCodexSwitchUnsupportedWorkerLiveDB(t *testing.T) {
	cases := []struct {
		name        string
		mutate      string
		unsupported bool
	}{
		{"supported", "", false},
		{"missing_switch_capability", "UPDATE workers SET protocol_capabilities=ARRAY['codex_switch_stopped_v1']::text[] WHERE id=$1", true},
		{"stale_apply_registration", "UPDATE workers SET credential_switch_apply_nonce=credential_switch_apply_nonce || '-stale' WHERE id=$1", true},
		{"missing_stopped_capability", "UPDATE workers SET protocol_capabilities=ARRAY['" + capability.CodexCredentialSwitchV1 + "']::text[] WHERE id=$1", true},
		{"stale_heartbeat", "UPDATE workers SET last_heartbeat_at=now() - interval '1 day' WHERE id=$1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, run, target, _ := codexUnrequestedFixture(t, false, false, false)
			// Fresh heartbeat so only the mutated property can make the worker unsupported.
			f.env.exec("UPDATE workers SET last_heartbeat_at=now() WHERE id=$1", f.o.workerID)
			if c.mutate != "" {
				f.env.exec(c.mutate, f.o.workerID)
			}
			before := switchStamp(t, f, run)
			_, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &target)
			if !c.unsupported {
				if err != nil {
					t.Fatalf("%s: owner request on a supported worker failed: %v", c.name, err)
				}
				if after := switchStamp(t, f, run); after.target == "" || after == before {
					t.Fatalf("%s: supported request did not write the switch stamp: %q", c.name, after)
				}
				return
			}
			var unsupported *CredentialSwitchWorkerUnsupportedError
			if !errors.As(err, &unsupported) {
				t.Fatalf("%s: owner request on an unsupported worker was not refused as unsupported: %v", c.name, err)
			}
			after := switchStamp(t, f, run)
			if after != before {
				t.Fatalf("%s: refused request changed the switch stamp: before=%q after=%q", c.name, before, after)
			}
			if after.target != "" {
				t.Fatalf("%s: refused request wrote a Codex switch target: %q", c.name, after.target)
			}
		})
	}
}
