package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4257: the fenced, server-ordered checkpoint-publication-withheld fact and its health
// projection, against a REAL Postgres. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// database (run via ./e2e/run-store-it.sh).

const cpGen = int64(3)

type cpFixture struct {
	*cncFixture
	claimedAt, statusSince time.Time
}

func newCPFixture(t *testing.T) *cpFixture {
	t.Helper()
	f := &cpFixture{cncFixture: newCNCFixture(t, setupCodexLiveDB(t), 3)}
	f.claimedAt = time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	f.statusSince = time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	return f
}

// runningRun inserts a running run owned by the fixture's worker at cpGen.
func (f *cpFixture) runningRun() uuid.UUID {
	run := f.seedRun("running", cpGen)
	f.env.exec(`UPDATE runs SET worker_id=$2, claimed_at=$3, started_at=$3, status_since=$4, last_activity_at=now() WHERE id=$1`,
		run, f.workerID, f.claimedAt, f.statusSince)
	return run
}

func (f *cpFixture) set(t *testing.T, run uuid.UUID, worker uuid.UUID, gen int64, nonce string, seq int64, since time.Time) string {
	t.Helper()
	out, err := f.env.q.RecordRunCheckpointWithheld(f.env.ctx, store.RecordRunCheckpointWithheldParams{
		ID: run, WorkerID: worker, Generation: gen, RegisterNonce: nonce, ReportSeq: seq,
		Since: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		t.Fatalf("RecordRunCheckpointWithheld: %v", err)
	}
	return out
}

func (f *cpFixture) clear(t *testing.T, run uuid.UUID, worker uuid.UUID, gen int64, nonce string, seq int64) string {
	t.Helper()
	out, err := f.env.q.ClearRunCheckpointWithheld(f.env.ctx, store.ClearRunCheckpointWithheldParams{
		ID: run, WorkerID: worker, Generation: gen, RegisterNonce: nonce, ReportSeq: seq,
	})
	if err != nil {
		t.Fatalf("ClearRunCheckpointWithheld: %v", err)
	}
	return out
}

type cpFact struct {
	since, statusSince *time.Time
	worker             *uuid.UUID
	generation, seq    *int64
	nonce              *string
}

func (f *cpFixture) fact(t *testing.T, run uuid.UUID) cpFact {
	t.Helper()
	var c cpFact
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT checkpoint_withheld_since, checkpoint_withheld_status_since, checkpoint_withheld_worker_id,
	  checkpoint_withheld_generation, checkpoint_withheld_nonce, checkpoint_withheld_report_seq FROM runs WHERE id=$1`, run).
		Scan(&c.since, &c.statusSince, &c.worker, &c.generation, &c.nonce, &c.seq); err != nil {
		t.Fatalf("read fact: %v", err)
	}
	return c
}

// projected returns the run's ListActiveRunsForHealth projection.
func (f *cpFixture) projected(t *testing.T, run uuid.UUID) pgtype.Timestamptz {
	t.Helper()
	rows, err := f.env.q.ListActiveRunsForHealth(f.env.ctx, codexCuratedModelsSlice())
	if err != nil {
		t.Fatalf("ListActiveRunsForHealth: %v", err)
	}
	for _, r := range rows {
		if r.ID == run {
			return r.PublicationWithheldSince
		}
	}
	t.Fatalf("run %s not in the health read", run)
	return pgtype.Timestamptz{}
}

func (f *cpFixture) since() time.Time { return f.claimedAt.Add(10 * time.Minute) }

func TestCheckpointWithheldFenceRefusalsLiveDB(t *testing.T) {
	f := newCPFixture(t)
	type mut struct {
		name string
		do   func(run uuid.UUID) (worker uuid.UUID, gen int64, nonce string)
	}
	muts := []mut{
		{"another worker", func(uuid.UUID) (uuid.UUID, int64, string) { return uuid.New(), cpGen, f.nonce }},
		{"older generation", func(uuid.UUID) (uuid.UUID, int64, string) { return f.workerID, cpGen - 1, f.nonce }},
		{"released claim", func(run uuid.UUID) (uuid.UUID, int64, string) {
			f.env.exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, run)
			return f.workerID, cpGen, f.nonce
		}},
		{"previous incarnation", func(uuid.UUID) (uuid.UUID, int64, string) {
			old := f.nonce
			f.env.exec(`UPDATE workers SET snapshot_register_nonce=$2 WHERE id=$1`, f.workerID, uuid.NewString())
			return f.workerID, cpGen, old
		}},
		{"non-running status", func(run uuid.UUID) (uuid.UUID, int64, string) {
			f.env.exec(`UPDATE runs SET status='awaiting_approval' WHERE id=$1`, run)
			return f.workerID, cpGen, f.nonce
		}},
	}
	// Positive control first: the fence admits the unmutated report.
	ctl := f.runningRun()
	if got := f.set(t, ctl, f.workerID, cpGen, f.nonce, 1, f.since()); got != "recorded" {
		t.Fatalf("control set = %q, want recorded", got)
	}
	if got := f.clear(t, ctl, f.workerID, cpGen, f.nonce, 2); got != "cleared" {
		t.Fatalf("control clear = %q, want cleared", got)
	}
	for _, m := range muts {
		for _, verb := range []string{"set", "clear"} {
			t.Run(m.name+"/"+verb, func(t *testing.T) {
				// restore the worker nonce this fixture rotates in one case
				f.env.exec(`UPDATE workers SET snapshot_register_nonce=$2 WHERE id=$1`, f.workerID, f.nonce)
				run := f.runningRun()
				w, g, n := m.do(run)
				var got string
				if verb == "set" {
					got = f.set(t, run, w, g, n, 1, f.since())
				} else {
					got = f.clear(t, run, w, g, n, 1)
				}
				if got != "refused" {
					t.Fatalf("outcome = %q, want refused", got)
				}
				if c := f.fact(t, run); c.seq != nil || c.since != nil || c.worker != nil || c.nonce != nil {
					t.Fatalf("a refused report wrote the fact: %+v", c)
				}
			})
		}
	}
	f.env.exec(`UPDATE workers SET snapshot_register_nonce=$2 WHERE id=$1`, f.workerID, f.nonce)
}

func TestCheckpointWithheldOrderRuleLiveDB(t *testing.T) {
	f := newCPFixture(t)
	run := f.runningRun()
	s1 := f.since()
	if got := f.set(t, run, f.workerID, cpGen, f.nonce, 5, s1); got != "recorded" {
		t.Fatalf("first set = %q", got)
	}
	// duplicate of the stored marker: ACKed with its original outcome, nothing changes (a different
	// since in the duplicate must not move the stored one).
	if got := f.set(t, run, f.workerID, cpGen, f.nonce, 5, s1.Add(time.Minute)); got != "recorded" {
		t.Fatalf("duplicate set = %q, want recorded", got)
	}
	if c := f.fact(t, run); c.since == nil || !c.since.Equal(s1) {
		t.Fatalf("duplicate moved since: %+v", c)
	}
	// a clear carrying the SAME marker as the stored withheld is not the duplicate of a clear.
	if got := f.clear(t, run, f.workerID, cpGen, f.nonce, 5); got != "refused" {
		t.Fatalf("same-marker clear over a withheld fact = %q, want refused", got)
	}
	// a lower marker never changes anything.
	if got := f.clear(t, run, f.workerID, cpGen, f.nonce, 4); got != "refused" {
		t.Fatalf("lower clear = %q, want refused", got)
	}
	if got := f.set(t, run, f.workerID, cpGen, f.nonce, 3, s1.Add(time.Hour)); got != "refused" {
		t.Fatalf("lower set = %q, want refused", got)
	}
	if c := f.fact(t, run); c.since == nil || !c.since.Equal(s1) || *c.seq != 5 {
		t.Fatalf("lower markers changed the fact: %+v", c)
	}
	// a higher withheld over a present fact of the same incarnation keeps the existing since.
	if got := f.set(t, run, f.workerID, cpGen, f.nonce, 6, s1.Add(5*time.Minute)); got != "recorded" {
		t.Fatalf("higher set = %q", got)
	}
	if c := f.fact(t, run); c.since == nil || !c.since.Equal(s1) || *c.seq != 6 {
		t.Fatalf("higher set over a present fact: %+v, want since kept %v and seq 6", c, s1)
	}
	// clear: since and status_since NULL, marker moves up.
	if got := f.clear(t, run, f.workerID, cpGen, f.nonce, 7); got != "cleared" {
		t.Fatalf("clear = %q", got)
	}
	if c := f.fact(t, run); c.since != nil || c.statusSince != nil || *c.seq != 7 {
		t.Fatalf("clear: %+v", c)
	}
	// duplicate clear is ACKed cleared without change; a lower withheld does not revive the fact.
	if got := f.clear(t, run, f.workerID, cpGen, f.nonce, 7); got != "cleared" {
		t.Fatalf("duplicate clear = %q, want cleared", got)
	}
	if got := f.set(t, run, f.workerID, cpGen, f.nonce, 6, s1); got != "refused" {
		t.Fatalf("lower withheld after a clear = %q, want refused", got)
	}
	if got := f.set(t, run, f.workerID, cpGen, f.nonce, 7, s1); got != "refused" {
		t.Fatalf("same-marker withheld after a clear = %q, want refused", got)
	}
	if c := f.fact(t, run); c.since != nil {
		t.Fatalf("a refused withheld revived the cleared fact: %+v", c)
	}
	// the marker survives the clear: a later higher withheld starts a FRESH fact with a fresh since.
	s2 := f.claimedAt.Add(40 * time.Minute)
	if got := f.set(t, run, f.workerID, cpGen, f.nonce, 8, s2); got != "recorded" {
		t.Fatalf("withheld after clear = %q", got)
	}
	if c := f.fact(t, run); c.since == nil || !c.since.Equal(s2) || *c.seq != 8 {
		t.Fatalf("fresh fact: %+v, want since %v", c, s2)
	}
}

func TestCheckpointWithheldSinceClampLiveDB(t *testing.T) {
	f := newCPFixture(t)
	// before claimed_at -> claimed_at
	run1, future := f.runningRun(), f.runningRun()
	f.set(t, run1, f.workerID, cpGen, f.nonce, 1, f.claimedAt.Add(-48*time.Hour))
	if c := f.fact(t, run1); c.since == nil || !c.since.Equal(f.claimedAt) {
		t.Fatalf("early since = %+v, want claimed_at %v", c, f.claimedAt)
	}
	// in the future -> not after now
	f.set(t, future, f.workerID, cpGen, f.nonce, 1, time.Now().Add(48*time.Hour))
	c := f.fact(t, future)
	if c.since == nil || c.since.After(time.Now().Add(time.Second)) || !c.since.After(f.claimedAt) {
		t.Fatalf("future since = %+v, want clamped to now", c)
	}
	// a run with no claimed_at takes the reported since (clamped only above)
	run3 := f.runningRun()
	f.env.exec(`UPDATE runs SET claimed_at=NULL WHERE id=$1`, run3)
	s := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	f.set(t, run3, f.workerID, cpGen, f.nonce, 1, s)
	if c := f.fact(t, run3); c.since == nil || !c.since.Equal(s) {
		t.Fatalf("no claimed_at since = %+v, want %v", c, s)
	}
}

func TestCheckpointWithheldReadFenceLiveDB(t *testing.T) {
	f := newCPFixture(t)
	fresh := func() uuid.UUID {
		run := f.runningRun()
		f.set(t, run, f.workerID, cpGen, f.nonce, 1, f.since())
		return run
	}
	ctl := fresh()
	if p := f.projected(t, ctl); !p.Valid || !p.Time.Equal(f.since()) {
		t.Fatalf("control projection = %+v, want %v", p, f.since())
	}
	for name, mutate := range map[string]string{
		"status_since changed":         `UPDATE runs SET status_since = status_since + interval '1 second' WHERE id=$1`,
		"claim generation bumped":      `UPDATE runs SET claim_generation = claim_generation + 1 WHERE id=$1`,
		"claim released":               `UPDATE runs SET claim_released_at = now() WHERE id=$1`,
		"status left running":          `UPDATE runs SET status = 'awaiting_approval' WHERE id=$1`,
		"owner worker changed":         `UPDATE runs SET worker_id = NULL WHERE id=$1`,
		"fact owner is another worker": `UPDATE runs SET checkpoint_withheld_worker_id = (SELECT id FROM workers WHERE id <> runs.worker_id LIMIT 1) WHERE id=$1`,
	} {
		t.Run(name, func(t *testing.T) {
			run := fresh()
			f.env.exec(mutate, run)
			rows, err := f.env.q.ListActiveRunsForHealth(f.env.ctx, codexCuratedModelsSlice())
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if r.ID == run && r.PublicationWithheldSince.Valid {
					t.Fatalf("stale fact projected: %+v", r.PublicationWithheldSince)
				}
			}
		})
	}
	t.Run("register nonce rotated", func(t *testing.T) {
		run := fresh()
		f.env.exec(`UPDATE workers SET snapshot_register_nonce=$2 WHERE id=$1`, f.workerID, uuid.NewString())
		defer f.env.exec(`UPDATE workers SET snapshot_register_nonce=$2 WHERE id=$1`, f.workerID, f.nonce)
		if p := f.projected(t, run); p.Valid {
			t.Fatalf("a previous incarnation's fact was projected: %+v", p)
		}
	})
	t.Run("cleared fact", func(t *testing.T) {
		run := fresh()
		f.clear(t, run, f.workerID, cpGen, f.nonce, 2)
		if p := f.projected(t, run); p.Valid {
			t.Fatalf("a cleared fact was projected: %+v", p)
		}
	})
}

// cpHealthStore restricts the detector to this fixture's runs; every read and write is real.
type cpHealthStore struct {
	*store.Queries
	runs map[uuid.UUID]bool
}

func (s cpHealthStore) ListActiveRunsForHealth(ctx context.Context, models []string) ([]store.ListActiveRunsForHealthRow, error) {
	rows, err := s.Queries.ListActiveRunsForHealth(ctx, models)
	if err != nil {
		return nil, err
	}
	var out []store.ListActiveRunsForHealthRow
	for _, r := range rows {
		if s.runs[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *cpFixture) healthRow(t *testing.T, run uuid.UUID) (health string, reason *string, since *time.Time) {
	t.Helper()
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT health, health_reason, health_since FROM runs WHERE id=$1`, run).Scan(&health, &reason, &since); err != nil {
		t.Fatal(err)
	}
	return
}

