package workersvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/codexselect"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_pool_select_livedb_test.go proves issue #3719's selection at run creation against a real
// Postgres: createRunAtomic binds a pooled Codex alias chosen by headroom, freezes it with the
// reason, and honours the consent and link fence under concurrent credential writers. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

const poolTestTimeout = 20 * time.Second

// poolFix is one user with a Codex pool under test.
type poolFix struct {
	t      *testing.T
	env    codexTestEnv
	aux    *pgxpool.Pool // a roomy second pool for concurrent writers
	userID uuid.UUID
	repoID uuid.UUID
	svc    *Service
	iid    int64
}

func newPoolFix(t *testing.T) *poolFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := atomicSvc(env)
	svc.p.Autoselect = autoselect.Policy{MinHeadroom: 10, HeadroomTiePct: 5, InflightPenalty: 10}
	svc.p.CodexUsagePollInterval = 10 * time.Minute
	cfg, err := pgxpool.ParseConfig(os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 12
	aux, err := pgxpool.NewWithConfig(env.ctx, cfg)
	if err != nil {
		t.Fatalf("aux pool: %v", err)
	}
	t.Cleanup(aux.Close)
	return &poolFix{t: t, env: env, aux: aux, userID: userID, repoID: repoID, svc: svc, iid: 100}
}

// account is a linked subscription account with its first alias.
type poolAccount struct {
	alias   uuid.UUID
	account uuid.UUID
}

func (fx *poolFix) accountOf(alias uuid.UUID) uuid.UUID {
	fx.t.Helper()
	st, err := fx.env.q.GetCodexCredentialState(fx.env.ctx, store.GetCodexCredentialStateParams{UserSecretID: alias, UserID: fx.userID})
	if err != nil || !st.ProviderAccountID.Valid {
		fx.t.Fatalf("alias %s has no linked account: %v", alias, err)
	}
	return uuid.UUID(st.ProviderAccountID.Bytes)
}

// newAccount seeds a linked, UNPOOLED subscription alias on its own account.
func (fx *poolFix) newAccount(label string) poolAccount {
	fx.t.Helper()
	alias := fx.env.seedLinkedSubscription(fx.t, fx.userID, label+"-"+uuid.NewString()[:8], codexToken("access"), codexToken("refresh"))
	return poolAccount{alias: alias, account: fx.accountOf(alias)}
}

// sibling seeds a second linked alias on the SAME account as of.
func (fx *poolFix) sibling(of poolAccount, label string) uuid.UUID {
	fx.t.Helper()
	acct, err := fx.env.q.GetCodexProviderAccountByID(fx.env.ctx, store.GetCodexProviderAccountByIDParams{UserID: fx.userID, ID: of.account})
	if err != nil {
		fx.t.Fatalf("read account: %v", err)
	}
	access := codexToken("access")
	alias := fx.env.seedStagingAlias(fx.t, fx.userID, label+"-"+uuid.NewString()[:8], codexLoginBlob{AccessToken: access, RefreshToken: codexToken("refresh")})
	fake := newFakeCodexIdentity()
	fake.idByToken[access] = codexauth.Identity{ProviderUserID: acct.ProviderUserID, WorkspaceAccountID: acct.WorkspaceAccountID}
	r := NewCodexReconciler(fx.env.q, fx.env.vlt, fx.env.box, fake, fx.env.pool)
	if err := r.ReconcileCodexAuthIdentity(fx.env.ctx, fx.userID, alias); err != nil {
		fx.t.Fatalf("reconcile sibling: %v", err)
	}
	if got := fx.accountOf(alias); got != of.account {
		fx.t.Fatalf("sibling linked to %s, want %s", got, of.account)
	}
	return alias
}

func (fx *poolFix) pool(aliases ...uuid.UUID) {
	fx.t.Helper()
	for _, a := range aliases {
		fx.env.exec(`UPDATE user_secrets SET auto_eligible = true WHERE id = $1`, a)
	}
}

func (fx *poolFix) mode(m string) {
	fx.t.Helper()
	fx.env.exec(`UPDATE users SET codex_account_mode = $2 WHERE id = $1`, fx.userID, m)
}

func (fx *poolFix) makeDefault(alias uuid.UUID) { makeCodexDefault(fx.t, fx.env, alias) }

// poolEpoch anchors every test window's ABSOLUTE reset, so two readings with the same
// resetAfter tie exactly on reset (the ranker's first tie-break) however far apart they are written.
var poolEpoch = time.Now().Unix()

// poolWindow renders one Codex window whose reset is resetAfter seconds after poolEpoch.
func poolWindow(secs int64, used float64, resetAfter int64) string {
	return fmt.Sprintf(`{"used_percent":%v,"limit_window_seconds":%d,"reset_after_seconds":null,"reset_at":%d}`, used, secs, poolEpoch+resetAfter)
}

func poolBuckets(allowed, limitReached bool, primary, secondary string) string {
	return fmt.Sprintf(`[{"id":"codex","allowed":%t,"limit_reached":%t,"primary":%s,"secondary":%s}]`, allowed, limitReached, primary, secondary)
}

// reading stores a fresh reading with both windows at util percent.
func (fx *poolFix) reading(a poolAccount, util float64) {
	fx.t.Helper()
	fx.readingRaw(a, poolBuckets(true, false, poolWindow(fiveHourSecs, util, 100000), poolWindow(weekSecs, util, 100000)), "1 minute")
}

func (fx *poolFix) readingRaw(a poolAccount, buckets, ago string) {
	fx.t.Helper()
	fx.env.exec(`INSERT INTO codex_account_rate_limits (user_id, provider_account_id, buckets, last_success_at, last_attempt_at, attempt_status)
	             VALUES ($1, $2, $3::jsonb, now() - $4::interval, now() - $4::interval, 'ok')
	             ON CONFLICT (user_id, provider_account_id) DO UPDATE SET buckets = EXCLUDED.buckets,
	                 last_success_at = EXCLUDED.last_success_at, last_attempt_at = EXCLUDED.last_attempt_at`,
		fx.userID, a.account, buckets, ago)
}

