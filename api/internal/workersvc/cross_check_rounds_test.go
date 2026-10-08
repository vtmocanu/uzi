package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type roundsMetadataStore struct {
	Store
	lead store.Run
	row  store.GetPlanCrossCheckMetadataRow
	caps []string
}

func (q *roundsMetadataStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return q.lead, nil
}
func (q *roundsMetadataStore) GetPlanCrossCheckMetadata(context.Context, uuid.UUID) (store.GetPlanCrossCheckMetadataRow, error) {
	return q.row, nil
}
func (q *roundsMetadataStore) GetPlanCrossCheckClaimingWorkerCaps(context.Context, store.GetPlanCrossCheckClaimingWorkerCapsParams) ([]string, error) {
	return q.caps, nil
}

type noAttemptCrossCheckStore struct{ Store }

func (*noAttemptCrossCheckStore) GetPlanCrossCheck(context.Context, uuid.UUID) (store.CrossCheck, error) {
	return store.CrossCheck{}, pgx.ErrNoRows
}

func TestPlanCrossCheckExhaustionRequiresStoredAttempt(t *testing.T) {
	for _, limit := range []int32{0, 2} {
		for _, generation := range []int64{1, 2} {
			q := &noAttemptCrossCheckStore{}
			svc := New(q, nil, Params{PlanCrossCheckMaxRevisions: limit, PlanCrossCheckMaxRevisionsSet: true})
			reason := "revisions_exhausted"
			lead := store.Run{ID: uuid.New(), Harness: "claude", AutoApprove: true, PlanCrossCheckRequired: true, ClaimGeneration: generation, Status: "running"}
			err := svc.validatePlanCrossCheckGateReason(t.Context(), q, store.Worker{}, lead, StateRequest{PlanCrossCheckGateReason: &reason}, false)
			if !errors.Is(err, ErrInvalidState) {
				t.Fatalf("no-attempt exhaustion accepted at limit=%d generation=%d: %v", limit, generation, err)
			}
		}
	}
}

func TestPlanCrossCheckRevisionConstructorDefaultAndZero(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params Params
		want   int32
	}{
		{"default", Params{}, 2},
		{"explicit zero", Params{PlanCrossCheckMaxRevisionsSet: true}, 0},
		{"explicit nonzero", Params{PlanCrossCheckMaxRevisions: 4}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(&roundsMetadataStore{}, nil, tc.params)
			if svc.p.PlanCrossCheckMaxRevisions != tc.want {
				t.Fatalf("revision limit=%d want=%d", svc.p.PlanCrossCheckMaxRevisions, tc.want)
			}
		})
	}
}

func TestPlanCrossCheckAutomaticRoundsMetadataContract(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for limit := int32(0); limit <= 4; limit++ {
		for round := int32(1); round <= limit+1; round++ {
			t.Run(fmt.Sprintf("budget%d/round%d", limit, round), func(t *testing.T) {
				w := worker()
				q := &roundsMetadataStore{lead: store.Run{ID: uuid.New(), UserID: w.UserID, WorkerID: pgconv.UUID(w.ID), ClaimGeneration: 2, Harness: "claude", Status: "running", AutoApprove: true, PlanCrossCheckRequired: true}, caps: []string{capability.CrossCheckRoundsV1},
					row: store.GetPlanCrossCheckMetadataRow{Round: round, LeadClaimGeneration: 1, Verdict: "revise", ReasonClass: pgtype.Text{String: "revise", Valid: true}, AutomaticRevisionLimit: limit, AutomaticRoundsEnabled: true, DeadlineAt: pgtype.Timestamptz{Time: now, Valid: true}, DecidedAt: pgtype.Timestamptz{Time: now.Add(-time.Second), Valid: true}}}
				svc := New(q, nil, testParams())
				svc.now = func() time.Time { return now }
				got, err := svc.LatestPlanCrossCheck(context.Background(), w, q.lead.ID, 2)
				if err != nil {
					t.Fatal(err)
				}
				if round <= limit {
					if !got.NextRoundEligible || got.NextRound == nil || *got.NextRound != round+1 || got.FallbackReason != "" {
						t.Fatalf("remaining budget: %+v", got)
					}
				} else if got.NextRoundEligible || got.NextRound != nil || got.FallbackReason != "revisions_exhausted" {
					t.Fatalf("exhausted budget: %+v", got)
				}
			})
		}
	}
	cases := []struct {
		name, verdict, reason string
		delta                 time.Duration
		proof, enabled        bool
		fallback              string
		eligible              bool
	}{
		{"superseded before", "failed", "superseded", -time.Second, true, true, "", true},
		{"superseded equal", "failed", "superseded", 0, true, true, "timed_out", false},
		{"superseded after", "failed", "superseded", time.Second, true, true, "timed_out", false},
		{"superseded unproven", "failed", "superseded", 0, false, true, "timed_out", false},
		{"approval equal", "approve", "approve", 0, true, true, "timed_out", false},
		{"approval unproven", "approve", "approve", 0, false, true, "timed_out", false},
		{"unstored before", "failed", "approved_not_stored", -time.Second, true, true, "", true},
		{"block", "block", "block", -time.Second, true, true, "block", false},
		{"timeout", "failed", "timed_out", 0, true, true, "timed_out", false},
		{"model error", "failed", "model_error", -time.Second, true, true, "model_error", false},
		{"legacy approval unproven", "approve", "approve", 0, false, false, "interrupted", false},
		{"legacy revise", "revise", "revise", -time.Second, true, false, "revise", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := worker()
			q := &roundsMetadataStore{lead: store.Run{ID: uuid.New(), UserID: w.UserID, WorkerID: pgconv.UUID(w.ID), ClaimGeneration: 2, Harness: "claude", Status: "running", AutoApprove: true, PlanCrossCheckRequired: true}, caps: []string{capability.CrossCheckRoundsV1},
				row: store.GetPlanCrossCheckMetadataRow{Round: 1, LeadClaimGeneration: 1, Verdict: tc.verdict, ReasonClass: pgtype.Text{String: tc.reason, Valid: true}, AutomaticRevisionLimit: 2, AutomaticRoundsEnabled: tc.enabled, DeadlineAt: pgtype.Timestamptz{Time: now, Valid: true}, DecidedAt: pgtype.Timestamptz{Time: now.Add(tc.delta), Valid: tc.proof}}}
			svc := New(q, nil, testParams())
			svc.now = func() time.Time { return now }
			got, err := svc.LatestPlanCrossCheck(context.Background(), w, q.lead.ID, 2)
			if err != nil || got.NextRoundEligible != tc.eligible || got.FallbackReason != tc.fallback {
				t.Fatalf("metadata=%+v err=%v", got, err)
			}
			q.lead.Status = "awaiting_approval"
			q.lead.PlanCrossCheckGateReason = pgtype.Text{String: "interrupted", Valid: true}
			got, err = svc.LatestPlanCrossCheck(context.Background(), w, q.lead.ID, 2)
			if err != nil || got.NextRoundEligible || got.FallbackReason != "interrupted" {
				t.Fatalf("human gate=%+v err=%v", got, err)
			}
		})
	}
}
