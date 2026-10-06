package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestTransitionDinDValidatedMatchingResponse(t *testing.T) {
	request := protocol.DindMaintenance{ID: "operation", Nonce: "nonce", Phase: "stopping", DeploymentUID: "deployment", PVCUID: "pvc", RegisterNonce: "registration", Fenced: true, ReadyACK: true}
	for _, mode := range []string{"valid", "conflict", "unsupported", "server-error", "bad-json", "oversized", "wrong-id", "wrong-nonce", "wrong-register", "wrong-pvc", "wrong-deployment", "wrong-phase", "unfenced", "empty", "trailing-json"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/controller/workers/worker/dind-maintenance" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("incorrect authenticated request")
				}
				var got protocol.DindMaintenance
				if json.NewDecoder(r.Body).Decode(&got) != nil || got != request {
					t.Error("binding was altered")
				}
				out := request
				switch mode {
				case "conflict":
					w.WriteHeader(http.StatusConflict)
					return
				case "unsupported":
					w.WriteHeader(http.StatusNotFound)
					return
				case "server-error":
					w.WriteHeader(http.StatusInternalServerError)
					return
				case "bad-json":
					_, _ = w.Write([]byte("{"))
					return
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat(" ", 17<<10)))
					return
				case "wrong-id":
					out.ID = "other"
				case "wrong-nonce":
					out.Nonce = "other"
				case "wrong-register":
					out.RegisterNonce = "other"
				case "wrong-pvc":
					out.PVCUID = "other"
				case "wrong-deployment":
					out.DeploymentUID = "other"
				case "wrong-phase":
					out.Phase = "recycling"
				case "unfenced":
					out.Fenced = false
				case "empty":
					out.Nonce = ""
				}
				_ = json.NewEncoder(w).Encode(out)
				if mode == "trailing-json" {
					_, _ = w.Write([]byte("{}"))
				}
			}))
			defer server.Close()
			c := New(server.URL, "token", time.Second, nil, nil)
			out, err := c.TransitionDinD(context.Background(), "worker", request)
			if mode == "valid" {
				if err != nil || out != request {
					t.Fatalf("valid binding rejected: %+v %v", out, err)
				}
			} else if err == nil {
				t.Fatal("invalid response authorized transition")
			}
		})
	}
}

func TestTransitionDinDInitialServerBindingAndRefresh(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "refresh"}[refresh], func(t *testing.T) {
			in := protocol.DindMaintenance{Phase: "requested", DeploymentUID: "deployment", PVCUID: "pvc"}
			if refresh {
				in.ID = "operation"
				in.Nonce = "old-nonce"
				in.RegisterNonce = "old-registration"
			}
			want := in
			want.ID = "operation"
			want.Nonce = "fresh-nonce"
			want.RegisterNonce = "new-registration"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(want) }))
			defer server.Close()
			c := New(server.URL, "token", time.Second, nil, nil)
			got, err := c.TransitionDinD(context.Background(), "worker", in)
			if err != nil || got != want {
				t.Fatalf("server-owned binding not accepted: %+v %v", got, err)
			}
		})
	}
}

func TestReportReadinessDoesNotTreatMissingEndpointAsPublication(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	c := New(server.URL, "token", time.Second, nil, nil)
	if err := c.ReportReadiness(context.Background(), protocol.StatusReport{}); err == nil {
		t.Fatal("404 accepted as readiness publication")
	}
	if err := c.Report(context.Background(), protocol.StatusReport{}); err != nil {
		t.Fatal("legacy report compatibility changed")
	}
}

func TestTransitionDinDTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(50 * time.Millisecond):
		}
	}))
	defer server.Close()
	c := New(server.URL, "token", 10*time.Millisecond, nil, nil)
	if _, err := c.TransitionDinD(context.Background(), "worker", protocol.DindMaintenance{Phase: "requested", DeploymentUID: "d", PVCUID: "p"}); err == nil {
		t.Fatal("hung server accepted")
	}
}
