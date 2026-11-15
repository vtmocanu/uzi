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
// parked blocker. Once the hold is released and the worker hibernated the check no longer lists
// it. The shared database may hold other tests' workers, so the assertions are relative to a
// baseline read before seeding and every listed worker is matched by its unique name.
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

	baseline := workersCount(read())

	// Seed: a repo, a persistent hosted worker idle 20 days with an open custody hold, and a
	// second worker whose only run is paused for 30 days.
	conn, repo := uuid.New(), uuid.New()
	cliMustExec(t, pool, `INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES ($1,$2,'gitlab','https://hib.example','bot',1,$3)`, conn, admin, []byte{1})
	cliMustExec(t, pool, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,enabled) VALUES ($1,$2,1,'g/hib','https://hib.example/g/hib',false)`, repo, conn)
	var iid int32 = int32(time.Now().UnixNano()%1_000_000_000) + 1
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
	idleID := newWorker(idleName, 20*24*time.Hour)
	parkedID := newWorker(parkedName, 60*24*time.Hour)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM recovery_custody_holds WHERE repo_id = $1`, repo)
		_, _ = pool.Exec(ctx, `DELETE FROM runs WHERE repo_id = $1`, repo)
		_, _ = pool.Exec(ctx, `DELETE FROM workers WHERE id = ANY($1)`, []uuid.UUID{idleID, parkedID})
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

	// The real pass refuses the held worker and records why.
	if _, err := svc.HibernationAutoPass(ctx); err != nil {
		t.Fatalf("HibernationAutoPass: %v", err)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(hibernation_blocked_reason,'') FROM workers WHERE id = $1`, idleID).Scan(&reason); err != nil || reason != workersvc.BlockedCustodyHold {
		t.Fatalf("recorded reason = %q, %v; want custody_hold", reason, err)
	}

	c := read()
	if c.Severity != "warn" {
		t.Fatalf("severity = %q, want warn: %+v", c.Severity, c)
	}
	if got, want := workersCount(c), baseline+2; got != want {
		t.Fatalf("Workers = %d, want %d (baseline %d + 2)", got, want, baseline)
	}
	if v, ok := listed(c, idleName); !ok || v != fmt.Sprintf("worker %s idle 20d, hibernation blocked: recovery custody hold", idleName) {
		t.Fatalf("idle worker evidence = %q (listed %v), all: %+v", v, ok, c.Evidence)
	}
	wantParked := fmt.Sprintf("worker %s blocked: assigned run parked (%s, paused)", parkedName, pausedRun.String()[:8])
	if v, ok := listed(c, parkedName); !ok || v != wantParked {
		t.Fatalf("parked worker evidence = %q (listed %v), want %q", v, ok, wantParked)
	}

	// Release the hold and hibernate the worker (state no longer none), and finish the paused
	// run: neither worker is listed any more.
	cliMustExec(t, pool, `DELETE FROM recovery_custody_holds WHERE id = $1`, hold)
	cliMustExec(t, pool, `UPDATE workers SET hibernation_state = 'hibernated', hibernation_op_id = $2, hibernated_at = $3 WHERE id = $1`, idleID, uuid.New(), now)
	cliMustExec(t, pool, `UPDATE runs SET status = 'cancelled', finished_at = $2, status_since = $2 WHERE id = $1`, pausedRun, now)
	c = read()
	if _, ok := listed(c, idleName); ok {
		t.Fatalf("hibernated worker still listed: %+v", c.Evidence)
	}
	if _, ok := listed(c, parkedName); ok {
		t.Fatalf("worker with a finished run still listed as parked: %+v", c.Evidence)
	}
	if got := workersCount(c); got != baseline {
		t.Fatalf("Workers = %d after release, want baseline %d", got, baseline)
	}
	if baseline == 0 && c.Severity != "ok" {
		t.Fatalf("severity = %q after release, want ok", c.Severity)
	}
}
