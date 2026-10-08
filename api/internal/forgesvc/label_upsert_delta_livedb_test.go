package forgesvc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func labelDeltaParams(row store.Issue, seed, add, remove string) store.UpsertIssueLabelsParams {
	return store.UpsertIssueLabelsParams{
		RepoID: row.RepoID, ForgeIssueIid: row.ForgeIssueIid, Title: "caller title",
		State: "closed", Labels: []byte(seed), AddLabels: []byte(add), RemoveLabels: []byte(remove),
		WebUrl: "https://forge.e2e/caller", Author: pgtype.Text{String: "caller author", Valid: true},
		HasPrdLink: true, ForgeUpdatedAt: pgtype.Timestamptz{Time: time.Date(2022, 2, 2, 0, 0, 0, 0, time.UTC), Valid: true},
	}
}

func TestUpsertIssueLabelsDeltasLiveDB(t *testing.T) {
	ctx, q, pool, stored := seedLabelDeltaLiveDB(t)
	cases := []struct {
		name, current, add, remove string
		want                       []string
	}{
		{"empty", "[]", "[]", "[]", []string{}},
		{"legacy null empty delta", "null", "[]", "[]", []string{}},
		{"legacy null add", "null", `["new"]`, "[]", []string{"new"}},
		{"empty delta preserves duplicates", `["b","a","b"]`, "[]", "[]", []string{"b", "a", "b"}},
		{"add only", `["b","b","a"]`, `["z","a","z","y","y"]`, "[]", []string{"b", "b", "a", "z", "y"}},
		{"remove only", `["b","a","b","c"]`, "[]", `["b"]`, []string{"a", "c"}},
		{"remove all", `["a","a"]`, "[]", `["a"]`, []string{}},
		{"overlap add wins", `["b","a","b","c"]`, `["z","b","z","a","b","y"]`, `["b"]`, []string{"a", "c", "z", "b", "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `UPDATE issues SET labels=$2::jsonb, synced_at='2021-01-01T00:00:00Z' WHERE id=$1`, stored.ID, tc.current); err != nil {
				t.Fatal(err)
			}
			p := labelDeltaParams(stored, `["stale-seed"]`, tc.add, tc.remove)
			got, err := q.UpsertIssueLabels(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range []store.Issue{got, readLabelDeltaIssue(t, ctx, q, stored)} {
				assertLabels(t, row.Labels, tc.want)
				assertLabelDeltaIdentity(t, "direct query", row, stored)
				if row.Title != p.Title || row.State != p.State || row.WebUrl != p.WebUrl || row.Author != p.Author || row.HasPrdLink != p.HasPrdLink ||
					row.ForgeUpdatedAt.Valid != p.ForgeUpdatedAt.Valid || row.ForgeUpdatedAt.InfinityModifier != p.ForgeUpdatedAt.InfinityModifier ||
					!row.ForgeUpdatedAt.Time.Equal(p.ForgeUpdatedAt.Time) {
					t.Fatalf("caller metadata lost: %+v", row)
				}
				if !row.SyncedAt.Valid || !row.SyncedAt.Time.After(time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)) {
					t.Fatalf("synced_at not refreshed: %v", row.SyncedAt)
				}
				var labels []string
				if err := json.Unmarshal(row.Labels, &labels); err != nil || labels == nil {
					t.Fatalf("labels must be array, not null: %s (%v)", row.Labels, err)
				}
			}
		})
	}
	t.Run("insert seed is distinct from deltas", func(t *testing.T) {
		p := labelDeltaParams(stored, `["seed","seed"]`, `["delta-only"]`, `["seed"]`)
		p.ForgeIssueIid = stored.ForgeIssueIid + 1
		got, err := q.UpsertIssueLabels(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		assertLabels(t, got.Labels, []string{"seed", "seed"})
		assertLabels(t, readLabelDeltaIssue(t, ctx, q, got).Labels, []string{"seed", "seed"})
		assertLabels(t, got.AssigneeIds, []string{}) // default JSON [] accepts either element type
		if got.BoardPosition.Valid {
			t.Fatalf("insert board position = %v", got.BoardPosition)
		}
	})
	t.Run("insert empty seed", func(t *testing.T) {
		p := labelDeltaParams(stored, "[]", `["delta-only"]`, "[]")
		p.ForgeIssueIid = stored.ForgeIssueIid + 2
		got, err := q.UpsertIssueLabels(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Labels) != "[]" {
			t.Fatalf("empty seed = %s", got.Labels)
		}
	})
}

func TestUpsertIssueLabelsWaitsForCurrentRowLiveDB(t *testing.T) {
	base, q, pool, stored := seedLabelDeltaLiveDB(t)
	ctx, cancel := context.WithTimeout(base, 15*time.Second)
	defer cancel()
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	holderPID := int64(holder.Conn().PgConn().PID())
	if _, err := tx.Exec(ctx, `UPDATE issues SET labels='["holder","holder","Planned"]'::jsonb WHERE id=$1`, stored.ID); err != nil {
		t.Fatal(err)
	}
	writer, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Release()
	writerPID := int64(writer.Conn().PgConn().PID())
	type result struct {
		row store.Issue
		err error
	}
	done := make(chan result, 1)
	writerCtx, stopWriter := context.WithCancel(ctx)
	joined := false
	defer func() {
		stopWriter()
		if !joined {
			<-done
		}
	}()
	go func() {
		row, err := store.New(writer).UpsertIssueLabels(writerCtx, labelDeltaParams(stored, `["stale"]`, `["In Progress"]`, `["Planned"]`))
		done <- result{row, err}
	}()
	probeCtx, stopProbe := context.WithTimeout(ctx, 5*time.Second)
	defer stopProbe()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	observed := false
	for !observed {
		err := pool.QueryRow(probeCtx, `SELECT EXISTS (
   SELECT 1 FROM pg_stat_activity AS activity
   WHERE activity.pid=$1 AND activity.wait_event_type='Lock'
    AND $2::int = ANY(pg_blocking_pids(activity.pid))
    AND EXISTS (SELECT 1 FROM pg_locks AS locks WHERE locks.pid=activity.pid AND NOT locks.granted)
  )`, writerPID, holderPID).Scan(&observed)
		if err != nil {
			t.Fatalf("observe writer %d blocked by holder %d: %v", writerPID, holderPID, err)
		}
		if observed {
			break
		}
		select {
		case r := <-done:
			joined = true
			t.Fatalf("writer completed before observed lock wait: %v", r.err)
		case <-probeCtx.Done():
			t.Fatalf("writer %d never observed blocked by holder %d: %v", writerPID, holderPID, probeCtx.Err())
		case <-ticker.C:
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-done:
		joined = true
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	assertLabels(t, r.row.Labels, []string{"holder", "holder", "In Progress"})
	assertLabels(t, readLabelDeltaIssue(t, ctx, q, stored).Labels, []string{"holder", "holder", "In Progress"})
}
