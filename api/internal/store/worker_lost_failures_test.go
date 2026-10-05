package store

import (
	"context"
	"testing"
)

// Embedding DBTX deliberately supplies no Begin; touching it would panic.
type workerLostNoBeginner struct{ DBTX }

func TestWorkerLostFailureRequiresTransaction(t *testing.T) {
	q := New(workerLostNoBeginner{})
	ctx := context.Background()
	checks := []struct {
		name string
		call func() error
	}{
		{"stale", func() error {
			_, err := q.FailRunsOfStaleWorkersOverCap(ctx, FailRunsOfStaleWorkersOverCapParams{})
			return err
		}},
		{"register", func() error { _, err := q.FailWorkerRunsOverCap(ctx, FailWorkerRunsOverCapParams{}); return err }},
		{"attested", func() error {
			_, err := q.FailAttestedFinalizeRunsOverCap(ctx, FailAttestedFinalizeRunsOverCapParams{})
			return err
		}},
		{"snapshot", func() error {
			_, err := q.FailRunsMissingFromSnapshot(ctx, FailRunsMissingFromSnapshotParams{})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.call(); err == nil || err.Error() != "worker-lost failure transaction unavailable" {
				t.Fatalf("failure = %v", err)
			}
		})
	}
}
