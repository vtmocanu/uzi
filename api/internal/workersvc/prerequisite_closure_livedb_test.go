package workersvc

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Each test owns a disposable, fully migrated database. Production triggers stay
// installed. The explicit search_path makes now() use only this database's clock.
type prerequisiteExecutorLiveDB struct {
	env        codexTestEnv
	s          *Service
	m          *prerequisiteMemory
	h          *prerequisiteHTTP
	builder    *prerequisiteTestBuilder
	fixturesMu sync.RWMutex
	fixtures   []*prerequisiteMemory
}

func newPrerequisiteExecutorLiveDB(t *testing.T, n int) *prerequisiteExecutorLiveDB {
	t.Helper()
	env := isolatedCoordinateDatabase(t)
	s, m, h, b := newPrerequisiteMemory(t, n)
	env.exec("CREATE TABLE prerequisite_test_clock (singleton boolean PRIMARY KEY CHECK(singleton), observed_at timestamptz NOT NULL)")
	env.exec("INSERT INTO prerequisite_test_clock VALUES (true,$1)", m.now)
	env.exec("CREATE FUNCTION public.now() RETURNS timestamptz LANGUAGE sql STABLE AS 'SELECT observed_at FROM public.prerequisite_test_clock WHERE singleton'")
	cfg := env.pool.Config()
	cfg.ConnConfig.RuntimeParams["search_path"] = "public,pg_catalog"
	pool, err := pgxpool.NewWithConfig(env.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	env.pool = pool
	env.q = store.New(pool)
	env.exec = func(sql string, args ...any) {
		t.Helper()
		if _, e := pool.Exec(env.ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	s.q = env.q
	s.SetPrerequisiteStore(env.q, nil)
	s.SetTxBeginner(pool)
	f := &prerequisiteExecutorLiveDB{env: env, s: s, m: m, h: h, builder: b}
	f.seedLiveDB(t, m)
	s.SetForges(&m3Builder{build: func(baseURL string) (forge.Forge, error) {
		b.builds++
		return f.fixtureForge(t, baseURL, b.f)
	}})
	return f
}
func (f *prerequisiteExecutorLiveDB) seedLiveDB(t *testing.T, m *prerequisiteMemory) {
	t.Helper()
	e := f.env
	r := m.row
	worker := uuid.New()
	e.exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", r.UserID, r.UserID.String()+"@example.test")
	e.exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github',$3,'bot',1,$4)", m.conn.ID, r.UserID, m.conn.BaseUrl, m.conn.TokenCiphertext)
	e.exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES($1,$2,1,'g/r',$3,'main',true)", r.RepoID, m.conn.ID, m.repo.WebUrl)
	e.exec("INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'prereq',$3,'online')", worker, r.UserID, worker[:])
	e.exec("INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,claim_generation) VALUES($1,$2,$3,$4,'issue',1,'prereq','body','claimed',1)", r.RunID, r.UserID, r.RepoID, worker)
	e.exec("INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded) VALUES($1,$2,$3,$4,1,'open',$5,'held-worker',$5,$4,true)", r.HoldID, r.UserID, r.RepoID, r.RunID, worker)
	e.exec("UPDATE runs SET status='failed' WHERE id=$1", r.RunID)
	e.exec(`INSERT INTO recovery_captures
 (id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,manifest_bound,byte_size,checksum,chunk_count,prerequisite_shas,reserved_bytes,coverage_digest,expires_at)
 VALUES($1,$2,$3,$4,$5,'held-worker',repeat('a',40),$1::uuid::text,'available',true,3,$6,1,$7,3,repeat('a',64),now()+interval '1 day')`, r.CaptureID, r.HoldID, r.RunID, r.UserID, worker, r.Checksum, r.PrerequisiteShas)
	row, err := e.q.InsertPrerequisiteClosure(e.ctx, r.CaptureID)
	if err != nil {
		t.Fatal(err)
	}
	m.row = row
	f.fixturesMu.Lock()
	f.fixtures = append(f.fixtures, m)
	f.fixturesMu.Unlock()
}
func (f *prerequisiteExecutorLiveDB) readLiveDB(t *testing.T) store.RecoveryPrerequisiteClosure {
	t.Helper()
	r, e := f.env.q.GetPrerequisiteClosure(f.env.ctx, store.GetPrerequisiteClosureParams{CaptureID: f.m.row.CaptureID, UserID: f.m.row.UserID})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func (f *prerequisiteExecutorLiveDB) advanceLiveDB(t *testing.T, d time.Duration) {
	t.Helper()
	f.m.now = f.m.now.Add(d)
	f.env.exec("UPDATE prerequisite_test_clock SET observed_at=$1", f.m.now)
}
func (f *prerequisiteExecutorLiveDB) runLiveDB(t *testing.T) {
	t.Helper()
	if _, e := f.s.prerequisiteOne(f.env.ctx, f.env.q, f.readLiveDB(t), newPrerequisitePass(f.m.now)); e != nil {
		t.Fatal(e)
	}
}
func (f *prerequisiteExecutorLiveDB) dueLiveDB(t *testing.T) {
	t.Helper()
	r := f.readLiveDB(t)
	if r.NextAttemptAt.Time.After(f.m.now) {
		f.advanceLiveDB(t, r.NextAttemptAt.Time.Sub(f.m.now))
	}
}
func (f *prerequisiteExecutorLiveDB) expireCaptureLiveDB(t *testing.T) {
	t.Helper()
	f.advanceLiveDB(t, 25*time.Hour)
	f.env.exec("UPDATE recovery_captures SET state='expired',reserved_bytes=0 WHERE id=$1", f.m.row.CaptureID)
}

// TestPrerequisiteEarlierItemsDeferLatePreparationLiveDB covers the companion
// case where earlier items repeatedly consume the real sweep's preparation budget.
func TestPrerequisiteRepeatedPrepareBudgetExhaustionLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 6)
	proofs := 0
	f.builder.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, f.m), compare: func(context.Context, int64, string, string) (forge.Ancestry, error) {
		proofs++
		f.advanceLiveDB(t, 750*time.Millisecond)
		return forge.AncestryAncestor, nil
	}}
	for attempt := 1; attempt <= 8; attempt++ {
		f.runLiveDB(t)
		r := f.readLiveDB(t)
		if r.Attempts != int32(attempt) || r.InvokedAt.Valid || f.h.sends != 0 || !r.NextAttemptAt.Time.After(f.m.now) {
			t.Fatalf("attempt=%d state=%s actual=%d marker=%t sends=%d next=%s", attempt, r.State, r.Attempts, r.InvokedAt.Valid, f.h.sends, r.NextAttemptAt.Time)
		}
		if attempt < 8 && r.State != "pending" {
			t.Fatalf("early state=%s", r.State)
		}
		f.dueLiveDB(t)
	}
	r := f.readLiveDB(t)
	if r.State != "refused" || r.Reason != "prerequisite_prepare_budget_exhausted" || proofs != 48 || f.builder.builds != 8 {
		t.Fatalf("state=%s reason=%s proofs=%d builds=%d", r.State, r.Reason, proofs, f.builder.builds)
	}
}

