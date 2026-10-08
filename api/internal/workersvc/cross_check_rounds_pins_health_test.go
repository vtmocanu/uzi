package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type roundsPinsHealthStore struct{ *roundsHealthStore }

func (f *roundsPinsHealthStore) CountOnlineWorkersSatisfyingCustomCodex(context.Context, uuid.UUID) (int64, error) {
	return 1, nil
}

func TestHealthRoundsPinsSameWorker(t *testing.T) {
	all := append(checkerPinCaps(true, true), capability.CrossCheckRoundsV1)
	for _, tc := range []struct {
		name         string
		round        int32
		pins, custom bool
		workers      []store.ListWorkersByUserRow
		want         string
	}{
		{"round1 legacy unpinned", 1, false, false, []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: checkerPinCaps(false, false)}}, reasonWaitingWorker},
		{"round2 unpinned", 2, false, false, []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: append(checkerPinCaps(false, false), capability.CrossCheckRoundsV1)}}, reasonWaitingWorker},
		{"round2 pins custom together", 2, true, true, []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: all}}, reasonWaitingWorker},
		{"split rounds pins custom", 2, true, true, []store.ListWorkersByUserRow{
			{Status: "online", ProtocolCapabilities: append(checkerPinCaps(false, true), capability.CrossCheckRoundsV1)},
			{Status: "online", ProtocolCapabilities: checkerPinCaps(true, true)},
		}, reasonNoCrossCheckRoundsCapableWorker},
		{"pins without custom", 2, true, true, []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: append(checkerPinCaps(true, false), capability.CrossCheckRoundsV1)}}, reasonNoCrossCheckRoundsCapableWorker},
		{"runtime missing", 2, true, true, []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: []string{capability.CrossCheckV1, capability.CrossCheckRoundsV1, capability.CrossCheckPinsV1, capability.CodexCustomModelV1, capability.CodexHarnessV1}}}, reasonNoCrossCheckRoundsCapableWorker},
		{"maintenance fenced", 2, true, true, []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: all, MaintenanceFenced: true}}, reasonNoCrossCheckRoundsCapableWorker},
		{"maintenance requested", 2, true, true, []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: all, MaintenancePhase: "requested"}}, reasonNoCrossCheckRoundsCapableWorker},
		{"draining", 2, true, true, []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: all, DrainingSince: pgtype.Timestamptz{Time: t0, Valid: true}}}, reasonNoCrossCheckRoundsCapableWorker},
		{"offline", 2, true, true, []store.ListWorkersByUserRow{{Status: "offline", ProtocolCapabilities: all}}, reasonNoCrossCheckRoundsCapableWorker},
		{"ephemeral", 2, true, true, []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: all, Ephemeral: true}}, reasonNoCrossCheckRoundsCapableWorker},
		{"isolated", 2, true, true, []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: all, IsolatedLane: true}}, reasonNoCrossCheckRoundsCapableWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute)
			r.CrossCheckPinRequired, r.CodexCustomRoot = tc.pins, tc.custom
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1,
				crossCheckRun: store.Run{ID: r.ID, Kind: "cross_check", Harness: "codex"}, crossCheckWorkers: tc.workers}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.q = &roundsPinsHealthStore{&roundsHealthStore{healthFakeStore: fs, round: tc.round}}
			svc.detectRunHealth(context.Background(), t0)
			got := lastWrite(t, fs, r.ID)
			if got.Health != healthWaitingWorker || got.HealthReason.String != tc.want {
				t.Fatalf("health=%s reason=%q want=%q", got.Health, got.HealthReason.String, tc.want)
			}
		})
	}
}

func TestHealthRoundsPinsLookupErrorFallsThrough(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute)
	r.CrossCheckPinRequired, r.CodexCustomRoot = true, true
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1,
		crossCheckRun: store.Run{ID: r.ID, Kind: "cross_check", Harness: "codex"}, priorityClass: map[uuid.UUID]string{r.ID: "background"}}
	q := &roundsHealthStore{healthFakeStore: fs, protocolErr: errors.New("protocol unavailable")}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.q = &roundsPinsHealthStore{q}
	svc.detectRunHealth(context.Background(), t0)
	got := lastWrite(t, fs, r.ID)
	if got.HealthReason.String != reasonDeprioritized || len(q.protocolCalls) != 1 || len(fs.crossCheckWorkerCalls) != 0 || len(fs.priorityCalls) != 1 {
		t.Fatalf("lookup failure did not fall through: reason=%q protocol=%v fleet=%v priority=%v", got.HealthReason.String, q.protocolCalls, fs.crossCheckWorkerCalls, fs.priorityCalls)
	}
}
