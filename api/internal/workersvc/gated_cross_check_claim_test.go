package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimGatedFieldNeedsRunAndWorkerCapability pins PRD #3708 seam 2: the claim carries
// plan_cross_check_gated only for a gated run claimed by a worker that advertises
// cross_check_gated_v1, and omits the wire key otherwise.
func TestClaimGatedFieldNeedsRunAndWorkerCapability(t *testing.T) {
	for _, gated := range []bool{false, true} {
		for _, advertises := range []bool{false, true} {
			fs, svc, worker := selfImproveClaimFixture(t, uuid.New(), uuid.New())
			fs.claimRun.Kind = runkind.Issue
			fs.claimRun.PlanCrossCheckGated = gated
			if advertises {
				worker.ProtocolCapabilities = []string{capability.CrossCheckGatedV1}
			}
			payload, err := svc.Claim(context.Background(), worker, nil)
			if err != nil || payload == nil {
				t.Fatalf("Claim: payload=%v err=%v", payload, err)
			}
			want := gated && advertises
			if payload.PlanCrossCheckGated != want {
				t.Fatalf("gated=%v advertises=%v: plan_cross_check_gated=%v", gated, advertises, payload.PlanCrossCheckGated)
			}
			wire, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), `"plan_cross_check_gated":true`) != want ||
				(!want && strings.Contains(string(wire), `"plan_cross_check_gated"`)) {
				t.Fatalf("gated=%v advertises=%v: additive claim field mismatch: %s", gated, advertises, wire)
			}
			if payload.PlanCrossCheckRequired {
				t.Fatalf("a gated run must not claim as required: %+v", payload)
			}
		}
	}
}

func TestPlanCrossCheckCodexLeadSupportedForGated(t *testing.T) {
	cases := []struct {
		name            string
		harness         string
		required, gated bool
		want            bool
	}{
		{"codex lead, gated", harnessCodex, false, true, true},
		{"claude lead, gated", "claude", false, true, false},
		{"codex lead, neither", harnessCodex, false, false, false},
		{"codex lead, required", harnessCodex, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planCrossCheckCodexLeadSupported(store.Run{Harness: tc.harness, PlanCrossCheckRequired: tc.required, PlanCrossCheckGated: tc.gated})
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGatedPlanApprovalViolationMapping pins the trigger-error mapping: only SQLSTATE 23514
// naming the runs_gated_plan_approval constraint is the stale/refused outcome.
func TestGatedPlanApprovalViolationMapping(t *testing.T) {
	trigger := &pgconn.PgError{Code: "23514", ConstraintName: "runs_gated_plan_approval"}
	if !isGatedPlanApprovalViolation(trigger) || !isGatedPlanApprovalViolation(fmt.Errorf("wrap: %w", trigger)) {
		t.Fatal("the gated approval trigger error must map")
	}
	for _, err := range []error{
		nil,
		errors.New("plain"),
		&pgconn.PgError{Code: "23514", ConstraintName: "runs_plan_cross_check_mode_check"},
		&pgconn.PgError{Code: "23505", ConstraintName: "runs_gated_plan_approval"},
	} {
		if isGatedPlanApprovalViolation(err) {
			t.Fatalf("%v must not map", err)
		}
	}
}

func TestGatedServerGateReason(t *testing.T) {
	declared := "interrupted"
	gated := store.Run{PlanCrossCheckGated: true}
	oldWorker := store.Worker{}
	newWorker := store.Worker{ProtocolCapabilities: []string{capability.CrossCheckGatedV1}}
	cases := []struct {
		name string
		lead store.Run
		w    store.Worker
		req  StateRequest
		want bool
	}{
		{"old worker, no declaration", gated, oldWorker, StateRequest{State: "awaiting_approval"}, true},
		{"new worker", gated, newWorker, StateRequest{State: "awaiting_approval"}, false},
		{"worker declared a reason", gated, oldWorker, StateRequest{State: "awaiting_approval", PlanCrossCheckGateReason: &declared}, false},
		{"not gated", store.Run{PlanCrossCheckRequired: true}, oldWorker, StateRequest{State: "awaiting_approval"}, false},
		{"other state", gated, oldWorker, StateRequest{State: "running"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gatedServerGateReason(tc.lead, tc.w, tc.req)
			if got.Valid != tc.want || (tc.want && got.String != "worker_unsupported") {
				t.Fatalf("got %+v, want valid=%v", got, tc.want)
			}
		})
	}
}
