package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4731: live-database proofs of the current provider gauge statements and of
// AnalyticsSnapshot.Savepoint. Skipped without UZI_TEST_DATABASE_URL.

func TestAnalyticsCapacityAnthropicGaugesLiveDB(t *testing.T) {
	ctx, pool, q, user := rateLimitEnablementDB(t)
	other := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, other, fmt.Sprintf("capag-%s@e2e", other))
	def := enablementToken(ctx, t, q, user, "Default", true)
	spare := enablementToken(ctx, t, q, user, "spare", false)
	gone := enablementToken(ctx, t, q, user, "gone", false)
	foreign := enablementToken(ctx, t, q, other, "foreign", true)
	for _, c := range []struct {
		owner, id uuid.UUID
		pct       int16
	}{{user, def, 10}, {user, spare, 20}, {user, gone, 30}, {other, foreign, 40}} {
		if n := fencedUpsert(ctx, t, q, c.owner, c.id, 0, c.pct); n != 1 {
			t.Fatalf("seed reading = %d rows", n)
		}
	}
	setEnabled(ctx, t, q, user, gone, false)

	ids := func(rows []store.AnalyticsOwnerAnthropicGaugesRow) []uuid.UUID {
		out := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			out[i] = r.UserSecretID
		}
		return out
	}
	rows, err := q.AnalyticsOwnerAnthropicGauges(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	// Default first, then by label; the disabled token and the other owner's are absent.
	if got := ids(rows); len(got) != 2 || got[0] != def || got[1] != spare {
		t.Fatalf("owner gauge rows = %v, want [default %s, spare %s]", got, def, spare)
	}
	if !rows[0].IsDefault || rows[0].Label != "Default" || rows[0].FiveHourPct.Int16 != 10 || !rows[0].SyncedAt.Valid {
		t.Errorf("default row %+v", rows[0])
	}
	// Parity with the canonical owner read it was copied from.
	canon, err := q.ListRateLimitsForUser(ctx, user)
	if err != nil || len(canon) != len(rows) {
		t.Fatalf("canonical rows = %d (%v)", len(canon), err)
	}
	for i := range rows {
		if canon[i].UserSecretID != rows[i].UserSecretID || canon[i].FiveHourPct != rows[i].FiveHourPct ||
			canon[i].SyncedAt != rows[i].SyncedAt || canon[i].Source != rows[i].Source {
			t.Errorf("row %d differs from ListRateLimitsForUser: %+v vs %+v", i, rows[i], canon[i])
		}
	}

	// A stale reading is returned as stored: staleness is the handler's judgement.
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	mustExec(ctx, t, pool, `UPDATE anthropic_rate_limits SET synced_at = $2 WHERE user_secret_id = $1`, def, old)
	if rows, err = q.AnalyticsOwnerAnthropicGauges(ctx, user); err != nil || !rows[0].SyncedAt.Time.Equal(old) {
		t.Fatalf("stale reading not passed through: %+v (%v)", rows, err)
	}

	// Disable then re-enable: the older-revision reading is not joined until a fresh one lands.
	setEnabled(ctx, t, q, user, spare, false)
	if rows, err = q.AnalyticsOwnerAnthropicGauges(ctx, user); err != nil || len(rows) != 1 {
		t.Fatalf("disabled spare still listed: %d rows (%v)", len(rows), err)
	}
	rev := setEnabled(ctx, t, q, user, spare, true)
	if rows, err = q.AnalyticsOwnerAnthropicGauges(ctx, user); err != nil || len(rows) != 2 || rows[1].UserSecretID != spare || rows[1].SyncedAt.Valid {
		t.Fatalf("re-enabled spare = %+v (%v), want listed with no current reading", rows, err)
	}
	fencedUpsert(ctx, t, q, user, spare, rev, 55)
	if rows, err = q.AnalyticsOwnerAnthropicGauges(ctx, user); err != nil || !rows[1].SyncedAt.Valid || rows[1].FiveHourPct.Int16 != 55 {
		t.Fatalf("fresh reading at the new revision not visible: %+v (%v)", rows, err)
	}
}

