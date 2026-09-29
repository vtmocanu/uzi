package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestFindingGroupLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	user, other, conn, repo, run := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x'),($3,$4,'x')`, user, fmt.Sprintf("group-%s@e2e", user), other, fmt.Sprintf("group-%s@e2e", other))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
        VALUES($1,$2,'gitlab','https://forge.e2e','bot',1,$3)`, conn, user, []byte{1})
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
        VALUES($1,$2,9001,'g/group','https://forge.e2e/g/group','main',true)`, repo, conn)
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,kind)
        VALUES($1,$2,$3,1,'Group','desc','completed','issue')`, run, user, repo)
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	for i, id := range ids {
		loc := fmt.Sprintf("group/%d.go", i)
		mustExec(ctx, t, pool, `INSERT INTO finding_dispositions(id,user_id,repo_id,location,status) VALUES($1,$2,$3,$4,'open')`, id, user, repo, loc)
		mustExec(ctx, t, pool, `INSERT INTO findings(run_id,user_id,repo_id,location,title,description_md) VALUES($1,$2,$3,$4,$5,'body')`, run, user, repo, loc, loc)
	}
	if _, _, err := store.ClaimFindingGroup(ctx, pool, user, []uuid.UUID{ids[0], uuid.New()}, time.Now().Add(time.Hour)); !errors.Is(err, store.ErrFindingGroupUnavailable) {
		t.Fatalf("all-or-nothing: %v", err)
	}
	op, members, err := store.ClaimFindingGroup(ctx, pool, user, []uuid.UUID{ids[1], ids[0], ids[0]}, time.Now().Add(time.Hour))
	if err != nil || len(members) != 2 {
		t.Fatalf("claim: %+v %v", members, err)
	}
	if _, _, err := store.ClaimFindingGroup(ctx, pool, other, ids, time.Now().Add(time.Hour)); !errors.Is(err, store.ErrFindingGroupUnavailable) {
		t.Fatalf("foreign claim: %v", err)
	}
	if n, err := store.New(pool).SweepStrandedFilingFindings(ctx, pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}); err != nil || n != 0 {
		t.Fatalf("sweep group: %d %v", n, err)
	}
	if ok, err := store.BeginFindingGroupCall(ctx, pool, user, op.ID); err != nil || !ok {
		t.Fatalf("begin: %v %v", ok, err)
	}
	if ok, err := store.MarkFindingGroupUncertain(ctx, pool, user, op.ID); err != nil || !ok {
		t.Fatalf("uncertain: %v %v", ok, err)
	}
	if ok, err := store.ReleaseFindingGroupAfterDeadline(ctx, pool, user, op.ID); err != nil || ok {
		t.Fatalf("early release: %v %v", ok, err)
	}
	if ok, err := store.RecordFindingGroupIssue(ctx, pool, user, op.ID, 42, "https://forge.e2e/g/group/issues/42"); err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	if ok, err := store.ReleaseFindingGroupAfterDeadline(ctx, pool, user, op.ID); err != nil || ok {
		t.Fatalf("recorded release: %v %v", ok, err)
	}
	if ok, err := store.SettleFindingGroup(ctx, pool, user, op.ID); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM finding_dispositions WHERE id=ANY($1::uuid[]) AND status='filed' AND filed_issue_iid=42 AND group_operation_id IS NULL`, ids).Scan(&n); err != nil || n != 2 {
		t.Fatalf("settled members: %d %v", n, err)
	}
	if _, err := store.GetPendingFindingGroup(ctx, pool, other, op.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign lookup: %v", err)
	}
	for _, phase := range []string{"pre_call", "in_flight", "returned_uncertain"} {
		t.Run(phase+"_deadline_release", func(t *testing.T) {
			id := uuid.New()
			loc := "group/" + id.String() + ".go"
			mustExec(ctx, t, pool, `INSERT INTO finding_dispositions(id,user_id,repo_id,location,status) VALUES($1,$2,$3,$4,'open')`, id, user, repo, loc)
			mustExec(ctx, t, pool, `INSERT INTO findings(run_id,user_id,repo_id,location,title,description_md) VALUES($1,$2,$3,$4,$4,'body')`, run, user, repo, loc)
			op, _, err := store.ClaimFindingGroup(ctx, pool, user, []uuid.UUID{id}, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if phase != "pre_call" {
				if ok, err := store.BeginFindingGroupCall(ctx, pool, user, op.ID); err != nil || !ok {
					t.Fatalf("begin before deadline: %v %v", ok, err)
				}
			}
			if phase == "returned_uncertain" {
				if ok, err := store.MarkFindingGroupUncertain(ctx, pool, user, op.ID); err != nil || !ok {
					t.Fatalf("uncertain before deadline: %v %v", ok, err)
				}
			}
			if ok, err := store.ReleaseFindingGroupAfterDeadline(ctx, pool, user, op.ID); err != nil || ok {
				t.Fatalf("release before deadline: %v %v", ok, err)
			}
			mustExec(ctx, t, pool, `UPDATE finding_group_operations SET deadline_at=now()-interval '1 second' WHERE id=$1`, op.ID)
			if phase == "pre_call" {
				if ok, err := store.BeginFindingGroupCall(ctx, pool, user, op.ID); err != nil || ok {
					t.Fatalf("begin after deadline: %v %v", ok, err)
				}
			}
			if ok, err := store.ReleaseFindingGroupAfterDeadline(ctx, pool, user, op.ID); err != nil || !ok {
				t.Fatalf("release after deadline: %v %v", ok, err)
			}
			if ok, err := store.BeginFindingGroupCall(ctx, pool, user, op.ID); err != nil || ok {
				t.Fatalf("released begin: %v %v", ok, err)
			}
			if ok, err := store.MarkFindingGroupUncertain(ctx, pool, user, op.ID); err != nil || ok {
				t.Fatalf("released uncertain: %v %v", ok, err)
			}
			if ok, err := store.RecordFindingGroupIssue(ctx, pool, user, op.ID, 43, "https://forge.e2e/g/group/issues/43"); err != nil || ok {
				t.Fatalf("released record: %v %v", ok, err)
			}
			if ok, err := store.SettleFindingGroup(ctx, pool, user, op.ID); err != nil || ok {
				t.Fatalf("released settle: %v %v", ok, err)
			}
			if _, err := store.GetPendingFindingGroup(ctx, pool, user, op.ID); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("released resume: %v", err)
			}
			var status string
			if err := pool.QueryRow(ctx, `SELECT status FROM finding_dispositions WHERE id=$1`, id).Scan(&status); err != nil || status != "open" {
				t.Fatalf("released member: %s %v", status, err)
			}
		})
	}
	count, oldest, err := store.FindingGroupPendingStats(ctx, pool, user)
	if err != nil || count != 0 || oldest != nil {
		t.Fatalf("pending: %d %v %v", count, oldest, err)
	}
}