func (fx *poolFix) limitReached(a poolAccount, resetAfter int64) {
	fx.t.Helper()
	fx.readingRaw(a, poolBuckets(false, true, poolWindow(fiveHourSecs, 100, resetAfter), poolWindow(weekSecs, 40, 100000)), "1 minute")
}

func (fx *poolFix) cooldown(a poolAccount, d time.Duration) {
	fx.t.Helper()
	fx.env.exec(`INSERT INTO codex_account_admission_throttle (user_id, provider_account_id, spread_width_ms, effective_expiry)
	             VALUES ($1, $2, 0, now() + $3::interval)
	             ON CONFLICT (user_id, provider_account_id) DO UPDATE SET effective_expiry = EXCLUDED.effective_expiry`,
		fx.userID, a.account, fmt.Sprintf("%d seconds", int(d.Seconds())))
}

func (fx *poolFix) reserve(pct int) {
	fx.t.Helper()
	fx.env.exec(`UPDATE users SET weekly_reserve_pct = $2 WHERE id = $1`, fx.userID, pct)
}

// create runs createRunAtomic with a manual-issue-shaped insert on a fresh issue iid.
func (fx *poolFix) create() (store.Run, error) {
	fx.t.Helper()
	fx.iid++
	iid := fx.iid
	ctx, cancel := context.WithTimeout(fx.env.ctx, poolTestTimeout)
	defer cancel()
	run, _, err := fx.svc.createRunAtomic(ctx, fx.userID, nil, func(q *store.Queries, resolved resolvedHarness) (store.Run, error) {
		return q.CreateRun(ctx, store.CreateRunParams{
			UserID: fx.userID, RepoID: fx.repoID, IssueIid: pgtype.Int8{Int64: iid, Valid: true},
			IssueTitle: "pool-select", IssueDescription: "d", PlanSource: planSourceAgent,
			TriggerSource: "manual", Harness: string(resolved.Harness),
		})
	})
	return run, err
}

func (fx *poolFix) mustCreate() store.Run {
	fx.t.Helper()
	run, err := fx.create()
	if err != nil {
		fx.t.Fatalf("createRunAtomic: %v", err)
	}
	return mustRun(fx.t, fx.env, run.ID)
}

func (fx *poolFix) runs() int { return fx.env.countRunsForUser(fx.t, fx.userID) }

func assertBound(t *testing.T, got store.Run, alias uuid.UUID, reason codexselect.Reason) {
	t.Helper()
	if !got.CodexSecretID.Valid || uuid.UUID(got.CodexSecretID.Bytes) != alias {
		t.Fatalf("bound alias = %v, want %s", got.CodexSecretID, alias)
	}
	if got.CodexSelectReason.String != string(reason) {
		t.Fatalf("codex_select_reason = %q, want %q", got.CodexSelectReason.String, reason)
	}
}

func lowerAccount(a, b poolAccount) (lo, hi poolAccount) {
	if a.account.String() < b.account.String() {
		return a, b
	}
	return b, a
}

// --- modes ---------------------------------------------------------------------------------------

// TestCodexSelectModeDefaultLiveDB: mode default binds the owner's default and records
// 'default', even with a better pooled alias present (today's behaviour, untouched).
func TestCodexSelectModeDefaultLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	def := fx.newAccount("def")
	fx.makeDefault(def.alias)
	other := fx.newAccount("other")
	fx.pool(other.alias)
	fx.reading(other, 5)
	fx.reading(def, 90)

	assertBound(t, fx.mustCreate(), def.alias, codexselect.ReasonDefault)
}

// TestCodexSelectPoolEmptyLiveDB: mode auto with nothing pooled binds the default (subscription
// and API key alike) and records 'pool_empty'.
func TestCodexSelectPoolEmptyLiveDB(t *testing.T) {
	t.Run("subscription default", func(t *testing.T) {
		fx := newPoolFix(t)
		def := fx.newAccount("def")
		fx.makeDefault(def.alias)
		fx.mode("auto")
		assertBound(t, fx.mustCreate(), def.alias, codexselect.ReasonPoolEmpty)
	})
	t.Run("api key default", func(t *testing.T) {
		fx := newPoolFix(t)
		key := fx.env.seedStaticAPIKey(t, fx.userID, "key", codexToken("sk"))
		fx.makeDefault(key)
		fx.mode("auto")
		got := fx.mustCreate()
		assertBound(t, got, key, codexselect.ReasonPoolEmpty)
		if got.CodexAuthMode.String != codexAuthModeAPIKey {
			t.Fatalf("auth mode = %q, want api_key", got.CodexAuthMode.String)
		}
	})
}

// --- ranking -------------------------------------------------------------------------------------

// MUTATION: bypass the selection branch in createRunAtomic (bind res.Codex as resolved); every
// pooled-ranking test here fails on the bound alias and the reason.
func TestCodexSelectRanksByHeadroomLiveDB(t *testing.T) {
	cases := []struct {
		name         string
		utilA, utilB float64
		want         string // "a" | "b"
		reason       codexselect.Reason
	}{
		{"auto picks the emptier account", 60, 20, "b", codexselect.ReasonAuto},
		{"auto picks the emptier account (swapped)", 20, 60, "a", codexselect.ReasonAuto},
		{"best_of_pool when every account is below the minimum headroom", 99, 92, "b", codexselect.ReasonBestOfPool},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newPoolFix(t)
			def := fx.newAccount("def")
			fx.makeDefault(def.alias)
			a, b := fx.newAccount("a"), fx.newAccount("b")
			fx.pool(a.alias, b.alias)
			fx.reading(a, tc.utilA)
			fx.reading(b, tc.utilB)
			fx.mode("auto")
			want := a
			if tc.want == "b" {
				want = b
			}
			assertBound(t, fx.mustCreate(), want.alias, tc.reason)
		})
	}
}

