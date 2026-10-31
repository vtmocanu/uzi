package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests pin issue #3719's schema and queries at the SQL layer against a REAL Postgres:
// the widened user_secrets_auto_eligible_kind_check, the two new CHECKs, the defaults, and
// every new codex_pool.sql query. They skip unless UZI_TEST_DATABASE_URL is set
// (./e2e/run-store-it.sh provides a throwaway database and runs the LiveDB suffix).

// codexPoolFixture is one user with a linked, pooled codex_auth alias on one account.
type codexPoolFixture struct {
	user    uuid.UUID
	alias   uuid.UUID
	account uuid.UUID
}

func seedCodexAlias(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user uuid.UUID, pooled, linked bool, account uuid.UUID) uuid.UUID {
	t.Helper()
	alias := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO user_secrets (id,user_id,kind,label,ciphertext,sealed_with,auto_eligible)
		VALUES ($1,$2,'codex_auth',$3,$4,'master',$5)`, alias, user, alias.String(), []byte("fixture"), pooled)
	if linked {
		mustExec(ctx, t, pool, `INSERT INTO codex_credential_state (user_secret_id,user_id,status,provider_account_id)
			VALUES ($1,$2,'linked',$3)`, alias, user, account)
	}
	return alias
}

func seedCodexAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user uuid.UUID) (uuid.UUID, string, string) {
	t.Helper()
	account := uuid.New()
	puid, wsid := "pu-"+account.String(), "ws-"+account.String()
	mustExec(ctx, t, pool, `INSERT INTO codex_provider_account
		(id,user_id,provider_user_id,workspace_account_id,sealed_login,sealed_with)
		VALUES ($1,$2,$3,$4,$5,'master')`, account, user, puid, wsid, []byte("fixture"))
	return account, puid, wsid
}

func seedCodexPool(ctx context.Context, t *testing.T, pool *pgxpool.Pool) codexPoolFixture {
	t.Helper()
	user := seedBareUser(ctx, t, pool)
	account, _, _ := seedCodexAccount(ctx, t, pool, user)
	return codexPoolFixture{user: user, account: account, alias: seedCodexAlias(ctx, t, pool, user, true, true, account)}
}

// seedCodexRun inserts a chat-lane run (no repo needed) on the given harness.
func seedCodexRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user uuid.UUID, harness, status string, accountKey any) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO runs (id,user_id,kind,issue_title,issue_description,status,harness,codex_account_key)
		VALUES ($1,$2,'chat','t','d',$3,$4,$5)`, id, user, status, harness, accountKey)
	return id
}

func requireCheckViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected CHECK %s to reject the row, got no error", constraint)
	}
	if !strings.Contains(err.Error(), constraint) {
		t.Fatalf("expected violation of %s, got %v", constraint, err)
	}
}

func TestCodexPoolChecksLiveDB(t *testing.T) {
	ctx, q, pool := autoEligPool(t)
	user := seedBareUser(ctx, t, pool)

	// The widened auto_eligible CHECK admits codex_auth and still rejects openai_api_key.
	_ = seedCodexAlias(ctx, t, pool, user, true, false, uuid.Nil)
	_, err := pool.Exec(ctx, `INSERT INTO user_secrets (id,user_id,kind,label,ciphertext,sealed_with,auto_eligible)
		VALUES ($1,$2,'openai_api_key','k',$3,'master',true)`, uuid.New(), user, []byte("x"))
	requireCheckViolation(t, err, "user_secrets_auto_eligible_kind_check")

	// users.codex_account_mode defaults to 'default' and rejects anything but default|auto.
	mode, err := q.GetUserCodexAccountMode(ctx, user)
	if err != nil || mode != "default" {
		t.Fatalf("default mode = %q, %v", mode, err)
	}
	_, err = pool.Exec(ctx, `UPDATE users SET codex_account_mode='x' WHERE id=$1`, user)
	requireCheckViolation(t, err, "users_codex_account_mode_check")
	if n, err := q.SetUserCodexAccountMode(ctx, store.SetUserCodexAccountModeParams{ID: user, Mode: "auto"}); err != nil || n != 1 {
		t.Fatalf("set auto: n=%d err=%v", n, err)
	}
	if mode, _ = q.GetUserCodexAccountMode(ctx, user); mode != "auto" {
		t.Fatalf("mode after set = %q", mode)
	}
	if n, err := q.SetUserCodexAccountMode(ctx, store.SetUserCodexAccountModeParams{ID: uuid.New(), Mode: "auto"}); err != nil || n != 0 {
		t.Fatalf("set on unknown user: n=%d err=%v", n, err)
	}

	// runs.codex_select_reason: a known value on a codex run is fine; an unknown value and a
	// non-codex harness are rejected.
	run := seedCodexRun(ctx, t, pool, user, "codex", "queued", nil)
	mustExec(ctx, t, pool, `UPDATE runs SET codex_select_reason='best_of_pool' WHERE id=$1`, run)
	_, err = pool.Exec(ctx, `UPDATE runs SET codex_select_reason='bogus' WHERE id=$1`, run)
	requireCheckViolation(t, err, "runs_codex_select_reason_check")
	claude := seedCodexRun(ctx, t, pool, user, "claude", "queued", nil)
	_, err = pool.Exec(ctx, `UPDATE runs SET codex_select_reason='auto' WHERE id=$1`, claude)
	requireCheckViolation(t, err, "runs_codex_select_reason_check")
}