// fgsEnv is a fresh user pair, a connection, one repo and a run, for the group store tests. Every
// uuid is fresh: the live-DB runner shares one database across the whole suite.
type fgsEnv struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	user  uuid.UUID
	other uuid.UUID
	repo  uuid.UUID
	run   uuid.UUID
	seq   int
}

func newFGSEnv(t *testing.T) *fgsEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	e := &fgsEnv{t: t, ctx: ctx, pool: pool, user: uuid.New(), other: uuid.New(), repo: uuid.New(), run: uuid.New()}
	conn := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x'),($3,$4,'x')`,
		e.user, fmt.Sprintf("fgs-%s@e2e", e.user), e.other, fmt.Sprintf("fgs-%s@e2e", e.other))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES($1,$2,'gitlab','https://forge.e2e','bot',1,$3)`, conn, e.user, []byte{1})
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES($1,$2,9101,$3,'https://forge.e2e/g/fgs','main',true)`, e.repo, conn, "g/fgs-"+e.repo.String())
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,kind)
		VALUES($1,$2,$3,1,'t','d','completed','issue')`, e.run, e.user, e.repo)
	return e
}

// member seeds one open disposition with evidence and returns (disposition id, location).
func (e *fgsEnv) member() (uuid.UUID, string) {
	e.t.Helper()
	e.seq++
	id := uuid.New()
	loc := fmt.Sprintf("fgs/%s/%03d.go", e.repo, e.seq)
	mustExec(e.ctx, e.t, e.pool, `INSERT INTO finding_dispositions(id,user_id,repo_id,location,status) VALUES($1,$2,$3,$4,'open')`, id, e.user, e.repo, loc)
	mustExec(e.ctx, e.t, e.pool, `INSERT INTO findings(run_id,user_id,repo_id,location,title,description_md) VALUES($1,$2,$3,$4,$4,'body')`, e.run, e.user, e.repo, loc)
	return id, loc
}

func (e *fgsEnv) members(n int) []uuid.UUID {
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i], _ = e.member()
	}
	return ids
}

func (e *fgsEnv) claim(n int) (store.FindingGroupClaimOperation, []uuid.UUID) {
	e.t.Helper()
	ids := e.members(n)
	op, _, err := store.ClaimFindingGroup(e.ctx, e.pool, e.user, ids, time.Now().Add(time.Hour))
	if err != nil {
		e.t.Fatalf("claim: %v", err)
	}
	return op, ids
}

func (e *fgsEnv) statuses(ids []uuid.UUID) map[string]int {
	e.t.Helper()
	out := map[string]int{}
	rows, err := e.pool.Query(e.ctx, `SELECT status FROM finding_dispositions WHERE id=ANY($1::uuid[])`, ids)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			e.t.Fatal(err)
		}
		out[st]++
	}
	return out
}

func TestFindingGroupClaimBoundsAndDeadlineLiveDB(t *testing.T) {
	e := newFGSEnv(t)
	fifty := e.members(50)
	if _, ms, err := store.ClaimFindingGroup(e.ctx, e.pool, e.user, fifty, time.Now().Add(time.Hour)); err != nil || len(ms) != 50 {
		t.Fatalf("50 members: %d %v", len(ms), err)
	}
	over := e.members(51)
	if _, _, err := store.ClaimFindingGroup(e.ctx, e.pool, e.user, over, time.Now().Add(time.Hour)); !errors.Is(err, store.ErrFindingGroupUnavailable) {
		t.Fatalf("51 members: %v", err)
	}
	if got := e.statuses(over); got["open"] != 51 {
		t.Fatalf("a refused 51-member claim moved members: %v", got)
	}
	one := e.members(1)
	if _, _, err := store.ClaimFindingGroup(e.ctx, e.pool, e.user, one, time.Now().Add(-time.Second)); !errors.Is(err, store.ErrFindingGroupUnavailable) {
		t.Fatalf("past deadline: %v", err)
	}
	if _, _, err := store.ClaimFindingGroup(e.ctx, e.pool, e.user, nil, time.Now().Add(time.Hour)); !errors.Is(err, store.ErrFindingGroupUnavailable) {
		t.Fatalf("empty: %v", err)
	}
	if got := e.statuses(one); got["open"] != 1 {
		t.Fatalf("a refused claim moved a member: %v", got)
	}
}

func TestFindingGroupClaimConcurrentExactlyOneWinsLiveDB(t *testing.T) {
	e := newFGSEnv(t)
	ids := e.members(3)
	const racers = 8
	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Each racer lists the members in a different order: the ordered row locks must
			// serialize them without deadlocking.
			mine := append([]uuid.UUID(nil), ids...)
			for r := 0; r < i%3; r++ {
				mine = append(mine[1:], mine[0])
			}
			_, _, errs[i] = store.ClaimFindingGroup(e.ctx, e.pool, e.user, mine, time.Now().Add(time.Hour))
		}()
	}
	close(start)
	wg.Wait()
	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, store.ErrFindingGroupConflict):
		default:
			t.Errorf("racer %d: %v", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d claims won, want exactly 1", won)
	}
	var ops int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(DISTINCT group_operation_id) FROM finding_dispositions WHERE id=ANY($1::uuid[]) AND status='filing'`, ids).Scan(&ops); err != nil || ops != 1 {
		t.Fatalf("members claimed under %d operations (%v), want 1", ops, err)
	}
	var total int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM finding_group_operations WHERE user_id=$1`, e.user).Scan(&total); err != nil || total != 1 {
		t.Fatalf("%d operations exist (%v), want 1: losers must not leave rows", total, err)
	}
}

func TestFindingGroupDefinitiveReleaseGuardsLiveDB(t *testing.T) {
	e := newFGSEnv(t)

	// pre_call and in_flight before the deadline release definitively.
	op1, ids1 := e.claim(2)
	if ok, err := store.ReleaseFindingGroupDefinitive(e.ctx, e.pool, e.user, op1.ID); err != nil || !ok {
		t.Fatalf("pre_call definitive: %v %v", ok, err)
	}
	if got := e.statuses(ids1); got["open"] != 2 {
		t.Fatalf("members after release: %v", got)
	}
	op2, ids2 := e.claim(2)
	if ok, err := store.BeginFindingGroupCall(e.ctx, e.pool, e.user, op2.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := store.ReleaseFindingGroupDefinitive(e.ctx, e.pool, e.user, op2.ID); err != nil || !ok {
		t.Fatalf("in_flight definitive: %v %v", ok, err)
	}
	if got := e.statuses(ids2); got["open"] != 2 {
		t.Fatalf("members after release: %v", got)
	}

	// An uncertain outcome is never definitive.
	op3, ids3 := e.claim(1)
	_, _ = store.BeginFindingGroupCall(e.ctx, e.pool, e.user, op3.ID)
	_, _ = store.MarkFindingGroupUncertain(e.ctx, e.pool, e.user, op3.ID)
	if ok, err := store.ReleaseFindingGroupDefinitive(e.ctx, e.pool, e.user, op3.ID); err != nil || ok {
		t.Fatalf("uncertain definitive: %v %v", ok, err)
	}
	// Past its deadline a claim is no longer definitively releasable either, only after-deadline.
	op4, ids4 := e.claim(1)
	mustExec(e.ctx, t, e.pool, `UPDATE finding_group_operations SET deadline_at = now() - interval '1 second' WHERE id=$1`, op4.ID)
	if ok, err := store.ReleaseFindingGroupDefinitive(e.ctx, e.pool, e.user, op4.ID); err != nil || ok {
		t.Fatalf("expired definitive: %v %v", ok, err)
	}
	// Another user's release changes nothing, before or after the deadline.
	if ok, err := store.ReleaseFindingGroupAfterDeadline(e.ctx, e.pool, e.other, op4.ID); err != nil || ok {
		t.Fatalf("foreign release: %v %v", ok, err)
	}
	if got := e.statuses(append(ids3, ids4...)); got["filing"] != 2 {
		t.Fatalf("members after refused releases: %v", got)
	}
	// A recorded issue can never be released.
	op5, ids5 := e.claim(1)
	_, _ = store.BeginFindingGroupCall(e.ctx, e.pool, e.user, op5.ID)
	if ok, err := store.RecordFindingGroupIssue(e.ctx, e.pool, e.user, op5.ID, 9, "https://forge.e2e/i/9"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	mustExec(e.ctx, t, e.pool, `UPDATE finding_group_operations SET deadline_at = now() - interval '1 second' WHERE id=$1`, op5.ID)
	if ok, err := store.ReleaseFindingGroupAfterDeadline(e.ctx, e.pool, e.user, op5.ID); err != nil || ok {
		t.Fatalf("recorded release: %v %v", ok, err)
	}
	if got := e.statuses(ids5); got["filing"] != 1 {
		t.Fatalf("recorded members: %v", got)
	}
}

func TestFindingGroupSettleRefusesBrokenMembershipLiveDB(t *testing.T) {
	e := newFGSEnv(t)
	op, ids := e.claim(3)
	if ok, err := store.BeginFindingGroupCall(e.ctx, e.pool, e.user, op.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := store.RecordFindingGroupIssue(e.ctx, e.pool, e.user, op.ID, 31, "https://forge.e2e/i/31"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// One member was pulled out from under the operation (as a concurrent writer might): all-or-nothing.
	mustExec(e.ctx, t, e.pool, `UPDATE finding_dispositions SET status='open', filing_since=NULL, group_operation_id=NULL WHERE id=$1`, ids[0])
	if ok, err := store.SettleFindingGroup(e.ctx, e.pool, e.user, op.ID); ok || !errors.Is(err, store.ErrFindingGroupUnavailable) {
		t.Fatalf("settle with a missing member: %v %v", ok, err)
	}
	if got := e.statuses(ids); got["filed"] != 0 || got["filing"] != 2 || got["open"] != 1 {
		t.Fatalf("a refused settle moved members: %v", got)
	}
	var phase string
	if err := e.pool.QueryRow(e.ctx, `SELECT phase FROM finding_group_operations WHERE id=$1`, op.ID).Scan(&phase); err != nil || phase != "issue_recorded" {
		t.Fatalf("phase = %q (%v), want issue_recorded", phase, err)
	}
	// Recording twice never overwrites the first identity.
	if ok, err := store.RecordFindingGroupIssue(e.ctx, e.pool, e.user, op.ID, 32, "https://forge.e2e/i/32"); err != nil || ok {
		t.Fatalf("second record: %v %v", ok, err)
	}
	if ok, err := store.RecordFindingGroupIssue(e.ctx, e.pool, e.user, op.ID, 0, ""); !errors.Is(err, store.ErrFindingGroupUnavailable) || ok {
		t.Fatalf("empty identity: %v %v", ok, err)
	}
}

// The single-file settle, revert and sweep are keyed on the coordinate: none may act on a row that
// a group operation owns.
func TestFindingGroupRowsAreInvisibleToSingleFilingQueriesLiveDB(t *testing.T) {
	e := newFGSEnv(t)
	q := store.New(e.pool)
	op, ids := e.claim(1)
	var loc string
	if err := e.pool.QueryRow(e.ctx, `SELECT location FROM finding_dispositions WHERE id=$1`, ids[0]).Scan(&loc); err != nil {
		t.Fatal(err)
	}
	if n, err := q.ClaimFindingForFiling(e.ctx, store.ClaimFindingForFilingParams{UserID: e.user, RepoID: e.repo, Location: loc}); err != nil || n != 0 {
		t.Fatalf("single claim of a group member: %d %v", n, err)
	}
	if n, err := q.RevertFindingFiling(e.ctx, store.RevertFindingFilingParams{UserID: e.user, RepoID: e.repo, Location: loc}); err != nil || n != 0 {
		t.Fatalf("single revert of a group member: %d %v", n, err)
	}
	if n, err := q.SettleFindingFiled(e.ctx, store.SettleFindingFiledParams{
		FiledIssueIid: pgtype.Int8{Int64: 5, Valid: true}, FiledIssueUrl: "u", UserID: e.user, RepoID: e.repo, Location: loc,
	}); err != nil || n != 0 {
		t.Fatalf("single settle of a group member: %d %v", n, err)
	}
	if n, err := q.SweepStrandedFilingFindings(e.ctx, pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true}); err != nil {
		t.Fatalf("sweep: %v (%d)", err, n)
	}
	if got := e.statuses(ids); got["filing"] != 1 {
		t.Fatalf("member after single-file queries: %v", got)
	}
	var group pgtype.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT group_operation_id FROM finding_dispositions WHERE id=$1`, ids[0]).Scan(&group); err != nil || !group.Valid || uuid.UUID(group.Bytes) != op.ID {
		t.Fatalf("group link lost: %v %v", group, err)
	}
}

