package handler

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestWorkerHeartbeatRefreshNegotiationAndSoftInput(t *testing.T) {
	nonce := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for _, disabled := range []bool{false, true} {
		for _, held := range []bool{false, true} {
			for _, refresh := range []string{
				"", "null", "[]", "{}", `{"protocol_capabilities":null}`,
				`{"register_nonce":"` + nonce + `","protocol_capabilities":[],"max_concurrent_runs":1,"max_cross_check_slots":0}`,
			} {
				st := &protocolStore{}
				h := newProtocolHandler(t, st)
				h.cfg.ActiveSnapshotDisabled = disabled
				h.cfg.HeldPublication = held
				h.cfg.WorkerOutboxMaxPending = 37
				body := "{}"
				if refresh != "" {
					body = `{"protocol_refresh":` + refresh + "}"
				}
				rec := httptest.NewRecorder()
				h.WorkerHeartbeat(rec, workerReq(http.MethodPost, body, uuid.New()))
				if rec.Code != http.StatusOK {
					t.Fatalf("heartbeat %s: %d %s", body, rec.Code, rec.Body.String())
				}
				var response struct {
					Features []string `json:"protocol_features"`
					Quota    int      `json:"worker_outbox_max_pending"`
					Nonce    *string  `json:"register_nonce"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(response.Features, protocolFeatures(!disabled, held)) ||
					!slices.Contains(response.Features, "heartbeat_protocol_refresh_v1") || response.Quota != 37 || response.Nonce != nil {
					t.Fatalf("heartbeat negotiation = %+v", response)
				}
				// The fake has no transaction beginner. Valid refresh also must
				// continue plain liveness without invoking register or a racy refresh.
				if st.heartbeatArg.ID == uuid.Nil || st.registerArg.ID != uuid.Nil {
					t.Fatal("liveness skipped or register called")
				}
			}
		}
	}
}
