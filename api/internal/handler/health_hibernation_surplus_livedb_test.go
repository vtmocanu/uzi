package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/healthsvc"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestAdminHealthHibernationSurplusModeLiveDB drives fleet.hibernation through GET
// /api/admin/health with worker_hibernation_surplus on (issue #4395 M2). In surplus mode a pooled
// worker is judged by the owner-clock long anchor, so:
//   - a pooled worker of an owner with no recent activity and a controller_refused block is listed,
//     with the controller's reason rendered through the same label as today (including
//     roll_cohort_cleanup_pending);
//   - an old unused pooled worker of an owner that is active elsewhere is NOT listed, and neither is
//     one whose only recorded block is a surplus refusal (a real pass recorded it);
//   - an unpooled worker is still judged by today's per-worker anchor, and its parked run still
//     raises the check;
//   - the same blocked pooled worker IS listed with the surplus setting off (today's query), which
//     is the difference the owner clock makes.
//
// Assertions are per seeded worker, matched by unique name: the shared database may hold other
// tests' workers. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestAdminHealthHibernationSurplusModeLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	ctx := context.Background()
	admin := cliSeedUser(t, pool, true)
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)

	now := time.Now().UTC().Truncate(time.Second)
	rows := func(surplus string) []store.AppSetting {
		return []store.AppSetting{
			{Key: settings.KeyWorkerHibernationEnabled, Value: "true"},
			{Key: settings.KeyWorkerHibernationAuto, Value: "true"},
			{Key: settings.KeyWorkerHibernationIdleAfter, Value: "336h"},
			{Key: settings.KeyWorkerHibernationSurplus, Value: surplus},
			{Key: settings.KeyWorkerHibernationSurplusIdleAfter, Value: "2h"},
		}
	}
	on := settings.New(&settingsStore{rows: rows("true")}, time.Minute)
	off := settings.New(&settingsStore{rows: rows("false")}, time.Minute)
	useSettings := func(s *settings.Cache) {
		h.SetHealthService(healthsvc.New(healthsvc.Config{Store: h.q, Pool: pool, Settings: s, Now: func() time.Time { return now }}))
	}
	useSettings(on)
	h.now = func() time.Time { return now }

	svc := workersvc.New(h.q, h.box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), WorkerHeartbeatStale: time.Minute})
	svc.SetTxBeginner(pool)
	svc.SetVault(h.vault)
	svc.SetHibernationSettings(on)
	svc.SetHibernationAutoSettings(on)
	svc.SetClock(func() time.Time { return now })
	advertise := func() {
		t.Helper()
		if err := h.q.UpsertControllerHibernationAdvertisement(ctx, store.UpsertControllerHibernationAdvertisementParams{
			AdvertisedAt:        pgtype.Timestamptz{Time: now, Valid: true},
			HibernationVersion:  pgtype.Int4{Int32: 1, Valid: true},
			PollIntervalSeconds: pgtype.Int4{Int32: 300, Valid: true},
		}); err != nil {
			t.Fatalf("advertise: %v", err)
		}
	}
	advertise()

	type evidence struct{ Label, Value string }
	read := func() (severity string, ev []evidence) {
		t.Helper()
		now = now.Add(10 * time.Second) // step past the handler's document cache
		rec := bearerReq(router, http.MethodGet, "/api/admin/health", adminUza)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/admin/health = %d: %s", rec.Code, rec.Body.String())
		}
		var doc struct {
			Checks []struct {
				ID       string `json:"id"`
				Severity string `json:"severity"`
				Evidence []struct {
					Label string `json:"label"`
					Value string `json:"value"`
				} `json:"evidence"`
			} `json:"checks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, c := range doc.Checks {
			if c.ID == "fleet.hibernation" {
				for _, e := range c.Evidence {
					ev = append(ev, evidence{e.Label, e.Value})
				}
				return c.Severity, ev
			}
		}
		t.Fatal("fleet.hibernation missing from the health doc")
		return "", nil
	}
	listed := func(ev []evidence, name string) (string, bool) {
		for _, e := range ev {
			if e.Label == "Worker" && strings.HasPrefix(e.Value, "worker "+name+" ") {
				return e.Value, true
			}
		}
		return "", false
	}

	// Worker names are 8 bytes: the evidence value keeps at least that much of a name once the
	// controller-refusal suffix is long, so a longer name could be cut.
	tag := func(prefix string) string { return prefix + uuid.NewString()[:7] }
	owners := []uuid.UUID{cliSeedUser(t, pool, false), cliSeedUser(t, pool, false)}
	inactive, active := owners[0], owners[1]
	var created []uuid.UUID
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM recovery_custody_holds WHERE user_id = ANY($1)`, owners)
		_, _ = pool.Exec(ctx, `DELETE FROM runs WHERE user_id = ANY($1)`, owners)
		_, _ = pool.Exec(ctx, `DELETE FROM workers WHERE id = ANY($1)`, created)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`, owners)
	})
	newWorker := func(owner uuid.UUID, name string, pooled bool, createdAgo time.Duration) uuid.UUID {
		id, pod := uuid.New(), uuid.New()
		var docker any
		if pooled {
			docker = false
		}
		cliMustExec(t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, docker_enabled, max_concurrent_runs, registered_pod_uid, created_at, last_heartbeat_at)
		                      VALUES ($1,$2,$3,$4,'online','hosted','m','base',$5,2,$6,$7,$8)`, id, owner, name, id[:], docker, pod, now.Add(-createdAgo), now)
		created = append(created, id)
		return id
	}
	block := func(id uuid.UUID, reason, controllerReason string) {
		cliMustExec(t, pool, `UPDATE workers SET hibernation_blocked_reason = $2, hibernation_blocked_at = $3, hibernation_blocked_since = $3,
		                      hibernation_reason = NULLIF($4, '') WHERE id = $1`, id, reason, now, controllerReason)
	}
	// cordon really cordons the worker (so the real pass keeps refusing it) and records the block.
	cordon := func(id uuid.UUID, owner uuid.UUID) {
		cliMustExec(t, pool, `UPDATE workers SET manual_cordoned_at = $2, manual_cordoned_by = $3, manual_cordoned_by_label = 'admin' WHERE id = $1`, id, now, owner)
		block(id, "manual_cordon", "")
	}
	run := func(owner uuid.UUID, kind, status string, worker any, createdAgo, statusAgo time.Duration, finished bool) {
		var fin any
		if finished {
			fin = now.Add(-statusAgo)
		}
		cliMustExec(t, pool, `INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, worker_id, created_at, status_since, finished_at)
		                      VALUES ($1,$2,$3,'t','d',$4,$5,$6,$7,$8)`, uuid.New(), owner, kind, status, worker, now.Add(-createdAgo), now.Add(-statusAgo), fin)
	}

	// Owner `inactive`: two pooled workers, no run ever. Owner `active`: pooled workers and an
	// unpooled one, with a run that finished an hour ago.
	refusedName := tag("r")
	refused := newWorker(inactive, refusedName, true, 60*24*time.Hour)
	newWorker(inactive, tag("s"), true, 60*24*time.Hour)
	block(refused, workersvc.BlockedControllerRefused, "roll_cohort_cleanup_pending")

	run(active, "chat", "completed", nil, time.Hour, time.Hour, true)
	activeOldName, surplusRefusedName := tag("a"), tag("f")
	activeOld := newWorker(active, activeOldName, true, 60*24*time.Hour)
	cordon(activeOld, active)
	surplusRefused := newWorker(active, surplusRefusedName, true, 6*time.Hour)
	keeper := newWorker(active, tag("k"), true, time.Hour)
	// A pooled worker whose only parked run is 30 days old: the owner is active now (non-terminal
	// run), so the pooled worker is not long-idle and the parked kind covers unpooled workers only.
	pooledParkedName := tag("p")
	pooledParked := newWorker(active, pooledParkedName, true, 60*24*time.Hour)
	run(active, "chat", "paused", pooledParked, 30*24*time.Hour, 30*24*time.Hour, false)
	// The unpooled workers: judged by today's anchor whatever the owner does.
	unpooledName, unpooledParkedName := tag("u"), tag("q")
	unpooled := newWorker(active, unpooledName, false, 60*24*time.Hour)
	cordon(unpooled, active)
	unpooledParked := newWorker(active, unpooledParkedName, false, 60*24*time.Hour)
	run(active, "chat", "paused", unpooledParked, 30*24*time.Hour, 30*24*time.Hour, false)

	// A real surplus refusal: an open custody hold on the candidate makes the pass record custody_hold.
	holdRun := uuid.New()
	cliMustExec(t, pool, `INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, created_at, finished_at, status_since)
	                      VALUES ($1,$2,'chat','t','d','completed',$3,$3,$3)`, holdRun, active, now.Add(-2*time.Hour))
	cliMustExec(t, pool, `INSERT INTO recovery_custody_holds(id,user_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
	                      VALUES($1,$2,$3,1,'open',$4,'ident',$4,$3,true)`, uuid.New(), active, holdRun, surplusRefused)
	for i := 0; i < 20; i++ { // residue workers of other tests may be served first
		now = now.Add(2 * time.Minute)
		advertise()
		cliMustExec(t, pool, `UPDATE workers SET last_heartbeat_at = $2 WHERE id = ANY($1)`, created, now)
		if _, err := svc.HibernationAutoPass(ctx); err != nil {
			t.Fatalf("HibernationAutoPass: %v", err)
		}
		var reason string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(hibernation_blocked_reason,'') FROM workers WHERE id = $1`, surplusRefused).Scan(&reason); err != nil {
			t.Fatal(err)
		}
		if reason == workersvc.BlockedCustodyHold {
			break
		}
		if i == 19 {
			t.Fatalf("the pass never recorded the surplus refusal (reason %q)", reason)
		}
	}
	_ = keeper

	sev, ev := read()
	if sev != "warn" {
		t.Fatalf("severity = %q, want warn: %+v", sev, ev)
	}
	if v, ok := listed(ev, refusedName); !ok || v != fmt.Sprintf("worker %s idle 60d, hibernation blocked: controller refused (roll cohort cleanup pending)", refusedName) {
		t.Fatalf("controller-refused pooled worker = %q (listed %v), all: %+v", v, ok, ev)
	}
	for _, name := range []string{activeOldName, surplusRefusedName, pooledParkedName} {
		if v, ok := listed(ev, name); ok {
			t.Fatalf("%s must not warn in surplus mode (owner active elsewhere / surplus refusal only): %q", name, v)
		}
	}
	if v, ok := listed(ev, unpooledName); !ok || v != fmt.Sprintf("worker %s idle 60d, hibernation blocked: manual cordon", unpooledName) {
		t.Fatalf("unpooled worker = %q (listed %v), all: %+v", v, ok, ev)
	}
	if v, ok := listed(ev, unpooledParkedName); !ok || !strings.Contains(v, "blocked: assigned run parked") {
		t.Fatalf("unpooled worker with a parked run = %q (listed %v)", v, ok)
	}

	// Control: with the setting off the same database state is judged by today's per-worker
	// anchor, so the old cordoned pooled worker of the active owner IS listed. This is what the
	// owner clock changes, and only in surplus mode.
	useSettings(off)
	_, ev = read()
	if _, ok := listed(ev, activeOldName); !ok {
		t.Fatalf("control: with surplus off %s must be listed by today's query: %+v", activeOldName, ev)
	}
}
