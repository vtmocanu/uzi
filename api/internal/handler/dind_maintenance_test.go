package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func dindHandlerBinding() workersvc.DindMaintenance {
	return workersvc.DindMaintenance{
		ID: uuid.New().String(), Nonce: "maintenance-nonce", Phase: "requested",
		DeploymentUID: "deployment-uid", PVCUID: "pvc-uid", RegisterNonce: "registration-nonce",
	}
}

func dindHandlerJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(body)
}

func TestControllerDindMaintenanceRequiresControllerBearer(t *testing.T) {
	for _, auth := range []struct{ name, header string }{
		{"missing", ""},
		{"invalid", "Bearer uzc_wrong"},
		{"worker credential", "Bearer test-worker-credential"},
	} {
		t.Run(auth.name, func(t *testing.T) {
			// No service is installed: a missing controller guard would reach it.
			h := newControllerHandler(t, nil, true)
			req := httptest.NewRequest(http.MethodPost,
				"/api/controller/workers/"+uuid.New().String()+"/dind-maintenance",
				strings.NewReader(dindHandlerJSON(t, dindHandlerBinding())))
			req.Header.Set("Authorization", auth.header)
			rec := httptest.NewRecorder()
			controllerRoutes(h).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestControllerDindMaintenanceWireAndServiceConflict(t *testing.T) {
	valid := dindHandlerJSON(t, dindHandlerBinding())
	badPhase := dindHandlerBinding()
	badPhase.Phase = "unsupported"
	for _, tc := range []struct {
		name, workerID, body string
		want                 int
	}{
		{"valid binding reaches service", uuid.New().String(), valid, http.StatusConflict},
		{"unsupported phase fails closed", uuid.New().String(), dindHandlerJSON(t, badPhase), http.StatusConflict},
		{"missing binding fails closed", uuid.New().String(), "{}", http.StatusConflict},
		{"malformed worker ID", "not-a-uuid", valid, http.StatusBadRequest},
		{"empty JSON", uuid.New().String(), "", http.StatusBadRequest},
		{"malformed JSON", uuid.New().String(), "{", http.StatusBadRequest},
		{"unknown field", uuid.New().String(), `{"unexpected":true}`, http.StatusBadRequest},
		{"wrong phase type", uuid.New().String(), `{"phase":42}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newControllerHandler(t, nil, true)
			h.wsvc = newProtocolHandler(t, &protocolStore{}).wsvc
			req := httptest.NewRequest(http.MethodPost,
				"/api/controller/workers/"+tc.workerID+"/dind-maintenance", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+testControllerToken)
			rec := httptest.NewRecorder()
			controllerRoutes(h).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusConflict && !strings.Contains(rec.Body.String(), "maintenance precondition failed") {
				t.Fatalf("want service precondition conflict, got %s", rec.Body.String())
			}
		})
	}
}

// This fixture reuses the heartbeat fake and adds only the token lookup seam.
// It deliberately has no TxBeginner: these are HTTP/auth tests, not SQL authority tests.
type dindHandlerStore struct {
	protocolStore
	worker store.Worker
}

func (s *dindHandlerStore) GetWorkerByTokenHash(_ context.Context, hash []byte) (store.Worker, error) {
	if !jointoken.Equal(hash, s.worker.TokenHash) {
		return store.Worker{}, pgx.ErrNoRows
	}
	return s.worker, nil
}

func TestControllerDindMaintenanceWithoutTransactionsCannotAuthorizeWorkers(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		ephemeral  bool
		docker     bool
	}{
		{"external", "external", false, true},
		{"ephemeral", "hosted", true, true},
		{"plain", "hosted", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &dindHandlerStore{worker: store.Worker{
				ID: uuid.New(), Kind: tc.kind, Ephemeral: tc.ephemeral,
				DockerEnabled: pgtype.Bool{Bool: tc.docker, Valid: true},
			}}
			h := newControllerHandler(t, nil, true)
			h.wsvc = newProtocolHandler(t, st).wsvc
			req := httptest.NewRequest(http.MethodPost,
				"/api/controller/workers/"+st.worker.ID.String()+"/dind-maintenance",
				strings.NewReader(dindHandlerJSON(t, dindHandlerBinding())))
			req.Header.Set("Authorization", "Bearer "+testControllerToken)
			rec := httptest.NewRecorder()
			controllerRoutes(h).ServeHTTP(rec, req)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestWorkerDindMaintenanceACKAuthWireAndFailClosed(t *testing.T) {
	const token = "test-dind-worker-credential"
	zero := 0
	ack := workersvc.DindMaintenanceReadyACK{
		DindMaintenance: dindHandlerBinding(), LocalClaims: &zero, LocalExecutions: &zero, CustodyClear: true,
	}
	ack.Phase = "ready"
	valid := dindHandlerJSON(t, map[string]any{"version": "1", "dind_maintenance_ready_ack": ack})
	foreign := ack
	foreign.DeploymentUID = "another-worker-deployment"
	foreign.PVCUID = "another-worker-pvc"
	foreign.RegisterNonce = "another-worker-registration"
	for _, tc := range []struct {
		name, auth, body string
		unsupported      bool
		want             int
	}{
		{"missing bearer", "", valid, false, http.StatusUnauthorized},
		{"invalid bearer", "Bearer wrong-worker", valid, false, http.StatusUnauthorized},
		{"controller bearer", "Bearer " + testControllerToken, valid, false, http.StatusUnauthorized},
		{"unknown operation", "Bearer " + token, valid, false, http.StatusConflict},
		{"foreign binding", "Bearer " + token, dindHandlerJSON(t, map[string]any{"dind_maintenance_ready_ack": foreign}), false, http.StatusConflict},
		{"unsupported worker", "Bearer " + token, valid, true, http.StatusConflict},
		{"ACK omitted", "Bearer " + token, `{"version":"1"}`, false, http.StatusOK},
		{"malformed JSON", "Bearer " + token, "{", false, http.StatusBadRequest},
		{"unknown ACK field", "Bearer " + token, `{"dind_maintenance_ready_ack":{"worker_id":"foreign"}}`, false, http.StatusBadRequest},
		{"wrong count type", "Bearer " + token, `{"dind_maintenance_ready_ack":{"local_claims":"0"}}`, false, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &dindHandlerStore{worker: store.Worker{
				ID: uuid.New(), UserID: uuid.New(), TokenHash: jointoken.Hash(token), Kind: "hosted",
				DockerEnabled:        pgtype.Bool{Bool: true, Valid: true},
				ProtocolCapabilities: []string{capability.DindMaintenanceV1},
			}}
			if tc.unsupported {
				st.worker.Kind = "external"
				st.worker.ProtocolCapabilities = nil
			}
			h := newProtocolHandler(t, st)
			// Exercise RequireWorker directly with its narrow store seam; the full
			// router takes concrete Queries and is not backed by this unit fixture.
			endpoint := mw.RequireWorker(st)(http.HandlerFunc(h.WorkerHeartbeat))
			req := httptest.NewRequest(http.MethodPost, "/api/worker/heartbeat", strings.NewReader(tc.body))
			req.Header.Set("Authorization", tc.auth)
			rec := httptest.NewRecorder()
			endpoint.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusConflict || tc.want == http.StatusOK {
				if st.heartbeatArg.ID != st.worker.ID || !st.custodyCalled {
					t.Fatalf("heartbeat must use token identity: got %s, want %s; custody called = %v",
						st.heartbeatArg.ID, st.worker.ID, st.custodyCalled)
				}
			} else if st.heartbeatArg.ID != uuid.Nil || st.custodyCalled {
				t.Fatal("rejected authentication or wire body reached heartbeat persistence")
			}
		})
	}
}
