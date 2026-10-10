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

// PRD #2603: the Now summary stores its usage under "progress_note:<model>". The health query
// must name and count that row under the real model, so a priced model's note rows are not
// reported as an unpriced model and an unpriced model's note rows join its plain rows.
func TestRecentUnpricedCodexModelsProgressNoteLiveDB(t *testing.T) {
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
	cutoff := time.Date(2041, 2, 3, 4, 5, 6, 0, time.UTC)
	use := func(model string) {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status)
   VALUES($1,$2,$3,1,'pricing fixture','fixture','completed')`, id, e.userID, e.repoID)
		exec(`INSERT INTO run_usage(run_id,model,harness,session_id,lineage_epoch,updated_at)
   VALUES($1,$2,'codex','s',0,$3)`, id, model, cutoff)
	}
	query := func(priced []string) []store.ListRecentUnpricedCodexModelsRow {
		t.Helper()
		rows, err := q.ListRecentUnpricedCodexModels(e.ctx, store.ListRecentUnpricedCodexModelsParams{Cutoff: pgconv.Time(cutoff), Priced: priced})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	want := func(got, w []store.ListRecentUnpricedCodexModelsRow) {
		t.Helper()
		if !reflect.DeepEqual(got, w) {
			t.Fatalf("rows=%+v want=%+v", got, w)
		}
	}

	// A priced model's note row is not reported (it would otherwise appear as the unpriced
	// "progress_note:gpt-6-luna").
	use("progress_note:gpt-6-luna")
	use("gpt-6-luna")
	want(query([]string{"gpt-6-luna"}), []store.ListRecentUnpricedCodexModelsRow{})

	// An unpriced model's note row and plain row, in different runs, group into one entry; a
	// look-alike that only differs at the "_" (LIKE's single-character wildcard) is untouched.
	use("progress_note:odd-model")
	use("odd-model")
	use("progressXnote:odd-model")
	want(query([]string{"gpt-6-luna"}), []store.ListRecentUnpricedCodexModelsRow{
		{Model: "odd-model", Runs: 2},
		{Model: "progressXnote:odd-model", Runs: 1},
	})

	// The LIMIT applies after the normalisation: twelve distinct models, the first also seen as a
	// note row, give eleven entries, with the merged one on top and the alphabetically last cut.
	exec("DELETE FROM run_usage")
	for i := 0; i < 12; i++ {
		use(fmt.Sprintf("m-%02d", i))
	}
	use("progress_note:m-00")
	got := query([]string{})
	if len(got) != 11 {
		t.Fatalf("len=%d want 11: %+v", len(got), got)
	}
	if got[0] != (store.ListRecentUnpricedCodexModelsRow{Model: "m-00", Runs: 2}) {
		t.Fatalf("first=%+v", got[0])
	}
	for _, r := range got {
		if r.Model == "m-11" || r.Model == "progress_note:m-00" {
			t.Fatalf("unexpected row %+v", r)
		}
	}
}
