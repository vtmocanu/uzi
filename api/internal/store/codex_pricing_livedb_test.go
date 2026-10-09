package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Run only through the throwaway LiveDB harness. Removing relevant usage inside
// this rollback-only transaction keeps earlier tests from filling LIMIT 11.
func TestRecentUnpricedCodexModelsLiveDB(t *testing.T) {
	e := setupHarnessEnv(t)
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(e.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("DELETE FROM run_usage")
	q := store.New(tx)
	// Deliberately unrelated to PostgreSQL now(): the query must use its parameter.
	cutoff := time.Date(2041, 2, 3, 4, 5, 6, 0, time.UTC)
	newRun := func() uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status)
   VALUES($1,$2,$3,1,'pricing fixture','fixture','completed')`, id, e.userID, e.repoID)
		return id
	}
	usage := func(id uuid.UUID, model, harness, session string, epoch int, at time.Time) {
		exec(`INSERT INTO run_usage(run_id,model,harness,session_id,lineage_epoch,updated_at)
   VALUES($1,$2,$3,$4,$5,$6)`, id, model, harness, session, epoch, at)
	}
	seed := func(model string, n int) {
		for i := 0; i < n; i++ {
			usage(newRun(), model, "codex", "s", 0, cutoff)
		}
	}
	query := func(priced []string) []store.ListRecentUnpricedCodexModelsRow {
		t.Helper()
		rows, err := q.ListRecentUnpricedCodexModels(e.ctx, store.ListRecentUnpricedCodexModelsParams{Cutoff: pgconv.Time(cutoff), Priced: priced})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	assert := func(got, want []store.ListRecentUnpricedCodexModelsRow) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rows=%+v want=%+v", got, want)
		}
	}
	seed("a-tie", 2)
	seed("b-tie", 2)
	id := newRun()
	usage(id, "top", "codex", "first", 0, cutoff)
	usage(id, "top", "codex", "second", 0, cutoff) // different session, same run
	usage(id, "top", "codex", "first", 1, cutoff)  // duplicate lineage leg
	seed("top", 2)
	usage(newRun(), "top", "claude", "s", 0, cutoff.Add(time.Hour))
	usage(newRun(), "claude-only", "claude", "s", 0, cutoff)
	usage(newRun(), "older", "codex", "s", 0, cutoff.Add(-time.Microsecond))
	usage(newRun(), "future", "codex", "s", 0, cutoff.Add(time.Hour))
	seed("supported", 9)
	assert(query([]string{"supported"}), []store.ListRecentUnpricedCodexModelsRow{
		{Model: "top", Runs: 3}, {Model: "a-tie", Runs: 2}, {Model: "b-tie", Runs: 2}, {Model: "future", Runs: 1},
	})
	assert(query([]string{}), []store.ListRecentUnpricedCodexModelsRow{
		{Model: "supported", Runs: 9}, {Model: "top", Runs: 3}, {Model: "a-tie", Runs: 2}, {Model: "b-tie", Runs: 2}, {Model: "future", Runs: 1},
	})
	// More than eleven supported models rank above every unsupported one.
	// A limit applied before the priced filter would hide all unsupported rows.
	priced := []string{"supported"}
	for i := 0; i < 12; i++ {
		model := fmt.Sprintf("supported-%02d", i)
		priced = append(priced, model)
		seed(model, 4)
	}
	assert(query(priced), []store.ListRecentUnpricedCodexModelsRow{
		{Model: "top", Runs: 3}, {Model: "a-tie", Runs: 2}, {Model: "b-tie", Runs: 2}, {Model: "future", Runs: 1},
	})
	for i := 0; i < 12; i++ {
		seed(fmt.Sprintf("overflow-%02d", i), 1)
	}
	assert(query(priced), []store.ListRecentUnpricedCodexModelsRow{
		{Model: "top", Runs: 3}, {Model: "a-tie", Runs: 2}, {Model: "b-tie", Runs: 2}, {Model: "future", Runs: 1},
		{Model: "overflow-00", Runs: 1}, {Model: "overflow-01", Runs: 1}, {Model: "overflow-02", Runs: 1},
		{Model: "overflow-03", Runs: 1}, {Model: "overflow-04", Runs: 1}, {Model: "overflow-05", Runs: 1}, {Model: "overflow-06", Runs: 1},
	})
	exec("DELETE FROM run_usage WHERE harness='codex'")
	assert(query([]string{}), []store.ListRecentUnpricedCodexModelsRow{})
}
