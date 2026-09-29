package forgesvc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB proof for the finding-group reconciler (issue #1724 M4a): the repo-scoped pending read,
// the marker match, the settlement transaction and the WARN diagnostics, all against the real
// schema. The forge is a forgetest.BaseFake whose only behaviour is ListIssues.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

type rcFake struct {
	forgetest.BaseFake
	mu     sync.Mutex
	issues []forge.Issue
	lists  int
}

func (f *rcFake) ListIssues(context.Context, int64, forge.ListIssuesOptions) ([]forge.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return append([]forge.Issue(nil), f.issues...), nil
}

func (f *rcFake) set(issues ...forge.Issue) {
	f.mu.Lock()
	f.issues = issues
	f.mu.Unlock()
}

type rcEnv struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	svc    *Service
	fake   *rcFake
	user   uuid.UUID
	other  uuid.UUID
	repoID uuid.UUID
	run    uuid.UUID
	prefix string
	seq    int
}

func newRCEnv(t *testing.T) *rcEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	e := &rcEnv{
		t: t, ctx: ctx, pool: pool, fake: &rcFake{},
		user: uuid.New(), other: uuid.New(), repoID: uuid.New(), run: uuid.New(),
		prefix: "rc" + uuid.NewString()[:8] + "/",
	}
	conn := uuid.New()
	e.exec(`INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x'),($3,$4,'x')`,
		e.user, fmt.Sprintf("rc-%s@e2e", e.user), e.other, fmt.Sprintf("rc-%s@e2e", e.other))
	e.exec(`INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES ($1,$2,'gitlab','https://forge.e2e','bot',1,$3)`, conn, e.user, []byte{1})
	e.exec(`INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES ($1,$2,7001,$3,'https://forge.e2e/g/rc','main',true)`, e.repoID, conn, "g/rc-"+e.prefix)
	e.exec(`INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,status,kind)
		VALUES ($1,$2,$3,1,'t','d','completed','issue'),($4,$5,$3,2,'t','d','completed','issue')`, e.run, e.user, e.repoID, uuid.New(), e.other)
	e.svc = New(store.New(pool), nil, time.Second, nil)
	e.svc.SetFindingGroupDB(pool)
	return e
}

