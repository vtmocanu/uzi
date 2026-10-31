package workersvc

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestPrerequisiteAbsoluteOutcomeTail(t *testing.T) {
	for _, elapsed := range []time.Duration{12 * time.Second, 16 * time.Second} {
		t.Run(elapsed.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				p := newPrerequisitePass(start)
				if want := start.Add(15 * time.Second); !p.tailEnd.Equal(want) {
					t.Fatalf("absolute tail = %v, want %v", p.tailEnd, want)
				}
				parent, cancelParent := context.WithCancel(context.Background())
				defer cancelParent()
				cancelParent()
				<-time.NewTimer(elapsed).C

				ctx, cancel := prerequisiteOutcomeContext(parent, p)
				defer cancel()
				deadline, ok := ctx.Deadline()
				if !ok || !deadline.Equal(p.tailEnd) {
					t.Fatalf("outcome deadline = %v, %v; want fixed tail %v", deadline, ok, p.tailEnd)
				}
				if elapsed < 15*time.Second {
					if ctx.Err() != nil {
						t.Fatalf("parent cancellation reached detached outcome: %v", ctx.Err())
					}
					<-ctx.Done()
					if got := time.Since(start); got != 15*time.Second {
						t.Fatalf("outcome finished at %v, want absolute 15s", got)
					}
				} else {
					select {
					case <-ctx.Done():
					default:
						t.Fatal("expired absolute tail opened a new write window")
					}
				}
				if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
					t.Fatalf("outcome error = %v, want DeadlineExceeded", ctx.Err())
				}
			})
		})
	}
}
