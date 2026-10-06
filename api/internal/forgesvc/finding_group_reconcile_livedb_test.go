package forgesvc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
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

// rcFake answers ListIssues the way a forge does: it honours the label filter (AND),
// the state filter and the inclusive UpdatedAfter bound, and records every call's
// options so a test can tell the complete-set fetch from the filtered ones.
type rcFake struct {
	forgetest.BaseFake
	mu     sync.Mutex
	issues []forge.Issue
	lists  int
	calls  []forge.ListIssuesOptions
	// unfilteredErr fails only the complete-set fetch (no label, all states, no bound).
	unfilteredErr error
}

func (f *rcFake) ListIssues(_ context.Context, _ int64, opts forge.ListIssuesOptions) ([]forge.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	f.calls = append(f.calls, opts)
	if isUnfilteredList(opts) && f.unfilteredErr != nil {
		return nil, f.unfilteredErr
	}
	var out []forge.Issue
	for _, is := range f.issues {
		if opts.UpdatedAfter != nil && is.UpdatedAt.Before(*opts.UpdatedAfter) {
			continue
		}
		if opts.State != forge.StateAll && is.State != string(opts.State) {
			continue
		}
		hasAll := true
		for _, l := range opts.Labels {
			if !slices.Contains(is.Labels, l) {
				hasAll = false
			}
		}
		if hasAll {
			out = append(out, is)
		}
	}
	return out, nil
}

// unfilteredCalls counts the complete-set fetches made so far.
func (f *rcFake) unfilteredCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if isUnfilteredList(c) {
			n++
		}
	}
	return n
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

