package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestIgnoredInferenceSnapshotCreatorsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh")
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
	q := store.New(pool)
	user, conn := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, fmt.Sprintf("ignore-%s@e2e", user))
	mustExec(ctx, t, pool, "INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'gitlab','https://forge.e2e','bot',1,$3)", conn, user, []byte("x"))
	t.Cleanup(func() { mustExec(ctx, t, pool, "DELETE FROM users WHERE id=$1", user) })
	repo := seedRepo(ctx, t, pool, conn, "g/ignored", []string{"jvm"})
	mustExec(ctx, t, pool, "UPDATE repos SET ignored_inferred_capabilities='{jvm}' WHERE id=$1", repo)
	issue, err := q.CreateRun(ctx, store.CreateRunParams{UserID: user, RepoID: repo, PlanSource: "agent", TriggerSource: "manual", Harness: "claude", IssueIid: pgtype.Int8{Int64: 1, Valid: true}, IssueTitle: "snapshot", IssueDescription: "snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, "UPDATE runs SET status='completed' WHERE id=$1", issue.ID)
	auto, err := q.CreateAutoMRReworkRun(ctx, store.CreateAutoMRReworkRunParams{UserID: user, RepoID: repo, IssueTitle: "auto", IssueDescription: "auto", MrIid: pgtype.Int8{Int64: 3, Valid: true}, TargetRunID: pgtype.UUID{Bytes: issue.ID, Valid: true}, PipelineRef: pgtype.Text{String: "uzi/snapshot-auto", Valid: true}, TriggerSource: "mr_rework", Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	manual, err := q.CreateManualMRReworkRunAndAdvance(ctx, store.CreateManualMRReworkRunAndAdvanceParams{UserID: user, RepoID: repo, IssueTitle: "manual", IssueDescription: "manual", MrIid: pgtype.Int8{Int64: 4, Valid: true}, TargetRunID: pgtype.UUID{Bytes: issue.ID, Valid: true}, PipelineRef: pgtype.Text{String: "uzi/snapshot-manual", Valid: true}, Harness: "claude", Marks: []byte(`{}`), PendingAdd: []string{}, PendingRemove: []string{}, PendingSuperseded: []string{}, PendingSupersededBy: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []store.Run{issue, auto, manual} {
		assertCaps(t, "declared hint", run.RequiredCapabilities, []string{"jvm"})
		assertCaps(t, "hinted snapshot", run.HintedCapabilities, []string{"jvm"})
		assertCaps(t, "ignored snapshot", run.IgnoredInferredCapabilities, []string{"jvm"})
	}
	assertPathsInherit(ctx, t, q, pool, user, repo, []string{"jvm"}, []string{"jvm"})
	mustExec(ctx, t, pool, "UPDATE repos SET required_capabilities='{docker}',ignored_inferred_capabilities='{docker}' WHERE id=$1", repo)
	var frozen int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM runs WHERE repo_id=$1 AND required_capabilities='{jvm}' AND ignored_inferred_capabilities='{jvm}' AND hinted_capabilities='{jvm}'", repo).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if frozen != 9 {
		t.Fatalf("captured policy changed or creator omitted it: frozen=%d want=9", frozen)
	}
	next, err := q.CreateRun(ctx, store.CreateRunParams{UserID: user, RepoID: repo, PlanSource: "agent", TriggerSource: "manual", Harness: "claude", IssueIid: pgtype.Int8{Int64: 2, Valid: true}, IssueTitle: "new snapshot", IssueDescription: "snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	assertCaps(t, "new hint", next.RequiredCapabilities, []string{"docker"})
	assertCaps(t, "new policy", next.IgnoredInferredCapabilities, []string{"docker"})
}
