package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage of the namespaced mr_rework ledger state (ADR-1199): the per-namespace
// marks, the source-qualified pending keys, the legacy bootstrap and the binary-rollback
// behaviour. Skipped unless UZI_TEST_DATABASE_URL is set (reviewAuthorEnv).

func marksOf(t *testing.T, raw []byte) map[string]int64 {
	t.Helper()
	if raw == nil {
		return nil
	}
	m := map[string]int64{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("comment_marks %q: %v", raw, err)
	}
	return m
}

func TestMRReworkMergePendingKeysLiveDB(t *testing.T) {
	ctx, pool, _, _ := reviewAuthorEnv(t)
	merge := func(existing, added, removed, sup, supBy []string, marks string) []string {
		t.Helper()
		var got []string
		err := pool.QueryRow(ctx,
			`SELECT mr_rework_merge_pending_keys($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::jsonb)`,
			existing, added, removed, sup, supBy, marks).Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	eq := func(t *testing.T, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	t.Run("an equal id in two namespaces is two keys", func(t *testing.T) {
		got := merge(nil, []string{"review_comment:3", "issue_comment:3"}, nil, nil, nil, `{}`)
		eq(t, got, []string{"issue_comment:3", "review_comment:3"})
	})

	t.Run("the add filter is per namespace", func(t *testing.T) {
		got := merge(nil, []string{"review_comment:3", "issue_comment:3", "review_comment:5"}, nil, nil, nil, `{"review_comment":4}`)
		eq(t, got, []string{"issue_comment:3", "review_comment:5"})
	})

	t.Run("a NULL marks object filters nothing above zero", func(t *testing.T) {
		var got []string
		if err := pool.QueryRow(ctx, `SELECT mr_rework_merge_pending_keys(NULL, '{review_comment:1}', NULL, NULL, NULL, NULL)`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		eq(t, got, []string{"review_comment:1"})
	})

	t.Run("new keys are ordered numerically, not lexically", func(t *testing.T) {
		got := merge(nil, []string{"review_comment:10", "review_comment:9"}, nil, nil, nil, `{}`)
		eq(t, got, []string{"review_comment:9", "review_comment:10"})
	})

	t.Run("tied positions fall back to numeric id order, not lexical", func(t *testing.T) {
		// ns:5 is replaced by the kept ns:10 and ns:9, which both inherit its position.
		got := merge([]string{"ns:5"}, []string{"ns:10", "ns:9"}, nil,
			[]string{"ns:5", "ns:5"}, []string{"ns:10", "ns:9"}, `{"ns":1}`)
		eq(t, got, []string{"ns:9", "ns:10"})
	})

	t.Run("tied positions are ordered numerically", func(t *testing.T) {
		got := merge([]string{"ns:20", "ns:3"}, []string{"ns:100"}, nil,
			[]string{"ns:3", "ns:20"}, []string{"ns:20", "ns:100"}, `{"ns":1}`)
		eq(t, got, []string{"ns:20", "ns:100"})
	})

	t.Run("stored keys keep their position and precede new keys", func(t *testing.T) {
		got := merge([]string{"review_comment:50", "issue_comment:7"}, []string{"issue_comment:1", "review_comment:50"}, nil, nil, nil, `{}`)
		eq(t, got, []string{"review_comment:50", "issue_comment:7", "issue_comment:1"})
	})

	t.Run("the cap keeps the oldest-admitted, not the smallest ids", func(t *testing.T) {
		var existing []string
		for i := 20001; i <= 30000; i++ {
			existing = append(existing, fmt.Sprintf("review_comment:%d", i))
		}
		got := merge(existing, []string{"issue_comment:1", "issue_comment:2"}, nil, nil, nil, `{}`)
		eq(t, got, existing)
		if slices.Contains(got, "issue_comment:1") {
			t.Fatal("a later flood displaced an earlier finding")
		}
	})

	t.Run("a surviving replacement supersedes the older key", func(t *testing.T) {
		got := merge(nil, []string{"review_comment:100", "review_comment:170"}, nil,
			[]string{"review_comment:100"}, []string{"review_comment:170"}, `{}`)
		eq(t, got, []string{"review_comment:170"})
	})

	t.Run("a replacement rejected by the add filter keeps the older key", func(t *testing.T) {
		got := merge([]string{"review_comment:100"}, []string{"review_comment:140"}, nil,
			[]string{"review_comment:100"}, []string{"review_comment:140"}, `{"review_comment":150}`)
		eq(t, got, []string{"review_comment:100"})
	})

	t.Run("a removed replacement keeps the older key", func(t *testing.T) {
		got := merge([]string{"review_comment:100"}, []string{"review_comment:170"}, []string{"review_comment:170"},
			[]string{"review_comment:100"}, []string{"review_comment:170"}, `{}`)
		eq(t, got, []string{"review_comment:100"})
	})

	t.Run("a replacement takes the older key's slot", func(t *testing.T) {
		got := merge([]string{"ns_a:5", "ns_b:1", "ns_c:9"}, []string{"ns_d:2"}, nil,
			[]string{"ns_b:1"}, []string{"ns_d:2"}, `{}`)
		eq(t, got, []string{"ns_a:5", "ns_d:2", "ns_c:9"})
	})

	t.Run("at the cap a replacement keeps its slot and the newer add is cut", func(t *testing.T) {
		var existing []string
		for i := 1; i <= 10000; i++ {
			existing = append(existing, fmt.Sprintf("review_comment:%d", i))
		}
		got := merge(existing, []string{"issue_comment:5", "review_comment:15000"}, nil,
			[]string{"review_comment:10000"}, []string{"review_comment:15000"}, `{}`)
		if len(got) != 10000 || got[9999] != "review_comment:15000" || slices.Contains(got, "issue_comment:5") || slices.Contains(got, "review_comment:10000") {
			t.Fatalf("len=%d last=%q: want 10000 keys, the replacement in the old slot, the newer add cut", len(got), got[len(got)-1])
		}
	})
}

func TestMRReworkMergeMarksLiveDB(t *testing.T) {
	ctx, pool, _, _ := reviewAuthorEnv(t)
	merge := func(a, b *string) map[string]int64 {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT mr_rework_merge_marks($1::jsonb, $2::jsonb)`, a, b).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return marksOf(t, raw)
	}
	s := func(v string) *string { return &v }
	for name, tc := range map[string]struct {
		a, b *string
		want map[string]int64
	}{
		"empty and empty":     {s(`{}`), s(`{}`), map[string]int64{}},
		"NULL and NULL":       {nil, nil, map[string]int64{}},
		"NULL and a value":    {nil, s(`{"a":3}`), map[string]int64{"a": 3}},
		"per-key max + union": {s(`{"a":5,"b":9}`), s(`{"a":7,"c":1}`), map[string]int64{"a": 7, "b": 9, "c": 1}},
		"never backward":      {s(`{"a":9}`), s(`{"a":2}`), map[string]int64{"a": 9}},
		"JSON null":           {s(`null`), s(`null`), map[string]int64{}},
		"JSON null + value":   {s(`null`), s(`{"a":3}`), map[string]int64{"a": 3}},
		"array":               {s(`[]`), s(`{"a":3}`), map[string]int64{"a": 3}},
		"array incoming":      {s(`{"a":3}`), s(`[1,2]`), map[string]int64{"a": 3}},
		"scalar":              {s(`5`), s(`5`), map[string]int64{}},
	} {
		if got := merge(tc.a, tc.b); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

// legacyRow inserts a ledger row exactly as a binary that predates comment_marks would.
func legacyRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo uuid.UUID, ref string, hw int64, pending []int64) {
	t.Helper()
	if pending == nil {
		pending = []int64{}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO mr_rework_ledger (repo_id, ref, attempt_count, high_water, pending_unknown_ids) VALUES ($1, $2, 1, $3, $4)`,
		repo, ref, hw, pending); err != nil {
		t.Fatal(err)
	}
}

func TestMRReworkLedgerBootstrapLiveDB(t *testing.T) {
	ctx, pool, q, repo := reviewAuthorEnv(t)
	get := func(ref string) store.GetMRReworkLedgerRow {
		t.Helper()
		led, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repo, Ref: ref})
		if err != nil {
			t.Fatal(err)
		}
		return led
	}
	boot := func(ref string, hw int64, pending []int64) int64 {
		t.Helper()
		n, err := q.BootstrapMRReworkLedgerMarks(ctx, store.BootstrapMRReworkLedgerMarksParams{
			RepoID: repo, Ref: ref, Marks: []byte(`{"review_comment":50}`), PendingKeys: rkeys(7, 9),
			LegacyHighWater: hw, LegacyPending: pending,
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("applies once", func(t *testing.T) {
		ref := "agent/issue-boot-1"
		legacyRow(ctx, t, pool, repo, ref, 50, []int64{7, 9})
		if led := get(ref); !led.Legacy || led.CommentMarks != nil {
			t.Fatalf("a row written without marks must read legacy: %+v", led)
		}
		if n := boot(ref, 50, []int64{7, 9}); n != 1 {
			t.Fatalf("first bootstrap rows = %d, want 1", n)
		}
		led := get(ref)
		if led.Legacy || !reflect.DeepEqual(marksOf(t, led.CommentMarks), map[string]int64{"review_comment": 50}) || !slices.Equal(led.PendingUnknownKeys, rkeys(7, 9)) {
			t.Fatalf("after bootstrap: %+v", led)
		}
		if n := boot(ref, 50, []int64{7, 9}); n != 0 {
			t.Fatalf("second bootstrap rows = %d, want 0", n)
		}
	})

	t.Run("a changed legacy scalar blocks it", func(t *testing.T) {
		ref := "agent/issue-boot-2"
		legacyRow(ctx, t, pool, repo, ref, 60, []int64{7})
		if n := boot(ref, 50, []int64{7}); n != 0 {
			t.Fatalf("stale high_water matched %d rows", n)
		}
		if n := boot(ref, 60, []int64{7, 9}); n != 0 {
			t.Fatalf("stale pending matched %d rows", n)
		}
		if !get(ref).Legacy {
			t.Fatal("a refused bootstrap must leave the row legacy")
		}
		if n := boot(ref, 60, []int64{7}); n != 1 {
			t.Fatalf("matching bootstrap rows = %d, want 1", n)
		}
	})

	t.Run("a nil legacy_pending matches an empty set", func(t *testing.T) {
		ref := "agent/issue-boot-3"
		legacyRow(ctx, t, pool, repo, ref, 5, nil)
		if n := boot(ref, 5, nil); n != 1 {
			t.Fatalf("nil legacy_pending rows = %d, want 1", n)
		}
	})

	t.Run("an upsert on a legacy row writes nothing and returns 0 rows", func(t *testing.T) {
		ref := "agent/issue-boot-4"
		legacyRow(ctx, t, pool, repo, ref, 5, []int64{3})
		if _, err := pool.Exec(ctx, `UPDATE mr_rework_ledger SET pending_unknown_keys = '{review_comment:1}' WHERE repo_id = $1 AND ref = $2`, repo, ref); err != nil {
			t.Fatal(err)
		}
		before := get(ref)
		n, err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{
			RepoID: repo, Ref: ref, HighWater: 40, Marks: rmarks(40), PendingAdd: rkeys(30), PendingRemove: rkeys(1),
		})
		if err != nil || n != 0 {
			t.Fatalf("legacy upsert rows=%d err=%v, want 0", n, err)
		}
		after := get(ref)
		if !after.Legacy || after.CommentMarks != nil || after.AttemptCount != before.AttemptCount || after.HighWater != before.HighWater ||
			!slices.Equal(after.PendingUnknownKeys, before.PendingUnknownKeys) || !slices.Equal(after.PendingUnknownIds, before.PendingUnknownIds) ||
			!after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) || after.HaltNotified != before.HaltNotified {
			t.Fatalf("a refused legacy upsert changed the row:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("an upsert inserting a new row returns 1", func(t *testing.T) {
		n, err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{RepoID: repo, Ref: "agent/issue-boot-4b", HighWater: 4, Marks: rmarks(4)})
		if err != nil || n != 1 {
			t.Fatalf("insert rows=%d err=%v, want 1", n, err)
		}
		n, err = q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{RepoID: repo, Ref: "agent/issue-boot-4b", HighWater: 5, Marks: rmarks(5)})
		if err != nil || n != 1 {
			t.Fatalf("non-legacy update rows=%d err=%v, want 1", n, err)
		}
	})

	t.Run("bootstrap normalizes JSON null marks to an empty object", func(t *testing.T) {
		for i, raw := range [][]byte{[]byte("null"), nil, []byte("[]")} {
			ref := fmt.Sprintf("agent/issue-boot-nullmarks-%d", i)
			legacyRow(ctx, t, pool, repo, ref, 1, nil)
			n, err := q.BootstrapMRReworkLedgerMarks(ctx, store.BootstrapMRReworkLedgerMarksParams{RepoID: repo, Ref: ref, Marks: raw, LegacyHighWater: 1})
			if err != nil || n != 1 {
				t.Fatalf("marks %q: rows=%d err=%v", raw, n, err)
			}
			if led := get(ref); led.Legacy || !reflect.DeepEqual(marksOf(t, led.CommentMarks), map[string]int64{}) {
				t.Fatalf("marks %q stored %s legacy=%v, want {}", raw, led.CommentMarks, led.Legacy)
			}
		}
	})

	t.Run("bootstrap dedups keys keeping the first position", func(t *testing.T) {
		ref := "agent/issue-boot-dups"
		legacyRow(ctx, t, pool, repo, ref, 1, nil)
		keys := []string{"review_comment:9", "issue_comment:2", "review_comment:9", "review_comment:3", "issue_comment:2"}
		n, err := q.BootstrapMRReworkLedgerMarks(ctx, store.BootstrapMRReworkLedgerMarksParams{RepoID: repo, Ref: ref, Marks: []byte(`{}`), PendingKeys: keys, LegacyHighWater: 1})
		if err != nil || n != 1 {
			t.Fatalf("rows=%d err=%v", n, err)
		}
		if got, want := get(ref).PendingUnknownKeys, []string{"review_comment:9", "issue_comment:2", "review_comment:3"}; !slices.Equal(got, want) {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	})

	t.Run("bootstrap refuses a non-integer mark loudly and leaves the row legacy", func(t *testing.T) {
		for i, bad := range []string{`{"rc":1.5}`, `{"rc":"x"}`, `{"rc":true}`, `{"rc":99999999999999999999}`} {
			ref := fmt.Sprintf("agent/issue-boot-badmark-%d", i)
			legacyRow(ctx, t, pool, repo, ref, 1, nil)
			n, err := q.BootstrapMRReworkLedgerMarks(ctx, store.BootstrapMRReworkLedgerMarksParams{RepoID: repo, Ref: ref, Marks: []byte(bad), LegacyHighWater: 1})
			if err == nil || n != 0 {
				t.Fatalf("marks %s: rows=%d err=%v, want an error", bad, n, err)
			}
			if c := pgCode(err); c != "22P02" && c != "22003" && c != "23514" {
				t.Fatalf("marks %s: code %q (%v), want a cast (22P02/22003) or check (23514) error", bad, c, err)
			}
			if !get(ref).Legacy {
				t.Fatalf("marks %s: a failed bootstrap must leave the row legacy", bad)
			}
		}
	})

	t.Run("a halt latch on a new ref creates a non-legacy row", func(t *testing.T) {
		ref := "agent/issue-boot-5"
		if err := q.SetMRReworkHaltNotified(ctx, store.SetMRReworkHaltNotifiedParams{RepoID: repo, Ref: ref}); err != nil {
			t.Fatal(err)
		}
		led := get(ref)
		if led.Legacy || !led.HaltNotified || !reflect.DeepEqual(marksOf(t, led.CommentMarks), map[string]int64{}) {
			t.Fatalf("halt-created row: %+v", led)
		}
	})

	t.Run("malformed keys and marks are rejected", func(t *testing.T) {
		ref := "agent/issue-boot-6"
		for _, bad := range []string{"review:007", "Review:1", "review_comment:", "review_comment:-1", "nocolon"} {
			_, err := pool.Exec(ctx, `INSERT INTO mr_rework_ledger (repo_id, ref, pending_unknown_keys) VALUES ($1, $2, ARRAY[$3]::text[])`, repo, ref, bad)
			if pgCode(err) != "23514" {
				t.Fatalf("key %q: err = %v, want 23514", bad, err)
			}
		}
		_, err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{RepoID: repo, Ref: ref, HighWater: 1, PendingAdd: []string{"Review:1"}})
		if pgCode(err) != "23514" {
			t.Fatalf("upsert of a malformed key: err = %v, want 23514", err)
		}
		for _, bad := range []string{"review_comment:" + "12345678901234567890", "review_comment:0123"} {
			_, err = pool.Exec(ctx, `INSERT INTO mr_rework_ledger (repo_id, ref, pending_unknown_keys) VALUES ($1, $2, ARRAY[$3]::text[])`, repo, ref, bad)
			if pgCode(err) != "23514" {
				t.Fatalf("key %q: err = %v, want 23514", bad, err)
			}
		}
		_, err = pool.Exec(ctx, `INSERT INTO mr_rework_ledger (repo_id, ref, pending_unknown_keys) VALUES ($1, $2, ARRAY['review_comment:1', NULL]::text[])`, repo, ref)
		if pgCode(err) != "23514" {
			t.Fatalf("NULL key element: err = %v, want 23514", err)
		}
		for _, bad := range []string{`[]`, `null`, `5`, `{"rc":1.5}`, `{"rc":"x"}`, `{"rc":-1}`, `{"rc":null}`, `{"rc":true}`, `{"rc":99999999999999999999}`, `{"rc":9223372036854775808}`} {
			_, err = pool.Exec(ctx, `INSERT INTO mr_rework_ledger (repo_id, ref, comment_marks) VALUES ($1, $2, $3::jsonb)`, repo, ref, bad)
			if pgCode(err) != "23514" {
				t.Fatalf("comment_marks %s: err = %v, want 23514", bad, err)
			}
		}
		for _, good := range []string{`{}`, `{"rc":0}`, `{"rc":9223372036854775807}`} {
			if _, err = pool.Exec(ctx, `INSERT INTO mr_rework_ledger (repo_id, ref, comment_marks) VALUES ($1, $2, $3::jsonb) ON CONFLICT (repo_id, ref) DO UPDATE SET comment_marks = EXCLUDED.comment_marks`, repo, ref, good); err != nil {
				t.Fatalf("comment_marks %s: %v", good, err)
			}
		}
	})
}

// A binary rollback without running the migration Down: the old binary updates only the legacy
// columns. The bootstrapped state must survive, and the row must not bootstrap a second time.
func TestMRReworkLedgerBinaryRollbackKeepsNamespacedStateLiveDB(t *testing.T) {
	ctx, pool, q, repo := reviewAuthorEnv(t)
	ref := "agent/issue-rollback"
	legacyRow(ctx, t, pool, repo, ref, 50, []int64{7, 9})
	n, err := q.BootstrapMRReworkLedgerMarks(ctx, store.BootstrapMRReworkLedgerMarksParams{
		RepoID: repo, Ref: ref, Marks: []byte(`{"review_comment":50,"issue_comment":12}`), PendingKeys: rkeys(7, 9),
		LegacyHighWater: 50, LegacyPending: []int64{7, 9},
	})
	if err != nil || n != 1 {
		t.Fatalf("bootstrap rows=%d err=%v", n, err)
	}

	// OLD-code writes: advance ONLY the legacy columns.
	if _, err := pool.Exec(ctx, `UPDATE mr_rework_ledger SET high_water = high_water + 100, pending_unknown_ids = ARRAY[150, 151]::bigint[] WHERE repo_id = $1 AND ref = $2`, repo, ref); err != nil {
		t.Fatal(err)
	}

	led, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repo, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if led.Legacy {
		t.Fatal("a bootstrapped row must stay non-legacy after legacy-column writes")
	}
	if !reflect.DeepEqual(marksOf(t, led.CommentMarks), map[string]int64{"review_comment": 50, "issue_comment": 12}) || !slices.Equal(led.PendingUnknownKeys, rkeys(7, 9)) {
		t.Fatalf("namespaced state was lost or rewritten: marks=%s keys=%v", led.CommentMarks, led.PendingUnknownKeys)
	}
	if led.HighWater != 150 || !slices.Equal(led.PendingUnknownIds, []int64{150, 151}) {
		t.Fatalf("the simulated legacy writes did not land: %+v", led)
	}
	n, err = q.BootstrapMRReworkLedgerMarks(ctx, store.BootstrapMRReworkLedgerMarksParams{
		RepoID: repo, Ref: ref, Marks: []byte(`{"review_comment":150}`), PendingKeys: rkeys(150, 151),
		LegacyHighWater: 150, LegacyPending: []int64{150, 151},
	})
	if err != nil || n != 0 {
		t.Fatalf("re-bootstrap rows=%d err=%v, want 0", n, err)
	}
}

func TestMRReworkLedgerNonLegacyAdvanceLiveDB(t *testing.T) {
	ctx, pool, q, repo := reviewAuthorEnv(t)
	var user uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT user_id FROM forge_connections WHERE id = (SELECT connection_id FROM repos WHERE id = $1)`, repo).Scan(&user); err != nil {
		t.Fatal(err)
	}
	get := func(ref string) store.GetMRReworkLedgerRow {
		t.Helper()
		led, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repo, Ref: ref})
		if err != nil {
			t.Fatal(err)
		}
		return led
	}
	wantMarks := map[string]int64{"review_comment": 10, "issue_comment": 8}
	// Second call: the review_comment mark is lower than stored (GREATEST keeps 10); 7 is at or
	// below the PRE-update review_comment mark and is dropped, 11 is above and kept, and
	// issue_comment:3 is kept because that namespace had no mark BEFORE this update even though
	// the update itself moves it to 8.
	incoming := []byte(`{"review_comment":5,"issue_comment":8}`)
	add := []string{"review_comment:7", "review_comment:11", "issue_comment:3"}

	t.Run("upsert", func(t *testing.T) {
		ref := "agent/issue-adv-1"
		if _, err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{RepoID: repo, Ref: ref, HighWater: 10, Marks: []byte(`{"review_comment":10}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{RepoID: repo, Ref: ref, HighWater: 8, Marks: incoming, PendingAdd: add}); err != nil {
			t.Fatal(err)
		}
		led := get(ref)
		if led.Legacy || !reflect.DeepEqual(marksOf(t, led.CommentMarks), wantMarks) || !slices.Equal(led.PendingUnknownKeys, []string{"issue_comment:3", "review_comment:11"}) || led.HighWater != 10 {
			t.Fatalf("upsert: marks=%s keys=%v hw=%d", led.CommentMarks, led.PendingUnknownKeys, led.HighWater)
		}
	})

	t.Run("atomic manual create", func(t *testing.T) {
		ref := "agent/issue-adv-2"
		target := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status) VALUES ($1, $2, $3, 'issue', 903, 't', 'd', 'completed')`, target, user, repo); err != nil {
			t.Fatal(err)
		}
		create := func(hw int64, marks []byte, add []string) {
			t.Helper()
			run, err := q.CreateManualMRReworkRunAndAdvance(ctx, store.CreateManualMRReworkRunAndAdvanceParams{
				Harness: "claude", UserID: user, RepoID: repo, IssueTitle: "t", IssueDescription: "d",
				PipelineRef: pgtype.Text{String: ref, Valid: true}, MrIid: pgtype.Int8{Int64: 68, Valid: true},
				TargetRunID: pgtype.UUID{Bytes: target, Valid: true}, ReviewComments: []byte(`{}`),
				HighWater: hw, Marks: marks, PendingAdd: add,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE runs SET status = 'completed' WHERE id = $1`, run.ID); err != nil {
				t.Fatal(err)
			}
		}
		create(10, []byte(`{"review_comment":10}`), nil)
		create(8, incoming, add)
		led := get(ref)
		if led.Legacy || !reflect.DeepEqual(marksOf(t, led.CommentMarks), wantMarks) || !slices.Equal(led.PendingUnknownKeys, []string{"issue_comment:3", "review_comment:11"}) {
			t.Fatalf("manual create: marks=%s keys=%v", led.CommentMarks, led.PendingUnknownKeys)
		}
	})

	t.Run("removal preserves stored order and skips legacy rows", func(t *testing.T) {
		ref := "agent/issue-adv-3"
		if _, err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{RepoID: repo, Ref: ref, HighWater: 1, Marks: []byte(`{}`), PendingAdd: []string{"ns_b:9", "ns_a:3"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{RepoID: repo, Ref: ref, HighWater: 1, Marks: []byte(`{}`), PendingAdd: []string{"ns_c:1", "ns_a:2"}}); err != nil {
			t.Fatal(err)
		}
		if got := get(ref).PendingUnknownKeys; !slices.Equal(got, []string{"ns_a:3", "ns_b:9", "ns_c:1", "ns_a:2"}) {
			t.Fatalf("admission order = %v", got)
		}
		if err := q.RemoveMRReworkPendingKeys(ctx, store.RemoveMRReworkPendingKeysParams{RepoID: repo, Ref: ref, Keys: []string{"ns_b:9"}}); err != nil {
			t.Fatal(err)
		}
		if got := get(ref).PendingUnknownKeys; !slices.Equal(got, []string{"ns_a:3", "ns_c:1", "ns_a:2"}) {
			t.Fatalf("after removal = %v, want the stored order kept", got)
		}
		legacy := "agent/issue-adv-4"
		legacyRow(ctx, t, pool, repo, legacy, 1, nil)
		if _, err := pool.Exec(ctx, `UPDATE mr_rework_ledger SET pending_unknown_keys = '{ns_a:1}' WHERE repo_id = $1 AND ref = $2`, repo, legacy); err != nil {
			t.Fatal(err)
		}
		if err := q.RemoveMRReworkPendingKeys(ctx, store.RemoveMRReworkPendingKeysParams{RepoID: repo, Ref: legacy, Keys: []string{"ns_a:1"}}); err != nil {
			t.Fatal(err)
		}
		if got := get(legacy).PendingUnknownKeys; !slices.Equal(got, []string{"ns_a:1"}) {
			t.Fatalf("a legacy row's keys were changed: %v", got)
		}
	})
}

