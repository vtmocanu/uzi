package workersvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func progressReport(t *testing.T, gen int64, cause, evidence string) StateRequest {
	t.Helper()
	var req StateRequest
	body := fmt.Sprintf("{\"status\":\"recovery_wait\",\"claim_generation\":%d%s%s}", gen, cause, evidence)
	// Permissive decoding deliberately lets the baseline ignore the new extension.
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func assertProgressStreak(t *testing.T, row ppRow, count int32, start pgtype.Timestamptz) {
	t.Helper()
	if row.count != count || row.streakStart.Valid != start.Valid || (start.Valid && !row.streakStart.Time.Equal(start.Time)) {
		t.Fatalf("streak count=%d start=%v; want %d/%v", row.count, row.streakStart, count, start)
	}
}

func (e interlockLiveDB) progressReclaim(t *testing.T, svc *Service, worker store.Worker, id uuid.UUID) int64 {
	t.Helper()
	before := e.ppRun(t, id)
	e.exec(t, "UPDATE runs SET recovery_retry_not_before = now() - interval '1 minute' WHERE id = $1", id)
	if _, err := e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now())); err != nil {
		t.Fatal(err)
	}
	promoted := e.ppRun(t, id)
	if promoted.status != "queued" {
		t.Fatalf("promoted status=%s", promoted.status)
	}
	assertProgressStreak(t, promoted, before.count, before.streakStart)
	fresh, err := e.q.ClaimRun(e.ctx, claimRunParams(worker))
	if err != nil {
		t.Fatalf("actual ClaimRun: %v", err)
	}
	if fresh.ID != id || fresh.ClaimGeneration != before.generation+1 {
		t.Fatalf("unexpected actual claim: %+v", fresh)
	}
	assertProgressStreak(t, e.ppRun(t, id), before.count, before.streakStart)
	if _, applied, err := svc.SetState(e.ctx, worker, id, StateRequest{State: "running", ClaimGeneration: i64Ptr(fresh.ClaimGeneration)}); err != nil || !applied {
		t.Fatalf("running: applied=%v err=%v", applied, err)
	}
	assertProgressStreak(t, e.ppRun(t, id), before.count, before.streakStart)
	return fresh.ClaimGeneration
}

func TestProviderProgressFreshStreakLiveDB(t *testing.T) {
	for _, cause := range []string{"", ",\"recovery_cause\":\"empty_turn\"", ",\"recovery_cause\":\"data_volume_full\"", ",\"recovery_cause\":\"data_volume_full\",\"disk_park_preventive\":true"} {
		for _, evidence := range []struct {
			name, wire string
			positive   bool
		}{
			{"positive", ",\"provider_progress_turns\":2", true},
			{"zero", ",\"provider_progress_turns\":0", false},
			{"omitted", "", false},
		} {
			t.Run(cause+"/"+evidence.name, func(t *testing.T) {
				e := setupInterlockLiveDB(t)
				svc, clock := e.providerParkService(t, ppBound)
				start := *clock
				w := e.seedWorker(t, nil)
				worker := store.Worker{ID: w, UserID: e.userID}
				id := e.fpSeedRunning(t, w, 1)
				if _, applied, err := svc.SetState(e.ctx, worker, id, providerParkReport(1, nil)); err != nil || !applied {
					t.Fatalf("first provider park: %v/%v", applied, err)
				}
				gen := e.progressReclaim(t, svc, worker, id)
				req := progressReport(t, gen, cause, evidence.wire)
				if _, applied, err := svc.SetState(e.ctx, worker, id, req); err != nil || !applied {
					t.Fatalf("progress park: %v/%v", applied, err)
				}
				wantCount, wantStart := int32(1), pgconv.Time(start)
				if evidence.positive {
					wantCount, wantStart = 0, pgtype.Timestamptz{}
				}
				assertProgressStreak(t, e.ppRun(t, id), wantCount, wantStart)
				if _, applied, err := svc.SetState(e.ctx, worker, id, req); err != nil || applied {
					t.Fatalf("duplicate: applied=%v err=%v", applied, err)
				}
				assertProgressStreak(t, e.ppRun(t, id), wantCount, wantStart)
				gen = e.progressReclaim(t, svc, worker, id)
				*clock = start.Add(ppBound + time.Hour)
				if _, applied, err := svc.SetState(e.ctx, worker, id, providerParkReport(gen, nil)); err != nil || !applied {
					t.Fatalf("next provider park: %v/%v", applied, err)
				}
				row := e.ppRun(t, id)
				if evidence.positive {
					if row.status != "recovery_wait" {
						t.Fatalf("fresh streak: status=%s hold=%v", row.status, row.holdReason)
					}
					assertProgressStreak(t, row, 1, pgconv.Time(*clock))
					// Replay the OLD progress only after a nonzero NEW streak exists.
					disk := req.RecoveryCause != nil && *req.RecoveryCause == recoveryCauseDataVolumeFull
					if _, applied, err := svc.SetState(e.ctx, worker, id, req); applied || (disk && !errors.Is(err, ErrStaleClaim)) || (!disk && err != nil) {
						t.Fatalf("old replay: applied=%v err=%v", applied, err)
					}
					assertProgressStreak(t, e.ppRun(t, id), 1, pgconv.Time(*clock))
					e.progressReclaim(t, svc, worker, id)
					if _, applied, err := svc.SetState(e.ctx, worker, id, req); applied || (disk && !errors.Is(err, ErrStaleClaim)) || (!disk && err != nil) {
						t.Fatalf("reclaimed replay: applied=%v err=%v", applied, err)
					}
					assertProgressStreak(t, e.ppRun(t, id), 1, pgconv.Time(*clock))
				} else {
					if row.status != "paused" || row.holdReason.String != "provider_unavailable" {
						t.Fatalf("preserved streak: status=%s hold=%v", row.status, row.holdReason)
					}
					assertProgressStreak(t, row, 2, pgconv.Time(start))
				}
			})
		}
	}
}

