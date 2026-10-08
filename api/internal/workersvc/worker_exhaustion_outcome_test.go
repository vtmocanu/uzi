package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRegisterPublishesActualExhaustionDisposition(t *testing.T) {
	for _, laterStatus := range []string{"failed", "queued"} {
		t.Run(laterStatus, func(t *testing.T) {
			run := uuid.New()
			fs := &fakeStore{runByIDPlain: store.Run{ID: run, Status: laterStatus}, runByIDPlainErr: errors.New("later reads unavailable")}
			svc := New(fs, newBox(t), testParams())
			spy := &stateSpy{}
			svc.SetBroadcaster(spy)
			svc.publishRegisterOutcome(context.Background(), []store.WorkerRecoveryDisposition{{ID: run, Status: "recovery_wait"}}, nil, nil, nil)
			if got, ok := spy.statusFor(run); !ok || got != "recovery_wait" {
				t.Fatalf("published %q,%v instead of actual park", got, ok)
			}
			if fs.runByIDPlainCalls != 0 {
				t.Fatalf("park attempted %d judge reloads", fs.runByIDPlainCalls)
			}
			if fs.createdJudgeRun != nil {
				t.Fatal("park was offered to the judge")
			}
		})
	}
}
