package store_test

// Stage E of TestClaimRunPlanMeasurement (issue #4602, plan milestone M3): the end-to-end paired
// benchmark. Stages P0..R time the ClaimRun SEGMENT through test code; stage E times the real
// workersvc.Service.Claim (no snapshot) so a base revision and a fix revision can be compared on
// the whole call. It uses only APIs exported at the base revision (no production symbol added
// by the fix is referenced), so this file compiles unchanged on both and the same command runs
// on each. UZI_CLAIMRUN_PLAN_E_LABEL is a free label (base|fix) copied into the output.
//
// Cells: {(c) claimable, (a) idle-empty, (b) queued-but-ineligible} x 3 reps, each on its own
// fresh template clone. The first cell, (c) rep 1, is the pilot: if its pre-conditions or
// verification fail the stage stops. Per cell, on ONE fresh pgxpool (MaxConns=1, default
// statement-cache mode, plan_cache_mode auto) used both as store.New(pool) and as
// SetTxBeginner(pool) (and by vault.NewPostgres), it records the cold first Claim, 10 warm-up
// calls and UZI_CLAIMRUN_PLAN_N (default 100) timed calls, with the tracer's statements per call,
// ClaimRun statements per call and the ClaimRun statement's pg_prepared_statements generic/custom
// plan counts.
//
// Service construction uses a subset of cmd/server/main.go's workersvc.New(...) literal, with the config
// defaults of internal/config/config.go: WorkerHeartbeatStale 45s (WORKER_HEARTBEAT_STALE),
// WorkerSpreadGrace 9s (3 x WORKER_POLL_INTERVAL 3s), plus the other claim windows. Only
// CustodyHoldLimit is a fixed reader (the settings cache is not wired here).
//
// Scenario (c): the measured user (the heavy-history pmSeed user) has ONE live claiming worker (pmSeed's
// non-ephemeral worker); every other worker of that user is made heartbeat-stale in the clone, so
// there is no live same-user peer and the fleet-aware spread never defers. 1+warm+N+10 claimable
// queued runs older than the spread cutoff are seeded, no forge_origin_cooldowns row exists, the
// claimant's heartbeat is refreshed before every batch of 10 calls (untimed), and the custody
// limit is the pre-existing open holds + the seeded runs + 9. Every call must claim a distinct
// run, return a payload and add one open hold, or the cell fails. Scenarios (a) and (b) must each
// return (nil, nil); in (b) the one queued run requires a capability the worker lacks and must
// stay queued.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const (
	cpEWarm    = 10
	cpEBatch   = 10 // calls between untimed heartbeat refreshes
	cpESpare   = 10 // claimable runs seeded beyond the calls made
	cpESlack   = 9  // custody limit headroom over pre-existing holds + seeded runs
	cpECallMax = 60 * time.Second
)

type cpECell struct {
	Scen  string
	Rep   int
	Label string
}

// cpECells orders the nine cells: (c) rep 1 first (the pilot), then a rotation per rep.
func cpECells() []cpECell {
	var cells []cpECell
	for rep := 1; rep <= 3; rep++ {
		var combos []cpECell
		for _, s := range []string{"c", "a", "b"} {
			combos = append(combos, cpECell{Scen: s, Rep: rep, Label: fmt.Sprintf("e_%s_r%d", s, rep)})
		}
		cells = append(cells, cpRotate(combos, rep-1)...)
	}
	return cells
}

func cpEToken(prefix string) string {
	return strings.Join([]string{prefix, uuid.NewString(), "fixture", "not", "real"}, "-")
}

