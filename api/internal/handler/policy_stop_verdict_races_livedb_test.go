package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type policyStopDelayedRejectStore struct {
	*wallParkRaceStore
	attempt chan struct{}
	release <-chan struct{}
}

func (s *policyStopDelayedRejectStore) RejectRunServerSide(ctx context.Context, arg store.RejectRunServerSideParams) (int64, error) {
	// SubmitInput has read the paused run and passed its early Stop guard.
	close(s.attempt)
	select {
	case <-s.release:
		return s.wallParkRaceStore.RejectRunServerSide(ctx, arg)
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

type policyStopDelayedApprovalStore struct {
	*wallParkRaceStore
	attempt chan struct{}
	release <-chan struct{}
}

func (s *policyStopDelayedApprovalStore) CreateGateVerdictInput(ctx context.Context, arg store.CreateGateVerdictInputParams) (store.RunUserInput, error) {
	// Pause enqueueGateApproval immediately before the real verdict SQL.
	close(s.attempt)
	select {
	case <-s.release:
		return s.wallParkRaceStore.CreateGateVerdictInput(ctx, arg)
	case <-ctx.Done():
		return store.RunUserInput{}, ctx.Err()
	}
}

// These are fixture handler/SQL races, with no worker or runtime Stop operation.
// Each request executes once, bounded by policyStopRacePOST's ten-second context;
// cancellation releases either store wait and the helper joins the request.
func TestPolicyStopDelayedVerdictsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		stop bool
	}{
		{"reject_without_Stop", "reject_plan", false},
		{"reject_after_Stop_ACK", "reject_plan", true},
		{"plain_approval_without_Stop", "approve_plan", false},
		{"plain_approval_after_Stop_ACK", "approve_plan", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPolicyStopConfirmEnv(t, 1, "revise")
			token := f.review().StopIfMatch
			pool := policyStopRacePool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			run, err := f.race.GetRunByID(ctx, f.id)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != "paused" || len(run.PolicyStopResolution) != 0 || len(run.RequiredCapabilities) != 0 {
				t.Fatalf("unexpected pre-verdict fixture: status=%q resolution=%s capabilities=%v",
					run.Status, run.PolicyStopResolution, run.RequiredCapabilities)
			}

			attempt, release := make(chan struct{}), make(chan struct{})
			var q workersvc.Store
			if tc.kind == "reject_plan" {
				q = &policyStopDelayedRejectStore{f.race, attempt, release}
			} else {
				q = &policyStopDelayedApprovalStore{f.race, attempt, release}
			}
			policyStopRaceService(f, q, pool)
			// No selection or expected_gate_revision: exercise an unbound verdict
			// from the paused fixture, not a revision mismatch or selection approve.
			body := "delayed owner verdict"
			verdictResult := policyStopRacePOST(t, f, `{"kind":"`+tc.kind+`","body":"`+body+`"}`, "")
			policyStopRaceWait(t, ctx, attempt, tc.kind+" reached real SQL boundary")

			if tc.stop {
				stopResult := policyStopRacePOST(t, f, `{"kind":"stop","body":"keep completed work"}`, token)
				rec := policyStopRaceResponse(t, ctx, stopResult, http.StatusAccepted, "")
				var response apitypes.RunInputResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.PolicyStop == nil || response.PolicyStop.Disposition != "accepted" {
					t.Fatalf("Stop ACK missing accepted receipt: %s", rec.Body)
				}
				before := f.snapshot()
				close(release)
				// Check the full snapshot even if the buggy handler reports success.
				// The snapshot includes every run field, input/audit and cross-check.
				select {
				case rec := <-verdictResult:
					if rec.Code != http.StatusConflict {
						t.Errorf("delayed %s status=%d want=409 body=%s", tc.kind, rec.Code, rec.Body)
					}
					var refusal struct {
						Reason string `json:"reason"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
						t.Error(err)
					}
					if refusal.Reason != "policy_stop_gate_resolved" {
						t.Errorf("delayed %s reason=%q want=policy_stop_gate_resolved body=%s", tc.kind, refusal.Reason, rec.Body)
					}
				case <-ctx.Done():
					t.Fatalf("waiting for delayed %s response: %v", tc.kind, ctx.Err())
				}
				if after := f.snapshot(); after != before {
					t.Errorf("delayed %s mutated accepted Stop full snapshot: before=%s after=%s", tc.kind, before, after)
				}
				return
			}

			// Same store barrier and real SQL without Stop: an always-refusing
			// wrapper or a fixture that cannot reach the intended write is invalid.
			close(release)
			rec := policyStopRaceResponse(t, ctx, verdictResult, http.StatusAccepted, "")
			var response apitypes.RunInputResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			run, err = f.race.GetRunByID(ctx, f.id)
			if err != nil {
				t.Fatal(err)
			}
			if len(run.PolicyStopResolution) != 0 {
				t.Fatal("no-Stop control unexpectedly acquired a Stop resolution")
			}
			if tc.kind == "reject_plan" {
				if !response.ServerSide || run.Status != "failed" || run.FailureReason.String != body ||
					run.StopKind.String != "plan_rejected" {
					t.Fatalf("no-Stop reject did not fail normally: response=%s status=%q reason=%q stop_kind=%q",
						rec.Body, run.Status, run.FailureReason.String, run.StopKind.String)
				}
			} else {
				if response.ServerSide || run.Status != "paused" {
					t.Fatalf("plain approval did not enqueue normally: response=%s status=%q", rec.Body, run.Status)
				}
				var unbound bool
				if err := pool.QueryRow(ctx, `SELECT gate_binding='unbound' AND gate_revision IS NULL
 AND consumed_at IS NULL AND disposition IS NULL
 FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan' AND body=$2`, f.id, body).Scan(&unbound); err != nil {
					t.Fatal(err)
				}
				if !unbound {
					t.Fatal("no-Stop plain approval did not persist an unbound pending verdict")
				}
			}
		})
	}
}