func TestFindingGroupReconcileRecordedSettlementFailureContinuesIssueSyncLiveDB(t *testing.T) {
	for _, mode := range []string{"FullSync", "IncrementalSync"} {
		t.Run(mode, func(t *testing.T) {
			e := newRCEnv(t)
			fixed := time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC)
			first, firstIDs := e.inFlight(e.user, 2)
			later, laterIDs := e.inFlight(e.user, 1)
			for i, op := range []store.FindingGroupClaimOperation{first, later} {
				iid := int64(31 + i)
				if ok, err := store.RecordFindingGroupIssue(e.ctx, e.pool, e.user, op.ID, iid,
					fmt.Sprintf("https://forge.e2e/g/rc/-/issues/%d", iid)); err != nil || !ok {
					t.Fatalf("record %s: %v %v", op.ID, ok, err)
				}
				e.exec(`UPDATE finding_group_operations SET created_at=$2 WHERE id=$1`, op.ID, fixed.Add(time.Duration(i)*time.Second))
			}
			e.exec(`UPDATE finding_dispositions SET filing_since=$2 WHERE group_operation_id=$1`, first.ID, fixed)

			// The phase transition follows the member updates in SettleFindingGroup;
			// raising here exercises rollback of those updates as well as the phase.
			fixture := "rc_settle_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			t.Cleanup(func() {
				if _, err := e.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+fixture+` ON finding_group_operations`); err != nil {
					t.Errorf("drop settlement trigger: %v", err)
				}
				if _, err := e.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fixture+`_fn()`); err != nil {
					t.Errorf("drop settlement function: %v", err)
				}
			})
			e.exec(`CREATE FUNCTION ` + fixture + `_fn() RETURNS trigger LANGUAGE plpgsql AS $f$
				BEGIN RAISE EXCEPTION 'recorded settlement fault'; END $f$`)
			e.exec(`CREATE TRIGGER ` + fixture + ` AFTER UPDATE ON finding_group_operations FOR EACH ROW
				WHEN (OLD.phase IS DISTINCT FROM NEW.phase AND NEW.phase = 'settled'
					AND NEW.id = '` + first.ID.String() + `') EXECUTE FUNCTION ` + fixture + `_fn()`)

			capture := &captureHandler{}
			prev := slog.Default()
			slog.SetDefault(slog.New(capture))
			t.Cleanup(func() { slog.SetDefault(prev) })
			cursor := store.FindingGroupCursor{CreatedAt: fixed.Add(-time.Second), ID: uuid.New()}
			e.svc.groupCursors = map[uuid.UUID]store.FindingGroupCursor{e.repoID: cursor}
			want := Marks{PRD: fixed.Add(time.Minute), Open: fixed.Add(2 * time.Minute), Finding: fixed.Add(3 * time.Minute)}
			prd, open, finding := rcIssue(41, ""), rcIssue(42, ""), rcIssue(43, "")
			prd.Title, prd.State, prd.Labels, prd.UpdatedAt = "prd", "closed", []string{"uzi"}, want.PRD
			open.Title, open.Labels, open.UpdatedAt = "open", nil, want.Open
			finding.Title, finding.State, finding.UpdatedAt = "finding", "closed", want.Finding
			e.fake.set(prd, open, finding)
			start := Marks{PRD: fixed, Open: fixed, Finding: fixed}
			sync := func() (Marks, error) {
				if mode == "FullSync" {
					return e.svc.FullSync(e.ctx, e.repoID, 7001, e.fake)
				}
				return e.svc.IncrementalSync(e.ctx, e.repoID, 7001, e.fake, start)
			}
			for pass := 0; pass < 2; pass++ {
				capture.mu.Lock()
				capture.recs = nil
				capture.mu.Unlock()
				got, err := sync()
				if err != nil || got != want {
					t.Fatalf("pass %d: marks=%v error=%v, want %v", pass, got, err, want)
				}
				if n := e.cachedIssueRows(); n != 3 {
					t.Fatalf("pass %d: cached rows=%d, want 3", pass, n)
				}
				for _, issue := range []forge.Issue{prd, open, finding} {
					var title, state string
					var updated time.Time
					if err := e.pool.QueryRow(e.ctx, `SELECT title,state,forge_updated_at FROM issues
						WHERE repo_id=$1 AND forge_issue_iid=$2`, e.repoID, issue.IID).Scan(&title, &state, &updated); err != nil {
						t.Fatal(err)
					}
					if title != issue.Title || state != issue.State || !updated.Equal(issue.UpdatedAt) {
						t.Fatalf("cached #%d = %q %q %v, want %+v", issue.IID, title, state, updated, issue)
					}
				}
				rec, ok := capture.find("finding group reconciliation pending", e.repoID)
				if !ok || rec.Level != slog.LevelWarn {
					t.Fatalf("pass %d: missing settlement warning for repo %s", pass, e.repoID)
				}
				attrs := map[string]any{}
				rec.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.Any(); return true })
				warning := fmt.Sprint(attrs["error"])
				if !strings.Contains(warning, first.ID.String()) || !strings.Contains(warning, "recorded settlement fault") {
					t.Fatalf("pass %d: warning error=%q", pass, warning)
				}
				e.requireClaimed(firstIDs)
				for _, id := range firstIDs {
					var group uuid.UUID
					var since time.Time
					if err := e.pool.QueryRow(e.ctx, `SELECT group_operation_id,filing_since FROM finding_dispositions WHERE id=$1`, id).Scan(&group, &since); err != nil {
						t.Fatal(err)
					}
					if group != first.ID || !since.Equal(fixed) {
						t.Fatalf("member %s: group=%s filing_since=%v, want %s %v", id, group, since, first.ID, fixed)
					}
				}
				if phase, iid := e.phase(first.ID); phase != "issue_recorded" || iid == nil || *iid != 31 {
					t.Fatalf("faulted operation: phase=%s iid=%v", phase, iid)
				}
				e.requireSettled(32, laterIDs)
				if phase, iid := e.phase(later.ID); phase != "settled" || iid == nil || *iid != 32 {
					t.Fatalf("later operation: phase=%s iid=%v", phase, iid)
				}
				if got, exists := e.svc.groupCursors[e.repoID]; !exists || got != cursor {
					t.Fatalf("pass %d: cursor=%+v exists=%v, want %+v", pass, got, exists, cursor)
				}
			}
			e.exec(`DROP TRIGGER ` + fixture + ` ON finding_group_operations`)
			if got, err := sync(); err != nil || got != want {
				t.Fatalf("recovery: marks=%v error=%v", got, err)
			}
			e.requireSettled(31, firstIDs)
			e.requireSettled(32, laterIDs)
			if phase, _ := e.phase(first.ID); phase != "settled" {
				t.Fatalf("recovered operation phase=%s", phase)
			}
		})
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

