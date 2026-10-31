package workersvc

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPrerequisiteSameServiceBusyKickRecoveredBySweepLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	ctx := m3Context(t)
	second, h := f.siblingLiveDB(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var builds atomic.Int32
	f.s.SetForges(&m3Builder{build: func(baseURL string) (forge.Forge, error) {
		if builds.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return f.fixtureForge(t, baseURL, &prerequisiteTestForge{compare: func(context.Context, int64, string, string) (forge.Ancestry, error) {
			return forge.AncestryAncestor, nil
		}})
	}})
	f.s.StartPrerequisiteClosures(ctx)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if e := f.s.DrainPrerequisiteClosures(c); e != nil {
			t.Error(e)
		}
	})
	if !f.s.KickPrerequisiteClosure(f.m.row.UserID, f.m.row.CaptureID) {
		t.Fatal("first kick refused")
	}
	m3Wait(t, ctx, entered)
	if f.s.KickPrerequisiteClosure(second.row.UserID, second.row.CaptureID) {
		t.Fatal("second kick borrowed busy service slot")
	}
	m3Sweep(t, ctx, f.s)
	other, e := f.env.q.GetPrerequisiteClosure(ctx, store.GetPrerequisiteClosureParams{CaptureID: second.row.CaptureID, UserID: second.row.UserID})
	if e != nil || other.State != "pending" || other.Attempts != 0 || builds.Load() != 1 ||
		f.readLiveDB(t).Attempts != 1 || f.h.requests.Load() != 0 || h.requests.Load() != 0 {
		t.Fatalf("busy service entered second preparation: attempts=%d builds=%d err=%v", other.Attempts, builds.Load(), e)
	}
	unblock()
	joined := make(chan struct{})
	go func() { f.s.prerequisite.wg.Wait(); close(joined) }()
	m3Wait(t, ctx, joined)
	// The busy pass advanced a cursor. Bounded followups must wrap to the
	// durable dropped-kick row on this same Service.
	for i := 0; i < 4; i++ {
		m3Sweep(t, ctx, f.s)
	}
	other, e = f.env.q.GetPrerequisiteClosure(ctx, store.GetPrerequisiteClosureParams{CaptureID: second.row.CaptureID, UserID: second.row.UserID})
	if e != nil || other.State != "pinned" || other.Attempts != 1 || builds.Load() != 2 || f.readLiveDB(t).State != "pinned" {
		t.Fatal("dropped kick did not recover through public sweep")
	}
	f.h.mu.Lock()
	firstSends := f.h.sends
	f.h.mu.Unlock()
	h.mu.Lock()
	secondSends := h.sends
	h.mu.Unlock()
	if firstSends != 1 || secondSends != 1 {
		t.Fatalf("actual creates=%d,%d", firstSends, secondSends)
	}
}

// Counters wrap real generated queries, including the transaction adapter.
// Empty wrap probes cost no returned candidates; each nonempty cleanup page
// belongs to one of the two independently capped cleanup classes.
type m3FairStore struct {
	PrerequisiteStore
	raw                [4]int // pending, stale invocation, reconcile, audit
	cleanupPages       []int
	admitted           []uuid.UUID
	selected           []uuid.UUID
	auditPass          map[uuid.UUID]pgtype.UUID
	unknownRepo        map[uuid.UUID]uuid.UUID
	reservePreparation map[uuid.UUID]int
	reserveCleanup     map[uuid.UUID]int
}