func TestAnalyticsCapacityAnthropicGaugesCapLiveDB(t *testing.T) {
	ctx, pool, q, user := rateLimitEnablementDB(t)
	var want []string
	for i := 0; i < 230; i++ {
		label := fmt.Sprintf("t%03d", i)
		if _, err := insertSecret(ctx, pool, user, store.KindAnthropicToken, label, false); err != nil {
			t.Fatal(err)
		}
		if i <= 200 {
			want = append(want, label)
		}
	}
	rows, err := q.AnalyticsOwnerAnthropicGauges(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 201 {
		t.Fatalf("%d rows for 230 identities, want exactly 201", len(rows))
	}
	for i, r := range rows {
		if r.Label != want[i] {
			t.Fatalf("row %d is %q, want %q (the ordered prefix by label then id)", i, r.Label, want[i])
		}
	}
}

// seedCapUser inserts a user with a fixed email, so all-scope ordering is under the test's control.
func seedCapUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, id, email)
	return id
}

func TestAnalyticsCapacityAllAnthropicGaugesLiveDB(t *testing.T) {
	ctx, pool := capPrivateDB(t)
	q := store.New(pool)
	a := seedCapUser(ctx, t, pool, "cap-a@e2e")
	b := seedCapUser(ctx, t, pool, "cap-b@e2e")
	c := seedCapUser(ctx, t, pool, "cap-c@e2e")

	// Ordering and the reading join: b's default sorts before b's spare, users sort by email.
	bDef := enablementToken(ctx, t, q, b, "Default", true)
	bSpare := enablementToken(ctx, t, q, b, "spare", false)
	aTok := enablementToken(ctx, t, q, a, "only", true)
	fencedUpsert(ctx, t, q, a, aTok, 0, 11)
	fencedUpsert(ctx, t, q, b, bSpare, 0, 22)
	setEnabled(ctx, t, q, b, bSpare, false)
	rev := setEnabled(ctx, t, q, b, bSpare, true) // its reading is now an older revision
	rows, err := q.AnalyticsAllAnthropicGauges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].UserSecretID != aTok || rows[1].UserSecretID != bDef || rows[2].UserSecretID != bSpare {
		t.Fatalf("all-users rows = %+v", rows)
	}
	if rows[0].Email != "cap-a@e2e" || rows[0].UserID != a || rows[0].VaultMode == "" || rows[0].FiveHourPct.Int16 != 11 {
		t.Errorf("first row %+v", rows[0])
	}
	if rows[1].SyncedAt.Valid || rows[2].SyncedAt.Valid {
		t.Errorf("rows without a current-revision reading carry one: %+v / %+v", rows[1], rows[2])
	}
	fencedUpsert(ctx, t, q, b, bSpare, rev, 33)
	if rows, err = q.AnalyticsAllAnthropicGauges(ctx); err != nil || rows[2].FiveHourPct.Int16 != 33 {
		t.Fatalf("fresh reading not visible: %+v (%v)", rows, err)
	}

	// 230 identities across three users: exactly 201 rows, the ordered prefix (a, then b, then
	// the first of c's by label).
	type key struct{ email, label string }
	var want []key // 3 earlier tokens + 227 = 230 identities
	seed := func(user uuid.UUID, n int) {
		for i := 0; i < n; i++ {
			label := fmt.Sprintf("t%03d", i)
			if _, err := insertSecret(ctx, pool, user, store.KindAnthropicToken, label, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed(a, 69)
	seed(b, 80)
	seed(c, 78)
	// Per user: the default token first, then lower(label): "spare" sorts before "t000".
	want = append(want, key{"cap-a@e2e", "only"})
	for i := 0; i < 69; i++ {
		want = append(want, key{"cap-a@e2e", fmt.Sprintf("t%03d", i)})
	}
	want = append(want, key{"cap-b@e2e", "Default"}, key{"cap-b@e2e", "spare"})
	for i := 0; i < 80; i++ {
		want = append(want, key{"cap-b@e2e", fmt.Sprintf("t%03d", i)})
	}
	for i := 0; i < 78; i++ {
		want = append(want, key{"cap-c@e2e", fmt.Sprintf("t%03d", i)})
	}
	if total := len(want); total != 230 {
		t.Fatalf("fixture: %d identities, want 230", total)
	}
	if rows, err = q.AnalyticsAllAnthropicGauges(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 201 {
		t.Fatalf("%d rows for 230 identities, want exactly 201", len(rows))
	}
	for i, r := range rows {
		if (key{r.Email, r.Label}) != want[i] {
			t.Fatalf("row %d is %v, want %v", i, key{r.Email, r.Label}, want[i])
		}
	}
}

// codexGaugeMatches reports the first difference between a gauge row and the canonical owner
// row it copies, over every field the gauge selects.
func codexGaugeMatches(g store.AnalyticsOwnerCodexGaugesRow, c store.GetCodexAccountRateLimitsForUserRow) string {
	switch {
	case g.ProviderAccountID != c.ProviderAccountID:
		return "provider_account_id"
	case fmt.Sprint(g.Aliases) != fmt.Sprint(c.Aliases):
		return "aliases"
	case g.IsDefault != c.IsDefault:
		return "is_default"
	case g.ReauthRequired != c.ReauthRequired || g.ReauthReason != c.ReauthReason:
		return "reauth"
	case !bytes.Equal(g.Buckets, c.Buckets):
		return "buckets"
	case g.ObservedGeneration != c.ObservedGeneration || g.ObservedCredentialRevision != c.ObservedCredentialRevision:
		return "observed"
	case g.LastSuccessAt != c.LastSuccessAt || g.LastAttemptAt != c.LastAttemptAt || g.AttemptStatus != c.AttemptStatus:
		return "last attempt"
	}
	return ""
}

func TestAnalyticsCapacityCodexGaugesLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	stamp := func(acc uuid.UUID) {
		t.Helper()
		n, err := upsertCodexRows(ctx, q, store.UpsertCodexAccountRateLimitsParams{
			UserID: user, ProviderAccountID: acc, Buckets: []byte(`[{"id":"primary"}]`), AttemptStatus: "ok",
			EnablementSig: codexEnablementSig(ctx, t, q, user, acc),
		})
		if err != nil || n != 1 {
			t.Fatalf("UpsertCodexAccountRateLimits = (%d, %v)", n, err)
		}
	}
	gauge := func(acc uuid.UUID) (store.AnalyticsOwnerCodexGaugesRow, bool) {
		t.Helper()
		rows, err := q.AnalyticsOwnerCodexGauges(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		canon, err := q.GetCodexAccountRateLimitsForUser(ctx, user)
		if err != nil || len(canon) != len(rows) {
			t.Fatalf("canonical read: %d rows (%v), gauge %d", len(canon), err, len(rows))
		}
		for i := range rows {
			if d := codexGaugeMatches(rows[i], canon[i]); d != "" {
				t.Fatalf("row %d differs from GetCodexAccountRateLimitsForUser in %s", i, d)
			}
		}
		for _, r := range rows {
			if r.ProviderAccountID == acc {
				return r, true
			}
		}
		return store.AnalyticsOwnerCodexGaugesRow{}, false
	}

	// Re-enable fence, signature shape: a reading stamped before a disable and re-enable is not
	// current until a fresh poll stamps the new revision.
	solo, alias := mkLinkedCodexAccount(ctx, t, pool, q, user, "solo", true)
	stamp(solo)
	if r, ok := gauge(solo); !ok || !r.LastSuccessAt.Valid || len(r.Buckets) == 0 {
		t.Fatalf("control: fresh reading not visible: %+v", r)
	}
	setEnabled(ctx, t, q, user, alias, false)
	if _, ok := gauge(solo); ok {
		t.Fatal("an account with no enabled alias is listed")
	}
	setEnabled(ctx, t, q, user, alias, true)
	if r, ok := gauge(solo); !ok || r.LastSuccessAt.Valid || len(r.Buckets) != 0 {
		t.Fatalf("pre-disable reading visible after re-enable (pending expected): %+v", r)
	}
	stamp(solo) // a signature covering the new revision
	if r, ok := gauge(solo); !ok || !r.LastSuccessAt.Valid {
		t.Fatalf("fresh signature covering revision 1 not visible: %+v", r)
	}

	// Re-enable fence, legacy shape: a NULL signature covers revision-0 aliases only.
	legacy, lalias := mkLinkedCodexAccount(ctx, t, pool, q, user, "legacy", false)
	stamp(legacy)
	mustExec(ctx, t, pool, `UPDATE codex_account_rate_limits SET enablement_sig = NULL WHERE user_id = $1 AND provider_account_id = $2`, user, legacy)
	if r, ok := gauge(legacy); !ok || !r.LastSuccessAt.Valid {
		t.Fatalf("legacy NULL signature at revision 0 not visible: %+v", r)
	}
	setEnabled(ctx, t, q, user, lalias, false)
	setEnabled(ctx, t, q, user, lalias, true)
	if r, ok := gauge(legacy); !ok || r.LastSuccessAt.Valid {
		t.Fatalf("legacy NULL signature visible at revision 1: %+v", r)
	}

	// Two aliases of one account are one row, both labels rolled up.
	pair, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "pair-a", false)
	addLinkedAlias(ctx, t, pool, q, user, pair, "pair-b", false)
	rows, err := q.AnalyticsOwnerCodexGauges(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.ProviderAccountID == pair {
			n++
			if len(r.Aliases) != 2 {
				t.Errorf("aliases %v, want both labels", r.Aliases)
			}
		}
	}
	if n != 1 {
		t.Errorf("an account with two aliases appears %d times, want 1", n)
	}
	if !sort.SliceIsSorted(rows, func(i, j int) bool { return rows[i].ProviderAccountID.String() < rows[j].ProviderAccountID.String() }) {
		t.Error("rows are not ordered by account id")
	}
}

func TestAnalyticsCapacityCodexGaugesCapLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	var ids []string
	for i := 0; i < 230; i++ {
		acc, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, fmt.Sprintf("c%03d", i), false)
		ids = append(ids, acc.String())
	}
	sort.Strings(ids)
	rows, err := q.AnalyticsOwnerCodexGauges(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 201 {
		t.Fatalf("%d rows for 230 accounts, want exactly 201", len(rows))
	}
	for i, r := range rows {
		if r.ProviderAccountID.String() != ids[i] {
			t.Fatalf("row %d is %s, want %s (the first 201 by account id)", i, r.ProviderAccountID, ids[i])
		}
	}
}

func TestAnalyticsCapacityAllCodexGaugesLiveDB(t *testing.T) {
	ctx, pool := capPrivateDB(t)
	q := store.New(pool)
	type acct struct {
		user uuid.UUID
		id   string
	}
	var byUser [3][]acct
	users := []uuid.UUID{
		seedCapUser(ctx, t, pool, "cap-a@e2e"),
		seedCapUser(ctx, t, pool, "cap-b@e2e"),
		seedCapUser(ctx, t, pool, "cap-c@e2e"),
	}
	for u, n := range []int{70, 80, 80} {
		for i := 0; i < n; i++ {
			acc, _ := mkLinkedCodexAccount(ctx, t, pool, q, users[u], fmt.Sprintf("u%dc%03d", u, i), false)
			byUser[u] = append(byUser[u], acct{users[u], acc.String()})
		}
		sort.Slice(byUser[u], func(i, j int) bool { return byUser[u][i].id < byUser[u][j].id })
	}
	var want []acct
	for u := range byUser {
		want = append(want, byUser[u]...)
	}
	rows, err := q.AnalyticsAllCodexGauges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 201 {
		t.Fatalf("%d rows for 230 accounts, want exactly 201", len(rows))
	}
	for i, r := range rows {
		if r.UserID != want[i].user || r.ProviderAccountID.String() != want[i].id {
			t.Fatalf("row %d is %s/%s, want %s/%s (email, then account id)", i, r.UserID, r.ProviderAccountID, want[i].user, want[i].id)
		}
	}
	if rows[0].Email != "cap-a@e2e" || rows[0].VaultMode == "" || rows[200].Email != "cap-c@e2e" {
		t.Errorf("identity metadata: first %+v last email %q", rows[0], rows[200].Email)
	}

	// The reading fence is the canonical one: a re-enabled alias hides the older reading.
	acc, alias := mkLinkedCodexAccount(ctx, t, pool, q, users[0], "fence", false)
	stamp := func() {
		t.Helper()
		if n, err := upsertCodexRows(ctx, q, store.UpsertCodexAccountRateLimitsParams{
			UserID: users[0], ProviderAccountID: acc, Buckets: []byte(`[{"id":"primary"}]`), AttemptStatus: "ok",
			EnablementSig: codexEnablementSig(ctx, t, q, users[0], acc),
		}); err != nil || n != 1 {
			t.Fatalf("UpsertCodexAccountRateLimits = (%d, %v)", n, err)
		}
	}
	reading := func() (visible, found bool) {
		t.Helper()
		all, err := q.AnalyticsAllCodexGauges(ctx)
		if err != nil {
			t.Fatal(err)
		}
		canon, err := q.ListCodexAccountRateLimits(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range canon {
			if c.ProviderAccountID == acc {
				for _, r := range all {
					if r.ProviderAccountID == acc {
						if r.LastSuccessAt != c.LastSuccessAt || !bytes.Equal(r.Buckets, c.Buckets) {
							t.Fatal("all-users gauge differs from ListCodexAccountRateLimits")
						}
						return r.LastSuccessAt.Valid, true
					}
				}
			}
		}
		return false, false
	}
	stamp()
	if v, ok := reading(); !ok || !v {
		t.Fatalf("control: fresh reading visible=%v found=%v", v, ok)
	}
	setEnabled(ctx, t, q, users[0], alias, false)
	setEnabled(ctx, t, q, users[0], alias, true)
	if v, ok := reading(); !ok || v {
		t.Fatalf("pre-disable reading after re-enable: visible=%v found=%v, want pending", v, ok)
	}
	stamp()
	if v, ok := reading(); !ok || !v {
		t.Fatalf("fresh signature: visible=%v found=%v", v, ok)
	}
}

func TestAnalyticsSavepointGaugeErrorKeepsSnapshotLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("capsp1")
	tok := enablementToken(f.ctx, t, f.q, o.User, "only", true)
	fencedUpsert(f.ctx, t, f.q, o.User, tok, 0, 10)
	repo, _ := f.repo(o, "capsp1")
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(20, 0), Started: capDay(20, 0), Finished: capDay(20, 6)})
	hist := func(ctx context.Context, q *store.Queries) time.Time {
		t.Helper()
		got, err := q.AnalyticsOwnerHistoryStart(ctx, store.AnalyticsOwnerHistoryStartParams{UserID: o.User, Kinds: runkind.All()})
		if err != nil || !got.Valid {
			t.Fatalf("history start: %v (%v)", got, err)
		}
		return got.Time
	}
	err := store.RunAnalyticsSnapshot(f.ctx, f.pool, func(dctx context.Context, s *store.AnalyticsSnapshot) error {
		first := hist(dctx, s.Q) // fixes the snapshot
		// A second connection commits a newer reading after the snapshot was taken.
		if n := fencedUpsert(f.ctx, t, f.q, o.User, tok, 0, 99); n != 1 {
			t.Fatalf("competing reading affected %d rows, want exactly 1 (a no-op would make the isolation check vacuous)", n)
		}

		gaugeErr, err := s.Savepoint(dctx, func(gctx context.Context, q *store.Queries, db store.DBTX) error {
			_, e := db.Exec(gctx, "SELECT 1/0")
			return e
		})
		var pgErr *pgconn.PgError
		if err != nil || !errors.As(gaugeErr, &pgErr) || pgErr.Code != "22012" {
			t.Fatalf("Savepoint = (%v, %v), want (division_by_zero, nil)", gaugeErr, err)
		}
		// The next gauge runs on the restored transaction and the same snapshot.
		var seen int16
		gaugeErr, err = s.Savepoint(dctx, func(gctx context.Context, q *store.Queries, db store.DBTX) error {
			rows, e := q.AnalyticsOwnerAnthropicGauges(gctx, o.User)
			if e != nil {
				return e
			}
			if len(rows) != 1 {
				return fmt.Errorf("%d gauge rows", len(rows))
			}
			seen = rows[0].FiveHourPct.Int16
			return nil
		})
		if err != nil || gaugeErr != nil {
			t.Fatalf("second Savepoint = (%v, %v)", gaugeErr, err)
		}
		if seen != 10 {
			t.Errorf("gauge after the failure read %d%%, want 10 (the snapshot value, not the newly committed 99)", seen)
		}
		if again := hist(dctx, s.Q); !again.Equal(first) {
			t.Errorf("occupancy read changed after the failed gauge: %v vs %v", again, first)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAnalyticsSavepointTimeoutIsFatalLiveDB(t *testing.T) {
	ctx, pool := openAnalyticsViewsDB(t)
	err := store.RunAnalyticsSnapshot(ctx, pool, func(dctx context.Context, s *store.AnalyticsSnapshot) error {
		gaugeErr, err := s.Savepoint(dctx, func(gctx context.Context, _ *store.Queries, db store.DBTX) error {
			if _, e := db.Exec(gctx, "SET LOCAL statement_timeout = '50ms'"); e != nil {
				return e
			}
			_, e := db.Exec(gctx, "SELECT pg_sleep(1)")
			return e
		})
		if gaugeErr != nil {
			t.Errorf("a statement timeout was reported as a gauge error: %v", gaugeErr)
		}
		return err
	})
	if err == nil || !errors.Is(store.ClassifyAnalyticsErr(err), store.ErrAnalyticsTimeout) {
		t.Fatalf("snapshot error = %v, want a classified analytics timeout", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Errorf("error %v does not carry SQLSTATE 57014", err)
	}
}

func TestAnalyticsSavepointSuccessLiveDB(t *testing.T) {
	ctx, pool := openAnalyticsViewsDB(t)
	err := store.RunAnalyticsSnapshot(ctx, pool, func(dctx context.Context, s *store.AnalyticsSnapshot) error {
		var got int
		gaugeErr, err := s.Savepoint(dctx, func(gctx context.Context, _ *store.Queries, db store.DBTX) error {
			return db.QueryRow(gctx, "SELECT 7").Scan(&got)
		})
		if gaugeErr != nil || err != nil || got != 7 {
			t.Fatalf("Savepoint = (%v, %v) got %d", gaugeErr, err, got)
		}
		// The snapshot continues after RELEASE.
		if _, err := s.Q.AnalyticsOwnerHistoryStart(dctx, store.AnalyticsOwnerHistoryStartParams{UserID: uuid.New(), Kinds: runkind.All()}); err != nil {
			t.Errorf("snapshot unusable after RELEASE: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAnalyticsSavepointSnapshotDeadlineRaceLiveDB(t *testing.T) {
	ctx, pool := openAnalyticsViewsDB(t)
	plain := errors.New("gauge failed")
	// Control: an ordinary error is not a timeout, so returning it unchanged would be a 500.
	if got := store.ClassifyAnalyticsErr(plain); got != plain {
		t.Fatalf("ClassifyAnalyticsErr changed an ordinary error: %v", got)
	}
	var entered bool
	err := store.RunAnalyticsSnapshot(ctx, pool, func(dctx context.Context, s *store.AnalyticsSnapshot) error {
		// A snapshot-style context that expires while its parent (the request) stays live.
		sctx, cancel := context.WithTimeout(dctx, 200*time.Millisecond)
		defer cancel()
		gaugeErr, err := s.Savepoint(sctx, func(gctx context.Context, _ *store.Queries, _ store.DBTX) error {
			entered = true
			<-gctx.Done()
			return plain
		})
		if !entered {
			t.Error("the gauge function never ran")
		}
		if dctx.Err() != nil {
			t.Errorf("parent context ended: %v", dctx.Err())
		}
		if gaugeErr != nil {
			t.Errorf("gaugeErr = %v, want nil", gaugeErr)
		}
		if err != context.DeadlineExceeded {
			t.Errorf("err = %v (%T), want context.DeadlineExceeded itself", err, err)
		}
		if !errors.Is(store.ClassifyAnalyticsErr(err), store.ErrAnalyticsTimeout) {
			t.Errorf("ClassifyAnalyticsErr(%v) is not a timeout", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
