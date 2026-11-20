package reconcile

import (
	"context"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

type desiredWorkersKey struct{}

// WithDesiredWorkers attaches the tick's polled desired workers to ctx so Observe can
// judge a deliberately held worker's claims. Absent on ctx, Observe grants no exemption.
func WithDesiredWorkers(ctx context.Context, workers []protocol.DesiredWorker) context.Context {
	return context.WithValue(ctx, desiredWorkersKey{}, workers)
}

// DesiredWorkers returns the desired workers attached by WithDesiredWorkers.
func DesiredWorkers(ctx context.Context) ([]protocol.DesiredWorker, bool) {
	w, ok := ctx.Value(desiredWorkersKey{}).([]protocol.DesiredWorker)
	return w, ok
}

type pollStartKey struct{}

func WithPollStart(ctx context.Context, start time.Time) context.Context {
	return context.WithValue(ctx, pollStartKey{}, start)
}
func PollStart(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(pollStartKey{}).(time.Time)
	return t, ok
}