// TestCodexSelectPoolStaleLiveDB: with the reserve off, accounts with no measurable reading
// (none, aged out, or polling disabled) fall to the lowest account key with 'pool_stale'.
func TestCodexSelectPoolStaleLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(fx *poolFix, a, b poolAccount)
	}{
		{"no readings", func(fx *poolFix, a, b poolAccount) {}},
		{"aged readings", func(fx *poolFix, a, b poolAccount) {
			fx.readingRaw(a, poolBuckets(true, false, poolWindow(fiveHourSecs, 5, 100000), poolWindow(weekSecs, 5, 100000)), "3 hours")
			fx.readingRaw(b, poolBuckets(true, false, poolWindow(fiveHourSecs, 5, 100000), poolWindow(weekSecs, 5, 100000)), "3 hours")
		}},
		{"polling disabled", func(fx *poolFix, a, b poolAccount) {
			fx.svc.p.CodexUsagePollInterval = 0
			fx.reading(a, 5)
			fx.reading(b, 5)
		}},
		{"a bucket without the codex id", func(fx *poolFix, a, b poolAccount) {
			raw := `[{"id":"other","allowed":true,"limit_reached":false,"primary":` + poolWindow(fiveHourSecs, 5, 100000) + `,"secondary":null}]`
			fx.readingRaw(a, raw, "1 minute")
			fx.readingRaw(b, raw, "1 minute")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newPoolFix(t)
			def := fx.newAccount("def")
			fx.makeDefault(def.alias)
			a, b := fx.newAccount("a"), fx.newAccount("b")
			fx.pool(a.alias, b.alias)
			tc.setup(fx, a, b)
			fx.mode("auto")
			lo, _ := lowerAccount(a, b)
			assertBound(t, fx.mustCreate(), lo.alias, codexselect.ReasonPoolStale)
		})
	}
}

// TestCodexSelectUnpooledSiblingNeverChosenLiveDB: an account's UNPOOLED alias is never bound;
// among several pooled aliases of one account the lowest alias id is.
func TestCodexSelectUnpooledSiblingNeverChosenLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	def := fx.newAccount("def")
	fx.makeDefault(def.alias)
	x := fx.newAccount("x")
	sib := fx.sibling(x, "sib")
	other := fx.newAccount("other")
	fx.pool(other.alias)
	fx.reading(other, 90)
	fx.reading(x, 10)
	// Only the first alias of x is pooled, so only it may carry x. Whichever of x.alias and sib
	// has the lower id, the unpooled one must never be bound.
	fx.pool(sib)
	fx.mode("auto")
	assertBound(t, fx.mustCreate(), sib, codexselect.ReasonAuto)

	fx.pool(x.alias)
	want := x.alias
	if sib.String() < x.alias.String() {
		want = sib
	}
	assertBound(t, fx.mustCreate(), want, codexselect.ReasonAuto)
}

// TestCodexSelectInFlightBiasLiveDB: queued runs count against their account across aliases, so a
// tie breaks toward the account carrying fewer nonterminal runs; terminal runs do not count.
func TestCodexSelectInFlightBiasLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	def := fx.newAccount("def")
	fx.makeDefault(def.alias)
	a, b := fx.newAccount("a"), fx.newAccount("b")
	sibA := fx.sibling(a, "siba")
	fx.pool(a.alias, sibA, b.alias)
	fx.reading(a, 30)
	fx.reading(b, 30)
	fx.mode("auto")

	lo, hi := lowerAccount(a, b)
	// No load: the tie breaks on the lowest account key.
	assertBound(t, fx.mustCreate(), aliasForAccount(fx, lo, a, sibA), codexselect.ReasonAuto)

	// Two queued runs on the low account, bound through DIFFERENT aliases of it: the count is per
	// account, so the bias moves the next pick to the other account.
	key := accountKeyOf(fx, lo)
	for i := 0; i < 2; i++ {
		fx.insertCodexRun(key, "queued")
	}
	assertBound(t, fx.mustCreate(), hiAlias(fx, hi, b), codexselect.ReasonAuto)
}

func aliasForAccount(fx *poolFix, acct, a poolAccount, sibA uuid.UUID) uuid.UUID {
	if acct.account != a.account {
		return acct.alias
	}
	if sibA.String() < a.alias.String() {
		return sibA
	}
	return a.alias
}

func hiAlias(fx *poolFix, acct, b poolAccount) uuid.UUID {
	if acct.account == b.account {
		return b.alias
	}
	// The high account is the one with two pooled aliases: the lowest of them.
	var lo uuid.UUID
	rows, err := fx.env.pool.Query(fx.env.ctx, `SELECT user_secret_id FROM codex_credential_state WHERE user_id = $1 AND provider_account_id = $2 ORDER BY user_secret_id`, fx.userID, acct.account)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&lo); err != nil {
			fx.t.Fatal(err)
		}
	}
	return lo
}

func accountKeyOf(fx *poolFix, a poolAccount) string {
	acct, err := fx.env.q.GetCodexProviderAccountByID(fx.env.ctx, store.GetCodexProviderAccountByIDParams{UserID: fx.userID, ID: a.account})
	if err != nil {
		fx.t.Fatal(err)
	}
	key, err := codexAccountKey(acct.ProviderUserID, acct.WorkspaceAccountID)
	if err != nil {
		fx.t.Fatal(err)
	}
	return key
}

