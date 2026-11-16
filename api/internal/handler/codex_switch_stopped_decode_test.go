package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerStateStoppedStrictBody(t *testing.T) {
	valid := `{"status":"codex_switch_stopped","claim_generation":7,"register_nonce":"nonce","expected_epoch":41}`
	for _, body := range []string{
		valid + "{}",
		valid + " trailing",
		strings.TrimSuffix(valid, "}") + ",\"op_id\":null}",
		strings.Replace(valid, "41", "null", 1),
		strings.Replace(valid, "41", "0", 1),
		strings.Replace(valid, "41", "-1", 1),
		strings.Replace(valid, "41", "9007199254740992", 1),
		strings.Replace(valid, ":7", ":-1", 1),
		strings.Replace(valid, ":7", ":9007199254740992", 1),
		strings.Replace(valid, "expected_epoch", "EXPECTED_EPOCH", 1),
		"{\"status\":\"running\",\"EXPECTED_EPOCH\":null}",
		"{\"status\":\"paused\",\"Expected_Epoch\":41}",
		strings.Replace(valid, ":7", ":null", 1),
		strings.Replace(valid, "\"nonce\"", "null", 1),
		strings.Replace(valid, "codex_switch_stopped", "running", 1),
		"{\"status\":\"running\",\"expected_epoch\":null}",
		strings.TrimSuffix(valid, "}") + ",\"unknown\":true}",
		valid + strings.Repeat(" ", 1<<20),
		"null",
	} {
		rec := httptest.NewRecorder()
		h := newProtocolHandler(t, &protocolStore{})
		h.WorkerRunState(rec, workerReq(http.MethodPost, body, uuid.New()))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed request reached service: status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	for _, body := range []string{valid, valid + strings.Repeat(" ", (1<<20)-len(valid)), "{\"status\":\"running\"}"} {
		var req workersvc.StateRequest
		if err := decodeWorkerState(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), &req); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoppedSettlementAck(t *testing.T) {
	ack := map[string]any{"run": map[string]any{"status": "paused"}}
	stoppedSettlementAck(ack)
	wire, err := json.Marshal(ack)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Applied     bool   `json:"applied"`
		Disposition string `json:"disposition"`
		Run         struct {
			Status string `json:"status"`
		} `json:"run"`
	}
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Applied || got.Disposition != "codex_switch_stopped" || got.Run.Status != "paused" {
		t.Fatal("stopped success acknowledgement lost explicit disposition")
	}
}

func TestCodexPreflightReadyEpochWire(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		epoch   int64
		want    string
	}{
		{"ready", 41, "41"}, {"not_applied", 0, ""}, {"fenced", 0, ""},
	} {
		wire, err := json.Marshal(workersvc.CodexSwitchPreflightResult{
			Outcome: tc.outcome, ExpectedEpoch: tc.epoch, ClaimGeneration: 7, RegisterNonce: "nonce",
		})
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(wire, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["expected_epoch"]) != tc.want || string(fields["claim_generation"]) != "7" || string(fields["register_nonce"]) != `"nonce"` {
			t.Fatal("preflight epoch or claim wire shape mismatch")
		}
	}
}
