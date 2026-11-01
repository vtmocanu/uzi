package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestIncompleteInventoryClaim(t *testing.T) {
	for _, name := range []string{"valid", "absent", "false", "null", "malformed", "complete empty", "complete group", "complete valid proof", "malformed pending", "malformed group", "no overflow", "zero capacity", "no locked capability", "stale nonce", "equal epoch"} {
		t.Run(name, func(t *testing.T) {
			svc, tx, source := scopedClaimFixture(t)
			input := tx.worker // The request may still carry a pre-lock capability/epoch.
			marker := `,"pending_inventory_incomplete":true`
			extra := ""
			overflow := true
			switch name {
			case "absent":
				marker = ""
			case "false":
				marker = `,"pending_inventory_incomplete":false`
			case "null":
				marker = `,"pending_inventory_incomplete":null`
			case "malformed":
				marker = `,"pending_inventory_incomplete":"true"`
			case "complete empty":
				extra = `,"pending_all":[]`
			case "complete group":
				extra = `,"pending_policy_protection":[]`
			case "complete valid proof":
				extra = fmt.Sprintf(`,"pending_all":[{"run_id":%q,"claim_generation":7}],"pending_policy_protection":[{"run_id":%q,"owner_generation":9}]`, source.String(), source.String())
			case "malformed pending":
				extra = `,"pending_all":{}`
			case "malformed group":
				extra = `,"pending_policy_protection":{}`
			case "no overflow":
				overflow = false
			case "zero capacity":
				svc.p.WorkerOutboxMaxPending = 0
			case "no locked capability":
				tx.worker.ProtocolCapabilities = nil
			case "equal epoch":
				tx.worker.SnapshotEpoch = 1
			}
			var snap ActiveSnapshot
			raw := fmt.Sprintf(`{"snapshot_epoch":1,"register_nonce":"scoped","active":[],"pending_overflow":%t%s%s}`, overflow, marker, extra)
			if err := json.Unmarshal([]byte(raw), &snap); err != nil {
				t.Fatal(err)
			}
			if name == "stale nonce" {
				snap.RegisterNonce = "stale"
			}
			payload, err := svc.Claim(context.Background(), input, &snap)
			if name == "stale nonce" || name == "equal epoch" {
				if !errors.Is(err, ErrActiveSnapshotInvalid) || tx.claims != 0 {
					t.Fatalf("invalid snapshot escaped fence: err=%v claims=%d", err, tx.claims)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			allow := name == "valid" || name == "no overflow"
			if (payload != nil) != allow {
				t.Fatalf("payload=%v want allowed=%v", payload != nil, allow)
			}
			if len(tx.identityWrites) != 0 {
				t.Fatalf("incomplete evidence invented complete identity writes: %v", tx.identityWrites)
			}
		})
	}
}