// insertCodexRun seeds a Codex run frozen to the account key in the given status.
func (fx *poolFix) insertCodexRun(key, status string) {
	fx.t.Helper()
	fx.iid++
	fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, harness, codex_account_key)
	             VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, 'codex', $6)`,
		uuid.New(), fx.userID, fx.repoID, fx.iid, status, key)
}

// TestCodexSelectTerminalRunsDoNotCountLiveDB: only nonterminal runs bias the pick.
func TestCodexSelectTerminalRunsDoNotCountLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	def := fx.newAccount("def")
	fx.makeDefault(def.alias)
	a, b := fx.newAccount("a"), fx.newAccount("b")
	fx.pool(a.alias, b.alias)
	fx.reading(a, 30)
	fx.reading(b, 30)
	fx.mode("auto")
	lo, _ := lowerAccount(a, b)
	key := accountKeyOf(fx, lo)
	for i := 0; i < 3; i++ {
		fx.insertCodexRun(key, "completed")
	}
	assertBound(t, fx.mustCreate(), lo.alias, codexselect.ReasonAuto)
}

// --- exclusions ----------------------------------------------------------------------------------

// TestCodexSelectStructuralExclusionsLiveDB: a disabled, unlinked, quarantined or reauth-flagged
// alias is never bound, however good its reading; with only such aliases the create is refused.
func TestCodexSelectStructuralExclusionsLiveDB(t *testing.T) {
	type breakFn func(fx *poolFix, bad poolAccount)
	cases := map[string]breakFn{
		"disabled": func(fx *poolFix, bad poolAccount) {
			fx.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, bad.alias)
		},
		"unlinked": func(fx *poolFix, bad poolAccount) {
			fx.env.exec(`UPDATE codex_credential_state SET status = 'staging', provider_account_id = NULL WHERE user_secret_id = $1`, bad.alias)
		},
		"quarantined": func(fx *poolFix, bad poolAccount) {
			fx.env.exec(`UPDATE codex_provider_account SET coord_state = 'quarantined' WHERE id = $1`, bad.account)
		},
		"reauth required": func(fx *poolFix, bad poolAccount) {
			fx.env.exec(`UPDATE codex_provider_account SET reauth_required = true, reauth_generation = 1, reauth_credential_revision = 1 WHERE id = $1`, bad.account)
		},
	}
	for name, brk := range cases {
		for _, polling := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/polling=%t", name, polling), func(t *testing.T) {
				fx := newPoolFix(t)
				if !polling {
					fx.svc.p.CodexUsagePollInterval = 0
				}
				def := fx.newAccount("def")
				fx.makeDefault(def.alias)
				bad, good := fx.newAccount("bad"), fx.newAccount("good")
				fx.pool(bad.alias, good.alias)
				fx.reading(bad, 1)
				fx.reading(good, 70)
				brk(fx, bad)
				fx.mode("auto")
				want := codexselect.ReasonAuto
				if !polling {
					want = codexselect.ReasonPoolStale
				}
				for i := 0; i < 3; i++ {
					assertBound(t, fx.mustCreate(), good.alias, want)
				}

				// Break the good one as well: nothing bindable remains, so creation is refused
				// (implicit: no usable credential; explicit codex: the pool refusal).
				brk(fx, good)
				before := fx.runs()
				if _, err := fx.create(); !errors.Is(err, errNoUsableCredential) {
					t.Fatalf("implicit create err = %v, want errNoUsableCredential", err)
				}
				ctx := fx.env.ctx
				_, _, err := fx.svc.createRunAtomic(ctx, fx.userID, h(HarnessCodex), func(q *store.Queries, r resolvedHarness) (store.Run, error) {
					t.Fatal("insert must not run when nothing is selectable")
					return store.Run{}, nil
				})
				if !errors.Is(err, ErrCodexPoolUnavailable) || !errors.Is(err, ErrNoCredentialForHarness) {
					t.Fatalf("explicit codex err = %v, want ErrCodexPoolUnavailable wrapping ErrNoCredentialForHarness", err)
				}
				if fx.runs() != before {
					t.Fatalf("a refused create left runs: %d -> %d", before, fx.runs())
				}
			})
		}
	}
}

// TestCodexSelectCapacityExclusionsLiveDB: a limit-reached, cooling-down or reserve-held account
// is reachable only through pool_exhausted; a healthy account beats it, and among all-excluded
// accounts the earliest known release wins.
func TestCodexSelectCapacityExclusionsLiveDB(t *testing.T) {
	type cause struct {
		name string
		// excluded makes a excluded; soon/late order two excluded accounts by release time.
		exclude func(fx *poolFix, a poolAccount)
		soon    func(fx *poolFix, a poolAccount)
		late    func(fx *poolFix, a poolAccount)
		prep    func(fx *poolFix)
	}
	causes := []cause{
		{
			name:    "limit reached",
			exclude: func(fx *poolFix, a poolAccount) { fx.limitReached(a, 3600) },
			soon:    func(fx *poolFix, a poolAccount) { fx.limitReached(a, 1800) },
			late:    func(fx *poolFix, a poolAccount) { fx.limitReached(a, 7200) },
		},
		{
			name:    "admission cooldown",
			exclude: func(fx *poolFix, a poolAccount) { fx.cooldown(a, time.Hour) },
			soon:    func(fx *poolFix, a poolAccount) { fx.cooldown(a, 30*time.Minute) },
			late:    func(fx *poolFix, a poolAccount) { fx.cooldown(a, 2*time.Hour) },
		},
		{
			name: "weekly reserve hold",
			prep: func(fx *poolFix) { fx.reserve(80) },
			exclude: func(fx *poolFix, a poolAccount) {
				fx.readingRaw(a, poolBuckets(true, false, poolWindow(fiveHourSecs, 10, 100000), poolWindow(weekSecs, 90, 100000)), "1 minute")
			},
			soon: func(fx *poolFix, a poolAccount) {
				fx.readingRaw(a, poolBuckets(true, false, poolWindow(fiveHourSecs, 10, 100000), poolWindow(weekSecs, 90, 50000)), "1 minute")
			},
			late: func(fx *poolFix, a poolAccount) {
				fx.readingRaw(a, poolBuckets(true, false, poolWindow(fiveHourSecs, 10, 100000), poolWindow(weekSecs, 95, 150000)), "1 minute")
			},
		},
	}
	for _, c := range causes {
		t.Run(c.name+"/healthy account wins", func(t *testing.T) {
			fx := newPoolFix(t)
			def := fx.newAccount("def")
			fx.makeDefault(def.alias)
			a, b := fx.newAccount("a"), fx.newAccount("b")
			fx.pool(a.alias, b.alias)
			if c.prep != nil {
				c.prep(fx)
			}
			c.exclude(fx, a)
			// b is healthy with a worse headroom than a would have had: the excluded account must
			// still lose.
			fx.readingRaw(b, poolBuckets(true, false, poolWindow(fiveHourSecs, 60, 100000), poolWindow(weekSecs, 60, 100000)), "1 minute")
			fx.mode("auto")
			assertBound(t, fx.mustCreate(), b.alias, codexselect.ReasonAuto)
		})
		t.Run(c.name+"/earliest release wins", func(t *testing.T) {
			fx := newPoolFix(t)
			def := fx.newAccount("def")
			fx.makeDefault(def.alias)
			a, b := fx.newAccount("a"), fx.newAccount("b")
			fx.pool(a.alias, b.alias)
			if c.prep != nil {
				c.prep(fx)
			}
			// Assign soon/late so that the winner is NOT the lower account key.
			lo, hi := lowerAccount(a, b)
			c.late(fx, lo)
			c.soon(fx, hi)
			fx.mode("auto")
			assertBound(t, fx.mustCreate(), hi.alias, codexselect.ReasonPoolExhausted)
		})
	}

	t.Run("reserve on, no readings: unknown is exhausted, lowest key", func(t *testing.T) {
		fx := newPoolFix(t)
		def := fx.newAccount("def")
		fx.makeDefault(def.alias)
		a, b := fx.newAccount("a"), fx.newAccount("b")
		fx.pool(a.alias, b.alias)
		fx.reserve(80)
		fx.mode("auto")
		lo, _ := lowerAccount(a, b)
		assertBound(t, fx.mustCreate(), lo.alias, codexselect.ReasonPoolExhausted)
	})
	t.Run("reserve off, no readings: stale not exhausted", func(t *testing.T) {
		fx := newPoolFix(t)
		def := fx.newAccount("def")
		fx.makeDefault(def.alias)
		a, b := fx.newAccount("a"), fx.newAccount("b")
		fx.pool(a.alias, b.alias)
		fx.mode("auto")
		lo, _ := lowerAccount(a, b)
		assertBound(t, fx.mustCreate(), lo.alias, codexselect.ReasonPoolStale)
	})
}

// TestCodexSelectPoolExhaustedKeepsClaimFieldsLiveDB: a pool_exhausted run carries the same
// wait/limit and hold fields as a default-bound run: selection changes only the bound alias and
// the reason, never the claim gates.
func TestCodexSelectPoolExhaustedKeepsClaimFieldsLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	def := fx.newAccount("def")
	fx.makeDefault(def.alias)
	a := fx.newAccount("a")
	fx.pool(a.alias)
	fx.limitReached(a, 3600)

	baseline := fx.mustCreate() // mode default
	fx.mode("auto")
	exhausted := fx.mustCreate()
	assertBound(t, exhausted, a.alias, codexselect.ReasonPoolExhausted)

	type claimFields struct {
		Status         string
		WaitOnLimit    bool
		Harness        string
		AuthMode       string
		ReserveHold    pgtype.Text
		RetryNotBefore pgtype.Timestamptz
		Priority       pgtype.Int2
	}
	pick := func(r store.Run) claimFields {
		return claimFields{r.Status, r.WaitOnLimit, r.Harness, r.CodexAuthMode.String, r.ReserveHoldReason, r.RetryNotBefore, r.Priority}
	}
	if pick(baseline) != pick(exhausted) {
		t.Fatalf("pool_exhausted run fields %+v differ from the default-bound run %+v", pick(exhausted), pick(baseline))
	}
	if exhausted.WaitOnLimit {
		t.Fatal("pool_exhausted must keep wait_on_limit=false")
	}
}

// --- usability readers against the real store ------------------------------------------------------

// TestCodexPoolUnusableReadersLiveDB: with a configured pool whose only alias is disabled, the
// implicit harness falls back to Claude, the explicit Codex harness is refused with the pool
// error through every real-store reader, and mode default is unaffected.
func TestCodexPoolUnusableReadersLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	seedAnthropicToken(t, fx.env, fx.userID)
	def := fx.newAccount("def")
	fx.makeDefault(def.alias)
	a := fx.newAccount("a")
	fx.pool(a.alias)
	fx.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, a.alias)
	fx.mode("auto")

	ctx := fx.env.ctx
	got, err := fx.svc.ResolveHarnessForUser(ctx, fx.userID, nil)
	if err != nil || got != HarnessClaude {
		t.Fatalf("implicit = (%q, %v), want claude", got, err)
	}
	if _, err := fx.svc.ResolveHarnessForUser(ctx, fx.userID, h(HarnessCodex)); !errors.Is(err, ErrCodexPoolUnavailable) {
		t.Fatalf("ResolveHarnessForUser explicit codex err = %v, want ErrCodexPoolUnavailable", err)
	}
	if _, err := fx.svc.ResolveHarnessForUserOn(ctx, fx.env.q, fx.userID, h(HarnessCodex)); !errors.Is(err, ErrCodexPoolUnavailable) {
		t.Fatalf("ResolveHarnessForUserOn explicit codex err = %v, want ErrCodexPoolUnavailable", err)
	}
	if err := fx.svc.ScheduleCredentialDisabled(ctx, fx.userID, nil, h(HarnessCodex)); !errors.Is(err, ErrCodexPoolUnavailable) {
		t.Fatalf("ScheduleCredentialDisabled = %v, want ErrCodexPoolUnavailable", err)
	}
	if both, err := fx.svc.BothHarnessesUsable(ctx, fx.userID); err != nil || both {
		t.Fatalf("BothHarnessesUsable = (%t, %v), want false", both, err)
	}

	// Back to mode default: the (enabled, unpooled) default is usable again.
	fx.mode("default")
	if err := fx.svc.ScheduleCredentialDisabled(ctx, fx.userID, nil, h(HarnessCodex)); err != nil {
		t.Fatalf("mode default ScheduleCredentialDisabled = %v, want nil", err)
	}
	if both, err := fx.svc.BothHarnessesUsable(ctx, fx.userID); err != nil || !both {
		t.Fatalf("mode default BothHarnessesUsable = (%t, %v), want true", both, err)
	}
}

// --- derived origins -----------------------------------------------------------------------------

// TestCodexSelectMRReworkUsesPoolLiveDB: a rework of a Codex run created under 'auto' goes
// through selection (reason is not 'default') rather than inheriting the source's alias.
func TestCodexSelectMRReworkUsesPoolLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	fx := &poolFix{t: t, env: env, userID: userID, repoID: repoID, svc: atomicMRReworkSvc(env), iid: 100}
	fx.svc.p.Autoselect = autoselect.Policy{MinHeadroom: 10, HeadroomTiePct: 5, InflightPenalty: 10}
	fx.svc.p.CodexUsagePollInterval = 10 * time.Minute

	def := fx.newAccount("def")
	fx.makeDefault(def.alias)
	a, b := fx.newAccount("a"), fx.newAccount("b")
	fx.pool(a.alias, b.alias)
	fx.reading(a, 80)
	fx.reading(b, 15)
	fx.mode("auto")

	sourceRunID := env.seedCodexRun(t, userID, workerID, repoID)
	bare := &Service{q: env.q, box: env.box}
	if err := bare.FreezeCodexBinding(env.ctx, userID, sourceRunID, def.alias, codexAuthModeSubscription); err != nil {
		t.Fatalf("freeze source: %v", err)
	}

	run, err := fx.svc.CreateAutoMRReworkRun(env.ctx, userID, repoID, "agent/issue-9", 90, sourceRunID, "Rework MR review", "desc", sampleReviewSnapshot())
	if err != nil {
		t.Fatalf("CreateAutoMRReworkRun: %v", err)
	}
	assertBound(t, mustRun(t, env, run.ID), b.alias, codexselect.ReasonAuto)
}

// TestCodexSelectPlanCrossCheckChildUsesPoolLiveDB: the Codex checker child of a Claude lead is
// created through selection under 'auto'.
func TestCodexSelectPlanCrossCheckChildUsesPoolLiveDB(t *testing.T) {
	f, w := admissionCheckerBase(t, "plan")
	fx := &poolFix{t: t, env: f.env, userID: f.userID, svc: f.svc}
	f.svc.p.Autoselect = autoselect.Policy{MinHeadroom: 10, HeadroomTiePct: 5, InflightPenalty: 10}
	f.svc.p.CodexUsagePollInterval = 10 * time.Minute
	def := poolAccount{alias: f.aliasID, account: fx.accountOf(f.aliasID)}
	other := fx.newAccount("other")
	fx.pool(def.alias, other.alias)
	fx.reading(def, 85)
	fx.reading(other, 10)
	fx.mode("auto")

	f2, cc := admissionCheckerSubmit(t, f, w, "plan", 0, nil)
	if !cc.CheckerRunID.Valid {
		t.Fatal("no checker child")
	}
	assertBound(t, mustRun(t, f2.env, uuid.UUID(cc.CheckerRunID.Bytes)), other.alias, codexselect.ReasonAuto)
}

// --- locks and the fence -------------------------------------------------------------------------

// secretWriter runs fn the way every credential writer does: a transaction whose first
// statement takes the user's EXCLUSIVE secret-mutation lock.
func (fx *poolFix) secretWriter(fn func(ctx context.Context, q *store.Queries) error) <-chan error {
	out := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(fx.env.ctx, poolTestTimeout)
		defer cancel()
		out <- func() error {
			tx, err := fx.aux.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			q := store.New(tx)
			if err := q.LockSecretMutation(ctx, fx.userID); err != nil {
				return err
			}
			if err := fn(ctx, q); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
	}()
	return out
}

func (fx *poolFix) unpool(alias uuid.UUID) func(ctx context.Context, q *store.Queries) error {
	return func(ctx context.Context, q *store.Queries) error {
		_, err := q.SetUserSecretAutoEligible(ctx, store.SetUserSecretAutoEligibleParams{AutoEligible: false, ID: alias, UserID: fx.userID, Kind: store.KindCodexAuth})
		return err
	}
}

// disable mirrors handler.PatchSecretEnabled's disable path, including the default hand-off.
func (fx *poolFix) disable(alias, replacement uuid.UUID) func(ctx context.Context, q *store.Queries) error {
	return func(ctx context.Context, q *store.Queries) error {
		cur, err := q.GetUserSecretForUpdate(ctx, store.GetUserSecretForUpdateParams{ID: alias, UserID: fx.userID})
		if err != nil {
			return err
		}
		if cur.IsDefault {
			if _, err := q.CountEnabledSecretSlot(ctx, store.CountEnabledSecretSlotParams{UserID: fx.userID, Kind: store.KindCodexAuth}); err != nil {
				return err
			}
			if _, err := q.ClearCodexDefaults(ctx, fx.userID); err != nil {
				return err
			}
			if replacement != uuid.Nil {
				if _, err := q.SetUserSecretDefault(ctx, store.SetUserSecretDefaultParams{ID: replacement, UserID: fx.userID}); err != nil {
					return err
				}
			}
		}
		_, err = q.SetSecretEnablement(ctx, store.SetSecretEnablementParams{ID: alias, UserID: fx.userID, Enabled: false})
		return err
	}
}

// relink mirrors the reconcile relink: point the alias at another account.
func (fx *poolFix) relink(alias, account uuid.UUID) func(ctx context.Context, q *store.Queries) error {
	return func(ctx context.Context, q *store.Queries) error {
		st, err := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{UserSecretID: alias, UserID: fx.userID})
		if err != nil {
			return err
		}
		n, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
			ProviderAccountID: pgtype.UUID{Bytes: account, Valid: true}, UserSecretID: alias, UserID: fx.userID, MaterialRevision: st.MaterialRevision,
		})
		if err == nil && n != 1 {
			err = fmt.Errorf("relink wrote %d rows", n)
		}
		return err
	}
}

// replace mirrors a credential replace: the material revision bumps and the link drops.
func (fx *poolFix) replace(alias uuid.UUID) func(ctx context.Context, q *store.Queries) error {
	return func(ctx context.Context, q *store.Queries) error {
		_, err := q.BumpCodexMaterialRevision(ctx, store.BumpCodexMaterialRevisionParams{Status: "staging", UserSecretID: alias, UserID: fx.userID})
		return err
	}
}

func (fx *poolFix) awaitWriter(name string, ch <-chan error) {
	fx.t.Helper()
	select {
	case err := <-ch:
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
				fx.t.Fatalf("%s: DEADLOCK (40P01): %v", name, err)
			}
			fx.t.Fatalf("%s: %v", name, err)
		}
	case <-time.After(poolTestTimeout):
		fx.t.Fatalf("%s did not complete within %s", name, poolTestTimeout)
	}
}

// waitBlocked waits until n advisory-lock requests are queued behind a holder.
func (fx *poolFix) waitBlocked(n int) {
	fx.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var c int
		if err := fx.aux.QueryRow(fx.env.ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&c); err != nil {
			fx.t.Fatal(err)
		}
		if c >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	fx.t.Fatalf("fewer than %d writers queued behind the create's shared lock", n)
}

// twoPooled is the common fixture: a default plus pooled accounts a (better) and b.
func (fx *poolFix) twoPooled() (a, b poolAccount) {
	def := fx.newAccount("def")
	fx.makeDefault(def.alias)
	a, b = fx.newAccount("a"), fx.newAccount("b")
	fx.pool(a.alias, b.alias)
	fx.reading(a, 10)
	fx.reading(b, 50)
	fx.mode("auto")
	return a, b
}

// TestCodexSelectConcurrentChangeBeforeLockLiveDB: a writer that commits after resolve's
// usability read but before the create takes its shared lock is honoured by the fresh selection:
// the changed alias is never bound.
func TestCodexSelectConcurrentChangeBeforeLockLiveDB(t *testing.T) {
	cases := []struct {
		name  string
		write func(fx *poolFix, a, b poolAccount) func(ctx context.Context, q *store.Queries) error
	}{
		{"unpooled", func(fx *poolFix, a, b poolAccount) func(context.Context, *store.Queries) error {
			return fx.unpool(a.alias)
		}},
		{"disabled", func(fx *poolFix, a, b poolAccount) func(context.Context, *store.Queries) error {
			return fx.disable(a.alias, uuid.Nil)
		}},
		{"relinked", func(fx *poolFix, a, b poolAccount) func(context.Context, *store.Queries) error {
			return fx.relink(a.alias, b.account)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newPoolFix(t)
			a, b := fx.twoPooled()
			fx.svc.codexSelectHook = func(context.Context) {
				fx.awaitWriter(tc.name, fx.secretWriter(tc.write(fx, a, b)))
			}
			got := fx.mustCreate()
			bound := uuid.UUID(got.CodexSecretID.Bytes)
			if tc.name == "relinked" {
				// a now lives on b's account: whichever alias is bound, the run must be frozen to
				// b's account (a's old account is gone), and the reason is freshly computed.
				if got.CodexAccountKey.String != accountKeyOf(fx, b) {
					t.Fatalf("frozen account key %q, want b's %q", got.CodexAccountKey.String, accountKeyOf(fx, b))
				}
				if bound != a.alias && bound != b.alias {
					t.Fatalf("bound %s is neither a nor b", bound)
				}
				return
			}
			assertBound(t, got, b.alias, codexselect.ReasonAuto)
		})
	}

	t.Run("pool emptied: refused, no run", func(t *testing.T) {
		fx := newPoolFix(t)
		def := fx.newAccount("def")
		fx.makeDefault(def.alias)
		a := fx.newAccount("a")
		fx.pool(a.alias)
		fx.reading(a, 10)
		fx.mode("auto")
		fx.svc.codexSelectHook = func(context.Context) { fx.awaitWriter("unpool", fx.secretWriter(fx.unpool(a.alias))) }
		if _, err := fx.create(); !errors.Is(err, ErrCodexPoolUnavailable) {
			t.Fatalf("err = %v, want ErrCodexPoolUnavailable", err)
		}
		if fx.runs() != 0 {
			t.Fatalf("runs = %d, want 0", fx.runs())
		}
	})
}

// TestCodexSelectWritersBlockUntilCommitLiveDB: with the create held open after it took its
// shared lock, a disable, an unpool and a relink on other connections each wait for the commit,
// none deadlocks, and the run's frozen account is the account that was scored.
func TestCodexSelectWritersBlockUntilCommitLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	a, b := fx.twoPooled()
	var writers []<-chan error
	names := []string{"disable", "unpool", "relink"}
	fx.svc.codexSelectLockedHook = func(context.Context) {
		writers = append(writers,
			fx.secretWriter(fx.disable(a.alias, uuid.Nil)),
			fx.secretWriter(fx.unpool(b.alias)),
			fx.secretWriter(fx.relink(b.alias, a.account)),
		)
		fx.waitBlocked(3)
		for i, w := range writers {
			select {
			case err := <-w:
				t.Errorf("writer %s finished (%v) while the create still held its shared lock", names[i], err)
			default:
			}
		}
	}
	got := fx.mustCreate()
	assertBound(t, got, a.alias, codexselect.ReasonAuto)
	if got.CodexAccountKey.String != accountKeyOf(fx, a) {
		t.Fatalf("frozen account key %q, want the scored account's %q", got.CodexAccountKey.String, accountKeyOf(fx, a))
	}
	for i, w := range writers {
		fx.awaitWriter(names[i], w)
	}
}

// TestCodexSelectFenceAndReplaceLiveDB: a credential replace racing a create's fence waits for
// the commit; the committed run keeps the identity it was scored on.
func TestCodexSelectFenceAndReplaceLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	a, _ := fx.twoPooled()
	var replace <-chan error
	fx.svc.codexSelectLockedHook = func(context.Context) {
		replace = fx.secretWriter(fx.replace(a.alias))
		fx.waitBlocked(1)
	}
	got := fx.mustCreate()
	assertBound(t, got, a.alias, codexselect.ReasonAuto)
	if got.CodexAccountKey.String != accountKeyOf(fx, a) {
		t.Fatalf("frozen account key %q, want %q", got.CodexAccountKey.String, accountKeyOf(fx, a))
	}
	fx.awaitWriter("replace", replace)
	st, err := fx.env.q.GetCodexCredentialState(fx.env.ctx, store.GetCodexCredentialStateParams{UserSecretID: a.alias, UserID: fx.userID})
	if err != nil || st.ProviderAccountID.Valid {
		t.Fatalf("after the replace commits the alias must be unlinked: %+v %v", st, err)
	}
}

// TestCodexSelectFenceReselectionLiveDB: when the fence rejects the first pick (forced by an
// out-of-protocol unpool, since the shared lock otherwise prevents it), the pool is re-read and
// the next alias is bound while two alias share locks are held; a disable of the default with a
// replacement queued on the secret-mutation lock completes without a deadlock.
func TestCodexSelectFenceReselectionLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	def := fx.newAccount("def")
	repl := fx.newAccount("repl")
	fx.makeDefault(def.alias)
	a, b := fx.newAccount("a"), fx.newAccount("b")
	fx.pool(a.alias, b.alias)
	fx.reading(a, 10)
	fx.reading(b, 50)
	fx.mode("auto")

	var writer <-chan error
	fx.svc.codexSelectBeforeFenceHook = func(_ context.Context, picked uuid.UUID) {
		// Out of protocol on purpose: no advisory lock, a plain autocommit write.
		fx.env.exec(`UPDATE user_secrets SET auto_eligible = false WHERE id = $1`, picked)
		writer = fx.secretWriter(fx.disable(def.alias, repl.alias))
		fx.waitBlocked(1)
	}
	got := fx.mustCreate()
	assertBound(t, got, b.alias, codexselect.ReasonAuto)
	fx.awaitWriter("disable default with replacement", writer)
	var isDefault bool
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT is_default FROM user_secrets WHERE id = $1`, repl.alias).Scan(&isDefault); err != nil || !isDefault {
		t.Fatalf("replacement default = %t, %v", isDefault, err)
	}
}

