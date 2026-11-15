package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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

// TestAdminHealthHibernationAutoLiveDB drives the fleet.hibernation check through
// GET /api/admin/health (issue #4207, PRD example 3). A persistent hosted worker idle 20 days
// with an open recovery custody hold is refused by the REAL automatic pass
// (workersvc.HibernationAutoPass), which records custody_hold; the check is then `warn` with the
// worker listed. A second worker whose only run is paused beyond the threshold is listed as a
// parked blocker, and a third with BOTH a recorded reason and a 30-day paused run is listed once
// (as parked) and counted once. Workers the check must NOT list are seeded too: a stale recorded
// reason from before the worker's latest activity, a run paused for less than the threshold, and
// a worker with a paused and a running run. After the hold is released the real pass admits the
// worker (preparing) and the check no longer lists it.
//
// The shared database may hold other tests' workers (run-store-it never truncates them), so the
// assertions are per seeded worker, matched by unique name. The only count asserted is the
// Workers delta around the seeding: the pass is first run until it admits nothing, so residue
// workers no longer change between the baseline read and the read after seeding.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestAdminHealthHibernationAutoLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	ctx := context.Background()

	admin := cliSeedUser(t, pool, true)
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)

	now := time.Now().UTC().Truncate(time.Second)
	sett := settings.New(&settingsStore{rows: []store.AppSetting{
		{Key: settings.KeyWorkerHibernationEnabled, Value: "true"},
		{Key: settings.KeyWorkerHibernationAuto, Value: "true"},
		{Key: settings.KeyWorkerHibernationIdleAfter, Value: "336h"},
	}}, time.Minute)
	h.SetHealthService(healthsvc.New(healthsvc.Config{Store: h.q, Pool: pool, Settings: sett, Now: func() time.Time { return now }}))
	h.now = func() time.Time { return now }

	// The real automatic pass, on the same fixed clock, with a fresh controller advertisement.
	svc := workersvc.New(h.q, h.box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	svc.SetTxBeginner(pool)
	svc.SetVault(h.vault)
	svc.SetHibernationSettings(sett)
	svc.SetHibernationAutoSettings(sett)
	svc.SetClock(func() time.Time { return now })
	if err := h.q.UpsertControllerHibernationAdvertisement(ctx, store.UpsertControllerHibernationAdvertisementParams{
		AdvertisedAt:        pgtype.Timestamptz{Time: now, Valid: true},
		HibernationVersion:  pgtype.Int4{Int32: 1, Valid: true},
		PollIntervalSeconds: pgtype.Int4{Int32: 300, Valid: true},
	}); err != nil {
		t.Fatalf("advertise: %v", err)
	}

	type evidence struct{ Label, Value string }
	type check struct {
		ID       string     `json:"id"`
		Severity string     `json:"severity"`
		Evidence []evidence `json:"evidence"`
	}
	read := func() check {
		t.Helper()
		// The handler caches the document for a few seconds on its clock; step past it.
		now = now.Add(10 * time.Second)
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
				out := check{ID: c.ID, Severity: c.Severity}
				for _, e := range c.Evidence {
					out.Evidence = append(out.Evidence, evidence{e.Label, e.Value})
				}
				return out
			}
		}
		t.Fatal("fleet.hibernation missing from the health doc")
		return check{}
	}
	workersCount := func(c check) int {
		t.Helper()
		if len(c.Evidence) == 0 {
			return 0
		}
		if c.Evidence[0].Label != "Workers" {
			t.Fatalf("first evidence = %+v, want Workers", c.Evidence[0])
		}
		n, err := strconv.Atoi(c.Evidence[0].Value)
		if err != nil {
			t.Fatalf("Workers value %q: %v", c.Evidence[0].Value, err)
		}
		return n
	}
	listed := func(c check, name string) (string, bool) {
		for _, e := range c.Evidence {
			if e.Label == "Worker" && strings.HasPrefix(e.Value, "worker "+name+" ") {
				return e.Value, true
			}
		}
		return "", false
	}

	// advance moves the shared clock and refreshes the controller advertisement so the pass
	// (once per minute on its own clock) can run again.
	advance := func(d time.Duration) {
		t.Helper()
		now = now.Add(d)
		if err := h.q.UpsertControllerHibernationAdvertisement(ctx, store.UpsertControllerHibernationAdvertisementParams{
			AdvertisedAt:        pgtype.Timestamptz{Time: now, Valid: true},
			HibernationVersion:  pgtype.Int4{Int32: 1, Valid: true},
			PollIntervalSeconds: pgtype.Int4{Int32: 300, Valid: true},
		}); err != nil {
			t.Fatalf("advertise: %v", err)
		}
	}
	pass := func() int64 {
		t.Helper()
		advance(2 * time.Minute)
		n, err := svc.HibernationAutoPass(ctx)
		if err != nil {
			t.Fatalf("HibernationAutoPass: %v", err)
		}
		return n
	}
	// Settle residue workers from other tests: run the real pass until it admits nothing (it
	// admits a bounded number per run), then read the baseline.
	for i := 0; ; i++ {
		if pass() == 0 {
			break
		}
		if i > 50 {
			t.Fatal("the automatic pass keeps admitting residue workers")
		}
	}
	baseline := workersCount(read())

	// Seed: a repo, a persistent hosted worker idle 20 days with an open custody hold, and a
	// second worker whose only run is paused for 30 days.
	conn, repo := uuid.New(), uuid.New()
	cliMustExec(t, pool, `INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES ($1,$2,'gitlab','https://hib.example','bot',1,$3)`, conn, admin, []byte{1})
	cliMustExec(t, pool, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,enabled) VALUES ($1,$2,1,'g/hib','https://hib.example/g/hib',false)`, repo, conn)
	iid := int32(time.Now().UnixNano()%1_000_000_000) + 1
	newWorker := func(name string, createdAgo time.Duration) uuid.UUID {
		id, pod := uuid.New(), uuid.New()
		cliMustExec(t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, registered_pod_uid, created_at, last_heartbeat_at)
		                      VALUES ($1,$2,$3,$4,'online','hosted','m','base',$5,$6,$7)`, id, admin, name, id[:], pod, now.Add(-createdAgo), now)
		return id
	}
	newRun := func(worker uuid.UUID, status string, statusSince, finishedAt any) uuid.UUID {
		iid++
		id := uuid.New()
		cliMustExec(t, pool, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, status_since, finished_at)
		                      VALUES ($1,$2,$3,'issue',$4,'t','d',$5,$6,1,$7,$8)`, id, admin, repo, iid, status, worker, statusSince, finishedAt)
		return id
	}

	suffix := uuid.NewString()[:8]
	idleName, parkedName := "hib-idle-"+suffix, "hib-parked-"+suffix
	both := "hib-both-" + suffix
	idleID := newWorker(idleName, 20*24*time.Hour)
	parkedID := newWorker(parkedName, 60*24*time.Hour)
	bothID := newWorker(both, 60*24*time.Hour)
	all := []uuid.UUID{idleID, parkedID, bothID}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM recovery_custody_holds WHERE repo_id = $1`, repo)
		_, _ = pool.Exec(ctx, `DELETE FROM runs WHERE repo_id = $1`, repo)
		_, _ = pool.Exec(ctx, `DELETE FROM workers WHERE id = ANY($1)`, all)
		_, _ = pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, repo)
		_, _ = pool.Exec(ctx, `DELETE FROM forge_connections WHERE id = $1`, conn)
	})

	// A completed run on the idle worker finished 30 days ago (older than the worker, so the
	// worker's creation 20 days ago is the anchor) and the open hold that names it.
	doneRun := newRun(idleID, "completed", now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour))
	hold := uuid.New()
	cliMustExec(t, pool, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
	                      VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, admin, repo, doneRun, idleID)
	pausedRun := newRun(parkedID, "paused", now.Add(-30*24*time.Hour), nil)
	// A recorded reason AND a 30-day paused run: the worker is listed once, as parked.
	bothRun := newRun(bothID, "paused", now.Add(-30*24*time.Hour), nil)
	cliMustExec(t, pool, `UPDATE workers SET hibernation_blocked_reason = 'custody_hold', hibernation_blocked_at = $2, hibernation_blocked_since = $2 WHERE id = $1`, bothID, now)

	// The real pass refuses the held worker and records why.
	pass()
	var reason string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(hibernation_blocked_reason,'') FROM workers WHERE id = $1`, idleID).Scan(&reason); err != nil || reason != workersvc.BlockedCustodyHold {
		t.Fatalf("recorded reason = %q, %v; want custody_hold", reason, err)
	}

	c := read()
	if c.Severity != "warn" {
		t.Fatalf("severity = %q, want warn: %+v", c.Severity, c)
	}
	if got, want := workersCount(c), baseline+3; got != want {
		t.Fatalf("Workers = %d, want %d (baseline %d + 3 distinct seeded workers)", got, want, baseline)
	}
	if v, ok := listed(c, idleName); !ok || v != fmt.Sprintf("worker %s idle 20d, hibernation blocked: recovery custody hold", idleName) {
		t.Fatalf("idle worker evidence = %q (listed %v), all: %+v", v, ok, c.Evidence)
	}
	wantParked := fmt.Sprintf("worker %s blocked: assigned run parked (%s, paused)", parkedName, pausedRun.String()[:8])
	if v, ok := listed(c, parkedName); !ok || v != wantParked {
		t.Fatalf("parked worker evidence = %q (listed %v), want %q", v, ok, wantParked)
	}

	wantBoth := fmt.Sprintf("worker %s blocked: assigned run parked (%s, paused)", both, bothRun.String()[:8])
	if v, ok := listed(c, both); !ok || v != wantBoth {
		t.Fatalf("worker with a reason and a parked run = %q (listed %v), want %q", v, ok, wantBoth)
	}
	count := func(c check, name string) int {
		n := 0
		for _, e := range c.Evidence {
			if e.Label == "Worker" && strings.HasPrefix(e.Value, "worker "+name+" ") {
				n++
			}
		}
		return n
	}
	if n := count(c, both); n != 1 {
		t.Fatalf("worker with a reason and a parked run listed %d times, want 1", n)
	}

	// Workers the check must not list, seeded in state none after the pass above so nothing
	// has acted on them.
	// stale: idle past the threshold with a recorded reason last seen BEFORE its latest run
	// finished (blocked_at < anchor), so the marker is stale.
	staleName, youngName, mixedName := "hib-stale-"+suffix, "hib-young-"+suffix, "hib-mixed-"+suffix
	staleID := newWorker(staleName, 40*24*time.Hour)
	youngID := newWorker(youngName, 60*24*time.Hour)
	mixedID := newWorker(mixedName, 60*24*time.Hour)
	all = append(all, staleID, youngID, mixedID)
	newRun(staleID, "completed", now.Add(-25*24*time.Hour), now.Add(-25*24*time.Hour))
	cliMustExec(t, pool, `UPDATE workers SET hibernation_blocked_reason = 'manual_cordon', hibernation_blocked_at = $2, hibernation_blocked_since = $2 WHERE id = $1`, staleID, now.Add(-30*24*time.Hour))
	// young: its only run has been paused for 3 days, under the 14-day threshold.
	newRun(youngID, "paused", now.Add(-3*24*time.Hour), nil)
	// mixed: one paused run for 30 days and one running run.
	newRun(mixedID, "paused", now.Add(-30*24*time.Hour), nil)
	mixedRunning := newRun(mixedID, "running", now.Add(-30*24*time.Hour), nil)

	c = read()
	for _, name := range []string{staleName, youngName, mixedName} {
		if _, ok := listed(c, name); ok {
			t.Fatalf("%s must not be listed: %+v", name, c.Evidence)
		}
	}
	if got, want := workersCount(c), baseline+3; got != want {
		t.Fatalf("Workers = %d after seeding the unlisted workers, want %d", got, want)
	}

	// PRD example 3, end to end: release the custody hold, finish the parked runs, and run the
	// REAL pass again. The held worker (reason custody_hold, so at the back of the queue but
	// still eligible) is admitted and is no longer listed.
	cliMustExec(t, pool, `DELETE FROM recovery_custody_holds WHERE id = $1`, hold)
	cliMustExec(t, pool, `UPDATE runs SET status = 'cancelled', finished_at = $2, status_since = $2 WHERE id = ANY($1)`, []uuid.UUID{pausedRun, bothRun}, now)
	// Keep the mixed run from affecting anything else.
	cliMustExec(t, pool, `UPDATE runs SET status = 'cancelled', finished_at = $2, status_since = $2 WHERE id = $1`, mixedRunning, now)
	pass()
	var state string
	if err := pool.QueryRow(ctx, `SELECT hibernation_state FROM workers WHERE id = $1`, idleID).Scan(&state); err != nil || state != "preparing" {
		t.Fatalf("released worker state = %q, %v; want preparing", state, err)
	}
	c = read()
	for _, name := range []string{idleName, parkedName, both} {
		if _, ok := listed(c, name); ok {
			t.Fatalf("%s still listed: %+v", name, c.Evidence)
		}
	}
}