func TestFindingDispositionForEvidenceLiveDB(t *testing.T) {
	e := newFGSEnv(t)
	id, loc := e.member()
	var older uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM findings WHERE user_id=$1 AND repo_id=$2 AND location=$3`, e.user, e.repo, loc).Scan(&older); err != nil {
		t.Fatal(err)
	}
	mustExec(e.ctx, t, e.pool, `UPDATE findings SET created_at = now() - interval '2 hours' WHERE id=$1`, older)
	var newer uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO findings(run_id,user_id,repo_id,location,title,description_md) VALUES($1,$2,$3,$4,'again','body2') RETURNING id`,
		e.run, e.user, e.repo, loc).Scan(&newer); err != nil {
		t.Fatal(err)
	}
	for name, ev := range map[string]uuid.UUID{"older": older, "newer": newer} {
		got, err := store.FindingDispositionForEvidence(e.ctx, e.pool, e.user, ev)
		if err != nil || got != id {
			t.Errorf("%s evidence: %v %v, want %s", name, got, err, id)
		}
	}
	if _, err := store.FindingDispositionForEvidence(e.ctx, e.pool, e.other, older); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("foreign evidence: %v", err)
	}
	if _, err := store.FindingDispositionForEvidence(e.ctx, e.pool, e.user, uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("unknown evidence: %v", err)
	}
	// The draft read picks the LATEST evidence for the coordinate.
	ms, err := store.ReadFindingGroupDraftMembers(e.ctx, e.pool, e.user, []uuid.UUID{id})
	if err != nil || len(ms) != 1 || !ms[0].FindingID.Valid || uuid.UUID(ms[0].FindingID.Bytes) != newer {
		t.Fatalf("draft members = %+v %v, want latest evidence %s", ms, err, newer)
	}
	if ms, err := store.ReadFindingGroupDraftMembers(e.ctx, e.pool, e.other, []uuid.UUID{id}); err != nil || len(ms) != 0 {
		t.Fatalf("foreign draft members = %+v %v", ms, err)
	}
}

