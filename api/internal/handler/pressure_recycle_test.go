package handler

import (
	"context"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type pressurePollStore struct {
	*pollStore
	calls int
}

func (s *pressurePollStore) CordonHostedWorkerPressureRecycle(_ context.Context, _ uuid.UUID) (int64, error) {
	s.calls++
	return 1, nil
}
func TestControllerPressureReason(t *testing.T) {
	s := &pressurePollStore{pollStore: &pollStore{}}
	router := controllerRoutes(newControllerHandler(t, s, true))
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"reason":"pressure_recycle"}`, 204},
		{`{"reason":"unknown"}`, 400},
		{`{"reason":"pressure_recycle","extra":true}`, 400},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/controller/workers/"+uuid.NewString()+"/drain", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+testControllerToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	// A store without a transaction source must not use the generic release channel.
	req := httptest.NewRequest(http.MethodDelete, "/api/controller/workers/"+uuid.NewString()+"/drain", strings.NewReader(`{"reason":"pressure_recycle"}`))
	req.Header.Set("Authorization", "Bearer "+testControllerToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || len(s.uncordoned) != 0 {
		t.Fatalf("transaction-less pressure release status=%d", rec.Code)
	}
	if s.calls != 1 || len(s.cordoned) != 0 {
		t.Fatal("pressure route used generic cordon")
	}
}
