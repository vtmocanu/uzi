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
	if !slices.Contains(response.Features, "pending_outcome_scoped_claim_v1") || slices.Contains(response.Features, "provider_policy_hold_v1") {
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