// TestCodexSelectFenceExhaustionRefusesLiveDB: when every candidate keeps failing the fence the
// create is refused with the pool error and leaves no run.
func TestCodexSelectFenceExhaustionRefusesLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	a, b := fx.twoPooled()
	fx.svc.codexSelectBeforeFenceHook = func(_ context.Context, picked uuid.UUID) {
		fx.env.exec(`UPDATE user_secrets SET auto_eligible = false WHERE id = ANY($1)`, []uuid.UUID{a.alias, b.alias})
	}
	if _, err := fx.create(); !errors.Is(err, ErrCodexPoolUnavailable) {
		t.Fatalf("err = %v, want ErrCodexPoolUnavailable", err)
	}
	if fx.runs() != 0 {
		t.Fatalf("runs = %d, want 0 (the whole create rolls back)", fx.runs())
	}
}

// TestCodexSelectFrozenAccountMismatchRefusesLiveDB: the post-freeze comparison refuses a run
// whose frozen account is not the scored one, and rolls the run back.
func TestCodexSelectFrozenAccountMismatchRefusesLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	a, b := fx.twoPooled() // a scores better and is picked
	_ = a
	fx.svc.codexFreezeFn = func(ctx context.Context, q codexFreezeStore, userID, runID, _ uuid.UUID, authMode string) error {
		// Freeze the OTHER account's alias than the one selection picked.
		return fx.svc.freezeCodexBinding(ctx, q, userID, runID, b.alias, authMode)
	}
	if _, err := fx.create(); !errors.Is(err, ErrCodexPoolUnavailable) {
		t.Fatalf("err = %v, want ErrCodexPoolUnavailable", err)
	}
	if fx.runs() != 0 {
		t.Fatalf("runs = %d, want 0", fx.runs())
	}
}