// The manual create's led CTE keeps CASE arms for a legacy row (the service guards legacy under
// the branch lock); this executes that arm: the legacy row's marks and keys must stay untouched.
func TestMRReworkManualCreateOnLegacyRowLiveDB(t *testing.T) {
	ctx, pool, q, repo := reviewAuthorEnv(t)
	var user uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT user_id FROM forge_connections WHERE id = (SELECT connection_id FROM repos WHERE id = $1)`, repo).Scan(&user); err != nil {
		t.Fatal(err)
	}
	ref := "agent/issue-legacy-manual"
	legacyRow(ctx, t, pool, repo, ref, 5, []int64{3})
	if _, err := pool.Exec(ctx, `UPDATE mr_rework_ledger SET pending_unknown_keys = '{review_comment:1}' WHERE repo_id = $1 AND ref = $2`, repo, ref); err != nil {
		t.Fatal(err)
	}
	target := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status) VALUES ($1, $2, $3, 'issue', 904, 't', 'd', 'completed')`, target, user, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateManualMRReworkRunAndAdvance(ctx, store.CreateManualMRReworkRunAndAdvanceParams{
		Harness: "claude", UserID: user, RepoID: repo, IssueTitle: "t", IssueDescription: "d",
		PipelineRef: pgtype.Text{String: ref, Valid: true}, MrIid: pgtype.Int8{Int64: 69, Valid: true},
		TargetRunID: pgtype.UUID{Bytes: target, Valid: true}, ReviewComments: []byte(`{}`),
		HighWater: 40, Marks: rmarks(40), PendingAdd: rkeys(30), PendingRemove: rkeys(1),
	}); err != nil {
		t.Fatal(err)
	}
	led, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repo, Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if !led.Legacy || led.CommentMarks != nil || !slices.Equal(led.PendingUnknownKeys, rkeys(1)) || !slices.Equal(led.PendingUnknownIds, []int64{3}) || led.HighWater != 40 {
		t.Fatalf("manual create on a legacy row must advance only high_water: %+v", led)
	}
}
