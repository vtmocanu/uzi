package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// isolated_lane_livedb_test.go pins PRD #1906 M5 (D-B, D-D) against a real Postgres: the
// isolated-lane provisioning trigger and the exclusion of profile-bound runs from the two
// ordinary triggers, the lane-mismatch reap arm, and the lane-worker route allowlist through
// the REAL routers with a real worker token. The provisioner tests live here, not in hostedsvc,
// because the store-it sweep and CI run LiveDB tests only in the packages they enumerate.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// boundProfile seeds a site list for the fixture and returns its id.
func (fx *ephemeralFixture) boundProfile() uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	name := "lane-" + strings.ReplaceAll(id.String(), "-", "")[:20]
	if _, err := fx.pool.Exec(fx.ctx, `INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, '{docs.example.com}')`, id, name); err != nil {
		fx.t.Fatalf("seed profile: %v", err)
	}
	fx.t.Cleanup(func() {
		_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM workers WHERE user_id = $1`, fx.userID)
		_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE user_id = $1`, fx.userID)
		_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM egress_profiles WHERE id = $1`, id)
	})
	return id
}

// boundQueuedRun inserts a queued profile-bound run carrying caps, queued since an hour ago.
func (fx *ephemeralFixture) boundQueuedRun(profile uuid.UUID, caps []string) uuid.UUID {
	fx.t.Helper()
	if caps == nil {
		caps = []string{}
	}
	id := uuid.New()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, required_capabilities,
		                   egress_profile_id, status_since)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', $5, $6, now() - interval '1 hour')`,
		id, fx.userID, fx.repoID, fx.nextIID(), caps, profile); err != nil {
		fx.t.Fatalf("seed bound run: %v", err)
	}
	return id
}

// laneOf reads (isolated_lane, docker_enabled) of the ephemeral worker bound to run.
func (fx *ephemeralFixture) laneOf(run uuid.UUID) (isolated, docker, found bool) {
	fx.t.Helper()
	err := fx.pool.QueryRow(fx.ctx, `SELECT isolated_lane, docker_enabled FROM workers WHERE ephemeral AND ephemeral_run_id = $1`, run).
		Scan(&isolated, &docker)
	if err != nil {
		return false, false, false
	}
	return isolated, docker, true
}

func runIDs(ids []uuid.UUID) map[uuid.UUID]bool {
	m := map[uuid.UUID]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// TestIsolatedLaneProvisionLiveDB: a queued profile-bound run gets a lane worker
// (isolated_lane = true, no docker) even for a user who never opted into ephemeral workers,
// the controller poll renders it Isolated, and an unbound run beside it gets nothing (the user
// did not opt in). With the instance kill-switch off, nothing is provisioned.
func TestIsolatedLaneProvisionLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false)
	prof := fx.boundProfile()
	bound := fx.boundQueuedRun(prof, nil)
	unbound := fx.queuedRun([]string{"jvm"})

	if _, err := fx.provisioner(false, 2).ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (flag off): %v", err)
	}
	if _, _, found := fx.laneOf(bound); found {
		t.Fatal("the kill-switch is off but a lane worker was provisioned")
	}

	// Issue #1965: kill-switch on but the deployment never enabled the lane, so no lane worker.
	if _, err := fx.provisionerLaneOff(true, 2).ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (lane off): %v", err)
	}
	if _, _, found := fx.laneOf(bound); found {
		t.Fatal("the isolated lane is not enabled but a lane worker was provisioned")
	}

	if _, err := fx.provisioner(true, 2).ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass: %v", err)
	}
	iso, docker, found := fx.laneOf(bound)
	if !found || !iso || docker {
		t.Fatalf("bound run's worker found=%v isolated=%v docker=%v, want a lane worker without docker", found, iso, docker)
	}
	if _, _, found := fx.laneOf(unbound); found {
		t.Fatal("an unbound run of a user who did not opt in got an ephemeral worker")
	}

	resp, err := hostedsvc.New(fx.q, fx.box, time.Now, time.Hour).Poll(fx.ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	var wid uuid.UUID
	if err := fx.pool.QueryRow(fx.ctx, `SELECT id FROM workers WHERE ephemeral_run_id = $1`, bound).Scan(&wid); err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, w := range resp.Workers {
		if w.ID == wid.String() {
			seen = true
			if !w.Isolated || w.Docker || !w.Ephemeral {
				t.Fatalf("desired lane worker = %+v, want Isolated, Ephemeral, no Docker", w)
			}
		}
	}
	if !seen {
		t.Fatal("the lane worker is not in the controller poll")
	}

	// The cap still binds the lane: at cap 1 a second profile-bound run waits.
	second := fx.boundQueuedRun(prof, nil)
	if _, err := fx.provisioner(true, 1).ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass (cap 1): %v", err)
	}
	if _, _, found := fx.laneOf(second); found {
		t.Fatal("the per-user cap did not bind the lane trigger")
	}
}

