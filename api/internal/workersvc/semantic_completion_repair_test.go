package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The embedded Store panics on every unexpected operation, including every write.
type semanticNoTxReadStore struct {
	Store
	run store.Run
}

func (s *semanticNoTxReadStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return s.run, nil
}

func TestSemanticCompletionNoTx(t *testing.T) {
	for _, contaminated := range []bool{false, true} {
		for _, operation := range []string{"permit", "attempt", "hold", "running", "awaiting_approval", "awaiting_input", "paused", "failed"} {
			t.Run(operation+map[bool]string{false: "/semantic", true: "/contaminated"}[contaminated], func(t *testing.T) {
				run := semanticCompletionFixture(t, false)
				if contaminated {
					run.CompletionContract = contractJSON(t, "m1")
					c, _ := parseCompletionContract(run.CompletionContract)
					c.SourceOutcome = "complete"
					run.CompletionContract = semanticJSON(t, c)
				}
				svc := New(&semanticNoTxReadStore{run: run}, newBox(t), testParams())
				// A panic names a forbidden call and must fail this regression.
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("unlocked mutation reached store: %v", p)
					}
				}()
				var err error
				switch operation {
				case "permit":
					_, err = svc.RequestCompletionPermit(context.Background(), worker(), run.ID, CompletionPermitRequest{ContractRevision: 1, Branch: "agent/test", Head: "head"})
				case "attempt":
					_, err = svc.RecordCompletionAttempt(context.Background(), worker(), run.ID, CompletionAttemptRequest{})
				case "hold":
					_, _, err = svc.SetRunCompletionHold(context.Background(), worker(), run.ID, "head", nil)
				default:
					_, _, _, err = svc.SetStateReport(context.Background(), worker(), run.ID, StateRequest{State: operation})
				}
				if !errors.Is(err, ErrSemanticCompletionAuthority) {
					t.Fatalf("got %v, want authority unavailable", err)
				}
			})
		}
	}
}

func TestSemanticCompletionSourceOutcomeContamination(t *testing.T) {
	run := interlockedRun(worker())
	run.CompletionContract = contractJSON(t, "m1")
	run.MilestonesCompleted = idsJSON(t, "m1")
	before := string(run.CompletionContract)
	if pending, err := CompletionPending(run); err != nil || pending != nil {
		t.Fatalf("structural: %v %v", pending, err)
	}
	if _, valid := computeUnmetCriteria(run); !valid {
		t.Fatal("structural contract invalid")
	}
	c, _ := parseCompletionContract(run.CompletionContract)
	if string(semanticJSON(t, c)) != before {
		t.Fatal("structural serialization changed")
	}
	c.SourceOutcome = "complete"
	run.CompletionContract = semanticJSON(t, c)
	if _, err := CompletionPending(run); !errors.Is(err, ErrSemanticCompletionAuthority) {
		t.Errorf("contamination projected: %v", err)
	}
	if _, valid := computeUnmetCriteria(run); valid {
		t.Fatal("contamination considered verifiable")
	}
}

func semanticCompletionTwoMaintainers(t *testing.T) store.Run {
	t.Helper()
	run := semanticCompletionFixture(t, false)
	c, _ := parseCompletionContract(run.CompletionContract)
	second := c.SemanticPresentation.Candidate.Criteria[0]
	second.ID = uuid.NewString()
	copyMaintainer := *second.Maintainer
	copyMaintainer.Conditions = []string{"second deployment available"}
	second.Maintainer = &copyMaintainer
	c.SemanticPresentation.Candidate.Criteria = append(c.SemanticPresentation.Candidate.Criteria, second)
	p := semanticGateProjection(&semanticCoverageTx{candidate: c.SemanticPresentation.Candidate}, run)
	a := semanticApprovalFor(run, p)
	var err error
	run.CompletionContract, err = semanticGateContract(&p, &a)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestSemanticCompletionSelectedAuthorizationReplay(t *testing.T) {
	run := semanticCompletionTwoMaintainers(t)
	prior, _ := parseCompletionContract(run.CompletionContract)
	all := semanticMaintainers(prior)
	dec := CompletionDecisionInput{Decision: "authorize_maintainer", Maintainers: all[:1], Reason: "renew selected", ContractRevision: 1}
	ms, _ := DecodeMilestones(run.MilestonesFrozen)
	if err := validateDecision(run, dec, ms); err != nil {
		t.Fatal(err)
	}
	next, err := buildRevisedContract(run.CompletionContract, dec, ms, 2)
	if err != nil {
		t.Fatal(err)
	}
	run.CompletionContract = next
	run.ContractRevision.Int32 = 2
	current, _ := parseCompletionContract(next)
	for _, auth := range current.MaintainerAuthorizations {
		want := 1
		if auth.ID == all[0].ID {
			want = 2
		}
		if auth.Revision != want {
			t.Fatalf("authorization %s revision=%d want=%d", auth.ID, auth.Revision, want)
		}
	}
	if !decisionAlreadyEncoded(run, dec, 2) {
		t.Fatal("exact selected replay lost")
	}
	broader := dec
	broader.Maintainers = all
	if decisionAlreadyEncoded(run, broader, 2) {
		t.Fatal("all authorizations falsely replayed subset")
	}
	other := dec
	other.Maintainers = all[1:]
	if decisionAlreadyEncoded(run, other, 2) {
		t.Fatal("different subset falsely replayed")
	}
	if !decisionAlreadyEncoded(run, dec, 2) || string(run.CompletionContract) != string(next) {
		t.Fatal("replay changed revision")
	}
}