// detector is a FRESH Service over the same database, as after an api restart.
func (f *cpFixture) detector(runs ...uuid.UUID) *Service {
	set := map[uuid.UUID]bool{}
	for _, r := range runs {
		set[r] = true
	}
	svc := New(cpHealthStore{Queries: f.env.q, runs: set}, f.env.box, testParams())
	svc.SetHealthSettings(defaultHealthSettings())
	return svc
}

// TestCheckpointPublicationSeamLiveDB drives the REAL service entry a worker's /state report
// reaches, then a health pass, then an api restart (a fresh Service).
func TestCheckpointPublicationSeamLiveDB(t *testing.T) {
	f := newCPFixture(t)
	run, older := f.runningRun(), f.runningRun()
	gen := cpGen
	since := f.since()
	res, err := f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{
		State: "running", ClaimGeneration: &gen,
		CheckpointPublication: &CheckpointPublicationReport{
			State: "withheld", Since: since.Format(time.RFC3339), ClaimGeneration: gen, RegisterNonce: f.nonce, ReportSeq: 1,
		},
	})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if res.CheckpointPublication != CheckpointPublicationRecorded {
		t.Fatalf("ACK = %q, want recorded", res.CheckpointPublication)
	}

	det := f.detector(run, older)
	det.detectRunHealth(f.env.ctx, time.Now())
	h, reason, hs := f.healthRow(t, run)
	if h != healthPublicationWithheld || reason == nil || *reason != apitypes.ReasonCheckpointPublicationWithheld || hs == nil || !hs.Equal(since) {
		t.Fatalf("health = %q / %v / %v, want publication_withheld with the fixed reason and since %v", h, reason, hs, since)
	}
	// An older worker's run (no fact) keeps today's health.
	if h, reason, _ := f.healthRow(t, older); h != healthOK || reason != nil {
		t.Fatalf("a run with no fact has health %q / %v, want ok", h, reason)
	}

	// api restart: a fresh Service over the same database still projects the fact. Reset the stored
	// flag to prove the NEW detector derives it from the database alone.
	f.env.exec(`UPDATE runs SET health='ok', health_reason=NULL, health_since=NULL WHERE id=$1`, run)
	f.detector(run).detectRunHealth(f.env.ctx, time.Now())
	if h, _, hs := f.healthRow(t, run); h != healthPublicationWithheld || hs == nil || !hs.Equal(since) {
		t.Fatalf("after restart health = %q since %v", h, hs)
	}

	// The worker clears it: the next tick returns the run to ok.
	res, err = f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{
		State: "running", ClaimGeneration: &gen,
		CheckpointPublication: &CheckpointPublicationReport{State: "cleared", ClaimGeneration: gen, RegisterNonce: f.nonce, ReportSeq: 2},
	})
	if err != nil || res.CheckpointPublication != CheckpointPublicationCleared {
		t.Fatalf("clear report: ack %q err %v", res.CheckpointPublication, err)
	}
	f.detector(run).detectRunHealth(f.env.ctx, time.Now())
	if h, reason, _ := f.healthRow(t, run); h != healthOK || reason != nil {
		t.Fatalf("after clear health = %q / %v, want ok", h, reason)
	}
}

