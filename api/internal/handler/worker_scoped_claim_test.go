package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestWorkerScopedClaimRegistration(t *testing.T) {
	st := &protocolStore{}
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"version":"test","template":"base","protocol_capabilities":["pending_outcome_scoped_claim_v1","provider_policy_hold_v1"]}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if !slices.Contains(st.registerArg.ProtocolCapabilities, "pending_outcome_scoped_claim_v1") {
		t.Fatal("registration lost scoped capability")
	}
	var response struct {
		Features []string `json:"protocol_features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(response.Features, "pending_outcome_scoped_claim_v1") || !slices.Contains(response.Features, "provider_policy_hold_v1") {
		t.Fatalf("readiness negotiation: %v", response.Features)
	}
}

func TestWorkerScopedClaimRawOptionalGroup(t *testing.T) {
	for _, group := range []string{`[{"run_id":"11111111-1742-4000-8000-000000000002","owner_generation":9}]`, `{"malformed":true}`, `[null]`} {
		h := newProtocolHandler(t, &protocolStore{claimErr: pgx.ErrNoRows})
		rec := httptest.NewRecorder()
		h.WorkerClaim(rec, workerReq(http.MethodPost, `{"active_snapshot":{"snapshot_epoch":1,"register_nonce":"n","active":[],"pending_overflow":true,"pending_all":[],"pending_policy_protection":`+group+`}}`, uuid.Nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("optional group poisoned claim body: %d %s", rec.Code, rec.Body.String())
		}
	}
}

// DecodeJSON rejects unknown outer fields, but snapshots are isolated RawMessage
// with their own lenient UnmarshalJSON. New nested fields must not 400 old routes.
func TestWorkerUnknownNestedSnapshotField(t *testing.T) {
	for _, lane := range []string{"claim", "heartbeat"} {
		t.Run(lane, func(t *testing.T) {
			h := newProtocolHandler(t, &protocolStore{claimErr: pgx.ErrNoRows})
			rec := httptest.NewRecorder()
			const snapshot = `{"snapshot_epoch":1,"register_nonce":"n","active":[],"pending_overflow":true,"future_snapshot_field":{"ignored":true}}`
			if parsed := parseActiveSnapshot(json.RawMessage(snapshot), uuid.Nil); parsed == nil || parsed.SnapshotEpoch != 1 || !parsed.PendingOverflow {
				t.Fatal("unknown nested field must preserve the snapshot, not drop it to a bodyless fallback")
			}
			req := workerReq(http.MethodPost, `{"active_snapshot":`+snapshot+`}`, uuid.Nil)
			if lane == "claim" {
				h.WorkerClaim(rec, req)
			} else {
				h.WorkerHeartbeat(rec, req)
			}
			want := http.StatusNoContent
			if lane == "heartbeat" {
				want = http.StatusOK
			}
			if rec.Code != want {
				t.Fatalf("unknown nested field poisoned %s: %d %s", lane, rec.Code, rec.Body.String())
			}
		})
	}
}
