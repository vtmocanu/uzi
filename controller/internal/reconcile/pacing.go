package reconcile

import (
	"context"
	"time"
)

type pollStartKey struct{}

func WithPollStart(ctx context.Context, start time.Time) context.Context {
	return context.WithValue(ctx, pollStartKey{}, start)
}
func PollStart(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(pollStartKey{}).(time.Time)
	return t, ok
}