// TestCodexSelectLockScopeLiveDB: only the pool path takes the shared secret-mutation lock. With
// an exclusive holder open, a mode-default create (and a pool_empty create) proceeds; a pool
// create waits for the holder's commit.
func TestCodexSelectLockScopeLiveDB(t *testing.T) {
	fx := newPoolFix(t)
	a, _ := fx.twoPooled()
	fx.mode("default")

	tx, err := fx.aux.Begin(fx.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.LockSecretMutation(fx.env.ctx, tx, fx.userID); err != nil {
		t.Fatal(err)
	}

	// mode default: no new lock.
	if run := fx.mustCreate(); run.CodexSelectReason.String != string(codexselect.ReasonDefault) {
		t.Fatalf("reason = %q", run.CodexSelectReason.String)
	}
	// mode auto with an empty pool: no new lock either.
	fx.env.exec(`UPDATE user_secrets SET auto_eligible = false WHERE user_id = $1`, fx.userID)
	fx.mode("auto")
	if run := fx.mustCreate(); run.CodexSelectReason.String != string(codexselect.ReasonPoolEmpty) {
		t.Fatalf("reason = %q", run.CodexSelectReason.String)
	}

	// A configured pool takes the shared lock and waits.
	fx.pool(a.alias)
	done := make(chan error, 1)
	go func() {
		_, err := fx.create()
		done <- err
	}()
	fx.waitBlocked(1)
	select {
	case err := <-done:
		t.Fatalf("a pool create finished (%v) while an exclusive secret-mutation writer was open", err)
	default:
	}
	if err := tx.Rollback(fx.env.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pool create after release: %v", err)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("pool create did not finish after the writer released")
	}
}