func TestPrerequisiteNoBudgetNoAttemptThenFullTurnLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	for i := 0; i < 3; i++ {
		p := newPrerequisitePass(f.m.now)
		p.end = f.m.now.Add(prerequisiteItemBudget - time.Nanosecond)
		if _, e := f.s.prerequisiteOne(f.env.ctx, f.env.q, f.readLiveDB(t), p); e != nil {
			t.Fatal(e)
		}
	}
	r := f.readLiveDB(t)
	if r.Attempts != 0 || r.State != "pending" || f.builder.builds != 0 || f.h.sends != 0 {
		t.Fatal("before-S1 decline spent work")
	}
	f.runLiveDB(t)
	r = f.readLiveDB(t)
	if r.State != "pinned" || r.Attempts != 1 || f.h.sends != 1 {
		t.Fatalf("state=%s attempts=%d sends=%d", r.State, r.Attempts, f.h.sends)
	}
}

func TestPrerequisiteReservationRaceAtSevenLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	f.builder.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, f.m), compare: func(context.Context, int64, string, string) (forge.Ancestry, error) {
		return forge.AncestryUnknown, nil
	}}
	for i := 0; i < 7; i++ {
		f.runLiveDB(t)
		f.dueLiveDB(t)
	}
	type staleResult struct {
		row     store.RecoveryPrerequisiteClosure
		n       int64
		err     error
		bounded bool
	}
	stale := make(chan staleResult, 2)
	f.builder.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, f.m), compare: func(ctx context.Context, _ int64, _, _ string) (forge.Ancestry, error) {
		deadline, ok := ctx.Deadline()
		result := staleResult{bounded: ok && time.Until(deadline) <= time.Second}
		result.row, result.err = f.env.q.GetPrerequisiteClosure(ctx, store.GetPrerequisiteClosureParams{
			CaptureID: f.m.row.CaptureID, UserID: f.m.row.UserID,
		})
		if result.err == nil {
			result.n, result.err = f.env.q.RecordPrerequisitePreparationRetry(ctx, store.RecordPrerequisitePreparationRetryParams{
				ID: f.m.row.ID, Generation: 7,
			})
		}
		stale <- result
		return forge.AncestryUnknown, nil
	}}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, e := f.s.prerequisiteOne(f.env.ctx, f.env.q, f.m.row, newPrerequisitePass(f.m.now))
			results <- e
		}()
	}
	close(start)
	// Join both processors before asserting, including on a database error.
	var processorErrors []error
	for i := 0; i < 2; i++ {
		if e := <-results; e != nil {
			processorErrors = append(processorErrors, e)
		}
	}
	if len(processorErrors) != 0 {
		t.Fatalf("processors: %v", processorErrors)
	}
	if len(stale) != 1 {
		t.Fatalf("attempt-eight callbacks=%d want=1", len(stale))
	}
	result := <-stale
	if result.err != nil || result.n != 0 {
		t.Fatalf("stale generation-seven write while pending eight: rows=%d err=%v", result.n, result.err)
	}
	if result.row.State != "pending" || result.row.Attempts != 8 || !result.row.AttemptDeadline.Valid || !result.bounded {
		t.Fatalf("callback state=%s attempts=%d deadline=%t bounded=%t", result.row.State, result.row.Attempts, result.row.AttemptDeadline.Valid, result.bounded)
	}
	r := f.readLiveDB(t)
	if r.Attempts != 8 || r.State != "refused" || f.builder.builds != 8 {
		t.Fatalf("attempts=%d state=%s proofs=%d", r.Attempts, r.State, f.builder.builds)
	}
}

func TestPrerequisiteCrashedEighthReservationLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	for i := 0; i < 8; i++ {
		if _, e := f.env.q.ReservePrerequisitePreparation(f.env.ctx, store.ReservePrerequisitePreparationParams{ID: f.m.row.ID, LeaseSeconds: 8}); e != nil {
			t.Fatal(e)
		}
		f.dueLiveDB(t)
	}
	f.runLiveDB(t)
	r := f.readLiveDB(t)
	if r.State != "refused" || r.Reason != "prerequisite_ancestry_unknown" || f.builder.builds != 0 || f.h.sends != 0 {
		t.Fatalf("state=%s reason=%s proof=%d sends=%d", r.State, r.Reason, f.builder.builds, f.h.sends)
	}
}

func TestPrerequisiteTwoProcessesOneMarkerLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	other := New(f.env.q, f.s.box, testParams())
	other.SetTxBeginner(f.env.pool)
	other.SetClock(func() time.Time { return f.m.now })
	other.SetForgeBaseURLAllowed(func(string) bool { return true })
	// A shared builder is protected because two executors can prepare concurrently.
	var builds atomic.Int32
	builder := &prerequisiteConcurrentBuilder{f: &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, f.m), compare: func(context.Context, int64, string, string) (forge.Ancestry, error) {
		builds.Add(1)
		return forge.AncestryAncestor, nil
	}}}
	f.s.SetForges(builder)
	other.SetForges(builder)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, s := range []*Service{f.s, other} {
		wg.Add(1)
		go func(s *Service) {
			defer wg.Done()
			<-start
			_, e := s.prerequisiteOne(f.env.ctx, f.env.q, f.m.row, newPrerequisitePass(f.m.now))
			results <- e
		}(s)
	}
	close(start)
	wg.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	r := f.readLiveDB(t)
	if r.State != "pinned" || r.Attempts != 1 || f.h.sends != 1 || builds.Load() != 1 {
		t.Fatalf("state=%s attempts=%d sends=%d proofs=%d", r.State, r.Attempts, f.h.sends, builds.Load())
	}
}

