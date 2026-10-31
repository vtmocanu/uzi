package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestDrainShutdownWorkConcurrentAndJoined(t *testing.T) {
	for _, generatedOK := range []bool{false, true} {
		t.Run(map[bool]string{false: "generated_incomplete", true: "generated_complete"}[generatedOK], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				prerequisiteEntered := make(chan struct{})
				generatedEntered := make(chan struct{})
				returned := make(chan struct{})
				wantErr := errors.New("controlled prerequisite drain failure")
				var gotOK bool
				var gotErr error
				go func() {
					gotOK, gotErr = drainShutdownWork(func(ctx context.Context) error {
						deadline, ok := ctx.Deadline()
						if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
							t.Errorf("prerequisite deadline = %v, present = %v", deadline, ok)
						}
						close(prerequisiteEntered)
						<-release
						return wantErr
					}, func() bool {
						close(generatedEntered)
						<-release
						return generatedOK
					})
					close(returned)
				}()
				synctest.Wait()
				for name, entered := range map[string]<-chan struct{}{
					"prerequisite": prerequisiteEntered,
					"generated":    generatedEntered,
				} {
					select {
					case <-entered:
					default:
						t.Errorf("%s drain did not enter concurrently", name)
					}
				}
				select {
				case <-returned:
					t.Error("helper returned before drains were released")
				default:
				}
				close(release)
				<-returned
				if gotOK != generatedOK || !errors.Is(gotErr, wantErr) {
					t.Fatalf("results = (%v, %v), want (%v, %v)", gotOK, gotErr, generatedOK, wantErr)
				}
			})
		})
	}
}

func TestDrainShutdownWorkJoinsEachCallback(t *testing.T) {
	for _, blocked := range []string{"prerequisite", "generated"} {
		t.Run(blocked, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				otherDone := make(chan struct{})
				returned := make(chan struct{})
				go func() {
					generatedOK, err := drainShutdownWork(func(context.Context) error {
						if blocked == "prerequisite" {
							<-release
						} else {
							close(otherDone)
						}
						return nil
					}, func() bool {
						if blocked == "generated" {
							<-release
						} else {
							close(otherDone)
						}
						return true
					})
					if !generatedOK || err != nil {
						t.Errorf("results = (%v, %v), want (true, nil)", generatedOK, err)
					}
					close(returned)
				}()
				synctest.Wait()
				select {
				case <-otherDone:
				default:
					t.Error("unblocked callback did not finish")
				}
				select {
				case <-returned:
					t.Error("helper returned without joining blocked callback")
				default:
				}
				close(release)
				<-returned
			})
		})
	}
}

// This is a virtual-time shutdown budget model, not a production latency measurement.
func TestDrainShutdownWorkVirtualBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		signalCtx, signalCancel := context.WithCancel(context.Background())
		signalCancel()
		httpTimer := time.NewTimer(10 * time.Second)
		<-httpTimer.C
		var prerequisiteElapsed time.Duration
		generatedOK, err := drainShutdownWork(func(ctx context.Context) error {
			if signalCtx.Err() == nil || ctx.Err() != nil {
				t.Error("drain must have a live context after signal cancellation")
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) != 5*time.Second {
				t.Errorf("fresh prerequisite deadline = %v, present = %v", deadline, ok)
			}
			<-ctx.Done()
			prerequisiteElapsed = time.Since(start)
			return ctx.Err()
		}, func() bool {
			timer := time.NewTimer(17 * time.Second)
			<-timer.C
			return true
		})
		if !generatedOK || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("results = (%v, %v)", generatedOK, err)
		}
		if prerequisiteElapsed != 15*time.Second || time.Since(start) != 27*time.Second {
			t.Fatalf("virtual prerequisite finish = %v, shutdown = %v; want 15s, 27s",
				prerequisiteElapsed, time.Since(start))
		}
	})
}