func (e *rcEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

// claim claims n fresh open members of user as one operation (phase pre_call, far deadline).
func (e *rcEnv) claim(user uuid.UUID, n int) (store.FindingGroupClaimOperation, []uuid.UUID) {
	e.t.Helper()
	run := e.run
	if user == e.other {
		if err := e.pool.QueryRow(e.ctx, `SELECT id FROM runs WHERE user_id=$1 AND repo_id=$2`, e.other, e.repoID).Scan(&run); err != nil {
			e.t.Fatal(err)
		}
	}
	ids := make([]uuid.UUID, n)
	for i := range ids {
		e.seq++
		loc := fmt.Sprintf("%sf%03d.go", e.prefix, e.seq)
		ids[i] = uuid.New()
		e.exec(`INSERT INTO finding_dispositions (id,user_id,repo_id,location,status) VALUES ($1,$2,$3,$4,'open')`, ids[i], user, e.repoID, loc)
		e.exec(`INSERT INTO findings (run_id,user_id,repo_id,location,title,description_md) VALUES ($1,$2,$3,$4,$4,'body')`, run, user, e.repoID, loc)
	}
	op, _, err := store.ClaimFindingGroup(e.ctx, e.pool, user, ids, time.Now().Add(time.Hour))
	if err != nil {
		e.t.Fatalf("claim: %v", err)
	}
	return op, ids
}

// inFlight is claim + the durable pre-CreateIssue write.
func (e *rcEnv) inFlight(user uuid.UUID, n int) (store.FindingGroupClaimOperation, []uuid.UUID) {
	e.t.Helper()
	op, ids := e.claim(user, n)
	if ok, err := store.BeginFindingGroupCall(e.ctx, e.pool, user, op.ID); err != nil || !ok {
		e.t.Fatalf("begin: %v %v", ok, err)
	}
	return op, ids
}

func (e *rcEnv) phase(op uuid.UUID) (string, *int64) {
	e.t.Helper()
	var phase string
	var iid pgtype.Int8
	if err := e.pool.QueryRow(e.ctx, `SELECT phase, issue_iid FROM finding_group_operations WHERE id=$1`, op).Scan(&phase, &iid); err != nil {
		e.t.Fatal(err)
	}
	if iid.Valid {
		v := iid.Int64
		return phase, &v
	}
	return phase, nil
}

func (e *rcEnv) status(id uuid.UUID) (string, *int64, bool) {
	e.t.Helper()
	var status string
	var iid pgtype.Int8
	var group pgtype.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT status, filed_issue_iid, group_operation_id FROM finding_dispositions WHERE id=$1`, id).Scan(&status, &iid, &group); err != nil {
		e.t.Fatal(err)
	}
	if iid.Valid {
		v := iid.Int64
		return status, &v, group.Valid
	}
	return status, nil, group.Valid
}

func (e *rcEnv) requireSettled(iid int64, ids []uuid.UUID) {
	e.t.Helper()
	for _, id := range ids {
		if s, got, grouped := e.status(id); s != "filed" || got == nil || *got != iid || grouped {
			e.t.Errorf("member %s = %s iid=%v grouped=%v, want filed #%d", id, s, got, grouped, iid)
		}
	}
}

func (e *rcEnv) requireClaimed(ids []uuid.UUID) {
	e.t.Helper()
	for _, id := range ids {
		if s, got, grouped := e.status(id); s != "filing" || got != nil || !grouped {
			e.t.Errorf("member %s = %s iid=%v grouped=%v, want still claimed", id, s, got, grouped)
		}
	}
}

func rcMarker(op uuid.UUID) string {
	return "<!-- uzi-finding-group-operation: " + op.String() + " -->"
}

func rcIssue(iid int64, description string) forge.Issue {
	return forge.Issue{
		IID: iid, Title: "grouped", State: "opened", Labels: []string{"agent-found"}, Description: description,
		WebURL: fmt.Sprintf("https://forge.e2e/g/rc/-/issues/%d", iid), UpdatedAt: time.Now(),
	}
}

func (e *rcEnv) fullSync() error {
	_, err := e.svc.FullSync(e.ctx, e.repoID, 7001, e.fake)
	return err
}

// ── marker match → record → settle ───────────────────────────────────────────────────────────

func TestFindingGroupReconcileMatchSettlesInFlightAndUncertainLiveDB(t *testing.T) {
	e := newRCEnv(t)
	flying, flyingIDs := e.inFlight(e.user, 2)
	uncertain, uncertainIDs := e.inFlight(e.user, 3)
	if ok, err := store.MarkFindingGroupUncertain(e.ctx, e.pool, e.user, uncertain.ID); err != nil || !ok {
		t.Fatalf("uncertain: %v %v", ok, err)
	}
	e.fake.set(
		rcIssue(1101, "unrelated"),
		rcIssue(1102, "## Findings\n\nbody\n\n"+rcMarker(flying.ID)),
		rcIssue(1103, "## Findings\n\nbody\n\n"+rcMarker(uncertain.ID)),
	)
	if err := e.fullSync(); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	e.requireSettled(1102, flyingIDs)
	e.requireSettled(1103, uncertainIDs)
	for _, op := range []uuid.UUID{flying.ID, uncertain.ID} {
		if p, _ := e.phase(op); p != "settled" {
			t.Errorf("operation %s phase = %s, want settled", op, p)
		}
	}
	// A repeat pass with the same forge state is a no-op, not an error and not a re-settlement.
	if err := e.fullSync(); err != nil {
		t.Fatalf("second FullSync: %v", err)
	}
	e.requireSettled(1102, flyingIDs)
}

func TestFindingGroupReconcileSettlesRecordedOperationWithoutForgeMatchLiveDB(t *testing.T) {
	e := newRCEnv(t)
	op, ids := e.inFlight(e.user, 2)
	if ok, err := store.RecordFindingGroupIssue(e.ctx, e.pool, e.user, op.ID, 1201, "https://forge.e2e/g/rc/-/issues/1201"); err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	e.requireClaimed(ids) // recorded but not settled
	e.fake.set()          // the forge listing has nothing: the durable record alone is enough
	if err := e.fullSync(); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	e.requireSettled(1201, ids)
	if p, _ := e.phase(op.ID); p != "settled" {
		t.Errorf("phase = %s, want settled", p)
	}
}

func TestFindingGroupReconcileNoMatchAndAmbiguousStayClaimedLiveDB(t *testing.T) {
	e := newRCEnv(t)
	absent, absentIDs := e.inFlight(e.user, 2)
	dup, dupIDs := e.inFlight(e.user, 2)
	preCall, preCallIDs := e.claim(e.user, 1)
	e.exec(`UPDATE finding_group_operations SET deadline_at = now() - interval '1 hour' WHERE id = ANY($1::uuid[])`, []uuid.UUID{absent.ID, dup.ID, preCall.ID})
	e.fake.set(
		rcIssue(1301, "body "+rcMarker(dup.ID)),
		rcIssue(1302, "another "+rcMarker(dup.ID)),
		rcIssue(1303, "a look-alike marker <!-- uzi-finding-group-operation:"+preCall.ID.String()+" -->"),
	)
	for pass := 0; pass < 3; pass++ {
		if err := e.fullSync(); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if _, err := e.svc.IncrementalSync(e.ctx, e.repoID, 7001, e.fake, Marks{}); err != nil {
			t.Fatalf("incremental pass %d: %v", pass, err)
		}
	}
	e.requireClaimed(absentIDs)
	e.requireClaimed(dupIDs)
	e.requireClaimed(preCallIDs)
	for op, want := range map[uuid.UUID]string{absent.ID: "in_flight", dup.ID: "in_flight", preCall.ID: "pre_call"} {
		if p, iid := e.phase(op); p != want || iid != nil {
			t.Errorf("operation %s = %s iid=%v, want %s untouched (the reconciler never releases)", op, p, iid, want)
		}
	}
}

// A marker planted by another user cannot settle an operation on a repo the operation's user does
// not own: the pending selection joins the repo's connection owner to the operation's user.
func TestFindingGroupReconcileIgnoresForeignOperationLiveDB(t *testing.T) {
	e := newRCEnv(t)
	op, ids := e.inFlight(e.other, 1)
	e.fake.set(rcIssue(1401, "planted "+rcMarker(op.ID)))
	if err := e.fullSync(); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	e.requireClaimed(ids)
	if p, iid := e.phase(op.ID); p != "in_flight" || iid != nil {
		t.Errorf("foreign operation = %s iid=%v, want untouched", p, iid)
	}
	if n, _, err := store.FindingGroupRepoPendingStats(e.ctx, e.pool, e.repoID); err != nil || n != 0 {
		t.Errorf("repo pending stats counted a foreign operation: %d %v", n, err)
	}
}

func TestFindingGroupReconcileConcurrentPassesSettleOnceLiveDB(t *testing.T) {
	e := newRCEnv(t)
	op, ids := e.inFlight(e.user, 4)
	e.fake.set(rcIssue(1501, "body "+rcMarker(op.ID)))
	// Count the phase transitions the operation goes through: however many passes race, the
	// recording and the settlement must each happen exactly once.
	tbl := "rc_transitions_" + uuid.NewString()[:8]
	tbl = strings.ReplaceAll(tbl, "-", "")
	e.exec(`CREATE TABLE ` + tbl + ` (phase text NOT NULL)`)
	e.exec(`CREATE FUNCTION ` + tbl + `_fn() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN INSERT INTO ` + tbl + ` VALUES (NEW.phase); RETURN NEW; END $f$`)
	e.exec(`CREATE TRIGGER ` + tbl + `_trg AFTER UPDATE ON finding_group_operations FOR EACH ROW
		WHEN (OLD.phase IS DISTINCT FROM NEW.phase AND NEW.id = '` + op.ID.String() + `') EXECUTE FUNCTION ` + tbl + `_fn()`)
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+tbl+`_trg ON finding_group_operations`)
		_, _ = e.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+tbl+`_fn()`)
		_, _ = e.pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+tbl)
	})
	var wg sync.WaitGroup
	errs := make([]error, 6)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = e.fullSync()
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("pass %d: %v", i, err)
		}
	}
	e.requireSettled(1501, ids)
	rows, err := e.pool.Query(e.ctx, `SELECT phase FROM `+tbl+` ORDER BY phase`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var phases []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		phases = append(phases, p)
	}
	if fmt.Sprint(phases) != "[issue_recorded settled]" {
		t.Errorf("phase transitions = %v, want exactly one issue_recorded and one settled", phases)
	}
}

// More recorded operations than one page: every one settles within two passes (cursor then wrap).
func TestFindingGroupReconcilePagesPastOnePageLiveDB(t *testing.T) {
	e := newRCEnv(t)
	type recorded struct {
		iid int64
		ids []uuid.UUID
	}
	var all []recorded
	for i := 0; i < 103; i++ {
		op, ids := e.inFlight(e.user, 1)
		iid := int64(2000 + i)
		if ok, err := store.RecordFindingGroupIssue(e.ctx, e.pool, e.user, op.ID, iid, fmt.Sprintf("https://forge.e2e/g/rc/-/issues/%d", iid)); err != nil || !ok {
			t.Fatalf("record %d: %v %v", i, ok, err)
		}
		all = append(all, recorded{iid, ids})
	}
	e.fake.set()
	for pass := 0; pass < 2; pass++ {
		if err := e.fullSync(); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	for _, r := range all {
		e.requireSettled(r.iid, r.ids)
	}
	if n, _, err := store.FindingGroupRepoPendingStats(e.ctx, e.pool, e.repoID); err != nil || n != 0 {
		t.Errorf("pending after two passes = %d (%v)", n, err)
	}
}

// ── diagnostics ──────────────────────────────────────────────────────────────────────────────

type captureHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.recs = append(h.recs, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) find(msg string, repo uuid.UUID) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.Message != msg {
			continue
		}
		match := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "repo_id" && a.Value.Any() == repo {
				match = true
			}
			return true
		})
		if match {
			return r, true
		}
	}
	return slog.Record{}, false
}

func TestFindingGroupReconcileWarnCarriesPendingCountAndOldestAgeLiveDB(t *testing.T) {
	e := newRCEnv(t)
	capture := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	oldest, _ := e.inFlight(e.user, 1)
	newer, _ := e.inFlight(e.user, 1)
	e.exec(`UPDATE finding_group_operations SET created_at = now() - interval '3 hours' WHERE id=$1`, oldest.ID)
	e.exec(`UPDATE finding_group_operations SET created_at = now() - interval '1 hour' WHERE id=$1`, newer.ID)
	e.fake.set()
	if err := e.fullSync(); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	rec, ok := capture.find("finding group reconciliation pending", e.repoID)
	if !ok {
		t.Fatal("no WARN for the pending groups")
	}
	if rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN", rec.Level)
	}
	attrs := map[string]any{}
	rec.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.Any(); return true })
	if got, _ := attrs["pending_group_operations"].(int64); got != 2 {
		t.Errorf("pending_group_operations = %v, want 2", attrs["pending_group_operations"])
	}
	age, ok := attrs["oldest_age"].(time.Duration)
	if !ok || age < 3*time.Hour || age > 3*time.Hour+5*time.Minute {
		t.Errorf("oldest_age = %v, want about 3h (the oldest operation, not the newest)", attrs["oldest_age"])
	}
	if _, has := attrs["error"]; has {
		t.Errorf("unexpected error attr: %v", attrs["error"])
	}

	// Once nothing is pending the pass is silent.
	if _, err := store.MarkFindingGroupUncertain(e.ctx, e.pool, e.user, oldest.ID); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE finding_group_operations SET deadline_at = now() - interval '1 minute' WHERE id = ANY($1::uuid[])`, []uuid.UUID{oldest.ID, newer.ID})
	for _, op := range []uuid.UUID{oldest.ID, newer.ID} {
		if ok, err := store.ReleaseFindingGroupAfterDeadline(e.ctx, e.pool, e.user, op); err != nil || !ok {
			t.Fatalf("release %s: %v %v", op, ok, err)
		}
	}
	capture.mu.Lock()
	capture.recs = nil
	capture.mu.Unlock()
	if err := e.fullSync(); err != nil {
		t.Fatal(err)
	}
	if _, ok := capture.find("finding group reconciliation pending", e.repoID); ok {
		t.Error("WARN emitted with no pending operations")
	}
}
