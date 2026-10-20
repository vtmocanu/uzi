package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestWorkerPausedOriginReplayLiveDB exercises worker ingest, jsonb storage and
// authenticated tail replay. Execution belongs to the LiveDB validation sweep.
func TestWorkerPausedOriginReplayLiveDB(t *testing.T) {
	fixture := newPoisonFixture(t)
	before := time.Now().Add(-time.Second)
	body := []byte(`{"messages":[
 {"seq":1,"kind":"paused","payload":{"origin":"lead_park","completed":2,"total":4}},
 {"seq":2,"kind":"paused","payload":{"origin":"owner","completed":3,"total":4}},
 {"seq":3,"kind":"paused","payload":{"completed":3,"total":4}}
 ]}`)
	posted := fixture.post(t, body)
	if posted.Code < 200 || posted.Code >= 300 {
		t.Fatalf("worker ingest: %d %s", posted.Code, posted.Body.String())
	}
	for _, viewer := range []store.User{
		{ID: fixture.wkr.UserID, IsActive: true},
		{ID: uuid.New(), IsAdmin: true, IsActive: true},
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/runs/"+fixture.runID.String()+"/messages?tail=100", nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("id", fixture.runID.String())
		ctx := context.WithValue(mw.ContextWithUser(request.Context(), viewer), chi.RouteCtxKey, route)
		response := httptest.NewRecorder()
		fixture.h.ListRunMessages(response, request.WithContext(ctx))
		if response.Code != http.StatusOK {
			t.Fatalf("replay admin=%v: %d %s", viewer.IsAdmin, response.Code, response.Body.String())
		}
		var replay struct {
			Messages []apitypes.MessageDTO `json:"messages"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &replay); err != nil {
			t.Fatal(err)
		}
		if len(replay.Messages) != 3 {
			t.Fatalf("replay: %+v", replay.Messages)
		}
		for i, message := range replay.Messages {
			if message.Seq != int32(i+1) || message.Kind != "paused" || message.CreatedAt.IsZero() || message.CreatedAt.Before(before) || message.CreatedAt.After(time.Now().Add(time.Second)) {
				t.Fatalf("invalid persisted marker: %+v", message)
			}
			var payload struct {
				Origin    *string `json:"origin"`
				Completed int     `json:"completed"`
				Total     int     `json:"total"`
			}
			if err := json.Unmarshal(message.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Total != 4 || payload.Completed != []int{2, 3, 3}[i] {
				t.Fatalf("payload semantics: %+v", payload)
			}
			if i < 2 && (payload.Origin == nil || *payload.Origin != []string{"lead_park", "owner"}[i]) {
				t.Fatalf("origin: %+v", payload)
			}
			if i == 2 && payload.Origin != nil {
				t.Fatalf("legacy origin-less payload changed: %+v", payload)
			}
		}
	}
}