// TestOrdinaryTriggersSkipProfileBoundRunsLiveDB: the capability-gap and saturation triggers
// never return a profile-bound run (an ordinary ephemeral worker bound to one would take the
// run's one slot and never claim it), while an unbound run of the same shape is returned.
// Asserted at the query, because at the pass level the lane trigger wins the dedup and would
// hide a regression.
func TestOrdinaryTriggersSkipProfileBoundRunsLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	prof := fx.boundProfile()

	// Capability gap: nothing online has docker.
	fx.onlineWorker("base-only", false)
	gapBound := fx.boundQueuedRun(prof, []string{"docker"})
	gapUnbound := fx.queuedRun([]string{"docker"})
	gap, err := fx.q.ListUnplaceableQueuedRunsForEphemeral(fx.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{MaxRows: 1000, MaxPerUser: 100})
	if err != nil {
		t.Fatal(err)
	}
	var gapIDs []uuid.UUID
	for _, r := range gap {
		gapIDs = append(gapIDs, r.ID)
	}
	if got := runIDs(gapIDs); got[gapBound] || !got[gapUnbound] {
		t.Fatalf("capability-gap trigger: bound listed=%v, unbound listed=%v; want false, true", got[gapBound], got[gapUnbound])
	}

	// Saturation: the only capable worker is at its cap, both runs queued for an hour.
	full := fx.onlineWorkerWithCap("full", false, 1)
	fx.activeRunOn(full)
	satBound := fx.boundQueuedRun(prof, nil)
	satUnbound := fx.queuedRun([]string{})
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET status_since = now() - interval '1 hour' WHERE id = $1`, satUnbound); err != nil {
		t.Fatal(err)
	}
	// base-only above has no cap (NULL = unbounded room), so give it one and fill it too.
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE workers SET max_concurrent_runs = 1 WHERE user_id = $1 AND name = 'base-only'`, fx.userID); err != nil {
		t.Fatal(err)
	}
	var baseOnly uuid.UUID
	if err := fx.pool.QueryRow(fx.ctx, `SELECT id FROM workers WHERE user_id = $1 AND name = 'base-only'`, fx.userID).Scan(&baseOnly); err != nil {
		t.Fatal(err)
	}
	fx.activeRunOn(baseOnly)
	sat, err := fx.q.ListSaturationQueuedRunsForEphemeral(fx.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
		SaturationDelay: pgtype.Interval{Microseconds: 0, Valid: true}, MaxRows: 1000, MaxPerUser: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	var satIDs []uuid.UUID
	for _, r := range sat {
		satIDs = append(satIDs, r.ID)
	}
	if got := runIDs(satIDs); got[satBound] || !got[satUnbound] {
		t.Fatalf("saturation trigger: bound listed=%v, unbound listed=%v; want false, true", got[satBound], got[satUnbound])
	}

	// And the lane trigger lists the bound run it can serve, and neither unbound run. gapBound
	// requires docker, which the lane never has, so the lane trigger excludes it too (see
	// TestLaneTriggerSkipsDockerRunsLiveDB).
	lane, err := fx.q.ListIsolatedQueuedRunsForEphemeral(fx.ctx, store.ListIsolatedQueuedRunsForEphemeralParams{MaxRows: 1000, MaxPerUser: 100})
	if err != nil {
		t.Fatal(err)
	}
	var laneIDs []uuid.UUID
	for _, r := range lane {
		laneIDs = append(laneIDs, r.ID)
	}
	got := runIDs(laneIDs)
	if got[gapBound] || !got[satBound] || got[gapUnbound] || got[satUnbound] {
		t.Fatalf("lane trigger lists docker-bound %v, bound %v, unbound %v/%v; want only the servable bound run", got[gapBound], got[satBound], got[gapUnbound], got[satUnbound])
	}
}