func TestProviderProgressFenceLiveDB(t *testing.T) {
	for _, cause := range []*string{nil, strPtr("empty_turn"), strPtr(recoveryCauseDataVolumeFull)} {
		for _, released := range []bool{false, true} {
			name := "untyped"
			if cause != nil {
				name = *cause
			}
			t.Run(fmt.Sprintf("%s/released=%v", name, released), func(t *testing.T) {
				e := setupInterlockLiveDB(t)
				svc, clock := e.providerParkService(t, ppBound)
				w := e.seedWorker(t, nil)
				id := e.fpSeedRunning(t, w, 2)
				e.seedStreak(t, id, 3, *clock)
				if released {
					e.exec(t, "UPDATE runs SET claim_released_at = now() WHERE id = $1", id)
				}
				gen := int64(1)
				if released {
					gen = 2
				}
				before := e.ppRun(t, id)
				_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{State: "recovery_wait", RecoveryCause: cause, ClaimGeneration: i64Ptr(gen), ProviderProgressTurns: i32Ptr(2)})
				disk := cause != nil && *cause == recoveryCauseDataVolumeFull
				if applied || (disk && !errors.Is(err, ErrStaleClaim)) || (!disk && err != nil) {
					t.Fatalf("fence: applied=%v err=%v", applied, err)
				}
				if after := e.ppRun(t, id); after != before {
					t.Fatalf("fenced report mutated run: before=%+v after=%+v", before, after)
				}
			})
		}
	}
}

func TestProviderProgressInvalidLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, ppBound)
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 1)
	e.seedStreak(t, id, 3, *clock)
	before := e.ppRun(t, id)
	for name, req := range map[string]StateRequest{
		"negative":            {State: "recovery_wait", ClaimGeneration: i64Ptr(1), ProviderProgressTurns: i32Ptr(-1)},
		"state":               {State: "running", ClaimGeneration: i64Ptr(1), ProviderProgressTurns: i32Ptr(1)},
		"provider":            {State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseProviderOutage), ClaimGeneration: i64Ptr(1), ProviderProgressTurns: i32Ptr(1)},
		"vault":               {State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseVaultLocked), ClaimGeneration: i64Ptr(1), ProviderProgressTurns: i32Ptr(1)},
		"forge":               {State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1), ProviderProgressTurns: i32Ptr(1)},
		"codex":               {State: "recovery_wait", RecoveryCause: strPtr("codex_account_unavailable"), ClaimGeneration: i64Ptr(1), ProviderProgressTurns: i32Ptr(1)},
		"nil gen":             {State: "recovery_wait", ProviderProgressTurns: i32Ptr(1)},
		"zero gen":            {State: "recovery_wait", ClaimGeneration: i64Ptr(0), ProviderProgressTurns: i32Ptr(0)},
		"negative gen":        {State: "recovery_wait", ClaimGeneration: i64Ptr(-1), ProviderProgressTurns: i32Ptr(1)},
		"productive ordinary": {State: "recovery_wait", ClaimGeneration: i64Ptr(1), ProductiveTurns: i32Ptr(1)},
		"productive disk":     {State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseDataVolumeFull), ClaimGeneration: i64Ptr(1), ProductiveTurns: i32Ptr(1)},
	} {
		_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, req)
		if !errors.Is(err, ErrInvalidState) || applied {
			t.Fatalf("%s: applied=%v err=%v", name, applied, err)
		}
		if after := e.ppRun(t, id); after != before {
			t.Fatalf("%s mutated run", name)
		}
	}
}