func (q *m3FairStore) reset() {
	q.raw = [4]int{}
	q.cleanupPages = nil
	q.admitted = nil
	q.selected = nil
	q.auditPass = map[uuid.UUID]pgtype.UUID{}
	q.unknownRepo = map[uuid.UUID]uuid.UUID{}
}
func (q *m3FairStore) ListPrerequisitePendingDue(ctx context.Context, a store.ListPrerequisitePendingDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	r, e := q.PrerequisiteStore.ListPrerequisitePendingDue(ctx, a)
	for _, row := range r {
		q.selected = append(q.selected, row.ID)
	}
	q.raw[0] += len(r)
	return r, e
}
func (q *m3FairStore) ListPrerequisiteStaleInvokedDue(ctx context.Context, a store.ListPrerequisiteStaleInvokedDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	r, e := q.PrerequisiteStore.ListPrerequisiteStaleInvokedDue(ctx, a)
	for _, row := range r {
		q.selected = append(q.selected, row.ID)
	}
	q.raw[1] += len(r)
	return r, e
}
func (q *m3FairStore) ListPrerequisiteReconcileDue(ctx context.Context, a store.ListPrerequisiteReconcileDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	r, e := q.PrerequisiteStore.ListPrerequisiteReconcileDue(ctx, a)
	for _, row := range r {
		q.selected = append(q.selected, row.ID)
	}
	q.raw[2] += len(r)
	for _, row := range r {
		q.unknownRepo[row.RepoID] = row.ID
	}
	return r, e
}
func (q *m3FairStore) ListPrerequisiteAuditDue(ctx context.Context, a store.ListPrerequisiteAuditDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	r, e := q.PrerequisiteStore.ListPrerequisiteAuditDue(ctx, a)
	for _, row := range r {
		q.selected = append(q.selected, row.ID)
	}
	q.raw[3] += len(r)
	return r, e
}
func (q *m3FairStore) ListPrerequisiteCleanupDue(ctx context.Context, a store.ListPrerequisiteCleanupDueParams) ([]store.RecoveryPrerequisiteClosure, error) {
	r, e := q.PrerequisiteStore.ListPrerequisiteCleanupDue(ctx, a)
	for _, row := range r {
		q.selected = append(q.selected, row.ID)
	}
	if len(r) > 0 {
		q.cleanupPages = append(q.cleanupPages, len(r))
	}
	return r, e
}
func (q *m3FairStore) ReservePrerequisitePreparation(ctx context.Context, a store.ReservePrerequisitePreparationParams) (store.RecoveryPrerequisiteClosure, error) {
	q.admitted = append(q.admitted, a.ID)
	r, e := q.PrerequisiteStore.ReservePrerequisitePreparation(ctx, a)
	if e == nil {
		q.reservePreparation[a.ID]++
	}
	return r, e
}
func (q *m3FairStore) SettleStalePrerequisiteInvocation(ctx context.Context, id uuid.UUID) (int64, error) {
	q.admitted = append(q.admitted, id)
	return q.PrerequisiteStore.SettleStalePrerequisiteInvocation(ctx, id)
}
func (q *m3FairStore) GetRepoForUser(ctx context.Context, a store.GetRepoForUserParams) (store.GetRepoForUserRow, error) {
	if id, ok := q.unknownRepo[a.ID]; ok {
		q.admitted = append(q.admitted, id)
	}
	return q.PrerequisiteStore.GetRepoForUser(ctx, a)
}
func (q *m3FairStore) LockPrerequisiteClosure(ctx context.Context, id uuid.UUID) (store.RecoveryPrerequisiteClosure, error) {
	r, e := q.PrerequisiteStore.LockPrerequisiteClosure(ctx, id)
	if e == nil && r.State == "pinned" {
		pass, ok := q.auditPass[id]
		// A later outcome/delete transaction in this pass shares the reserved ID.
		// An unsuitable AVAILABLE candidate never reserves and is counted on
		// every attempted admission, so a missing per-pass dedupe is observable.
		if !ok || r.LastAuditPassID != pass {
			q.admitted = append(q.admitted, id)
		}
	}
	return r, e
}
func (q *m3FairStore) ReservePrerequisiteAudit(ctx context.Context, a store.ReservePrerequisiteAuditParams) (store.RecoveryPrerequisiteClosure, error) {
	r, e := q.PrerequisiteStore.ReservePrerequisiteAudit(ctx, a)
	if e == nil {
		q.auditPass[a.ID] = a.PassID
	}
	return r, e
}
func (q *m3FairStore) ReservePrerequisiteCleanup(ctx context.Context, a store.ReservePrerequisiteCleanupParams) (store.RecoveryPrerequisiteClosure, error) {
	r, e := q.PrerequisiteStore.ReservePrerequisiteCleanup(ctx, a)
	if e == nil {
		q.reserveCleanup[a.ID]++
	}
	return r, e
}
func (q *m3FairStore) transaction(f *prerequisiteExecutorLiveDB) PrerequisiteTransaction {
	return func(ctx context.Context, fn func(PrerequisiteStore) error) error {
		tx, e := f.env.pool.Begin(ctx)
		if e != nil {
			return e
		}
		defer func() {
			c, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = tx.Rollback(c)
		}()
		local := *q
		local.PrerequisiteStore = store.New(tx)
		// Local methods must update the same counters even if their slice grows.
		e = fn(&local)
		q.admitted = local.admitted
		if e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
}
func (q *m3FairStore) assertPass(t *testing.T, pass int) {
	t.Helper()
	if len(q.admitted) > 5 {
		t.Fatalf("pass %d admitted %d items", pass, len(q.admitted))
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range q.admitted {
		if seen[id] {
			t.Fatalf("pass %d admitted row twice: %s", pass, id)
		}
		seen[id] = true
	}
	if q.raw[0] > 10 || q.raw[1] > 10 || q.raw[2]+q.raw[3] > 10 {
		t.Fatalf("pass %d raw page caps=%v", pass, q.raw)
	}
	if len(q.cleanupPages) > 2 {
		t.Fatalf("pass %d nonempty cleanup pages=%d", pass, len(q.cleanupPages))
	}
	for _, n := range q.cleanupPages {
		if n > 10 {
			t.Fatalf("pass %d cleanup raw count=%d", pass, n)
		}
	}
}

// Seeding invokes the production executor or ordered marker fixture queries.
// Public sweeps below, not these setup calls, are the fairness evidence.
func TestPrerequisiteMixedBacklogPublicSweepProgressLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	ctx := m3Context(t)
	type entry struct {
		m *prerequisiteMemory
		h *prerequisiteHTTP
	}
	var prefix []entry
	// An early failing proof spends one real reservation and schedules backoff;
	// it must not prevent the other classes or the later successful preparation.
	failed := prerequisiteBudgetSiblingLiveDB(t, f, []string{strings.Repeat("e", 40)})
	n, err := f.env.q.DeferPrerequisitePreparation(ctx, store.DeferPrerequisitePreparationParams{ID: f.m.row.ID, Generation: 0})
	if err != nil || n != 1 {
		t.Fatal("could not schedule preparation behind failing head")
	}
	f.builder.f = &prerequisiteTestForge{getProject: prerequisiteProjectStub(t, f.m), compare: func(_ context.Context, _ int64, _, sha string) (forge.Ancestry, error) {
		if sha == strings.Repeat("e", 40) {
			return forge.AncestryUnknown, errors.New("synthetic failing head proof")
		}
		return forge.AncestryAncestor, nil
	}}
	pin := func(m *prerequisiteMemory) {
		t.Helper()
		if _, e := f.s.prerequisiteOne(ctx, f.env.q, m.row, newPrerequisitePass(f.m.now)); e != nil {
			t.Fatal(e)
		}
		r, e := f.env.q.GetPrerequisiteClosure(ctx, store.GetPrerequisiteClosureParams{CaptureID: m.row.CaptureID, UserID: m.row.UserID})
		if e != nil || r.State != "pinned" {
			t.Fatalf("pin setup: state=%s err=%v", r.State, e)
		}
	}
	// Distinct real repos and loopback remotes prevent one ref from supplying
	// another row's expected tip. These rows are raw, unsuitable cleanup heads.
	for i := 0; i < 21; i++ {
		m, h := f.siblingLiveDB(t)
		pin(m)
		prefix = append(prefix, entry{m, h})
	}
	f.advanceLiveDB(t, time.Second)
	initial, initialHTTP := f.siblingLiveDB(t)
	pin(initial)
	retry, retryHTTP := f.siblingLiveDB(t)
	pin(retry)
	unknown, unknownHTTP := f.siblingLiveDB(t)
	// This sibling owns a different repository and connection; the marker
	// fixture must freeze that binding, not the primary preparation's binding.
	unknownFixture := &prerequisiteExecutorLiveDB{env: f.env, s: f.s, m: unknown}
	prerequisiteBudgetUnknownLiveDB(t, unknownFixture, unknown.row)
	stale, staleHTTP := f.siblingLiveDB(t)
	// Stop this row at the genuine marker without a broker send.
	q := &m3StoppedLookup{PrerequisiteStore: f.env.q}
	tx := &m3Transactions{f: f}
	q.tx = tx
	f.s.SetPrerequisiteStore(q, tx.transaction)
	if _, e := f.s.prerequisiteOne(ctx, q, stale.row, newPrerequisitePass(f.m.now)); e != nil {
		t.Fatal(e)
	}
	f.s.SetPrerequisiteStore(f.env.q, nil)
	if r, e := f.env.q.GetPrerequisiteClosure(ctx, store.GetPrerequisiteClosureParams{CaptureID: stale.row.CaptureID, UserID: stale.row.UserID}); e != nil || r.State != "invoked" {
		t.Fatal("stale marker setup")
	}
	f.advanceLiveDB(t, 25*time.Hour)
	for _, m := range []*prerequisiteMemory{initial, retry} {
		f.env.exec("UPDATE recovery_captures SET state='expired',reserved_bytes=0 WHERE id=$1", m.row.CaptureID)
	}
	// Prior crashed cleanup generation one: real reservation, lease and backoff.
	reserved, e := f.env.q.ReservePrerequisiteCleanup(ctx, store.ReservePrerequisiteCleanupParams{ID: retry.row.ID, PassID: prerequisiteID(uuid.New()), LeaseSeconds: 8})
	if e != nil || reserved.CleanupAttempts != 1 {
		t.Fatalf("retry seed: %v", e)
	}
	f.advanceLiveDB(t, 38*time.Second)
	counters := &m3FairStore{PrerequisiteStore: f.env.q, unknownRepo: map[uuid.UUID]uuid.UUID{},
		reservePreparation: map[uuid.UUID]int{}, reserveCleanup: map[uuid.UUID]int{}}
	f.s.SetPrerequisiteStore(counters, counters.transaction(f))
	listingBaseline := map[*prerequisiteHTTP]int32{
		unknownHTTP: unknownHTTP.requests.Load(),
		staleHTTP:   staleHTTP.requests.Load(),
	}
	visited := map[uuid.UUID]bool{}
	wrapped := false
	var previous [5]prerequisiteCursor
	// No fake clock advance here: successful listings and inconclusive unknown
	// rows move out of the due set. Unsuitable cleanup prefixes stay due.
	for pass := 0; pass < 60; pass++ {
		counters.reset()
		m3Sweep(t, ctx, f.s)
		counters.assertPass(t, pass)
		for _, id := range counters.admitted {
			visited[id] = true
		}
		for lane, c := range f.s.prerequisite.cursor {
			if previous[lane].due.Valid && (!c.due.Valid || c.due.Time.Before(previous[lane].due.Time) ||
				c.due.Time.Equal(previous[lane].due.Time) && c.id.String() < previous[lane].id.String()) {
				wrapped = true
			}
		}
		previous = f.s.prerequisite.cursor
	}
	// Complete ID membership, with only the first missing row in diagnostics.
	expected := []uuid.UUID{f.m.row.ID, failed.ID, initial.row.ID, retry.row.ID, unknown.row.ID, stale.row.ID}
	for _, x := range prefix {
		expected = append(expected, x.m.row.ID)
	}
	if len(visited) != len(expected) {
		t.Fatalf("visited IDs=%d want=%d", len(visited), len(expected))
	}
	for i, id := range expected {
		if !visited[id] {
			t.Fatalf("first missing expected ID at %d: %s", i, id)
		}
	}
	if !wrapped {
		t.Fatal("no cursor wrap observed")
	}
	read := func(m *prerequisiteMemory) store.RecoveryPrerequisiteClosure {
		t.Helper()
		r, e := f.env.q.GetPrerequisiteClosure(ctx, store.GetPrerequisiteClosureParams{CaptureID: m.row.CaptureID, UserID: m.row.UserID})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	if r := f.readLiveDB(t); r.State != "pinned" || r.Attempts != 1 || counters.reservePreparation[r.ID] != 1 {
		t.Fatal("preparation class did not reserve and create once")
	}
	failure := m3Read(t, f, failed.CaptureID)
	if failure.State != "pending" || failure.Attempts != 1 || failure.InvokedAt.Valid ||
		!failure.NextAttemptAt.Time.After(f.m.now) || counters.reservePreparation[failed.ID] != 1 {
		t.Fatal("failing head did not leave a bounded durable retry")
	}
	f.h.mu.Lock()
	targetCreates := f.h.sends
	f.h.mu.Unlock()
	if targetCreates != 1 {
		t.Fatalf("failing head or successful target sent extra create commands: %d", targetCreates)
	}
	for _, x := range prefix {
		r := read(x.m)
		if r.State != "pinned" || r.CleanupAttempts != 0 || !r.LastAuditPassID.Valid || !r.AuditNextAt.Time.After(f.m.now) {
			t.Fatalf("prefix audit failed: %s", r.ID)
		}
		x.h.mu.Lock()
		sends, deletes := x.h.sends, x.h.deletes
		x.h.mu.Unlock()
		if sends != 1 || deletes != 0 {
			t.Fatalf("prefix remote changed: %s", r.ID)
		}
	}
	for i, x := range []entry{{initial, initialHTTP}, {retry, retryHTTP}} {
		r := read(x.m)
		if r.State != "deleted" || r.CleanupAttempts != int32(i+1) || r.LiveRunID.Valid || counters.reserveCleanup[r.ID] != 1 {
			t.Fatalf("cleanup class %d: state=%s attempts=%d reservations=%d", i, r.State, r.CleanupAttempts, counters.reserveCleanup[r.ID])
		}
		x.h.mu.Lock()
		sends, deletes := x.h.sends, x.h.deletes
		x.h.mu.Unlock()
		if sends != 2 || deletes != 1 {
			t.Fatalf("cleanup actual commands=%d deletes=%d", sends, deletes)
		}
	}
	for _, x := range []entry{{unknown, unknownHTTP}, {stale, staleHTTP}} {
		r := read(x.m)
		if r.State != "create_unknown" || !r.LiveRunID.Valid || r.ReconciliationAttempts != 1 || !r.NextAttemptAt.Time.After(f.m.now) {
			t.Fatalf("unknown progress: state=%s generation=%d", r.State, r.ReconciliationAttempts)
		}
		if delta := x.h.requests.Load() - listingBaseline[x.h]; delta != 1 {
			t.Fatalf("listing-only request delta=%d", delta)
		}
	}
}

// The per-owner quota counts active captures, not unresolved prerequisite pins.
// M3 explicitly inserts the closure after an actual Reserve+Upload; S0 is M4.
func TestPrerequisiteExpiredPinsFreeOwnerQuotaForNewUploadLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	ctx := m3Context(t)
	remote := m3RemoteFor(t, f, nil)
	remote.mode = "ambiguous-absent"
	m3Sweep(t, ctx, f.s)
	old := f.readLiveDB(t)
	if old.State != "create_unknown" || !old.LiveRunID.Valid {
		t.Fatal("old unresolved pin missing")
	}
	f.expireCaptureLiveDB(t)
	// Other owners' available backlog must not count against this owner's limit.
	other, _ := f.siblingLiveDB(t)
	// Reserve is authorized by UUID. For a NEW thin closure only, its capture
	// provenance label must also match the historical hold. Rename the mutable
	// registered worker before reserving; never rewrite the hold or old capture.
	var workerID uuid.UUID
	if e := f.env.pool.QueryRow(ctx, "SELECT original_worker_id FROM recovery_custody_holds WHERE id=$1", old.HoldID).Scan(&workerID); e != nil {
		t.Fatal(e)
	}
	f.env.exec("UPDATE workers SET name='held-worker' WHERE id=$1", workerID)
	r := m3RecoveryFixture(t, f, 1)
	r.manifest.PrerequisiteShas = append([]string(nil), old.PrerequisiteShas...)
	up, e := r.upload(ctx, f, bytes.NewReader(r.body))
	if e != nil {
		t.Fatal(e)
	}
	m3UploadAssert(t, r, up)
	// A second active capture really is refused at the configured owner limit.
	gen := int64(1)
	_, quotaErr := r.svc.Reserve(ctx, r.worker, old.RunID, apitypes.RecoveryReserveRequest{
		Generation: &gen, IdempotencyKey: uuid.NewString(), SourceSha: "aaaa1111", CoverageDigest: r.final.FinalDisposition.CoverageDigest,
	})
	if !errors.Is(quotaErr, recovery.ErrQuota) {
		t.Fatalf("tight owner limit not enforced: %v", quotaErr)
	}
	freshRow, e := f.env.q.InsertPrerequisiteClosure(ctx, r.capture)
	if e != nil {
		t.Fatal(e)
	}
	// Admission with limit=1 succeeded while the old closure retained its pointer.
	frozen := f.readLiveDB(t)
	m3AssertFrozen(t, old, frozen)
	if frozen.State != "create_unknown" || freshRow.State != "pending" || freshRow.Attempts != 0 {
		t.Fatal("old unresolved pin changed or explicit M3 closure insertion failed")
	}
	remote.mu.Lock()
	remote.mode = ""
	remote.ref = freshRow.PinRef
	remote.mu.Unlock()
	m3Sweep(t, ctx, f.s)
	newRow := m3Read(t, f, r.capture)
	if newRow.State != "pinned" || newRow.Attempts != 1 || !newRow.LiveRunID.Valid {
		t.Fatalf("new admitted capture not eligible: state=%s", newRow.State)
	}
	otherRow, e := f.env.q.GetPrerequisiteClosure(ctx, store.GetPrerequisiteClosureParams{CaptureID: other.row.CaptureID, UserID: other.row.UserID})
	if e != nil || otherRow.State != "pinned" {
		t.Fatal("other owner backlog did not remain independently eligible")
	}
}

// This is a historical-anchor, externally absent selection fixture, not evidence
// that 512 captures were admitted in one claim under default quotas. Each owner
// has at most 256 active captures during construction. Real broker listings below
// observe absent refs; setup does not create 512 native pins.
func TestPrerequisiteHugeExpiredBacklogDoesNotBlockPreparationLiveDB(t *testing.T) {
	f := newPrerequisiteExecutorLiveDB(t, 1)
	ctx := m3Context(t)
	const owners, perOwner, passes = 2, 256, 8
	const total = owners * perOwner
	type background struct {
		m        *prerequisiteMemory
		h        *prerequisiteHTTP
		baseline int32
	}
	backlog := make([]background, 0, owners)
	for owner := 0; owner < owners; owner++ {
		m, h := f.siblingLiveDB(t)
		// Copy the bound manifest and original provenance from the valid capture.
		// All production triggers remain installed throughout this progression.
		f.env.exec(`INSERT INTO recovery_captures
			SELECT (jsonb_populate_record(NULL::recovery_captures,to_jsonb(c) ||
				jsonb_build_object('id',gen_random_uuid(),'idempotency_key',gen_random_uuid()::text))).*
			FROM recovery_captures c CROSS JOIN generate_series(1,$2::int) WHERE c.id=$1`,
			m.row.CaptureID, perOwner-1)
		f.env.exec(`INSERT INTO recovery_prerequisite_closures
			(capture_id,hold_id,run_id,user_id,repo_id,checksum,prerequisite_shas,pin_ref,next_attempt_at)
			SELECT c.id,c.hold_id,c.run_id,c.user_id,$2,c.checksum,c.prerequisite_shas,
				'refs/uzi-prereq/'||c.id::text,now()
			FROM recovery_captures c WHERE c.hold_id=$1 AND c.id<>$3`,
			m.row.HoldID, m.row.RepoID, m.row.CaptureID)
		f.env.exec(`UPDATE recovery_prerequisite_closures
			SET attempts=1,attempt_deadline=now()+interval '1 minute',next_attempt_at=now()+interval '2 minutes'
			WHERE hold_id=$1`, m.row.HoldID)
		f.env.exec(`UPDATE recovery_prerequisite_closures
			SET state='invoked',live_run_id=run_id,invoked_at=now(),invoked_deadline=now()+interval '30 seconds',
				pin_tip=$2,connection_id=$3,forge_kind=$4,normalized_base_url=$5,project_id=$6,default_branch=$7
			WHERE hold_id=$1`, m.row.HoldID, prerequisiteTestTip, m.conn.ID, m.conn.ForgeType,
			strings.TrimRight(m.conn.BaseUrl, "/"), m.repo.ForgeProjectID, m.repo.DefaultBranch)
		f.env.exec("UPDATE recovery_prerequisite_closures SET state='pinned' WHERE hold_id=$1", m.row.HoldID)
		f.env.exec("UPDATE recovery_captures SET state='expired',reserved_bytes=0 WHERE hold_id=$1", m.row.HoldID)
		f.env.exec(`UPDATE recovery_prerequisite_closures
			SET next_attempt_at=now()-interval '1 hour',audit_next_at=now()+interval '1 day'
			WHERE hold_id=$1`, m.row.HoldID)
		backlog = append(backlog, background{m, h, h.requests.Load()})
	}
	// Read every fixture ID and validate the complete expired/pinned skew before
	// any public sweep. Report only the first unexpected row.
	known := map[uuid.UUID]bool{f.m.row.ID: true}
	holds := []uuid.UUID{backlog[0].m.row.HoldID, backlog[1].m.row.HoldID}
	rows, err := f.env.pool.Query(ctx, `SELECT p.id,p.state,c.state,p.live_run_id IS NOT NULL,
		p.attempts,p.cleanup_attempts,c.reserved_bytes,
		p.next_attempt_at<now(),p.audit_next_at>now()
		FROM recovery_prerequisite_closures p JOIN recovery_captures c ON c.id=p.capture_id
		WHERE p.hold_id=ANY($1::uuid[])`, holds)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		var state, captureState string
		var live, due, auditFuture bool
		var attempts, cleanup int32
		var reserved int64
		if err := rows.Scan(&id, &state, &captureState, &live, &attempts, &cleanup, &reserved, &due, &auditFuture); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if known[id] || state != "pinned" || captureState != "expired" || !live ||
			attempts != 1 || cleanup != 0 || reserved != 0 || !due || !auditFuture {
			rows.Close()
			t.Fatalf("invalid expired background fixture: %s state=%s capture=%s", id, state, captureState)
		}
		known[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(known) != total+1 || f.readLiveDB(t).State != "pending" {
		t.Fatalf("fixture IDs=%d want=%d with pending primary", len(known), total+1)
	}
	counters := &m3FairStore{PrerequisiteStore: f.env.q,
		reservePreparation: map[uuid.UUID]int{}, reserveCleanup: map[uuid.UUID]int{}}
	f.s.SetPrerequisiteStore(counters, counters.transaction(f))
	f.s.prerequisite.lead = 3 // Start with initial cleanup, before preparation.
	admissions := 0
	// Eight synchronous passes, with no retries or clock advance. A failed
	// sweep or invariant stops the test; the parent context bounds all work.
	for pass := 0; pass < passes; pass++ {
		counters.reset()
		m3Sweep(t, ctx, f.s)
		counters.assertPass(t, pass)
		admissions += len(counters.admitted)
		for i, id := range counters.selected {
			if !known[id] {
				t.Fatalf("pass %d first unknown selected ID at %d: %s", pass, i, id)
			}
		}
		for i, id := range counters.admitted {
			if !known[id] {
				t.Fatalf("pass %d first unknown admitted ID at %d: %s", pass, i, id)
			}
		}
	}
	primary := f.readLiveDB(t)
	if primary.State != "pinned" || primary.Attempts != 1 || !primary.LiveRunID.Valid ||
		counters.reservePreparation[primary.ID] != 1 {
		t.Fatalf("primary did not get a full preparation turn: state=%s attempts=%d", primary.State, primary.Attempts)
	}
	f.h.mu.Lock()
	creates, primaryDeletes := f.h.sends, f.h.deletes
	f.h.mu.Unlock()
	if creates != 1 || primaryDeletes != 0 {
		t.Fatalf("primary actual create commands=%d deletes=%d", creates, primaryDeletes)
	}
	deletedTotal, remainingTotal := 0, 0
	// Inspect both owned remotes, including one that might receive no selection.
	for owner, x := range backlog {
		var count, deleted, remaining, expired, unchanged int
		err := f.env.pool.QueryRow(ctx, `SELECT count(*),
			count(*) FILTER (WHERE p.state='deleted' AND p.live_run_id IS NULL AND p.last_audit_pass_id IS NOT NULL),
			count(*) FILTER (WHERE p.state='pinned' AND p.live_run_id IS NOT NULL AND p.audit_next_at>now()),
			count(*) FILTER (WHERE c.state='expired' AND c.reserved_bytes=0),
			count(*) FILTER (WHERE p.attempts=1 AND p.cleanup_attempts=0)
			FROM recovery_prerequisite_closures p JOIN recovery_captures c ON c.id=p.capture_id
			WHERE p.hold_id=$1`, x.m.row.HoldID).Scan(&count, &deleted, &remaining, &expired, &unchanged)
		if err != nil || count != perOwner || deleted+remaining != perOwner ||
			expired != perOwner || unchanged != perOwner {
			t.Fatalf("owner %d durable states: count=%d deleted=%d remaining=%d expired=%d unchanged=%d err=%v",
				owner, count, deleted, remaining, expired, unchanged, err)
		}
		x.h.mu.Lock()
		sends, deletes := x.h.sends, x.h.deletes
		x.h.mu.Unlock()
		// One actual upload advertisement per successful absence cleanup, with
		// no receive-pack command: these historical refs were already absent.
		if delta := x.h.requests.Load() - x.baseline; int64(delta) != int64(deleted) || sends != 0 || deletes != 0 {
			t.Fatalf("owner %d actual request delta=%d absence outcomes=%d sends=%d deletes=%d",
				owner, delta, deleted, sends, deletes)
		}
		deletedTotal += deleted
		remainingTotal += remaining
	}
	if deletedTotal == 0 || deletedTotal+1 != admissions || admissions > passes*5 || remainingTotal < total-passes*5 {
		t.Fatalf("bounded progress: admissions=%d absent cleanups=%d retained live pointers=%d",
			admissions, deletedTotal, remainingTotal)
	}
}
