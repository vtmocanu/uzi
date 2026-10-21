package uzicli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestOwnerRunBoundedReader(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{
		{"12345678", true}, {"123456789", false}, {"{}", true},
		{"{}      x", false},
	} {
		raw, err := readOwnerResponse(strings.NewReader(tc.raw), 8)
		if (err == nil) != tc.ok || (tc.ok && string(raw) != tc.raw) {
			t.Fatalf("raw=%q result=%q err=%v", tc.raw, raw, err)
		}
	}
}

func TestOwnerRunTransportAgreement(t *testing.T) {
	gate := &apitypes.SemanticGateDTO{}
	gate.Presentation.Candidate.CandidateIdentityVersion = apitypes.SemanticIdentityVersion
	gate.Approval.CandidateIdentityVersion = apitypes.SemanticIdentityVersion
	invalid := *gate
	invalid.Approval.CandidateIdentityVersion = "future"
	for _, tc := range []struct {
		marker string
		gate   *apitypes.SemanticGateDTO
		ok     bool
	}{
		{"", nil, true}, {apitypes.SemanticIdentityVersion, gate, true},
		{"", gate, false}, {apitypes.SemanticIdentityVersion, nil, false},
		{"future", gate, false}, {"future", nil, false},
		{apitypes.SemanticIdentityVersion, &invalid, false},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.marker != "" {
					w.Header().Set(apitypes.SemanticIdentityHeader, tc.marker)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"run": apitypes.RunDTO{ID: "run", SemanticGate: tc.gate}})
			}))
			defer srv.Close()
			client := NewHTTPClient(Settings{URL: srv.URL})
			got, err := client.GetRun(context.Background(), "run")
			if (err == nil) != tc.ok || (tc.ok && got.ID != "run") {
				t.Fatalf("got=%s err=%v", got.ID, err)
			}
		})
	}
}

func TestOwnerRunRejectsExplicitNullSemanticField(t *testing.T) {
	for _, marker := range []string{"", apitypes.SemanticIdentityVersion} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(apitypes.SemanticIdentityHeader, marker)
			_, _ = io.WriteString(w, `{"run":{"id":"run","semantic_gate":null}}`)
		}))
		client := NewHTTPClient(Settings{URL: srv.URL})
		if _, err := client.GetRun(context.Background(), "run"); err == nil {
			t.Fatal("explicit null semantic field downgraded to structural")
		}
		srv.Close()
	}
}

func TestOwnerRunLegacyLimitAndHTTPError(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"run":{"id":"run"},"error":"denied","pad":"`+strings.Repeat("a", maxRespBytes)+`"}`)
		}))
		client := NewHTTPClient(Settings{URL: srv.URL})
		_, err := client.GetRun(context.Background(), "run")
		if err == nil {
			t.Fatal("large invalid response accepted")
		}
		if status == http.StatusForbidden {
			// Non-2xx stays on the existing statusError path.
			want := statusError(http.StatusForbidden, nil, "/api/runs/run")
			if err.(*ExitError).Code != want.Code {
				t.Fatalf("HTTP status lost: %v", err)
			}
		}
		srv.Close()
	}
}

func TestOwnerInputMarker(t *testing.T) {
	for _, present := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (r.Header.Get(apitypes.SemanticIdentityHeader) == apitypes.SemanticIdentityVersion) != present {
				t.Error("incorrect request marker")
			}
			raw, _ := io.ReadAll(r.Body)
			if bytes.Contains(raw, []byte("semantic_approval")) != present {
				t.Error("incorrect approval body")
			}
			_, _ = io.WriteString(w, "{}")
		}))
		client := NewHTTPClient(Settings{URL: srv.URL})
		opts := RunInputOptions{Kind: "approve_plan"}
		if present {
			opts.SemanticApproval = &apitypes.SemanticGateApproval{}
		}
		if _, err := client.SubmitRunInputWithOptions(context.Background(), "run", opts); err != nil {
			t.Fatal(err)
		}
		srv.Close()
	}
}
