package handler

// PRD #1391 M5 — handler-level (HTTP round-trip) coverage for the two wire values the
// M5 agent negotiates against: the register response's protocol_features union, and the
// worker DTO's overlaid outbox_* fields. worker_outbox_test.go covers the pure
// parseWorkerOutbox; these drive the real handlers through the shared protocol harness
// (newProtocolHandler / workerReq / protocolStore, defined in worker_protocol_test.go).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestWorkerRegisterAdvertisesHeartbeatOutboxFeature pins the negotiated wire value the
// M2 agent gates its heartbeat outbox field on: the register response's protocol_features
// MUST contain "heartbeat_outbox" and MUST NOT contain the fences that belong to later
// runs. Dropping "heartbeat_outbox" from workerProtocolFeatures' union fails the first
// check; advertising "claim_generation_fence" or "terminal_fence" early fails the second.
func TestWorkerRegisterAdvertisesHeartbeatOutboxFeature(t *testing.T) {
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3"}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	var resp struct {
		ProtocolFeatures []string `json:"protocol_features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode register response: %v (body %q)", err, rec.Body.String())
	}
	has := func(want string) bool {
		for _, f := range resp.ProtocolFeatures {
			if f == want {
				return true
			}
		}
		return false
	}
	if !has("heartbeat_outbox") {
		t.Fatalf("protocol_features = %v, must contain heartbeat_outbox (the M2 agent gates its heartbeat outbox field on it)", resp.ProtocolFeatures)
	}
	// No fences in Run A: these belong to #1247/#1390 and Run B, land later, and
	// advertising one now would tell the worker to send a fence a current api rejects.
	for _, forbidden := range []string{"claim_generation_fence", "terminal_fence"} {
		if has(forbidden) {
			t.Fatalf("protocol_features = %v, must NOT contain %q in Run A", resp.ProtocolFeatures, forbidden)
		}
	}
}

// TestWorkerHeartbeatOverlaysOutboxOntoDTO pins that a heartbeat reporting a non-zero
// outbox depth reaches the returned WorkerDTO via overlayOutbox: the depth is recorded
// in the tracker for this worker (Heartbeat) and overlaid onto the response DTO for the
// same worker. Disabling overlayOutbox (making it a no-op) leaves the four fields null
// and fails this test.
func TestWorkerHeartbeatOverlaysOutboxOntoDTO(t *testing.T) {
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	runID := uuid.New()
	body := `{"version":"1","outbox":[{"run_id":"` + runID.String() +
		`","pending_messages":5,"pending_terminal":0,"stale_retired":2,"since":1}]}`
	h.WorkerHeartbeat(rec, workerReq(http.MethodPost, body, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Worker struct {
			OutboxPendingMessages *int `json:"outbox_pending_messages"`
			OutboxPendingTerminal *int `json:"outbox_pending_terminal"`
			OutboxStaleRetired    *int `json:"outbox_stale_retired"`
		} `json:"worker"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode heartbeat response: %v (body %q)", err, rec.Body.String())
	}
	if resp.Worker.OutboxPendingMessages == nil || *resp.Worker.OutboxPendingMessages != 5 {
		t.Fatalf("outbox_pending_messages = %v, want 5 (the overlay must carry the reported depth onto the DTO)", resp.Worker.OutboxPendingMessages)
	}
	if resp.Worker.OutboxStaleRetired == nil || *resp.Worker.OutboxStaleRetired != 2 {
		t.Fatalf("outbox_stale_retired = %v, want 2", resp.Worker.OutboxStaleRetired)
	}
	if resp.Worker.OutboxPendingTerminal == nil || *resp.Worker.OutboxPendingTerminal != 0 {
		t.Fatalf("outbox_pending_terminal = %v, want 0 (set together the moment any component is non-zero)", resp.Worker.OutboxPendingTerminal)
	}
}

// TestWorkerHeartbeatWithoutOutboxLeavesDTONull is the control: a heartbeat with no
// outbox report leaves the DTO's outbox_* fields null, so the overlay test above proves
// the overlay, not an always-set field.
func TestWorkerHeartbeatWithoutOutboxLeavesDTONull(t *testing.T) {
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerHeartbeat(rec, workerReq(http.MethodPost, `{"version":"1"}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"outbox_pending_messages":null`) {
		t.Fatalf("no outbox report must leave outbox_pending_messages null, got %q", rec.Body.String())
	}
}
