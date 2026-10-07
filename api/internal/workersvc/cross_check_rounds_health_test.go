package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type roundsHealthStore struct {
	*healthFakeStore
}

func (f *roundsHealthStore) GetCrossCheckChildProtocol(context.Context, uuid.UUID) (int32, error) {
	return 2, nil
}

func TestHealthRoundTwoRequiresOneCapableWorker(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps [][]string
		want string
	}{
		{"legacy", [][]string{{"cross_check_v1"}}, reasonNoCrossCheckRoundsCapableWorker},
		{"split fleet", [][]string{{"cross_check_v1"}, {"cross_check_rounds_v1"}}, reasonNoCrossCheckRoundsCapableWorker},
		{"capable", [][]string{{"cross_check_v1", "cross_check_rounds_v1"}}, reasonWaitingWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute)
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1,
				crossCheckRun: store.Run{ID: r.ID, Kind: "cross_check"}}
			for _, caps := range tc.caps {
				fs.crossCheckWorkers = append(fs.crossCheckWorkers, store.ListWorkersByUserRow{Status: "online", ProtocolCapabilities: caps})
			}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.q = &roundsHealthStore{fs}
			svc.detectRunHealth(context.Background(), t0)
			got := lastWrite(t, fs, r.ID)
			if got.Health != healthWaitingWorker || got.HealthReason.String != tc.want {
				t.Fatalf("health=%s reason=%q want=%q", got.Health, got.HealthReason.String, tc.want)
			}
		})
	}
}
