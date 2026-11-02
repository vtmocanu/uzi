package workersvc

import (
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"testing"
	"time"
)

func TestRunPolicyOutcomePending(t *testing.T) {
	worker, other, run, outcome := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name     string
		alter    func(*store.Run, *OutboxEntry)
		reporter uuid.UUID
		want     bool
	}{
		{name: "pending", want: true},
		{name: "foreign reporter", reporter: other},
		{name: "unassigned", alter: func(r *store.Run, _ *OutboxEntry) { r.WorkerID.Valid = false }},
		{name: "same applied identity", alter: func(r *store.Run, _ *OutboxEntry) {
			r.PolicyOutcomeID = pgconv.UUID(outcome)
			v := int64(2)
			r.PolicySourceGeneration = pgconv.Int8Ptr(&v)
		}},
		{name: "newer applied", alter: func(r *store.Run, _ *OutboxEntry) { v := int64(3); r.PolicyApplyingGeneration = pgconv.Int8Ptr(&v) }},
		{name: "resolved older history", want: true, alter: func(r *store.Run, _ *OutboxEntry) {
			r.PolicyOutcomeID = pgconv.UUID(uuid.New())
			v := int64(1)
			r.PolicyApplyingGeneration = pgconv.Int8Ptr(&v)
		}},
		{name: "unknown facts", alter: func(_ *store.Run, e *OutboxEntry) { e.PolicyPendingOutcomeID = uuid.Nil }},
		{name: "terminal", alter: func(r *store.Run, _ *OutboxEntry) { r.Status = "cancelled" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(nil, nil, testParams())
			r := store.Run{ID: run, WorkerID: pgconv.UUID(worker), Status: "running"}
			e := OutboxEntry{RunID: run, PendingTerminal: 1, PolicyPendingOutcomeID: outcome, PolicyPendingSourceGeneration: 2}
			if tc.alter != nil {
				tc.alter(&r, &e)
			}
			reporter := worker
			if tc.reporter != uuid.Nil {
				reporter = tc.reporter
			}
			svc.outbox.record(reporter, []OutboxEntry{e}, time.Now())
			if got := svc.RunPolicyOutcomePending(r); got != tc.want {
				t.Fatalf("pending=%v want%v", got, tc.want)
			}
			if _, _, blocked := svc.RunBlockedOutcome(run); blocked {
				t.Fatal("transient policy telemetry became a permanent blocked reason")
			}
		})
	}
}