// Cover the other shared verdict write shapes without bypassing their real SQL.
func TestPolicyStopVerdictSQLGuardsLiveDB(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		name := "without_Stop"
		if stopped {
			name = "after_Stop_ACK"
		}
		t.Run(name, func(t *testing.T) {
			f := newPolicyStopConfirmEnv(t, 1, "revise")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pool := policyStopRacePool(t)
			q := store.New(pool)
			if stopped {
				rec := f.stop(f.owner, f.review().StopIfMatch)
				if rec.Code != http.StatusAccepted {
					t.Fatalf("Stop: status=%d body=%s", rec.Code, rec.Body)
				}
			}
			before := f.snapshot()
			_, err := q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
				RunID: f.id, Body: pgtype.Text{String: "{}", Valid: true},
				AgentSource: pgtype.Text{String: "own", Valid: true}, AgentExclusions: []byte("[]"),
				RunMaxIterations: 5, RunTimeoutSeconds: 57600, MilestoneBudgetCap: 10,
				SizeBudgetFactorL: 1, BudgetWallCeilingSeconds: 57600,
			})
			if stopped && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("selected approval after Stop: %v, want ErrNoRows", err)
			}
			if !stopped && err != nil {
				t.Fatalf("ordinary selected approval: %v", err)
			}
			_, err = q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
				RunID: f.id, Kind: "reject_plan", Body: pgtype.Text{String: "late live reject", Valid: true},
				StopKind: pgtype.Text{String: "plan_rejected", Valid: true},
			})
			if stopped && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("live reject after Stop: %v, want ErrNoRows", err)
			}
			if !stopped && err != nil {
				t.Fatalf("ordinary live reject: %v", err)
			}
			if !stopped {
				return
			}
			if after := f.snapshot(); after != before {
				t.Fatal("guarded SQL mutated accepted Stop")
			}
			// The shared verdict query must retain Cancel. It remains an owner action
			// after accepted Stop and cannot be blocked by the rejection-only guard.
			result := policyStopRacePOST(t, f, `{"kind":"cancel","body":"owner cancelled"}`, "")
			policyStopRaceResponse(t, ctx, result, http.StatusAccepted, "")
			run, err := f.race.GetRunByID(ctx, f.id)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != "cancelled" || len(run.PolicyStopResolution) == 0 {
				t.Fatalf("Cancel after Stop: status=%s resolution=%s", run.Status, run.PolicyStopResolution)
			}
		})
	}
}
