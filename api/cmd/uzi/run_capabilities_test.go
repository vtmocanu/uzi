package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestApproveOverrideCapabilitiesRealHTTP(t *testing.T) {
	for _, mode := range []string{"ordinary", "semantic", "reset"} {
		for _, flag := range []string{"omitted", "true", "false"} {
			t.Run(mode+"/"+flag, func(t *testing.T) {
				var got apitypes.RunInputRequest
				posts := 0
				gate := semanticCLIGate()
				gate.Approval.CandidateIdentityVersion = apitypes.SemanticIdentityVersion
				gate.Presentation.Candidate.CandidateIdentityVersion = apitypes.SemanticIdentityVersion
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.Method + " " + r.URL.Path {
					case "GET /api/runs/r1":
						run := apitypes.RunDTO{ID: "r1", Status: "awaiting_approval", GateRevision: 2}
						if mode == "semantic" {
							run.SemanticGate = gate
							w.Header().Set(apitypes.SemanticIdentityHeader, apitypes.SemanticIdentityVersion)
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"run": run})
					case "POST /api/runs/r1/inputs":
						posts++
						if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
							t.Error(err)
						}
						w.WriteHeader(http.StatusAccepted)
						_, _ = io.WriteString(w, `{"server_side":true}`)
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer srv.Close()
				args := []string{"run", "approve", "r1"}
				if mode == "reset" {
					args = resetArgs()
				}
				args = append(args, "--agent-source", "repo", "--exclude-agents", "tester")
				if flag != "omitted" {
					args = append(args, "--override-capabilities="+flag)
				}
				client := uzicli.NewHTTPClient(uzicli.Settings{URL: srv.URL, Token: "test"})
				_, stderr, code := runCLI(t, fakeEnv(client), args...)
				if code != 0 {
					t.Fatalf("exit=%d: %s", code, stderr)
				}
				if posts != 1 || got.Kind != kindApprovePlan || got.OverrideCapabilities != (flag == "true") {
					t.Fatalf("posts=%d input=%+v; override must follow explicit consent", posts, got)
				}
				if !reflect.DeepEqual(got.Selection, &apitypes.AgentSelection{Source: "repo", Exclusions: []string{"tester"}}) {
					t.Fatalf("lost selection: %+v", got.Selection)
				}
				expectedRevision := int64(2)
				if mode == "reset" {
					expectedRevision = 7
					if got.Reconciliation == nil || got.Reconciliation.Mode != "reset" || got.ExpectedResetStateDigest == "" {
						t.Fatalf("lost reset options: %+v", got)
					}
				}
				if got.ExpectedGateRevision == nil || *got.ExpectedGateRevision != expectedRevision {
					t.Fatalf("lost revision binding: %+v", got)
				}
				if mode == "semantic" && !reflect.DeepEqual(got.SemanticApproval, &gate.Approval) {
					t.Fatalf("lost semantic binding: %+v", got.SemanticApproval)
				}
			})
		}
	}
}