// One FullSync then nine IncrementalSyncs per reconcile cycle: incremental passes must not
// rotate the page, or every FullSync re-reads page 1 (all pre_call) and never reaches the
// in_flight op on page 2.
func TestFindingGroupReconcileIncrementalDoesNotStarvePageTwoLiveDB(t *testing.T) {
	e := newRCEnv(t)
	for i := 0; i < 100; i++ {
		e.claim(e.user, 1)
	}
	op, ids := e.inFlight(e.user, 1)
	e.fake.set(rcIssue(2500, "body "+rcMarker(op.ID)))
	if err := e.fullSync(); err != nil {
		t.Fatal(err)
	}
	e.requireClaimed(ids)
	for i := 0; i < 9; i++ {
		if _, err := e.svc.IncrementalSync(e.ctx, e.repoID, 7001, e.fake, Marks{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.fullSync(); err != nil {
		t.Fatal(err)
	}
	e.requireSettled(2500, ids)
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
	// A fixed shared clock avoids PostgreSQL/Go skew in the pending-warning age.
	fixedNow := time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC)
	e.svc.findingGroupSince = fixedNow.Sub
	capture := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	oldest, _ := e.inFlight(e.user, 1)
	newer, _ := e.inFlight(e.user, 1)
	e.exec(`UPDATE finding_group_operations SET created_at = $2 WHERE id=$1`, oldest.ID, fixedNow.Add(-3*time.Hour))
	e.exec(`UPDATE finding_group_operations SET created_at = $2 WHERE id=$1`, newer.ID, fixedNow.Add(-time.Hour))
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
	if !ok || age != 3*time.Hour {
		t.Errorf("oldest_age = %v, want time.Duration exactly 3h (the oldest operation, not the newest)", attrs["oldest_age"])
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

// ── regression: marker reconciliation runs over the COMPLETE issue list, in FullSync only ─────

func (e *rcEnv) cachedIssueRows() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM issues WHERE repo_id=$1`, e.repoID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *rcEnv) requireInFlightUntouched(op uuid.UUID) {
	e.t.Helper()
	if p, iid := e.phase(op); p != "in_flight" || iid != nil {
		e.t.Errorf("operation %s = %s iid=%v, want in_flight untouched", op, p, iid)
	}
}

// The marker is on two issues. The older one is closed and has lost its label, so the
// finding-labelled, UpdatedAfter-bounded fetch and the open fetch both miss it: only the
// complete (unlabelled, all-state) set shows both carriers.
func TestFindingGroupReconcileDuplicateHiddenFromFilteredFetchesStaysClaimedLiveDB(t *testing.T) {
	e := newRCEnv(t)
	op, ids := e.inFlight(e.user, 2)
	now := time.Now().UTC().Truncate(time.Second)
	older := rcIssue(1601, "first "+rcMarker(op.ID))
	older.Labels, older.State, older.UpdatedAt = nil, "closed", now.Add(-time.Hour)
	newer := rcIssue(1602, "second "+rcMarker(op.ID))
	newer.UpdatedAt = now
	e.fake.set(older, newer)

	if _, err := e.svc.IncrementalSync(e.ctx, e.repoID, 7001, e.fake, Marks{Finding: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("IncrementalSync: %v", err)
	}
	e.requireClaimed(ids)
	e.requireInFlightUntouched(op.ID)
	if err := e.fullSync(); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	e.requireClaimed(ids)
	e.requireInFlightUntouched(op.ID)
	if got := e.fake.unfilteredCalls(); got != 1 {
		t.Errorf("unfiltered calls = %d, want 1 (the FullSync)", got)
	}
}

// A created issue without the finding label is invisible to the labelled fetches; the
// complete set finds it on the next FullSync whether it is open or closed.
func TestFindingGroupReconcileSettlesUnlabelledIssueLiveDB(t *testing.T) {
	for _, state := range []string{"opened", "closed"} {
		t.Run(state, func(t *testing.T) {
			e := newRCEnv(t)
			op, ids := e.inFlight(e.user, 2)
			issue := rcIssue(1701, "body "+rcMarker(op.ID))
			issue.Labels, issue.State = nil, state
			e.fake.set(issue)
			if err := e.fullSync(); err != nil {
				t.Fatalf("FullSync: %v", err)
			}
			e.requireSettled(1701, ids)
			if p, _ := e.phase(op.ID); p != "settled" {
				t.Errorf("phase = %s, want settled", p)
			}
		})
	}
}

func TestFindingGroupReconcileUnfilteredFetchOnlyWhenMatchableLiveDB(t *testing.T) {
	t.Run("no pending op", func(t *testing.T) {
		e := newRCEnv(t)
		if err := e.fullSync(); err != nil {
			t.Fatal(err)
		}
		if got := e.fake.unfilteredCalls(); got != 0 {
			t.Errorf("unfiltered calls = %d, want 0", got)
		}
	})
	t.Run("only a recorded op", func(t *testing.T) {
		e := newRCEnv(t)
		op, ids := e.inFlight(e.user, 1)
		if ok, err := store.RecordFindingGroupIssue(e.ctx, e.pool, e.user, op.ID, 1801, "https://forge.e2e/g/rc/-/issues/1801"); err != nil || !ok {
			t.Fatalf("record: %v %v", ok, err)
		}
		if err := e.fullSync(); err != nil {
			t.Fatal(err)
		}
		e.requireSettled(1801, ids)
		if got := e.fake.unfilteredCalls(); got != 0 {
			t.Errorf("unfiltered calls = %d, want 0", got)
		}
	})
	t.Run("only a pre_call op", func(t *testing.T) {
		e := newRCEnv(t)
		op, ids := e.claim(e.user, 1)
		if err := e.fullSync(); err != nil {
			t.Fatal(err)
		}
		e.requireClaimed(ids)
		if p, _ := e.phase(op.ID); p != "pre_call" {
			t.Errorf("phase = %s, want pre_call", p)
		}
		if got := e.fake.unfilteredCalls(); got != 0 {
			t.Errorf("unfiltered calls = %d, want 0", got)
		}
	})
	t.Run("an in_flight op makes exactly one FullSync call and no incremental call", func(t *testing.T) {
		e := newRCEnv(t)
		e.inFlight(e.user, 1)
		if _, err := e.svc.IncrementalSync(e.ctx, e.repoID, 7001, e.fake, Marks{}); err != nil {
			t.Fatal(err)
		}
		if got := e.fake.unfilteredCalls(); got != 0 {
			t.Errorf("after IncrementalSync unfiltered calls = %d, want 0", got)
		}
		if err := e.fullSync(); err != nil {
			t.Fatal(err)
		}
		if got := e.fake.unfilteredCalls(); got != 1 {
			t.Errorf("after FullSync unfiltered calls = %d, want 1", got)
		}
	})
}

// A failed complete-set fetch settles nothing: claims stay and the op is untouched. The
// issue sync itself still succeeds, so the cache is written and the group cursor rotates.
func TestFindingGroupReconcileFetchErrorKeepsClaimsAndSyncsLiveDB(t *testing.T) {
	e := newRCEnv(t)
	op, ids := e.inFlight(e.user, 2)
	e.fake.set(rcIssue(1900, "unrelated"))
	e.fake.unfilteredErr = fmt.Errorf("forge pagination cap")
	marks, err := e.svc.FullSync(e.ctx, e.repoID, 7001, e.fake)
	if err != nil {
		t.Fatalf("FullSync failed on an incomplete marker scan: %v", err)
	}
	if marks == (Marks{}) {
		t.Error("marks are zero, want the issue sync's marks")
	}
	e.requireClaimed(ids)
	e.requireInFlightUntouched(op.ID)
	if n := e.cachedIssueRows(); n == 0 {
		t.Error("no cache rows written, want the issue sync to run")
	}
	e.svc.groupCursorMu.Lock()
	_, rotated := e.svc.groupCursors[e.repoID]
	e.svc.groupCursorMu.Unlock()
	if !rotated {
		t.Error("incomplete scan did not rotate the group cursor")
	}
}

// Issue text nobody at uzi controls must not stop settlement: markers for operations
// that are not pending (settled long ago, or planted by anyone who can open an issue)
// are ignored however many there are, and an oversized unrelated description is just
// scanned. A wanted marker repeated in one issue is still ambiguous.
func TestFindingGroupReconcileUntrustedIssueTextLiveDB(t *testing.T) {
	planted := func() forge.Issue {
		var b strings.Builder
		for i := 0; i <= 1000; i++ {
			b.WriteString(rcMarker(uuid.New()))
		}
		is := rcIssue(1903, b.String())
		is.Labels = nil
		return is
	}
	big := func() forge.Issue {
		is := rcIssue(1902, strings.Repeat("a", 2<<20))
		is.Labels = nil
		return is
	}
	for _, tc := range []struct {
		name    string
		issues  func(op uuid.UUID) []forge.Issue
		settles bool
	}{
		{"unwanted markers", func(op uuid.UUID) []forge.Issue { return []forge.Issue{planted(), rcIssue(1904, rcMarker(op))} }, true},
		{"oversized description", func(op uuid.UUID) []forge.Issue { return []forge.Issue{big(), rcIssue(1904, rcMarker(op))} }, true},
		{"repeated wanted marker", func(op uuid.UUID) []forge.Issue {
			return []forge.Issue{rcIssue(1904, strings.Repeat(rcMarker(op), 1001))}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRCEnv(t)
			op, ids := e.inFlight(e.user, 2)
			e.fake.set(append([]forge.Issue{rcIssue(1900, "unrelated")}, tc.issues(op.ID)...)...)
			if _, err := e.svc.FullSync(e.ctx, e.repoID, 7001, e.fake); err != nil {
				t.Fatalf("FullSync: %v", err)
			}
			if n := e.cachedIssueRows(); n == 0 {
				t.Error("no cache rows written, want the issue sync to run")
			}
			if tc.settles {
				e.requireSettled(1904, ids)
			} else {
				e.requireClaimed(ids)
				e.requireInFlightUntouched(op.ID)
			}
		})
	}
}
