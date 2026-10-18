package workersvc

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func witnessedReplayFixture(t *testing.T) (store.Run, StateRequest, gateReportFields) {
	t.Helper()
	pid, worker := uuid.New(), uuid.New()
	gen, count := int64(1), int32(0)
	f := gateReportFields{
		planMd: pgtype.Text{String: "# Plan", Valid: true},
		checkpointWitness: &apitypes.GateCheckpointWitness{
			SourceHead:          "1111111111111111111111111111111111111111",
			PublishedCheckpoint: "2222222222222222222222222222222222222222",
		},
		witnessWorkerID: &worker, witnessClaimGeneration: &gen, reviseCount: &count,
	}
	payload, digest, err := effectiveGatePayload(gatePresentedPayload{}, f).digest()
	if err != nil {
		t.Fatal(err)
	}
	locked := store.Run{Status: "awaiting_input", GateRevision: 1, ReviseCount: 1,
		GatePresentationID:   pgtype.UUID{Bytes: pid, Valid: true},
		GatePresentedPayload: payload, GatePayloadDigest: digest}
	req := StateRequest{State: "awaiting_approval", PresentationID: &pid,
		ClaimGeneration: &gen, ExpectedGateRevision: &gen}
	return locked, req, f
}

func TestGateWitnessReplayRetainsCapturedReviseCount(t *testing.T) {
	locked, req, fields := witnessedReplayFixture(t)
	for _, count := range []int32{1, 2} {
		locked.ReviseCount = count
		before := bytes.Clone(locked.GatePresentedPayload)
		decision, err := classifyGateReport(context.Background(), nil, locked, req, fields)
		if err != nil || decision.refusal != nil || decision.class != gateClassRetain {
			t.Fatalf("original witnessed source replay at count %d: class=%v refusal=%v err=%v", count, decision.class, decision.refusal, err)
		}
		payload, digest, err := decision.retained.digest()
		if err != nil || !bytes.Equal(payload, before) || !bytes.Equal(digest, locked.GatePayloadDigest) ||
			decision.retained.ReviseCount == nil || *decision.retained.ReviseCount != 0 {
			t.Fatalf("replay refreshed the immutable witness: payload=%s err=%v", payload, err)
		}
	}
}

func TestGateWitnessReplayRejectsMissingCapturedReviseCount(t *testing.T) {
	locked, req, fields := witnessedReplayFixture(t)
	snapshot, ok, err := decodeGatePresentedPayload(locked.GatePresentedPayload)
	if err != nil || !ok {
		t.Fatalf("decode witness fixture: found=%v err=%v", ok, err)
	}
	// Preserve a valid stored digest, but remove the count that proves when the
	// witness was captured. Replay must not infer or refresh the missing evidence.
	snapshot.ReviseCount = nil
	locked.GatePresentedPayload, locked.GatePayloadDigest, err = snapshot.digest()
	if err != nil {
		t.Fatal(err)
	}
	before := bytes.Clone(locked.GatePresentedPayload)
	decision, err := classifyGateReport(context.Background(), nil, locked, req, fields)
	if err != nil || !errors.Is(decision.refusal, ErrGatePresentationConflict) {
		t.Fatalf("missing captured count accepted: class=%v refusal=%v err=%v", decision.class, decision.refusal, err)
	}
	if !bytes.Equal(locked.GatePresentedPayload, before) {
		t.Fatal("replay repaired missing witness evidence")
	}
}

func TestGateWitnessReplayRejectsChangedContentAndBinding(t *testing.T) {
	for _, change := range []string{"plan", "source head", "published checkpoint", "worker", "generation"} {
		t.Run(change, func(t *testing.T) {
			locked, req, fields := witnessedReplayFixture(t)
			switch change {
			case "plan":
				fields.planMd.String = "# Changed plan"
			case "source head":
				fields.checkpointWitness.SourceHead = "3333333333333333333333333333333333333333"
			case "published checkpoint":
				fields.checkpointWitness.PublishedCheckpoint = "3333333333333333333333333333333333333333"
			case "worker":
				worker := uuid.New()
				fields.witnessWorkerID = &worker
			case "generation":
				gen := int64(2)
				fields.witnessClaimGeneration = &gen
			}
			decision, err := classifyGateReport(context.Background(), nil, locked, req, fields)
			if err != nil || !errors.Is(decision.refusal, ErrGatePresentationConflict) {
				t.Fatalf("changed %s accepted: class=%v refusal=%v err=%v", change, decision.class, decision.refusal, err)
			}
		})
	}
}

type witnessPresentationHistory struct {
	Store
	exists bool
}

func (s witnessPresentationHistory) RunGatePresentationExists(context.Context, store.RunGatePresentationExistsParams) (bool, error) {
	return s.exists, nil
}

func TestGateWitnessFreshPublicationCapturesLiveReviseCount(t *testing.T) {
	locked, req, fields := witnessedReplayFixture(t)
	req.PresentationID = uid()
	decision, err := classifyGateReport(context.Background(), witnessPresentationHistory{}, locked, req, fields)
	if err != nil || decision.refusal != nil || decision.class != gateClassPublish {
		t.Fatalf("fresh publication: class=%v refusal=%v err=%v", decision.class, decision.refusal, err)
	}
	payload, ok, err := decodeGatePresentedPayload(decision.payload)
	if err != nil || !ok || payload.ReviseCount == nil || *payload.ReviseCount != locked.ReviseCount {
		t.Fatalf("fresh witness used old count: %+v err=%v", payload.ReviseCount, err)
	}
}

func TestGateWitnessOlderPresentationRemainsHistorical(t *testing.T) {
	locked, req, fields := witnessedReplayFixture(t)
	locked.GatePresentationID.Bytes = uuid.New()
	locked.GateRevision = 2
	decision, err := classifyGateReport(context.Background(), witnessPresentationHistory{exists: true}, locked, req, fields)
	if err != nil || !errors.Is(decision.refusal, ErrGatePresentationHistorical) {
		t.Fatalf("old presentation replay: class=%v refusal=%v err=%v", decision.class, decision.refusal, err)
	}
}