type cpECellRes struct {
	Stage             string     `json:"stage"`
	Label             string     `json:"run_label"`
	Scenario          string     `json:"scenario"`
	Rep               int        `json:"rep"`
	Pilot             bool       `json:"pilot"`
	Database          string     `json:"database"`
	N                 int        `json:"n"`
	Warm              int        `json:"warmups"`
	SeededClaimable   int        `json:"seeded_claimable_runs"`
	CustodyLimit      int32      `json:"custody_limit"`
	PreOpenHolds      int64      `json:"pre_existing_open_holds"`
	Svc               cpPhaseRes `json:"service"`
	StatementsPerCall []int64    `json:"statements_per_call_all"`
	ClaimExecPerCall  []int64    `json:"claimrun_statements_per_call_all"`
	StatementsMedian  int64      `json:"statements_per_call_median"`
	StatementsMin     int64      `json:"statements_per_call_min"`
	StatementsMax     int64      `json:"statements_per_call_max"`
	ClaimRunPerCallLo int64      `json:"claimrun_statements_per_call_min"`
	ClaimRunPerCallHi int64      `json:"claimrun_statements_per_call_max"`
	StatsStart        cpStats    `json:"stats_start"`
	StatsEnd          cpStats    `json:"stats_end"`
	TotalSecs         float64    `json:"total_seconds"`
}

// runServiceStage is stage E.
func (e *cpEnv) runServiceStage(ctx context.Context) {
	t := e.t
	box, err := secretbox.New(bytes.Repeat([]byte{0x2a}, 32))
	if err != nil {
		t.Fatalf("stage E: secretbox: %v", err)
	}
	label := strings.TrimSpace(os.Getenv("UZI_CLAIMRUN_PLAN_E_LABEL"))
	if label == "" {
		label = "unlabeled"
	}
	e.out.emit("e_service", map[string]any{
		"run_label": label,
		"params_source": "api/cmd/server/main.go workersvc.New(q, box, workersvc.Params{...}) with the api/internal/config/config.go defaults: " +
			"WorkerHeartbeatStale 45s (WORKER_HEARTBEAT_STALE), WorkerSpreadGrace 3 x WORKER_POLL_INTERVAL(3s) = 9s, " +
			"WorkerHeartbeatInterval 15s, WorkerAffinityGrace 2m, WorkerAffinityCeiling 2h, WorkerStaleRequeueGrace 10m, WorkerBackgroundGrace 15m",
		"custody_hold_limit": "pre-existing open holds + seeded claimable runs + 9, a fixed reader",
		"pool":               "pgxpool MaxConns=1, default statement cache mode, plan_cache_mode auto; the same pool is store.New(pool), SetTxBeginner and vault.NewPostgres",
		"call":               "svc.Claim(ctx, wkr, nil)",
	})
	for i, c := range cpECells() {
		e.runECell(ctx, box, label, c, i == 0)
	}
}