type prerequisiteConcurrentBuilder struct{ f forge.Forge }

func (b *prerequisiteConcurrentBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

func TestPrerequisiteCleanupRecoveredAbsenceAndCeilingLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	f.runLiveDB(t)
	f.expireCaptureLiveDB(t)
	// Seven prior ambiguous deletes: reserve through production SQL, let leases
	// expire via the database clock, and never rewrite a lifecycle timestamp.
	var old store.RecoveryPrerequisiteClosure
	for i := 0; i < 7; i++ {
		var e error
		old, e = f.env.q.ReservePrerequisiteCleanup(f.env.ctx, store.ReservePrerequisiteCleanupParams{ID: f.m.row.ID, PassID: prerequisiteID(uuid.New()), LeaseSeconds: 8})
		if e != nil {
			t.Fatal(e)
		}
		f.dueLiveDB(t)
	}
	f.h.tip = ""
	f.runLiveDB(t)
	r := f.readLiveDB(t)
	if r.State != "deleted" || r.CleanupAttempts != 7 || r.LiveRunID.Valid || f.h.deletes != 0 {
		t.Fatalf("state=%s attempts=%d deletes=%d", r.State, r.CleanupAttempts, f.h.deletes)
	}
	n, e := f.env.q.RecordPrerequisiteDeleted(f.env.ctx, store.RecordPrerequisiteDeletedParams{ID: r.ID, Generation: old.CleanupAttempts, PassID: old.LastAuditPassID})
	if e != nil || n != 0 {
		t.Fatalf("stale delete rows=%d err=%v", n, e)
	}
}

func TestPrerequisiteAbsenceAuditLiveDB(t *testing.T) {
	for _, mode := range []string{"confirm", "positive", "failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newPrerequisiteExecutorLiveDB(t, 1)
			f.runLiveDB(t)
			f.advanceLiveDB(t, 24*time.Hour)
			f.h.tip = ""
			p := newPrerequisitePass(f.m.now)
			if _, e := f.s.prerequisiteOne(f.env.ctx, f.env.q, f.readLiveDB(t), p); e != nil {
				t.Fatal(e)
			}
			first := f.readLiveDB(t)
			if !first.AbsentSince.Valid || first.State != "pinned" || first.AuditNextAt.Time.Sub(f.m.now) != 10*time.Minute {
				t.Fatal("first absence did not schedule candidate")
			}
			f.advanceLiveDB(t, 10*time.Minute)
			// Isolate the durable pass fence from the expired wall-clock budget:
			// even an aged candidate cannot be confirmed by its first listing pass.
			n, err := f.env.q.ConfirmPrerequisiteLost(f.env.ctx, store.ConfirmPrerequisiteLostParams{
				ID: first.ID, Generation: first.CleanupAttempts, PassID: p.id,
			})
			if err != nil || n != 0 {
				t.Fatalf("same-pass loss rows=%d err=%v", n, err)
			}
			if f.readLiveDB(t).State != "pinned" {
				t.Fatal("same-pass absence lost pin")
			}
			if mode == "positive" {
				f.h.tip = prerequisiteTestTip
			}
			if mode == "failure" {
				f.env.exec("UPDATE repos SET default_branch='other' WHERE id=$1", first.RepoID)
			}
			f.runLiveDB(t)
			r := f.readLiveDB(t)
			switch mode {
			case "confirm":
				if r.State != "lost" || r.LiveRunID.Valid {
					t.Fatal("confirmed absence retained")
				}
			case "positive":
				if r.State != "pinned" || r.AbsentSince.Valid {
					t.Fatal("positive listing did not clear candidate")
				}
			case "failure":
				if r.State != "pinned" || !r.AbsentSince.Valid || r.AuditNextAt.Time.Sub(f.m.now) != 10*time.Minute {
					t.Fatal("failed candidate audit lost ten-minute retry")
				}
			}
		})
	}
}

// Keep a real HTTP fixture for additional rows on the same production database.
func (f *prerequisiteExecutorLiveDB) siblingLiveDB(t *testing.T) (*prerequisiteMemory, *prerequisiteHTTP) {
	t.Helper()
	_, m, h, _ := newPrerequisiteMemory(t, 1)
	// Credentials are sealed with the same per-test fixture key.
	f.seedLiveDB(t, m)
	return m, h
}
