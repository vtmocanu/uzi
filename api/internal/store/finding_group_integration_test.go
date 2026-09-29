package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
