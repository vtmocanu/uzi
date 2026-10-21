package workersvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise admission through the service, including the lock and persisted ledger.
func TestSemanticRevisionOperationAdmission(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, operation := range []string{"compile replay", "compile fresh", "payload", "status", "reserve replay", "reserve fresh", "settle replay", "settle fresh", "publish replay", "publish fresh"} {
			t.Run(operation+map[bool]string{false: "/no receipt", true: "/pending receipt"}[pending], func(t *testing.T) {
				worker, runID := uuid.New(), uuid.New()
				structural, proposal, sources := semanticFixture()
				// Match the public compiler's server-owned issue provenance.
				sources[0].Provenance = "server:run-issue"
				candidate, fingerprint, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources)
				if err != nil {
					t.Fatal(err)
				}
				candidate.Revision = 1
				candidate.Digest = "current"
				candidate.Criteria[0].ID = uuid.NewString()
				now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
				binding := SemanticCoverageBinding(candidate)
				requestID, reviewID, invocationID := uuid.NewString(), uuid.NewString(), uuid.NewString()
				tokens := 20
				settlement := apitypes.SemanticCoverageSettleRequest{ReviewID: reviewID, InvocationID: invocationID, Status: "success", Tokens: &tokens}
				report := apitypes.SemanticCoverageReport{SemanticCoverageBinding: binding, Requirements: []apitypes.SemanticCoverageResult{{RequirementID: "r1", Status: "represented", Explanation: "covered", Citations: candidate.Criteria[0].Citations, CriterionIDs: []string{candidate.Criteria[0].ID}}}, Criteria: []apitypes.SemanticCoverageAtomicity{{CriterionID: candidate.Criteria[0].ID, Atomic: true, Explanation: "one behavior"}}}
				invocation := semanticCoverageInvocation{ID: invocationID, RequestID: requestID, WorkerID: worker, Generation: 7, Reserved: 40000, Charged: 20, Status: "success", Settlement: &settlement}
				review := semanticCoverageReview{ID: reviewID, Binding: binding, Started: now, Deadline: now.Add(5 * time.Minute), Invocations: []semanticCoverageInvocation{invocation}, Report: &report}
				if operation == "settle fresh" {
					review.Invocations[0].Settlement = nil
					review.Invocations[0].Charged = 40000
					review.Invocations[0].Status = "pending"
				}
				if operation == "publish fresh" {
					review.Report = nil
				}
				ledger := semanticCoverageLedger{Version: 1, Started: now, Reviews: []semanticCoverageReview{review}}
				enrollment := apitypes.SemanticEnrollment{Profile: "semantic", Policy: apitypes.SemanticPolicy{Version: 1, Model: "sonnet", MaxReviews: 3, MaxInvocations: 2, MaxTokens: 120000, MaxSeconds: 900}}
				run := store.Run{ID: runID, WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, ClaimGeneration: 7, GateRevision: 4, Status: "awaiting_approval", CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true}, IssueDescription: string(sources[0].Bytes)}
				planning := store.GetSemanticPlanningRow{Enrollment: semanticJSON(t, enrollment), Revision: 1, Fingerprint: pgtype.Text{String: fingerprint, Valid: true}, Candidate: semanticJSON(t, candidate), ReviewState: semanticJSON(t, ledger)}
				tx := &crossCheckBoundaryTx{t: t}
				proofReads, writes := 0, 0
				tx.query = func(sql string, args []any) pgx.Row {
					switch {
					case strings.HasPrefix(sql, "-- name: GetRunByIDForUpdate"):
						return crossCheckBoundaryRow{value: run}
					case strings.HasPrefix(sql, "-- name: GetSemanticPlanning"):
						return crossCheckBoundaryRow{value: planning}
					case strings.HasPrefix(sql, "-- name: HasPendingSemanticRevision"):
						proofReads++
						if args[0] != runID || args[1] != (pgtype.UUID{Bytes: worker, Valid: true}) || args[2] != (pgtype.Int8{Int64: 7, Valid: true}) || args[3] != (pgtype.Int8{Int64: 4, Valid: true}) {
							t.Fatalf("unbound receipt query: %+v", args)
						}
						return crossCheckBoundaryRow{value: []any{pending}}
					case strings.HasPrefix(sql, "-- name: GetSemanticSnapshotBytes"):
						return crossCheckBoundaryRow{value: []any{int64(0)}}
					case strings.HasPrefix(sql, "-- name: HasSemanticSnapshot"):
						return crossCheckBoundaryRow{value: []any{true}}
					case strings.HasPrefix(sql, "-- name: UpdateSemanticPlanning"):
						writes++
						return crossCheckBoundaryRow{value: []any{int64(1)}}
					case strings.HasPrefix(sql, "-- name: GetSemanticSnapshot"):
						blob := sources[0].Bytes
						if args[1] == candidate.PlanHash {
							blob = []byte(structural.PlanMd)
						}
						return crossCheckBoundaryRow{value: []any{blob}}
					default:
						t.Fatalf("unexpected query: %s", sql)
						return nil
					}
				}
				tx.exec = func(sql string, args []any) (pgconn.CommandTag, error) {
					writes++
					return pgconn.NewCommandTag("UPDATE 1"), nil
				}
				svc := &Service{txBeginner: tx, now: func() time.Time { return now }}
				fresh := strings.HasSuffix(operation, "fresh")
				switch operation {
				case "compile fresh", "compile replay":
					if fresh {
						proposal.Criteria[0].DoneWhen += " revised"
					}
					_, err = svc.SubmitSemanticCandidate(context.Background(), worker, runID, 7, structural, semanticJSON(t, proposal), sources)
				case "payload":
					_, err = svc.SemanticCandidatePayload(t.Context(), worker, runID, 7, candidate.Digest)
				case "status":
					_, err = svc.SemanticCoverageStatus(t.Context(), worker, runID, 7)
				case "reserve fresh", "reserve replay":
					id := requestID
					if fresh {
						id = uuid.NewString()
					}
					var got apitypes.SemanticCoverageReservation
					got, err = svc.ReserveSemanticCoverage(t.Context(), worker, runID, 7, semanticJSON(t, apitypes.SemanticCoverageReserveRequest{RequestID: id, SemanticCoverageBinding: binding, Supported: true}))
					if !fresh && (err != nil || got.Outcome != "replay" || got.InvocationID != invocationID || got.Candidate != nil) {
						t.Fatalf("reservation replay granted call: %+v %v", got, err)
					}
				case "settle fresh", "settle replay":
					err = svc.SettleSemanticCoverage(t.Context(), worker, runID, 7, semanticJSON(t, settlement))
				case "publish fresh", "publish replay":
					err = svc.PublishSemanticCoverage(t.Context(), worker, runID, 7, semanticJSON(t, apitypes.SemanticCoveragePublishRequest{ReviewID: reviewID, InvocationID: invocationID, Report: report}))
				}
				refused := fresh && !pending
				if (err != nil) != refused {
					t.Fatalf("err=%v refused=%v", err, refused)
				}
				if refused && (writes != 0 || tx.committed) {
					t.Fatalf("unauthorized mutation wrote: %d", writes)
				}
				if !fresh && (writes != 0 || proofReads != 0 || !tx.committed) {
					t.Fatalf("read/replay mutated or demanded receipt: writes=%d proof=%d committed=%v", writes, proofReads, tx.committed)
				}
			})
		}
	}
}
