package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestTransitionDinDReadyRefreshReturnsServerBinding(t *testing.T) {
	request := protocol.DindMaintenance{
		ID:            "operation",
		DeploymentUID: "deployment",
		PVCUID:        "pvc",
		Phase:         "ready",
		Nonce:         "old-operation-nonce",
		RegisterNonce: "old-registration-nonce",
		Fenced:        true,
		ReadyACK:      false,
		Reason:        "waiting for registration binding refresh",
	}
	nonceOnly := request
	nonceOnly.Nonce = "fresh-operation-nonce"
	nonceOnly.ReadyACK = true
	nonceOnly.Reason = "operation nonce refreshed"
	registrationOnly := request
	registrationOnly.RegisterNonce = "fresh-registration-nonce"
	registrationOnly.ReadyACK = true
	registrationOnly.Reason = "registration nonce refreshed"
	both := request
	both.Nonce = "fresh-operation-nonce"
	both.RegisterNonce = "fresh-registration-nonce"
	both.ReadyACK = true
	both.Reason = "registration binding refreshed after legacy roll"

	stoppingRequest := request
	stoppingRequest.Phase = "stopping"
	stoppingResponse := both
	stoppingResponse.Phase = "stopping"

	// Ready may refresh the server binding; stopping must preserve both nonces.
	tests := []struct {
		name     string
		request  protocol.DindMaintenance
		response protocol.DindMaintenance
		want     protocol.DindMaintenance
		wantErr  bool
	}{
		{
			name: "unchanged", request: request, response: request,
			want: request, wantErr: false,
		},
		{
			name: "nonce-only refresh", request: request, response: nonceOnly,
			want: nonceOnly, wantErr: false,
		},
		{
			name: "registration-only refresh", request: request, response: registrationOnly,
			want: registrationOnly, wantErr: false,
		},
		{
			name: "both refreshed", request: request, response: both,
			want: both, wantErr: false,
		},
		{
			name: "stopping refresh rejected", request: stoppingRequest, response: stoppingResponse,
			want: protocol.DindMaintenance{}, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if err := json.NewEncoder(w).Encode(tt.response); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer server.Close()
			client := New(server.URL, "token", time.Second, nil, nil)
			got, err := client.TransitionDinD(context.Background(), "worker", tt.request)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("error present = %t, want %t (error: %v)", gotErr, tt.wantErr, err)
			}
			if got != tt.want {
				t.Errorf("returned binding = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestTransitionDinDPhaseFenceRequirements(t *testing.T) {
	ready := protocol.DindMaintenance{
		ID:            "operation",
		DeploymentUID: "deployment",
		PVCUID:        "pvc",
		Phase:         "ready",
		Nonce:         "operation-nonce",
		RegisterNonce: "registration-nonce",
		Fenced:        true,
		ReadyACK:      true,
		Reason:        "registration binding confirmed",
	}
	complete := ready
	complete.Phase = "complete"
	complete.Fenced = false
	cancelled := ready
	cancelled.Phase = "cancelled"
	cancelled.Fenced = false

	// Requests retain valid fences; only the response fence crosses the boundary.
	tests := []struct {
		name           string
		request        protocol.DindMaintenance
		responseFenced bool
		want           protocol.DindMaintenance
		wantErr        bool
	}{
		{
			name: "ready fenced", request: ready, responseFenced: true,
			want: ready, wantErr: false,
		},
		{
			name: "ready unfenced", request: ready, responseFenced: false,
			want: protocol.DindMaintenance{}, wantErr: true,
		},
		{
			name: "complete fenced", request: complete, responseFenced: true,
			want: protocol.DindMaintenance{}, wantErr: true,
		},
		{
			name: "complete unfenced", request: complete, responseFenced: false,
			want: complete, wantErr: false,
		},
		{
			name: "cancelled fenced", request: cancelled, responseFenced: true,
			want: protocol.DindMaintenance{}, wantErr: true,
		},
		{
			name: "cancelled unfenced", request: cancelled, responseFenced: false,
			want: cancelled, wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := tt.request
			response.Fenced = tt.responseFenced
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer server.Close()
			client := New(server.URL, "token", time.Second, nil, nil)
			got, err := client.TransitionDinD(context.Background(), "worker", tt.request)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("error present = %t, want %t (error: %v)", gotErr, tt.wantErr, err)
			}
			if got != tt.want {
				t.Errorf("returned binding = %+v, want %+v", got, tt.want)
			}
		})
	}
}