// TestReapLaneMismatchLiveDB: an ephemeral worker on the wrong side of the lane for its bound
// run is reaped at once (it could never claim the run and holds its one slot); matching
// workers of both kinds are kept.
func TestReapLaneMismatchLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	prof := fx.boundProfile()
	bound1 := fx.boundQueuedRun(prof, nil)
	bound2 := fx.boundQueuedRun(prof, nil)
	unbound1 := fx.queuedRun([]string{})
	unbound2 := fx.queuedRun([]string{})
	seed := func(run uuid.UUID, isolated bool) uuid.UUID {
		id := uuid.New()
		a, b := uuid.New(), uuid.New()
		if _, err := fx.pool.Exec(fx.ctx,
			`INSERT INTO workers (id, user_id, name, token_hash, kind, template_declared, hosted_size, docker_enabled,
			                      ephemeral, ephemeral_run_id, isolated_lane)
			 VALUES ($1, $2, $3, $4, 'hosted', 'base', 'm', false, true, $5, $6)`,
			id, fx.userID, "eph-"+id.String(), append(a[:], b[:]...), run, isolated); err != nil {
			t.Fatalf("seed ephemeral worker: %v", err)
		}
		return id
	}
	wrongOnBound := seed(bound1, false)
	laneOnBound := seed(bound2, true)
	laneOnUnbound := seed(unbound1, true)
	plainOnUnbound := seed(unbound2, false)

	// A long deadline, so only the mismatch arm can fire on these just-created workers.
	prov := hostedsvc.NewEphemeralProvisioner(fx.pool, fx.q, fx.box, nil, hostedsvc.EphemeralConfig{ProvisionDeadline: time.Hour})
	if _, err := prov.ReapPass(fx.ctx); err != nil {
		t.Fatalf("ReapPass: %v", err)
	}
	exists := func(id uuid.UUID) bool {
		var n int
		if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM workers WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if exists(wrongOnBound) || exists(laneOnUnbound) {
		t.Fatalf("mismatched workers kept: non-lane on bound=%v, lane on unbound=%v", exists(wrongOnBound), exists(laneOnUnbound))
	}
	if !exists(laneOnBound) || !exists(plainOnUnbound) {
		t.Fatalf("matching workers reaped: lane on bound kept=%v, plain on unbound kept=%v", exists(laneOnBound), exists(plainOnUnbound))
	}
}

// TestLaneWorkerRoutesLiveDB: through the REAL routers and a real worker token, a lane worker
// gets 403 on a chat route and on non-allowlisted run routes, 204 on the chat claim lane, and
// 2xx on the allowlisted heartbeat, run claim and input poll; an ordinary worker of the same
// user reaches the chat route (the control).
func TestLaneWorkerRoutesLiveDB(t *testing.T) {
	e := newFCEnv(t, nil, false)
	run, _ := e.boundRun("running", 1, 1)

	seed := func(isolated bool) string {
		tok, hash, err := jointoken.Generate()
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.New()
		if isolated {
			e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, kind, template_declared,
			                             hosted_size, docker_enabled, ephemeral, ephemeral_run_id, isolated_lane, protocol_capabilities)
			        VALUES ($1, $2, $3, $4, 'online', now(), 'hosted', 'base', 'm', false, true, $5, true, '{isolated_fetch_v1}')`,
				id, e.userID, "lane-"+id.String(), hash, run)
			e.exec(`UPDATE runs SET worker_id = $1 WHERE id = $2`, id, run)
		} else {
			e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at) VALUES ($1, $2, $3, $4, 'online', now())`,
				id, e.userID, "plain-"+id.String(), hash)
		}
		t.Cleanup(func() { _, _ = e.pool.Exec(e.ctx, `UPDATE runs SET worker_id = NULL WHERE worker_id = $1`, id) })
		return tok
	}
	lane := seed(true)
	plain := seed(false)

	do := func(router http.Handler, method, path, tok string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	runPath := "/api/worker/runs/" + run.String()
	for name, router := range map[string]http.Handler{"plain": e.router, "tls": e.tls} {
		refused := []struct{ method, path string }{
			{http.MethodGet, "/api/worker/chat/runs"},
			{http.MethodGet, "/api/worker/chat/runs/" + run.String()},
			{http.MethodGet, runPath + "/memory"},
			{http.MethodGet, runPath + "/forge/issues/1"},
			{http.MethodGet, runPath + "/trace"},
			{http.MethodPost, runPath + "/findings"},
			{http.MethodGet, runPath + "/ownership"},
		}
		for _, c := range refused {
			if got := do(router, c.method, c.path, lane); got != http.StatusForbidden {
				t.Errorf("%s: lane worker %s %s = %d, want 403", name, c.method, c.path, got)
			}
		}
		if got := do(router, http.MethodGet, "/api/worker/chat/runs", plain); got != http.StatusOK {
			t.Errorf("%s: ordinary worker GET /chat/runs = %d, want 200 (the control)", name, got)
		}
		if got := do(router, http.MethodPost, "/api/worker/runs/claim?lane=chat", lane); got != http.StatusNoContent {
			t.Errorf("%s: lane worker chat claim = %d, want 204", name, got)
		}
		for _, c := range []struct{ method, path string }{
			{http.MethodPost, "/api/worker/heartbeat"},
			{http.MethodPost, "/api/worker/runs/claim"},
			{http.MethodGet, runPath + "/inputs"},
		} {
			if got := do(router, c.method, c.path, lane); got < 200 || got > 299 {
				t.Errorf("%s: lane worker %s %s = %d, want 2xx", name, c.method, c.path, got)
			}
		}
	}
}