func TestFindingGroupPendingReadsLiveDB(t *testing.T) {
	e := newFGSEnv(t)
	// Three pending operations at known ages, one settled, one released.
	var ops []store.FindingGroupClaimOperation
	for i := 0; i < 3; i++ {
		op, _ := e.claim(1)
		mustExec(e.ctx, t, e.pool, `UPDATE finding_group_operations SET created_at = now() - make_interval(hours => $2) WHERE id=$1`, op.ID, 3-i)
		if i > 0 {
			if ok, err := store.BeginFindingGroupCall(e.ctx, e.pool, e.user, op.ID); err != nil || !ok {
				t.Fatal(ok, err)
			}
		}
		ops = append(ops, op)
	}
	settled, _ := e.claim(1)
	_, _ = store.BeginFindingGroupCall(e.ctx, e.pool, e.user, settled.ID)
	_, _ = store.RecordFindingGroupIssue(e.ctx, e.pool, e.user, settled.ID, 7, "https://forge.e2e/i/7")
	if ok, err := store.SettleFindingGroup(e.ctx, e.pool, e.user, settled.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	released, _ := e.claim(1)
	if ok, err := store.ReleaseFindingGroupDefinitive(e.ctx, e.pool, e.user, released.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}

	page, err := store.ListPendingFindingGroupsForRepo(e.ctx, e.pool, e.repo, nil)
	if err != nil || len(page) != 3 {
		t.Fatalf("pending page = %d %v, want 3 (settled and released excluded)", len(page), err)
	}
	for i := range page {
		if page[i].ID != ops[i].ID {
			t.Errorf("page[%d] = %s, want %s (oldest first)", i, page[i].ID, ops[i].ID)
		}
	}
	cursor := store.FindingGroupCursor{CreatedAt: page[0].CreatedAt, ID: page[0].ID}
	after, err := store.ListPendingFindingGroupsForRepo(e.ctx, e.pool, e.repo, &cursor)
	if err != nil || len(after) != 2 || after[0].ID != ops[1].ID {
		t.Fatalf("after cursor = %v %v", after, err)
	}
	last := store.FindingGroupCursor{CreatedAt: page[2].CreatedAt, ID: page[2].ID}
	if tail, err := store.ListPendingFindingGroupsForRepo(e.ctx, e.pool, e.repo, &last); err != nil || len(tail) != 0 {
		t.Fatalf("past the end = %v %v, want an empty page (the caller wraps)", tail, err)
	}

	count, oldest, err := store.FindingGroupRepoPendingStats(e.ctx, e.pool, e.repo)
	if err != nil || count != 3 || oldest == nil || time.Since(*oldest) < 3*time.Hour {
		t.Fatalf("repo stats = %d %v %v, want 3 pending, oldest >= 3h", count, oldest, err)
	}
	ucount, uoldest, err := store.FindingGroupPendingStats(e.ctx, e.pool, e.user)
	if err != nil || ucount != 3 || uoldest == nil {
		t.Fatalf("user stats = %d %v %v", ucount, uoldest, err)
	}
	if c, o, err := store.FindingGroupPendingStats(e.ctx, e.pool, e.other); err != nil || c != 0 || o != nil {
		t.Fatalf("foreign user stats = %d %v %v", c, o, err)
	}

	// ByIDs only returns in_flight / returned_uncertain operations without an issue, for the repo's owner.
	byIDs, err := store.ListPendingFindingGroupsByIDs(e.ctx, e.pool, e.repo, []uuid.UUID{ops[0].ID, ops[1].ID, ops[2].ID, settled.ID, released.ID})
	if err != nil || len(byIDs) != 2 {
		t.Fatalf("by ids = %d %v, want the two in_flight operations (pre_call, settled and released excluded)", len(byIDs), err)
	}
	if _, err := store.ListPendingFindingGroupsByIDs(e.ctx, e.pool, e.repo, nil); !errors.Is(err, store.ErrFindingGroupUnavailable) {
		t.Fatalf("empty ids: %v", err)
	}
	// An operation whose user does not own the repo's connection is invisible to both repo-scoped
	// reads, so a marker planted by that user can never settle anything (defense in depth: the
	// per-id read does its own owner join, independent of the page read).
	foreignRun, foreignDisp, foreignLoc := uuid.New(), uuid.New(), "fgs/foreign/"+uuid.NewString()+".go"
	mustExec(e.ctx, t, e.pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,kind)
		VALUES($1,$2,$3,9,'t','d','completed','issue')`, foreignRun, e.other, e.repo)
	mustExec(e.ctx, t, e.pool, `INSERT INTO finding_dispositions(id,user_id,repo_id,location,status) VALUES($1,$2,$3,$4,'open')`, foreignDisp, e.other, e.repo, foreignLoc)
	mustExec(e.ctx, t, e.pool, `INSERT INTO findings(run_id,user_id,repo_id,location,title,description_md) VALUES($1,$2,$3,$4,$4,'body')`, foreignRun, e.other, e.repo, foreignLoc)
	foreignOp, _, err := store.ClaimFindingGroup(e.ctx, e.pool, e.other, []uuid.UUID{foreignDisp}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.BeginFindingGroupCall(e.ctx, e.pool, e.other, foreignOp.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got, err := store.ListPendingFindingGroupsByIDs(e.ctx, e.pool, e.repo, []uuid.UUID{foreignOp.ID}); err != nil || len(got) != 0 {
		t.Fatalf("foreign operation by ids = %v %v", got, err)
	}
	if got, err := store.ListPendingFindingGroupsForRepo(e.ctx, e.pool, e.repo, nil); err != nil || len(got) != 3 {
		t.Fatalf("foreign operation in the repo page: %d %v, want the same 3", len(got), err)
	}
	if n, _, err := store.FindingGroupRepoPendingStats(e.ctx, e.pool, e.repo); err != nil || n != 3 {
		t.Fatalf("repo stats counted the foreign operation: %d %v", n, err)
	}
	// A repo the user does not own returns nothing even for a real operation id.
	otherRepo := uuid.New()
	otherConn := uuid.New()
	mustExec(e.ctx, t, e.pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES($1,$2,'gitlab','https://forge.e2e','bot',1,$3)`, otherConn, e.other, []byte{1})
	mustExec(e.ctx, t, e.pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES($1,$2,9102,$3,'https://forge.e2e/g/fgs2','main',true)`, otherRepo, otherConn, "g/fgs2-"+otherRepo.String())
	if got, err := store.ListPendingFindingGroupsByIDs(e.ctx, e.pool, otherRepo, []uuid.UUID{ops[1].ID}); err != nil || len(got) != 0 {
		t.Fatalf("other repo by ids = %v %v", got, err)
	}
}

// Deleting a repo or a user while a group operation is pending must not trip the operation <->
// disposition foreign keys (the disposition -> operation link has no ON DELETE action of its own).
func TestFindingGroupPendingOperationDoesNotBlockCascadeDeletesLiveDB(t *testing.T) {
	for _, target := range []string{"repo", "user"} {
		t.Run(target, func(t *testing.T) {
			e := newFGSEnv(t)
			op, ids := e.claim(2)
			if ok, err := store.BeginFindingGroupCall(e.ctx, e.pool, e.user, op.ID); err != nil || !ok {
				t.Fatal(ok, err)
			}
			switch target {
			case "repo":
				mustExec(e.ctx, t, e.pool, `DELETE FROM repos WHERE id=$1`, e.repo)
			default:
				mustExec(e.ctx, t, e.pool, `DELETE FROM users WHERE id=$1`, e.user)
			}
			var n int
			if err := e.pool.QueryRow(e.ctx, `SELECT (SELECT count(*) FROM finding_group_operations WHERE id=$1)
				+ (SELECT count(*) FROM finding_group_members WHERE operation_id=$1)
				+ (SELECT count(*) FROM finding_dispositions WHERE id=ANY($2::uuid[]))`, op.ID, ids).Scan(&n); err != nil || n != 0 {
				t.Fatalf("leftover rows after deleting the %s: %d (%v)", target, n, err)
			}
		})
	}
}