func TestProviderProgressSQLFenceLiveDB(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, gen := range []pgtype.Int8{{}, {Int64: 0, Valid: true}, {Int64: 1, Valid: true}, {Int64: 2, Valid: true}} {
			t.Run(fmt.Sprintf("disk=%v/gen=%v", disk, gen), func(t *testing.T) {
				e := setupInterlockLiveDB(t)
				w := e.seedWorker(t, nil)
				id := e.fpSeedRunning(t, w, 2)
				start := time.Now().UTC().Truncate(time.Microsecond)
				e.seedStreak(t, id, 3, start)
				var rows int64
				var err error
				if disk {
					_, err = e.q.ParkRunDataVolumeFull(e.ctx, store.ParkRunDataVolumeFullParams{ID: id, WorkerID: pgconv.UUID(w), ClaimGeneration: gen, ResetProviderStreak: true, RetryNotBefore: pgconv.Time(start)})
					if errors.Is(err, pgx.ErrNoRows) {
						err = nil
					} else if err == nil {
						rows = 1
					}
				} else {
					rows, err = e.q.SetRunRecoveryWait(e.ctx, store.SetRunRecoveryWaitParams{ID: id, WorkerID: pgconv.UUID(w), ClaimGeneration: gen, ResetProviderStreak: true, RetryNotBefore: pgconv.Time(start)})
				}
				if err != nil {
					t.Fatal(err)
				}
				if gen.Valid && gen.Int64 != 2 {
					if rows != 0 {
						t.Fatalf("mismatched gen rows=%d", rows)
					}
				} else if rows != 1 {
					t.Fatalf("park rows=%d", rows)
				}
				if gen.Valid && gen.Int64 == 2 {
					assertProgressStreak(t, e.ppRun(t, id), 0, pgtype.Timestamptz{})
				} else {
					assertProgressStreak(t, e.ppRun(t, id), 3, pgconv.Time(start))
				}
			})
		}
	}
}

func TestProviderProgressCodexFallbackPreservesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, w, _ := runningAccountParkFixture(t, env)
	start := time.Now().UTC().Truncate(time.Microsecond)
	env.exec("UPDATE runs SET provider_park_count=3, provider_park_streak_started_at=$2 WHERE id=$1", fx.runID, pgconv.Time(start))
	// The idle account takes codex_account_runpark's generic recovery writer.
	_, applied, err := reportAccountPark(t, fx, w)
	if err != nil || !applied {
		t.Fatalf("account fallback: applied=%v err=%v", applied, err)
	}
	run := mustRun(t, env, fx.runID)
	if run.Status != "recovery_wait" || run.RecoveryWaitCause.Valid || run.ProviderParkCount != 3 || !run.ProviderParkStreakStartedAt.Time.Equal(start) {
		t.Fatalf("fallback changed provider streak: %+v", run)
	}
}

func TestProviderProgressLegacyUnfencedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, clock := e.providerParkService(t, ppBound)
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 1)
	e.seedStreak(t, id, 3, *clock)
	_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{State: "recovery_wait"})
	if err != nil || !applied {
		t.Fatalf("legacy park: applied=%v err=%v", applied, err)
	}
	assertProgressStreak(t, e.ppRun(t, id), 3, pgconv.Time(*clock))
}

func TestProviderProgressDiskPolicyLiveDB(t *testing.T) {
	for _, outcome := range []string{"preventive", "stop", "cap"} {
		t.Run(outcome, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.diskParkService(t, 3)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			start := time.Now().UTC().Truncate(time.Microsecond)
			e.seedStreak(t, id, 3, start)
			e.exec(t, "UPDATE runs SET disk_park_count = 3 WHERE id = $1", id)
			if outcome == "stop" {
				e.exec(t, "UPDATE runs SET stop_kind = 'cancelled' WHERE id = $1", id)
			}
			req := dataVolumeFullReport(1, nil)
			req.ProviderProgressTurns = i32Ptr(2)
			if outcome == "preventive" {
				req.DiskParkPreventive = boolPtr(true)
			}
			run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, req)
			if err != nil || !applied {
				t.Fatalf("disk outcome: applied=%v err=%v", applied, err)
			}
			status, cause, disk, _, origin := e.dpRun(t, id)
			switch outcome {
			case "preventive":
				if status != "recovery_wait" || cause.String != recoveryCauseDataVolumeFull || disk != 3 || origin.Valid {
					t.Fatalf("preventive: %+v disk=%d", run, disk)
				}
				assertProgressStreak(t, e.ppRun(t, id), 0, pgtype.Timestamptz{})
			case "stop":
				if status != "cancelled" || cause.Valid || origin.Valid || disk != 3 {
					t.Fatalf("stop: %+v disk=%d", run, disk)
				}
				assertProgressStreak(t, e.ppRun(t, id), 3, pgconv.Time(start))
			case "cap":
				if status != "failed" || origin.String != recoveryCauseDataVolumeFull || disk != 3 {
					t.Fatalf("cap: %+v disk=%d", run, disk)
				}
				assertProgressStreak(t, e.ppRun(t, id), 3, pgconv.Time(start))
			}
		})
	}
}