// TestLaneTriggerSkipsDockerRunsLiveDB: the lane trigger never lists a profile-bound run that
// requires docker (the lane has no DinD sidecar, so nothing could serve it). With a one-row
// window and an OLDER docker-requiring run ahead of a servable one, the servable run is the one
// returned and the pass provisions it, so docker runs cannot hold the window every tick.
func TestLaneTriggerSkipsDockerRunsLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false)
	prof := fx.boundProfile()
	dockerRun := fx.boundQueuedRun(prof, []string{"docker"})
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET created_at = now() - interval '2 hours' WHERE id = $1`, dockerRun); err != nil {
		t.Fatal(err)
	}
	servable := fx.boundQueuedRun(prof, nil)

	rows, err := fx.q.ListIsolatedQueuedRunsForEphemeral(fx.ctx, store.ListIsolatedQueuedRunsForEphemeralParams{MaxRows: 1, MaxPerUser: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != servable {
		var ids []uuid.UUID
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		t.Fatalf("lane trigger (window 1) = %v, want only the servable run %s (docker run %s excluded)", ids, servable, dockerRun)
	}

	if _, err := fx.provisioner(true, 5).ProvisionPass(fx.ctx); err != nil {
		t.Fatalf("ProvisionPass: %v", err)
	}
	if iso, _, found := fx.laneOf(servable); !found || !iso {
		t.Fatalf("servable run's lane worker found=%v isolated=%v, want a lane worker", found, iso)
	}
	if _, _, found := fx.laneOf(dockerRun); found {
		t.Fatal("a docker-requiring profile-bound run got a lane worker")
	}
}

// TestReapLaneWorkerWithoutFetchProtocolLiveDB: an online lane worker that does not advertise
// isolated_fetch_v1 can never claim its bound run, so it is reaped on the next tick (freeing
// the run's one slot); a lane worker advertising it, a lane worker not yet online, and a busy
// lane worker (its run is in flight on it) are all kept.
func TestReapLaneWorkerWithoutFetchProtocolLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, true)
	prof := fx.boundProfile()
	seed := func(run uuid.UUID, online bool, caps []string) uuid.UUID {
		id := uuid.New()
		a, b := uuid.New(), uuid.New()
		status, since := "offline", pgtype.Timestamptz{}
		if online {
			status, since = "online", pgtype.Timestamptz{Time: time.Now(), Valid: true}
		}
		if _, err := fx.pool.Exec(fx.ctx,
			`INSERT INTO workers (id, user_id, name, token_hash, kind, template_declared, hosted_size, docker_enabled,
			                      ephemeral, ephemeral_run_id, isolated_lane, status, online_since, last_heartbeat_at,
			                      protocol_capabilities)
			 VALUES ($1, $2, $3, $4, 'hosted', 'base', 'm', false, true, $5, true, $6, $7, now(), $8)`,
			id, fx.userID, "lane-"+id.String(), append(a[:], b[:]...), run, status, since, caps); err != nil {
			t.Fatalf("seed lane worker: %v", err)
		}
		return id
	}
	noFetch := seed(fx.boundQueuedRun(prof, nil), true, []string{"recovery_archive_v1"})
	withFetch := seed(fx.boundQueuedRun(prof, nil), true, []string{"isolated_fetch_v1", "recovery_archive_v1"})
	notOnline := seed(fx.boundQueuedRun(prof, nil), false, []string{})
	busyRun := fx.boundQueuedRun(prof, nil)
	busy := seed(busyRun, true, []string{})
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET status = 'running', worker_id = $2 WHERE id = $1`, busyRun, busy); err != nil {
		t.Fatal(err)
	}

	// A long deadline, so only the no-protocol arm can fire on these just-created workers.
	prov := hostedsvc.NewEphemeralProvisioner(fx.pool, fx.q, fx.box, nil, hostedsvc.EphemeralConfig{ProvisionDeadline: time.Hour})
	if _, err := prov.ReapPass(fx.ctx); err != nil {
		t.Fatalf("ReapPass: %v", err)
	}
	exists := func(id uuid.UUID) bool {
		var n int
		if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM workers WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if exists(noFetch) {
		t.Fatal("an online lane worker without isolated_fetch_v1 was kept")
	}
	if !exists(withFetch) || !exists(notOnline) || !exists(busy) {
		t.Fatalf("kept: with the protocol=%v, not yet online=%v, busy=%v; want all true", exists(withFetch), exists(notOnline), exists(busy))
	}
}