func TestCodexPoolQueriesLiveDB(t *testing.T) {
	ctx, q, pool := autoEligPool(t)
	user := seedBareUser(ctx, t, pool)
	account, puid, wsid := seedCodexAccount(ctx, t, pool, user)
	other, puid2, wsid2 := seedCodexAccount(ctx, t, pool, user)

	pooled := seedCodexAlias(ctx, t, pool, user, true, true, account)
	disabled := seedCodexAlias(ctx, t, pool, user, true, true, other)
	mustExec(ctx, t, pool, `UPDATE user_secrets SET disabled_at=now() WHERE id=$1`, disabled)
	unlinked := seedCodexAlias(ctx, t, pool, user, true, false, uuid.Nil)
	_ = seedCodexAlias(ctx, t, pool, user, false, true, account) // unpooled sibling
	quarantinedAcct, _, _ := seedCodexAccount(ctx, t, pool, user)
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1`, quarantinedAcct)
	quarantined := seedCodexAlias(ctx, t, pool, user, true, true, quarantinedAcct)
	reauthAcct, _, _ := seedCodexAccount(ctx, t, pool, user)
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET reauth_required=true, reauth_generation=1, reauth_credential_revision=1 WHERE id=$1`, reauthAcct)
	reauth := seedCodexAlias(ctx, t, pool, user, true, true, reauthAcct)
	mustExec(ctx, t, pool, `INSERT INTO codex_account_admission_throttle
		(user_id,provider_account_id,effective_expiry,episode_id,spread_width_ms)
		VALUES ($1,$2,now()+interval '1 hour',gen_random_uuid(),1500)`, user, account)

	// ListCodexPoolAliases: pooled rows only, disabled and unlinked included, by alias id.
	rows, err := q.ListCodexPoolAliases(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("want 5 pooled aliases (enabled, disabled, unlinked, quarantined, reauth), got %d", len(rows))
	}
	if !sort.SliceIsSorted(rows, func(i, j int) bool { return rows[i].AliasID.String() < rows[j].AliasID.String() }) {
		t.Fatalf("rows not ordered by alias id")
	}
	byID := map[uuid.UUID]store.ListCodexPoolAliasesRow{}
	for _, r := range rows {
		byID[r.AliasID] = r
	}
	if r := byID[pooled]; !r.Enabled || r.LinkStatus.String != "linked" || uuid.UUID(r.ProviderAccountID.Bytes) != account ||
		r.ProviderUserID.String != puid || r.WorkspaceAccountID.String != wsid || r.CoordState.String != "idle" ||
		!r.CoordState.Valid || r.ReauthRequired.Bool || !r.ThrottleEffectiveExpiry.Valid || r.ThrottleSpreadWidthMs.Int32 != 1500 {
		t.Fatalf("pooled row = %+v", r)
	}
	if r := byID[disabled]; r.Enabled || !r.LinkStatus.Valid {
		t.Fatalf("disabled row = %+v", r)
	}
	if r := byID[unlinked]; !r.Enabled || r.LinkStatus.Valid || r.ProviderAccountID.Valid || r.ThrottleEffectiveExpiry.Valid {
		t.Fatalf("unlinked row = %+v", r)
	}
	if r := byID[quarantined]; r.CoordState.String != "quarantined" || r.ReauthRequired.Bool {
		t.Fatalf("quarantined row = %+v", r)
	}
	if r := byID[reauth]; !r.ReauthRequired.Bool || r.CoordState.String != "idle" {
		t.Fatalf("reauth row = %+v", r)
	}
	if foreign, err := q.ListCodexPoolAliases(ctx, uuid.New()); err != nil || len(foreign) != 0 {
		t.Fatalf("foreign user rows=%d err=%v", len(foreign), err)
	}

	// LockCodexPoolAliasForSelection: owner-scoped, returns the fence facts.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	got, err := q.WithTx(tx).LockCodexPoolAliasForSelection(ctx, store.LockCodexPoolAliasForSelectionParams{ID: pooled, UserID: user})
	if err != nil || got.Kind != "codex_auth" || !got.AutoEligible || !got.Enabled || got.Status.String != "linked" || uuid.UUID(got.ProviderAccountID.Bytes) != account {
		t.Fatalf("lock row = %+v err=%v", got, err)
	}
	if _, err := q.WithTx(tx).LockCodexPoolAliasForSelection(ctx, store.LockCodexPoolAliasForSelectionParams{ID: pooled, UserID: uuid.New()}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign owner: want ErrNoRows, got %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// CountCodexInFlightByAccount: nonterminal codex runs by frozen account key.
	key, _ := json.Marshal([]string{puid, wsid})
	for _, st := range []string{"queued", "running", "recovery_wait"} {
		seedCodexRun(ctx, t, pool, user, "codex", st, string(key))
	}
	seedCodexRun(ctx, t, pool, user, "codex", "completed", string(key)) // terminal: not counted
	seedCodexRun(ctx, t, pool, user, "claude", "running", nil)          // wrong harness
	seedCodexRun(ctx, t, pool, user, "codex", "queued", nil)            // NULL key
	seedCodexRun(ctx, t, pool, user, "codex", "queued", "not json")     // garbage key, must not error
	// A second account of the same user is attributed separately.
	key2, _ := json.Marshal([]string{puid2, wsid2})
	seedCodexRun(ctx, t, pool, user, "codex", "running", string(key2))
	seedCodexRun(ctx, t, pool, user, "codex", "queued", string(key2))
	// Another user's run carrying the SAME key must not count toward this user.
	stranger := seedBareUser(ctx, t, pool)
	seedCodexRun(ctx, t, pool, stranger, "codex", "running", string(key))
	counts, err := q.CountCodexInFlightByAccount(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	byAccount := map[uuid.UUID]int{}
	for _, c := range counts {
		byAccount[c.ProviderAccountID] = int(c.InFlight)
	}
	if len(counts) != 2 || byAccount[account] != 3 || byAccount[other] != 2 {
		t.Fatalf("counts = %+v, want {account:3, other:2}", counts)
	}

	// SetRunCodexSelectReason is write-once, owner-scoped and codex-only.
	run := seedCodexRun(ctx, t, pool, user, "codex", "queued", nil)
	set := func(id, owner uuid.UUID, reason string) int64 {
		n, err := q.SetRunCodexSelectReason(ctx, store.SetRunCodexSelectReasonParams{ID: id, UserID: owner, Reason: reason})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := set(run, uuid.New(), "auto"); n != 0 {
		t.Fatalf("foreign owner wrote %d rows", n)
	}
	if n := set(run, user, "auto"); n != 1 {
		t.Fatalf("first write rows=%d", n)
	}
	if n := set(run, user, "pool_stale"); n != 0 {
		t.Fatalf("second write rows=%d, want 0 (write-once)", n)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT codex_select_reason FROM runs WHERE id=$1`, run).Scan(&reason); err != nil || reason != "auto" {
		t.Fatalf("stored reason = %q err=%v", reason, err)
	}
	claude := seedCodexRun(ctx, t, pool, user, "claude", "queued", nil)
	if n := set(claude, user, "auto"); n != 0 {
		t.Fatalf("non-codex run wrote %d rows", n)
	}
}

// TestCodexPooledAliasInvisibleToAnthropicReadersLiveDB: pooling a codex_auth alias must not
// leak into any Anthropic auto-eligibility reader.
func TestCodexPooledAliasInvisibleToAnthropicReadersLiveDB(t *testing.T) {
	ctx, q, pool := autoEligPool(t)
	fx := seedCodexPool(ctx, t, pool)

	cands, err := q.ListAutoSelectCandidates(ctx, fx.user)
	if err != nil || len(cands) != 0 {
		t.Fatalf("ListAutoSelectCandidates = %d rows, err %v", len(cands), err)
	}
	reserve, err := q.ListReserveClaudeTokens(ctx, fx.user)
	if err != nil || len(reserve) != 0 {
		t.Fatalf("ListReserveClaudeTokens = %d rows, err %v", len(reserve), err)
	}
	if has, err := q.UserHasAutoEligibleAnthropicToken(ctx, fx.user); err != nil || has {
		t.Fatalf("UserHasAutoEligibleAnthropicToken = %v, %v", has, err)
	}
	if has, err := q.HasEnabledAnthropicForJudgeAuto(ctx, fx.user); err != nil || has {
		t.Fatalf("HasEnabledAnthropicForJudgeAuto = %v, %v", has, err)
	}
	forUser, err := q.ListRateLimitsForUser(ctx, fx.user)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range forUser {
		if r.UserSecretID == fx.alias {
			t.Fatalf("ListRateLimitsForUser lists the codex alias: %+v", r)
		}
	}
	all, err := q.ListRateLimits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.UserID == fx.user && r.UserSecretID.Valid {
			t.Fatalf("ListRateLimits lists a token row for the codex-only user: %+v", r)
		}
	}
}

// TestCodexInFlightStatusListMatchesRunsStatusCheck pins the explicit status list of
// CountCodexInFlightByAccount to the live runs_status_check (the highest-numbered migration
// whose Up section ADDs it) minus the terminal statuses, so a status added later cannot
// silently fall out of the in-flight bias. It needs no database.
func TestCodexInFlightStatusListMatchesRunsStatusCheck(t *testing.T) {
	quoted := regexp.MustCompile(`'([a-z_]+)'`)
	readFile := func(path string) string {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: test reads fixed repo files (migrations and queries dirs)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	stripComments := func(src string) string {
		var sb strings.Builder
		for _, line := range strings.Split(src, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				sb.WriteString(line + "\n")
			}
		}
		return sb.String()
	}
	values := func(list, where string) []string {
		var out []string
		for _, m := range quoted.FindAllStringSubmatch(list, -1) {
			out = append(out, m[1])
		}
		if len(out) == 0 {
			t.Fatalf("no quoted values in %s", where)
		}
		return out
	}

	// Lexical order equals numeric order: goose files are zero-padded to five digits.
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(files)
	addCheck := regexp.MustCompile(`ADD CONSTRAINT runs_status_check\s+CHECK \(status IN \(([^)]*)\)`)
	var latest string
	var latestList []string
	for _, f := range files {
		// "-- +goose Down" is itself a comment line, so cut on the raw text before stripping.
		raw := readFile(f)
		if k := strings.Index(raw, "-- +goose Down"); k >= 0 {
			raw = raw[:k]
		}
		up := stripComments(raw)
		if m := addCheck.FindStringSubmatch(up); m != nil {
			latest, latestList = f, values(m[1], f)
		}
	}
	if latest == "" {
		t.Fatal("no migration adds runs_status_check")
	}

	var want []string
	for _, s := range latestList {
		if !apitypes.IsTerminalRunStatus(s) {
			want = append(want, s)
		}
	}
	query := stripComments(readFile(filepath.Join("queries", "codex_pool.sql")))
	i := strings.Index(query, "AND r.status IN (")
	if i < 0 {
		t.Fatal("status list anchor not found in codex_pool.sql")
	}
	rest := query[i+len("AND r.status IN ("):]
	got := values(rest[:strings.Index(rest, ")")], "codex_pool.sql")
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(want, ",") != strings.Join(got, ",") {
		t.Fatalf("CountCodexInFlightByAccount status list drifted from %s.\n  query:        %v\n  nonterminal:  %v", latest, got, want)
	}
}

// TestSecretMutationSharedLockParityLiveDB pins LockSecretMutationShared to the key
// LockSecretMutation takes: while a transaction holds one mode for a user, a second connection
// sees exclusive blocked in both cases, shared allowed only against a shared holder.
func TestSecretMutationSharedLockParityLiveDB(t *testing.T) {
	ctx, _, pool := autoEligPool(t)
	user := seedBareUser(ctx, t, pool)
	try := func(fn string) bool {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var got bool
		if err := tx.QueryRow(ctx, "SELECT "+fn+"($1, $2)", store.SecretMutationLockClass, store.SecretMutationLockObjID(user)).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	hold := func(lock func(context.Context, store.DBTX, uuid.UUID) error) func() {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := lock(ctx, tx, user); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		return func() { _ = tx.Rollback(ctx) }
	}

	release := hold(store.LockSecretMutationShared)
	if try("pg_try_advisory_xact_lock") {
		t.Error("exclusive lock acquired while a shared holder exists")
	}
	if !try("pg_try_advisory_xact_lock_shared") {
		t.Error("second shared lock refused while only a shared holder exists")
	}
	release()

	release = hold(store.LockSecretMutation)
	if try("pg_try_advisory_xact_lock_shared") {
		t.Error("shared lock acquired while an exclusive holder exists")
	}
	if try("pg_try_advisory_xact_lock") {
		t.Error("second exclusive lock acquired while an exclusive holder exists")
	}
	release()

	if !try("pg_try_advisory_xact_lock") {
		t.Error("lock still held after both holders rolled back")
	}
}
