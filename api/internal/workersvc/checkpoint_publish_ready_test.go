package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type readinessStore struct {
	Store
	id              uuid.UUID
	marked, cleared int
	markErr         error
	markContextErr  error
	markDeadline    time.Time
}

func (q *readinessStore) RecordCheckpointPublishAttempt(context.Context, store.RecordCheckpointPublishAttemptParams) (uuid.UUID, error) {
	return q.id, nil
}
func (q *readinessStore) GetRunCheckpointTipForRetention(context.Context, uuid.UUID) (pgtype.Text, error) {
	return pgtype.Text{}, nil
}
func (q *readinessStore) GetCheckpointRetention(context.Context, uuid.UUID) (store.CheckpointRetention, error) {
	return store.CheckpointRetention{}, pgx.ErrNoRows
}
func (q *readinessStore) MarkCheckpointPublishAttemptReady(ctx context.Context, id uuid.UUID) (int64, error) {
	if id != q.id {
		panic("wrong readiness ID")
	}
	q.marked++
	q.markContextErr = ctx.Err()
	q.markDeadline, _ = ctx.Deadline()
	return 1, q.markErr
}
func (q *readinessStore) DeleteCheckpointPublishAttempt(context.Context, uuid.UUID) (int64, error) {
	q.cleared++
	return 1, nil
}

func TestCheckpointPublishReadiness(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		disposition           pushbroker.PublishDisposition
		err                   error
		already, cancel, fail bool
		wantReady, wantClear  int
	}{
		{"advanced", pushbroker.PublishAdvanced, nil, false, false, false, 1, 0},
		{"unknown cancelled", pushbroker.PublishOutcomeUnknown, context.Canceled, false, true, false, 1, 0},
		{"success readiness write failure", pushbroker.PublishAdvanced, nil, false, false, true, 1, 0},
		{"unknown readiness write failure", pushbroker.PublishOutcomeUnknown, context.Canceled, false, true, true, 1, 0},
		{"generic", pushbroker.PublishUnclassified, errors.New("receive-pack failed"), false, false, false, 0, 0},
		{"unclassified success", pushbroker.PublishUnclassified, nil, false, false, false, 0, 0},
		{"pre send", pushbroker.PublishUnclassified, pushbroker.ErrTipMissing, false, false, false, 0, 1},
		{"already current", pushbroker.PublishUnclassified, nil, true, false, false, 0, 0},
		{"advanced error contradiction", pushbroker.PublishAdvanced, context.Canceled, false, false, false, 0, 0},
		{"unknown success contradiction", pushbroker.PublishOutcomeUnknown, nil, false, false, false, 0, 0},
		{"unknown refusal contradiction", pushbroker.PublishOutcomeUnknown, pushbroker.ErrNotDescendant, false, false, false, 0, 1},
		{"advanced already current contradiction", pushbroker.PublishAdvanced, nil, true, false, false, 0, 0},
		{"unknown already current contradiction", pushbroker.PublishOutcomeUnknown, context.Canceled, true, false, false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &readinessStore{id: uuid.New()}
			if tc.fail {
				q.markErr = errors.New("readiness unavailable")
			}
			s := New(q, newBox(t), testParams())
			// No pool operation occurs at this public publish seam.
			s.SetRetentionLockPool((*pgxpool.Pool)(nil))
			s.SetForgeBaseURLAllowed(func(string) bool { return true })
			s.SetBackground(func(fn func()) { fn() })
			s.SetDeleteCheckpointFn(func(context.Context, pushbroker.DeleteOptions) error { return nil })
			s.SetCreateRefFn(func(context.Context, pushbroker.CreateRefOptions) error { return nil })
			s.SetListRefTipsFn(func(context.Context, pushbroker.ListRefsOptions, ...string) (map[string]string, error) {
				return nil, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
				if tc.cancel {
					cancel()
				}
				return pushbroker.Result{Disposition: tc.disposition, AlreadyCurrent: tc.already}, tc.err
			})
			p := &checkpointPush{s: s, runID: uuid.New()}
			err := p.pushOnce(ctx)
			if !errors.Is(err, tc.err) {
				t.Fatalf("push result = %v, want %v", err, tc.err)
			}
			if q.marked != tc.wantReady || q.cleared != tc.wantClear {
				t.Fatalf("ready writes=%d clear=%d, want ready=%d clear=%d", q.marked, q.cleared, tc.wantReady, tc.wantClear)
			}
			if q.marked > 0 && (q.markContextErr != nil || q.markDeadline.IsZero() || time.Until(q.markDeadline) > retentionRecordTimeout) {
				t.Fatalf("readiness context must survive cancellation with bounded deadline: err=%v deadline=%v", q.markContextErr, q.markDeadline)
			}
		})
	}
}