func (e *cpEnv) runECell(ctx context.Context, box *secretbox.Box, runLabel string, c cpECell, pilot bool) {
	t := e.t
	f := e.fx
	e.lab.withClone(ctx, c.Label, func(name, dsn string, obs *pmObserver) {
		started := time.Now()
		cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		fail := func(format string, args ...any) {
			t.Helper()
			who := ""
			if pilot {
				who = " [PILOT: stopping the table]"
			}
			t.Fatalf("stage E cell %s%s: %s", c.Label, who, fmt.Sprintf(format, args...))
		}
		ex := func(sql string, args ...any) {
			t.Helper()
			if err := obs.exec(cctx, sql, args...); err != nil {
				fail("setup %.80q: %v", sql, err)
			}
		}
		n := e.cfg.n
		calls := 1 + cpEWarm + n
		seeded := 0
		if c.Scen == "c" {
			seeded = calls + cpESpare
		}

		// Fixture: an unprotected owner (RequiresUnlock would otherwise report idle before ClaimRun),
		// a sealed forge token, a usable sealed default Anthropic credential, one live claimant.
		ex(`UPDATE users SET vault_mode = 'unprotected' WHERE id = $1`, f.User)
		pat, err := box.Seal([]byte(cpEToken("bot-pat")))
		if err != nil {
			fail("seal forge token: %v", err)
		}
		ex(`UPDATE forge_connections SET token_ciphertext = $1 WHERE id = $2`, pat, f.Conn)
		anth, err := box.Seal([]byte(cpEToken("anthropic")))
		if err != nil {
			fail("seal agent credential: %v", err)
		}
		ex(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		    VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`, uuid.New(), f.User, "anthropic-"+uuid.NewString(), anth)
		ex(`UPDATE workers SET last_heartbeat_at = now() - interval '1 hour' WHERE user_id = $1 AND id <> $2`, f.User, f.Worker)
		ex(`UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, f.Worker)
		ex(`DELETE FROM runs WHERE user_id = $1 AND status = 'queued'`, f.User)
		queuedIDs := make([]uuid.UUID, 0, seeded)
		var ineligible uuid.UUID
		switch c.Scen {
		case "a":
		case "b":
			ineligible = uuid.New()
			ex(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, required_capabilities, created_at, updated_at)
			    VALUES ($1, $2, $3, 200000, 'q', 'd', 'queued', 'issue', $4, now() - interval '2 hours', now() - interval '2 hours')`,
				ineligible, f.User, f.Repo, []string{cpMissingCap})
		case "c":
			for g := 1; g <= seeded; g++ {
				id := uuid.New()
				queuedIDs = append(queuedIDs, id)
				ex(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at, updated_at)
				    VALUES ($1, $2, $3, $4, 'q', 'd', 'queued', 'issue', now() - interval '2 hours' + make_interval(secs => $5::int), now() - interval '2 hours')`,
					id, f.User, f.Repo, 300000+g, g)
			}
		default:
			fail("unknown scenario %q", c.Scen)
		}
		if e.cfg.cloneAnalyze {
			ex(`ANALYZE runs, workers, cross_checks`)
		}
		for _, tbl := range cpWarmTables {
			var cnt int64
			if err := obs.queryRow(cctx, `SELECT count(*) FROM `+tbl, nil, &cnt); err != nil {
				fail("warm read %s: %v", tbl, err)
			}
		}
		var preHolds32 int32 // count(*)::int4: Postgres errors on an out-of-range count, so no narrowing conversion
		var cooldowns, liveSameUser int64
		if err := obs.queryRow(cctx, `SELECT (SELECT count(*)::int4 FROM recovery_custody_holds WHERE state = 'open'),
		        (SELECT count(*) FROM forge_origin_cooldowns),
		        (SELECT count(*) FROM workers WHERE user_id = $1 AND id <> $2 AND last_heartbeat_at >= now() - interval '45 seconds')`,
			[]any{f.User, f.Worker}, &preHolds32, &cooldowns, &liveSameUser); err != nil {
			fail("pre-conditions: %v", err)
		}
		preHolds := int64(preHolds32)
		if cooldowns != 0 || liveSameUser != 0 {
			fail("pre-conditions: %d forge_origin_cooldowns rows, %d live same-user peers, want 0 and 0", cooldowns, liveSameUser)
		}
		custody := preHolds32 + int32(seeded) + cpESlack
		stats0 := cpReadStats(cctx, t, obs)

		// One fresh pool for the whole cell.
		tr := &cpTracer{claimSQL: e.claimSQL}
		pcfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			fail("parse pool config: %v", err)
		}
		pcfg.MaxConns = 1
		pcfg.MinConns = 0
		pcfg.ConnConfig.Tracer = tr
		pool, err := pgxpool.NewWithConfig(cctx, pcfg)
		if err != nil {
			fail("open pool: %v", err)
		}
		defer pool.Close()
		svc := workersvc.New(store.New(pool), box, workersvc.Params{
			CustodyHoldLimit:            cpCustody(custody),
			RunTimeout:                  6 * time.Hour,
			RunWallCeiling:              24 * time.Hour,
			RunIdleTimeout:              10 * time.Minute,
			WorkerTaskIdleTimeout:       30 * time.Minute,
			RunMaxIterations:            5,
			PlanMaxRevisions:            3,
			QuestionMax:                 5,
			QuestionTimeoutSeconds:      86400,
			CompletionHoldWindowSeconds: 900,
			RunMaxRequeues:              3,
			WorkerHeartbeatStale:        45 * time.Second,
			WorkerHeartbeatInterval:     15 * time.Second,
			TerminalPendingLease:        time.Hour,
			ActiveSnapshotMaxEntries:    256,
			WorkerOutboxMaxPending:      32,
			WorkerGapFillMax:            10000,
			WorkerAffinityGrace:         2 * time.Minute,
			WorkerAffinityCeiling:       2 * time.Hour,
			WorkerStaleRequeueGrace:     10 * time.Minute,
			WorkerSpreadGrace:           9 * time.Second,
			WorkerBackgroundGrace:       15 * time.Minute,
			SkillMaxBytes:               65536,
			SkillsMaxPerRun:             32,
			RunRecoveryParkBase:         time.Minute,
			RunRecoveryMaxPark:          30 * time.Minute,
		})
		svc.SetVault(vault.NewPostgres(box, pool))
		svc.SetTxBeginner(pool)

		res := cpECellRes{Stage: "E", Label: runLabel, Scenario: c.Scen, Rep: c.Rep, Pilot: pilot, Database: name, N: n, Warm: cpEWarm,
			SeededClaimable: seeded, CustodyLimit: custody, PreOpenHolds: preHolds, StatsStart: stats0}
		ph := &res.Svc
		ph.Kind, ph.N, ph.Outcomes = "service", n, map[string]int{}
		snap := func(stage string) cpStmt {
			s, err := cpSnap(cctx, pool, e.claimSQL)
			if err != nil {
				fail("snapshot after %s: %v", stage, err)
			}
			if s.Mode != "auto" {
				fail("SHOW plan_cache_mode = %q after %s, want auto", s.Mode, stage)
			}
			return s
		}
		s0 := snap("connect")
		ph.PID, ph.ShowPlanCacheMode = s0.PID, s0.Mode

		seen := map[uuid.UUID]bool{}
		wkr, err := store.New(obs.conn).GetWorkerByID(cctx, f.Worker)
		if err != nil {
			fail("load claimant: %v", err)
		}
		var timed []float64
		var warms []float64
		var snaps []cpStmt
		call := func(idx int) (time.Duration, string) {
			if idx%cpEBatch == 0 {
				// Untimed: keep the claimant live (a stale claimant would count its own holds, migration 00427).
				if err := obs.exec(cctx, `UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, f.Worker); err != nil {
					fail("heartbeat refresh: %v", err)
				}
				w, err := store.New(obs.conn).GetWorkerByID(cctx, f.Worker)
				if err != nil {
					fail("reload claimant: %v", err)
				}
				wkr = w
			}
			callCtx, cancelCall := context.WithTimeout(cctx, cpECallMax)
			defer cancelCall()
			rt0, ce0 := tr.starts.Load(), tr.claimExecs.Load()
			t0 := time.Now()
			payload, cerr := svc.Claim(callCtx, wkr, nil)
			d := time.Since(t0)
			rt, ce := tr.starts.Load()-rt0, tr.claimExecs.Load()-ce0
			res.StatementsPerCall = append(res.StatementsPerCall, rt)
			res.ClaimExecPerCall = append(res.ClaimExecPerCall, ce)
			if cerr != nil {
				fail("call %d: Claim returned error: %v", idx+1, cerr)
			}
			if c.Scen == "b" && ce != 1 {
				// A queued-but-ineligible poll must reach ClaimRun on both builds (#4602).
				fail("call %d: scenario (b) ran ClaimRun %d times, want 1", idx+1, ce)
			}
			outcome := "idle"
			if payload != nil {
				outcome = "claimed"
			}
			if c.Scen == "c" {
				if payload == nil {
					fail("call %d: scenario (c) returned idle (nil payload), want a claimed run", idx+1)
				}
				rid, perr := uuid.Parse(payload.RunID)
				if perr != nil {
					fail("call %d: payload run id %q: %v", idx+1, payload.RunID, perr)
				}
				if seen[rid] {
					fail("call %d: run %s was claimed twice", idx+1, rid)
				}
				seen[rid] = true
				if !slices.Contains(queuedIDs, rid) {
					fail("call %d: claimed run %s is not one of the seeded claimable runs", idx+1, rid)
				}
				var open, own int64
				if err := obs.queryRow(cctx, `SELECT (SELECT count(*) FROM recovery_custody_holds WHERE state = 'open'),
				        (SELECT count(*) FROM recovery_custody_holds WHERE state = 'open' AND run_id = $1)`, []any{rid}, &open, &own); err != nil {
					fail("call %d: read holds: %v", idx+1, err)
				}
				if own != 1 || open != preHolds+int64(idx+1) {
					fail("call %d: run %s has %d open holds and %d open holds in total across all users, want 1 and %d", idx+1, rid, own, open, preHolds+int64(idx+1))
				}
			} else if payload != nil {
				fail("call %d: scenario (%s) must be idle, but a run %s was claimed", idx+1, c.Scen, payload.RunID)
			}
			return d, outcome
		}

		d0, o0 := call(0)
		ph.ColdMs, ph.ColdRoundTrips, ph.ColdOutcome = cpMs(d0), res.StatementsPerCall[0], o0
		ph.ColdPrepares = tr.prepares.Load()
		snaps = append(snaps, snap("cold call"))
		for i := 1; i <= cpEWarm; i++ {
			d, _ := call(i)
			warms = append(warms, cpMs(d))
			s := snap(fmt.Sprintf("call %d", i+1))
			ph.EarlyPlanCounts = append(ph.EarlyPlanCounts, cpEarly{Call: i + 1, Generic: s.Generic, Custom: s.Custom})
		}
		ph.WarmupMs = warms
		ph.FirstCallsSumMs = ph.ColdMs
		for _, w := range warms {
			ph.FirstCallsSumMs += w
		}
		ph.WarmupMedianMs = cpQ(warms, 0.5)
		snaps = append(snaps, snap("warm-up"))
		execsBefore := tr.claimExecs.Load()
		for i := 0; i < n; i++ {
			d, o := call(1 + cpEWarm + i)
			timed = append(timed, cpMs(d))
			ph.RoundTripsPerCall = append(ph.RoundTripsPerCall, res.StatementsPerCall[1+cpEWarm+i])
			ph.Outcomes[o]++
		}
		ph.TimedMs = timed
		ph.ClaimExecsTimed = tr.claimExecs.Load() - execsBefore
		ph.ClaimExecsTotal = tr.claimExecs.Load()
		snaps = append(snaps, snap("timed calls"))
		ph.Snapshots = snaps
		ph.P50Ms, ph.P90Ms, ph.MaxMs = cpQ(timed, 0.5), cpQ(timed, 0.9), cpQ(timed, 1)
		for _, v := range timed {
			ph.MeanMs += v / float64(n)
		}
		qn := max(n/4, 1)
		ph.Q1P50Ms, ph.Q4P50Ms = cpQ(timed[:qn], 0.5), cpQ(timed[n-qn:], 0.5)
		sorted := slices.Clone(ph.RoundTripsPerCall)
		slices.Sort(sorted)
		ph.RoundTripsMedian = sorted[len(sorted)/2]
		all := slices.Clone(res.StatementsPerCall)
		slices.Sort(all)
		res.StatementsMin, res.StatementsMax, res.StatementsMedian = all[0], all[len(all)-1], all[len(all)/2]
		ce := slices.Clone(res.ClaimExecPerCall)
		slices.Sort(ce)
		res.ClaimRunPerCallLo, res.ClaimRunPerCallHi = ce[0], ce[len(ce)-1]
		last := snaps[len(snaps)-1]
		ph.PrepareTimeUnchanged = true
		if last.Present {
			ph.StatementName = last.Name
			for _, s := range snaps {
				if s.Present && (!s.PrepareTime.Equal(last.PrepareTime) || s.Name != last.Name) {
					ph.PrepareTimeUnchanged = false
				}
			}
			ph.GenericDeltaTimed = last.Generic - snaps[1].Generic
			ph.CustomDeltaTimed = last.Custom - snaps[1].Custom
			ph.GenericDeltaTotal, ph.CustomDeltaTotal = last.Generic, last.Custom
			if !ph.PrepareTimeUnchanged {
				fail("the ClaimRun statement was re-prepared mid-cell (prepare_time or name moved)")
			}
		} else if ph.ClaimExecsTotal > 0 {
			fail("ClaimRun ran %d times but pg_prepared_statements never listed it", ph.ClaimExecsTotal)
		}
		for _, s := range snaps {
			if s.PID != ph.PID {
				fail("the connection changed backend (pid %d then %d)", ph.PID, s.PID)
			}
		}
		ph.ReplayVerified = true
		res.StatsEnd = cpReadStats(cctx, t, obs)

		// Cell post-conditions.
		var queued, claimed, open int64
		if err := obs.queryRow(cctx, `SELECT (SELECT count(*) FROM runs WHERE user_id = $1 AND status = 'queued'),
		        (SELECT count(*) FROM runs WHERE user_id = $1 AND status = 'claimed'),
		        (SELECT count(*) FROM recovery_custody_holds WHERE state = 'open')`, []any{f.User}, &queued, &claimed, &open); err != nil {
			fail("post-conditions: %v", err)
		}
		switch c.Scen {
		case "a":
			if queued != 0 || claimed != 0 || open != preHolds {
				fail("post-conditions (a): %d queued, %d claimed, %d open holds", queued, claimed, open)
			}
		case "b":
			var st string
			if err := obs.queryRow(cctx, `SELECT status FROM runs WHERE id = $1`, []any{ineligible}, &st); err != nil || st != "queued" || claimed != 0 || open != preHolds {
				fail("post-conditions (b): ineligible run status %q (err %v), %d claimed, %d open holds", st, err, claimed, open)
			}
		case "c":
			if claimed != int64(calls) || queued != int64(seeded-calls) || open != preHolds+int64(calls) {
				fail("post-conditions (c): %d claimed, %d queued, %d open holds; want %d, %d, %d", claimed, queued, open, calls, seeded-calls, preHolds+int64(calls))
			}
		}
		res.TotalSecs = time.Since(started).Seconds()
		e.out.emit("e_cell", res)
		fmt.Printf("EROW|label=%s|scen=%s|rep=%d|pilot=%v|cold=%.2f|first11sum=%.1f|p50=%.2f|p90=%.2f|max=%.2f|mean=%.2f|q1p50=%.2f|q4p50=%.2f|stmts_med=%d(%d..%d)|claimrun_stmts=%d..%d|gen_d=%d|cus_d=%d|gen_tot=%d|cus_tot=%d|outcomes=%v|secs=%.1f\n",
			runLabel, c.Scen, c.Rep, pilot, ph.ColdMs, ph.FirstCallsSumMs, ph.P50Ms, ph.P90Ms, ph.MaxMs, ph.MeanMs, ph.Q1P50Ms, ph.Q4P50Ms,
			res.StatementsMedian, res.StatementsMin, res.StatementsMax, res.ClaimRunPerCallLo, res.ClaimRunPerCallHi,
			ph.GenericDeltaTimed, ph.CustomDeltaTimed, ph.GenericDeltaTotal, ph.CustomDeltaTotal, ph.Outcomes, res.TotalSecs)
	})
}