// fenceFailStore makes the fact write fail while every other statement is real.
type fenceFailStore struct{ *store.Queries }

func (fenceFailStore) RecordRunCheckpointWithheld(context.Context, store.RecordRunCheckpointWithheldParams) (string, error) {
	return "", context.DeadlineExceeded
}

// TestCheckpointPublicationWriteFailureDoesNotFailStatusLiveDB: a fact-write error is ACKed
// "unrecorded" while the status write still lands.
func TestCheckpointPublicationWriteFailureDoesNotFailStatusLiveDB(t *testing.T) {
	f := newCPFixture(t)
	f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.workerID, f.wkr.ProtocolCapabilities)
	run := f.seedRun("claimed", cpGen)
	f.env.exec(`UPDATE runs SET worker_id=$2, claimed_at=now() WHERE id=$1`, run, f.workerID)
	svc := New(fenceFailStore{f.env.q}, f.env.box, testParams())
	svc.SetVault(f.env.vlt)
	svc.SetTxBeginner(f.env.pool)
	gen := cpGen
	res, err := svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{
		State: "running", ClaimGeneration: &gen,
		CheckpointPublication: &CheckpointPublicationReport{State: "withheld", Since: time.Now().UTC().Format(time.RFC3339), ClaimGeneration: gen, RegisterNonce: f.nonce, ReportSeq: 1},
	})
	if err != nil {
		t.Fatalf("a fact-write error failed the status write: %v", err)
	}
	if res.CheckpointPublication != CheckpointPublicationUnrecorded || !res.Applied {
		t.Fatalf("ack=%q applied=%v, want unrecorded and applied", res.CheckpointPublication, res.Applied)
	}
	if r := f.run(t, run); r.Status != "running" {
		t.Fatalf("status = %q, want running", r.Status)
	}
}

func TestCheckpointWithheldStatusSinceChangeStartsFreshEpisodeLiveDB(t *testing.T) {
	f := newCPFixture(t)
	run := f.runningRun()
	s1 := f.since()
	if got := f.set(t, run, f.workerID, cpGen, f.nonce, 1, s1); got != "recorded" {
		t.Fatalf("first set = %q", got)
	}
	// running -> park -> running under the same worker, generation and nonce moves status_since
	// while the fact stays present.
	newStatusSince := f.statusSince.Add(10 * time.Minute)
	f.env.exec(`UPDATE runs SET status_since=$2 WHERE id=$1`, run, newStatusSince)
	s2 := f.claimedAt.Add(45 * time.Minute)
	if got := f.set(t, run, f.workerID, cpGen, f.nonce, 2, s2); got != "recorded" {
		t.Fatalf("higher set = %q", got)
	}
	c := f.fact(t, run)
	if c.since == nil || !c.since.Equal(s2) || c.statusSince == nil || !c.statusSince.Equal(newStatusSince) {
		t.Fatalf("fact after status_since change: %+v, want fresh since %v and status_since %v", c, s2, newStatusSince)
	}
}
