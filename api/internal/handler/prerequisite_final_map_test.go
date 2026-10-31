package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func closureFinalHandler() *Handler {
	return &Handler{
		cfg:  config.Config{PrerequisiteClosureForges: []string{"github"}},
		wsvc: workersvc.New(nil, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()}),
	}
}

// The closure-backed FINAL needs the port, the GitHub knob and a limiter. The limiter arrives
// with the worker routes, so either boot order must end with all three wired, and a missing
// limiter must leave the feature off (a nil gate denies).
func TestPrerequisiteFinalWiredInEitherBootOrder(t *testing.T) {
	limiter := mw.NewLimiter(1, time.Minute, nil)
	t.Run("recovery service first", func(t *testing.T) {
		h := closureFinalHandler()
		svc := h.RecoveryService()
		if svc.PrerequisiteFinalEnabled() {
			t.Fatal("enabled before the worker routes supplied a limiter")
		}
		h.WorkerRoutes(limiter)
		if h.recovery() != svc || !svc.PrerequisiteFinalEnabled() {
			t.Fatal("the shared service was not wired by the worker routes")
		}
	})
	t.Run("worker routes first", func(t *testing.T) {
		h := closureFinalHandler()
		h.WorkerRoutes(limiter)
		if svc := h.RecoveryService(); !svc.PrerequisiteFinalEnabled() {
			t.Fatal("lazily built service lost the limiter")
		}
	})
	t.Run("no limiter", func(t *testing.T) {
		h := closureFinalHandler()
		h.WorkerRoutes(nil)
		if h.RecoveryService().PrerequisiteFinalEnabled() {
			t.Fatal("enabled without a limiter")
		}
	})
	t.Run("knob off", func(t *testing.T) {
		h := closureFinalHandler()
		h.cfg.PrerequisiteClosureForges = nil
		h.WorkerRoutes(limiter)
		if h.RecoveryService().PrerequisiteFinalEnabled() {
			t.Fatal("enabled with the GitHub knob off")
		}
	})
}

func TestPrerequisiteFinalAllowForIsPerWorkerAndApartFromHeld(t *testing.T) {
	if prerequisiteFinalAllowFor(nil) != nil {
		t.Fatal("a nil limiter must yield a nil (denying) gate")
	}
	lim := mw.NewLimiter(1, time.Minute, nil)
	final, held := prerequisiteFinalAllowFor(lim), heldListAllowFor(lim)
	a, b := uuid.New(), uuid.New()
	if !final(a) || final(a) {
		t.Fatal("worker a: want one admission, then refused")
	}
	if !final(b) {
		t.Fatal("worker b shares worker a's bucket")
	}
	if !held(a) {
		t.Fatal("the held-ref bucket shares the closure bucket")
	}
}
