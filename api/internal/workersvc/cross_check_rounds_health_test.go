package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type roundsHealthStore struct {
	*healthFakeStore
	round         int32
	protocolErr   error
	protocolCalls []uuid.UUID
}

func (f *roundsHealthStore) GetCrossCheckChildProtocol(_ context.Context, id uuid.UUID) (int32, error) {
	f.protocolCalls = append(f.protocolCalls, id)
	round := f.round
	if round == 0 {
		round = 2
	}
	return round, f.protocolErr
}

func TestHealthCrossCheckProtocolLookupFailureFallsThrough(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		class string
		want  string
	}{
		{"no rows generic", pgx.ErrNoRows, "normal", reasonWaitingWorker},
		{"error generic", errors.New("protocol lookup unavailable"), "normal", reasonWaitingWorker},
		{"no rows priority", pgx.ErrNoRows, "background", reasonDeprioritized},
		{"error priority", errors.New("protocol lookup unavailable"), "background", reasonDeprioritized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute)
			fs := &healthFakeStore{
				active:        []store.ListActiveRunsForHealthRow{r},
				onlineWorkers: 1, freeSlotWorkers: 1,
				crossCheckRun: store.Run{ID: r.ID, Kind: "cross_check"},
				priorityClass: map[uuid.UUID]string{r.ID: tc.class},
			}
			q := &roundsHealthStore{healthFakeStore: fs, protocolErr: tc.err}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.q = q
			svc.detectRunHealth(context.Background(), t0)

			got := lastWrite(t, fs, r.ID)
			if got.Health != healthWaitingWorker || got.HealthReason.String != tc.want {
				t.Errorf("health=%s reason=%q want=%q", got.Health, got.HealthReason.String, tc.want)
			}
			if got.HealthReason.String == reasonNoCrossCheckCapableWorker ||
				got.HealthReason.String == reasonNoCrossCheckRoundsCapableWorker {
				t.Errorf("lookup failure returned capability gap %q", got.HealthReason.String)
			}
			if len(q.protocolCalls) != 1 || q.protocolCalls[0] != r.ID {
				t.Errorf("protocol lookups=%v want [%s]", q.protocolCalls, r.ID)
			}
			if len(fs.crossCheckWorkerCalls) != 0 {
				t.Errorf("fleet queried after lookup failure: %v", fs.crossCheckWorkerCalls)
			}
			if len(fs.priorityCalls) != 1 {
				t.Errorf("priority lookups=%v want one downstream lookup", fs.priorityCalls)
			}
			var record struct {
				Level string
				RunID string `json:"run_id"`
				Error string
			}
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
				t.Errorf("expected error log, got %q: %v", logs.String(), err)
			} else if record.Level != "ERROR" || record.RunID != r.ID.String() || record.Error != tc.err.Error() {
				t.Errorf("error log=%+v want ERROR run_id=%s error=%q", record, r.ID, tc.err.Error())
			}
		})
	}
}

func TestHealthRoundOneRequiresCrossCheckCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string
		want string
	}{
		{"missing", nil, reasonNoCrossCheckCapableWorker},
		{"legacy capable", []string{"cross_check_v1"}, reasonWaitingWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute)
			fs := &healthFakeStore{
				active:        []store.ListActiveRunsForHealthRow{r},
				onlineWorkers: 1, freeSlotWorkers: 1,
				crossCheckRun:     store.Run{ID: r.ID, Kind: "cross_check"},
				crossCheckWorkers: []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: tc.caps}},
			}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.q = &roundsHealthStore{healthFakeStore: fs, round: 1}
			svc.detectRunHealth(context.Background(), t0)
			got := lastWrite(t, fs, r.ID)
			if got.Health != healthWaitingWorker || got.HealthReason.String != tc.want {
				t.Fatalf("health=%s reason=%q want=%q", got.Health, got.HealthReason.String, tc.want)
			}
		})
	}
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
			svc.q = &roundsHealthStore{healthFakeStore: fs}
			svc.detectRunHealth(context.Background(), t0)
			got := lastWrite(t, fs, r.ID)
			if got.Health != healthWaitingWorker || got.HealthReason.String != tc.want {
				t.Fatalf("health=%s reason=%q want=%q", got.Health, got.HealthReason.String, tc.want)
			}
		})
	}
}
