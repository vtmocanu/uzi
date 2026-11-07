package workersvc

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_catalog_default_livedb_test.go proves the Codex catalog default seam (issue #3251) against
// a real Postgres: the report's ownership, fence and context checks, rotation in both orders, the
// 32-row / 30-day retention under concurrency, the claim field, and the read.

type catalogEnv struct {
	codexTestEnv
	svc      *Service
	wkr      store.Worker
	userID   uuid.UUID
	workerID uuid.UUID
	runID    uuid.UUID
	aliasID  uuid.UUID
	mode     string
}

// setupCatalog seeds a user with a default Codex alias (a static API key, or a linked
// subscription) and a running Codex issue run, claim generation 1, bound to that alias.
func setupCatalog(t *testing.T, subscription bool) catalogEnv {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	mode := codexAuthModeAPIKey
	var aliasID uuid.UUID
	if subscription {
		mode = codexAuthModeSubscription
		aliasID = env.seedLinkedSubscription(t, userID, "codex-cat-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
	} else {
		aliasID = env.seedStaticAPIKey(t, userID, "key-"+uuid.NewString(), codexToken("key"))
	}
	env.exec(`UPDATE user_secrets SET is_default = true WHERE id = $1`, aliasID)
	runID := env.seedFoldRun(t, userID, workerID, repoID, 1, "sess-cat")
	env.exec(`UPDATE runs SET harness = 'codex' WHERE id = $1`, runID)
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	if err := svc.FreezeCodexBinding(env.ctx, userID, runID, aliasID, mode); err != nil {
		t.Fatalf("FreezeCodexBinding: %v", err)
	}
	return catalogEnv{
		codexTestEnv: env, svc: svc, wkr: store.Worker{ID: workerID, UserID: userID, Status: "online"},
		userID: userID, workerID: workerID, runID: runID, aliasID: aliasID, mode: mode,
	}
}

// claimContext is the context the run's claim ships.
func (e catalogEnv) claimContext(t *testing.T) string {
	t.Helper()
	got := claimCodexCatalogContext(mustRun(t, e.codexTestEnv, e.runID))
	if got == nil {
		t.Fatal("the run's claim carries no catalog context")
	}
	return *got
}

func (e catalogEnv) request(ctxID, model, version, effort string) CodexCatalogDefaultRequest {
	gen := int64(1)
	return CodexCatalogDefaultRequest{ClaimGeneration: &gen, CatalogContext: ctxID, Model: model, RuntimeVersion: version, DefaultEffort: effort}
}

func (e catalogEnv) report(ctxID, model, version, effort string) error {
	return e.svc.RecordCodexCatalogDefault(e.ctx, e.wkr, e.runID, e.request(ctxID, model, version, effort))
}

func (e catalogEnv) rows(t *testing.T) int {
	t.Helper()
	return e.userRows(t, e.userID)
}

func (e catalogEnv) userRows(t *testing.T, user uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM codex_catalog_defaults WHERE user_id = $1`, user).Scan(&n); err != nil {
		t.Fatalf("count catalog rows: %v", err)
	}
	return n
}

func (e catalogEnv) bump(t *testing.T) {
	t.Helper()
	status := "static"
	if e.mode == codexAuthModeSubscription {
		status = "staging"
	}
	if n, err := e.q.BumpCodexMaterialRevision(e.ctx, store.BumpCodexMaterialRevisionParams{Status: status, UserSecretID: e.aliasID, UserID: e.userID}); err != nil || n != 1 {
		t.Fatalf("BumpCodexMaterialRevision: n=%d err=%v", n, err)
	}
}

func TestRecordCodexCatalogDefaultStoresAndReadsLiveDB(t *testing.T) {
	for _, sub := range []bool{false, true} {
		name := "api_key"
		if sub {
			name = "subscription"
		}
		t.Run(name, func(t *testing.T) {
			e := setupCatalog(t, sub)
			ctxID := e.claimContext(t)
			if got, err := e.svc.CodexCatalogDefault(e.ctx, e.userID, "gpt-6.1-sol"); err != nil || got.State != CatalogStateUnknown {
				t.Fatalf("before any report: %+v, %v; want unknown", got, err)
			}
			if err := e.report(ctxID, "gpt-6.1-sol", "0.162.1", "low"); err != nil {
				t.Fatalf("report: %v", err)
			}
			got, err := e.svc.CodexCatalogDefault(e.ctx, e.userID, "gpt-6.1-sol")
			if err != nil {
				t.Fatal(err)
			}
			if got.State != CatalogStateKnown || got.Effort != "low" || got.Model != "gpt-6.1-sol" || got.RuntimeVersion != "0.162.1" || got.ObservedAt.IsZero() {
				t.Fatalf("known default = %+v", got)
			}
			// An explicit model argument returns that model's row only.
			if got, _ := e.svc.CodexCatalogDefault(e.ctx, e.userID, "gpt-6-sol"); got.State != CatalogStateUnknown {
				t.Fatalf("another model: %+v, want unknown", got)
			}
			// A second version that disagrees: the higher semver wins, flagged as varies.
			if err := e.report(ctxID, "gpt-6.1-sol", "0.163.0", "medium"); err != nil {
				t.Fatal(err)
			}
			got, _ = e.svc.CodexCatalogDefault(e.ctx, e.userID, "gpt-6.1-sol")
			if got.State != CatalogStateVaries || got.Effort != "medium" || got.RuntimeVersion != "0.163.0" {
				t.Fatalf("mixed versions = %+v", got)
			}
			// A repeat report for the same key updates in place.
			if err := e.report(ctxID, "gpt-6.1-sol", "0.163.0", "none"); err != nil {
				t.Fatal(err)
			}
			got, _ = e.svc.CodexCatalogDefault(e.ctx, e.userID, "gpt-6.1-sol")
			if got.Effort != "none" || got.State != CatalogStateVaries {
				t.Fatalf("after update = %+v", got)
			}
			if n := e.rows(t); n != 2 {
				t.Fatalf("rows = %d, want 2", n)
			}
		})
	}
}

func TestRecordCodexCatalogDefaultRefusalsLiveDB(t *testing.T) {
	e := setupCatalog(t, false)
	ctxID := e.claimContext(t)
	good := e.request(ctxID, "gpt-6.1-sol", "0.164.0", "low")

	t.Run("invalid", func(t *testing.T) {
		bad := good
		bad.DefaultEffort = "persistent"
		if err := e.svc.RecordCodexCatalogDefault(e.ctx, e.wkr, e.runID, bad); !errors.Is(err, ErrCodexCatalogInvalid) {
			t.Fatalf("persistent effort: %v", err)
		}
		bad = good
		bad.ClaimGeneration = nil
		if err := e.svc.RecordCodexCatalogDefault(e.ctx, e.wkr, e.runID, bad); !errors.Is(err, ErrMissingClaimGeneration) {
			t.Fatalf("no generation: %v", err)
		}
	})
	t.Run("not owned", func(t *testing.T) {
		other := e.wkr
		other.ID = uuid.New()
		if err := e.svc.RecordCodexCatalogDefault(e.ctx, other, e.runID, good); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("foreign worker: %v", err)
		}
		if err := e.svc.RecordCodexCatalogDefault(e.ctx, e.wkr, uuid.New(), good); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("unknown run: %v", err)
		}
	})
	t.Run("claude run", func(t *testing.T) {
		var repoID uuid.UUID
		if err := e.pool.QueryRow(e.ctx, `SELECT repo_id FROM runs WHERE id = $1`, e.runID).Scan(&repoID); err != nil {
			t.Fatal(err)
		}
		claudeRun := uuid.New()
		e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation)
		        VALUES ($1, $2, $3, 'issue', 202, 't', 'd', 'running', $4, 1)`, claudeRun, e.userID, repoID, e.workerID)
		if err := e.svc.RecordCodexCatalogDefault(e.ctx, e.wkr, claudeRun, good); !errors.Is(err, ErrCodexCatalogInvalid) {
			t.Fatalf("claude run: %v", err)
		}
	})
	t.Run("foreign user run", func(t *testing.T) {
		wrong := e.wkr
		wrong.UserID = uuid.New()
		if err := e.svc.RecordCodexCatalogDefault(e.ctx, wrong, e.runID, good); !errors.Is(err, ErrCodexCatalogInvalid) {
			t.Fatalf("worker of another user: %v", err)
		}
	})
	t.Run("stale generation", func(t *testing.T) {
		stale := good
		g := int64(2)
		stale.ClaimGeneration = &g
		if err := e.svc.RecordCodexCatalogDefault(e.ctx, e.wkr, e.runID, stale); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("generation 2 on a generation 1 claim: %v", err)
		}
	})
	t.Run("stale context", func(t *testing.T) {
		wrong := good
		wrong.CatalogContext = codexCatalogContext(e.userID, uuid.New(), e.mode, 1, "")
		if err := e.svc.RecordCodexCatalogDefault(e.ctx, e.wkr, e.runID, wrong); !errors.Is(err, ErrStaleCatalogContext) {
			t.Fatalf("echo != frozen: %v", err)
		}
	})
	if n := e.rows(t); n != 0 {
		t.Fatalf("refused reports stored %d rows", n)
	}
	t.Run("released claim", func(t *testing.T) {
		e.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, e.runID)
		if err := e.svc.RecordCodexCatalogDefault(e.ctx, e.wkr, e.runID, good); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("released claim: %v", err)
		}
		if n := e.rows(t); n != 0 {
			t.Fatalf("released claim stored %d rows", n)
		}
	})
}

// A release or reclaim committing after the early fence read discards the write (the locked
// recheck is the last statement before commit).
func TestRecordCodexCatalogDefaultReleaseAfterEarlyFenceLiveDB(t *testing.T) {
	e := setupCatalog(t, false)
	ctxID := e.claimContext(t)
	e.svc.codexCatalogHooks = &codexCatalogTestHooks{afterUpsert: func(codexCatalogRow) {
		e.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, e.runID)
	}}
	if err := e.report(ctxID, "gpt-6.1-sol", "0.164.0", "low"); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("release between upsert and commit: %v", err)
	}
	if n := e.rows(t); n != 0 {
		t.Fatalf("rows = %d, want 0 after a fenced-out report", n)
	}
}

// The report transaction holds the per-user secret mutation lock in shared mode: while it is open
// (inside the post-upsert hook) an exclusive LockSecretMutation from another connection cannot be
// granted, and the report still commits afterwards.
func TestCodexCatalogReportHoldsSecretLockSharedLiveDB(t *testing.T) {
	e := setupCatalog(t, false)
	ctxID := e.claimContext(t)
	other := e.dedicatedPool(t, 2)
	var lockErr error
	e.svc.codexCatalogHooks = &codexCatalogTestHooks{afterUpsert: func(codexCatalogRow) {
		tx, err := other.Begin(e.ctx)
		if err != nil {
			lockErr = err
			return
		}
		defer func() { _ = tx.Rollback(e.ctx) }()
		if _, err := tx.Exec(e.ctx, `SET LOCAL lock_timeout = '200ms'`); err != nil {
			lockErr = err
			return
		}
		lockErr = store.LockSecretMutation(e.ctx, tx, e.userID)
	}}
	if err := e.report(ctxID, "gpt-6.1-sol", "0.164.0", "low"); err != nil {
		t.Fatalf("report: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(lockErr, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("exclusive secret lock while the report is open: %v, want SQLSTATE 55P03", lockErr)
	}
	if n := e.rows(t); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// Rotation, order (a): the claim was assembled at revision N, the material is replaced, then the
// report arrives. It must be refused and store nothing.
func TestCodexCatalogRotationBeforeReportLiveDB(t *testing.T) {
	for _, sub := range []bool{false, true} {
		name := "api_key"
		if sub {
			name = "subscription"
		}
		t.Run(name, func(t *testing.T) {
			e := setupCatalog(t, sub)
			ctxID := e.claimContext(t)
			// BumpCodexMaterialRevision drops a subscription's account link, which leaves no
			// current context to derive. Put the link back after the bump so the current
			// context exists and the frozen-vs-echo half is the only half that can refuse.
			var link pgtype.UUID
			if err := e.pool.QueryRow(e.ctx, `SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id = $1`, e.aliasID).Scan(&link); err != nil {
				t.Fatal(err)
			}
			e.bump(t)
			if err := e.report(ctxID, "gpt-6.1-sol", "0.164.0", "low"); !errors.Is(err, ErrStaleCatalogContext) {
				t.Fatalf("report after rotation: %v, want ErrStaleCatalogContext", err)
			}
			if n := e.rows(t); n != 0 {
				t.Fatalf("rows = %d, want 0", n)
			}
			if sub {
				e.exec(`UPDATE codex_credential_state SET provider_account_id = $1 WHERE user_secret_id = $2`, link, e.aliasID)
			}
			// The context the alias yields now is derivable and matches the live alias, so the
			// current-context half passes; only the frozen half (echo != the run's frozen binding)
			// can refuse a report that echoes it.
			current, ok, err := currentCodexCatalogContext(e.ctx, e.q, e.userID, e.aliasID, e.mode)
			if err != nil || !ok || current == ctxID {
				t.Fatalf("post-rotation context = %q ok=%v err=%v; want a derivable context different from %q", current, ok, err, ctxID)
			}
			if err := e.report(current, "gpt-6.1-sol", "0.164.0", "low"); !errors.Is(err, ErrStaleCatalogContext) {
				t.Fatalf("report with the post-rotation context: %v, want ErrStaleCatalogContext (frozen mismatch)", err)
			}
			if n := e.rows(t); n != 0 {
				t.Fatalf("rows = %d, want 0 after the post-rotation report", n)
			}
		})
	}
}

// Rotation, order (b): the report was stored at revision N, then the material is replaced. The
// predicted context moves, so the label reads unknown; the stored row is untouched.
func TestCodexCatalogRotationAfterReportLiveDB(t *testing.T) {
	for _, sub := range []bool{false, true} {
		name := "api_key"
		if sub {
			name = "subscription"
		}
		t.Run(name, func(t *testing.T) {
			e := setupCatalog(t, sub)
			ctxID := e.claimContext(t)
			if err := e.report(ctxID, "gpt-6.1-sol", "0.164.0", "low"); err != nil {
				t.Fatal(err)
			}
			if got, _ := e.svc.CodexCatalogDefault(e.ctx, e.userID, "gpt-6.1-sol"); got.State != CatalogStateKnown {
				t.Fatalf("before rotation: %+v", got)
			}
			e.bump(t)
			got, err := e.svc.CodexCatalogDefault(e.ctx, e.userID, "gpt-6.1-sol")
			if err != nil || got.State != CatalogStateUnknown {
				t.Fatalf("after rotation: %+v, %v; want unknown", got, err)
			}
			var stored string
			if err := e.pool.QueryRow(e.ctx, `SELECT context_id FROM codex_catalog_defaults WHERE user_id = $1`, e.userID).Scan(&stored); err != nil || stored != ctxID {
				t.Fatalf("stored context_id = %q (%v), want it unchanged %q", stored, err, ctxID)
			}
		})
	}
}

// A subscription re-linked to another account at the same material revision changes the account
// key: a later report is stale, and a stored one no longer matches the prediction. A token refresh
// (generation bump only) leaves the label known.
func TestCodexCatalogSubscriptionRelinkAndRefreshLiveDB(t *testing.T) {
	e := setupCatalog(t, true)
	ctxID := e.claimContext(t)
	if err := e.report(ctxID, "gpt-6.1-sol", "0.164.0", "low"); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE codex_provider_account SET generation = generation + 1
	        WHERE id = (SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id = $1)`, e.aliasID)
	if got, _ := e.svc.CodexCatalogDefault(e.ctx, e.userID, "gpt-6.1-sol"); got.State != CatalogStateKnown {
		t.Fatalf("after a refresh-only generation bump: %+v, want known", got)
	}
	if err := e.report(ctxID, "gpt-6-sol", "0.164.0", "medium"); err != nil {
		t.Fatalf("report after a refresh-only bump: %v", err)
	}
	e.exec(`UPDATE codex_provider_account SET workspace_account_id = 'relinked-' || gen_random_uuid()::text
	        WHERE id = (SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id = $1)`, e.aliasID)
	if got, _ := e.svc.CodexCatalogDefault(e.ctx, e.userID, "gpt-6.1-sol"); got.State != CatalogStateUnknown {
		t.Fatalf("after an account change: %+v, want unknown", got)
	}
	if err := e.report(ctxID, "gpt-6-luna", "0.164.0", "low"); !errors.Is(err, ErrStaleCatalogContext) {
		t.Fatalf("report after an account change: %v, want ErrStaleCatalogContext", err)
	}
}

func TestCodexCatalogReadNoUsableCredentialAndPoolModeLiveDB(t *testing.T) {
	e := setupCatalog(t, false)
	ctxID := e.claimContext(t)
	if err := e.report(ctxID, "gpt-6.1-sol", "0.164.0", "low"); err != nil {
		t.Fatal(err)
	}
	// Pool mode: the alias is picked at creation, so nothing can be predicted.
	e.exec(`UPDATE users SET codex_account_mode = 'auto' WHERE id = $1`, e.userID)
	// 'auto' with no pooled alias reads as today's default path, so the value is still known;
	// a structurally eligible pool reads unknown.
	if got, _ := e.svc.CodexCatalogDefault(e.ctx, e.userID, "gpt-6.1-sol"); got.State != CatalogStateKnown {
		t.Fatalf("auto mode with an empty pool: %+v, want known (the default alias is what binds)", got)
	}
	pooled := e.seedLinkedSubscription(t, e.userID, "pooled-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
	e.exec(`UPDATE user_secrets SET auto_eligible = true WHERE id = $1`, pooled)
	if got, err := e.svc.CodexCatalogDefault(e.ctx, e.userID, "gpt-6.1-sol"); err != nil || got.State != CatalogStateUnknown {
		t.Fatalf("auto mode with an eligible pool: %+v, %v; want unknown", got, err)
	}
	// No usable credential at all.
	e.exec(`UPDATE users SET codex_account_mode = 'default' WHERE id = $1`, e.userID)
	e.exec(`DELETE FROM user_secrets WHERE user_id = $1`, e.userID)
	if got, err := e.svc.CodexCatalogDefault(e.ctx, e.userID, "gpt-6.1-sol"); err != nil || got.State != CatalogStateUnknown {
		t.Fatalf("no credential: %+v, %v; want unknown", got, err)
	}
}

// The claim carries the context for a Codex lead-lane run with a complete binding, and nothing
// else (the Claude, cross-check, review and judge scopes are pinned by TestClaimCodexCatalogContextScope).
func TestAssembleClaimCodexCatalogContextLiveDB(t *testing.T) {
	for _, sub := range []bool{false, true} {
		name := "api_key"
		if sub {
			name = "subscription"
		}
		t.Run(name, func(t *testing.T) {
			e := setupCatalog(t, sub)
			resealBotPAT(t, e.codexTestEnv, e.userID)
			worker := store.Worker{ID: e.workerID, UserID: e.userID, ProtocolCapabilities: []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1}}
			payload, err := e.svc.assembleClaim(e.ctx, worker, mustRun(t, e.codexTestEnv, e.runID))
			if err != nil {
				t.Fatal(err)
			}
			got := payload.Config.CodexCatalogContext
			if got == nil {
				t.Fatal("claim has no codex_catalog_context")
			}
			// Rebuild from the database, independently of the frozen run columns.
			var secret uuid.UUID
			var rev int64
			var accountKey string
			if err := e.pool.QueryRow(e.ctx, `SELECT codex_secret_id, codex_material_revision, COALESCE(codex_account_key, '') FROM runs WHERE id = $1`, e.runID).Scan(&secret, &rev, &accountKey); err != nil {
				t.Fatal(err)
			}
			if want := codexCatalogContext(e.userID, secret, e.mode, rev, accountKey); *got != want {
				t.Fatalf("claim context = %s, want %s", *got, want)
			}
			if sub && accountKey == "" {
				t.Fatal("subscription fixture froze no account key")
			}
		})
	}
	t.Run("judge", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, workerID, repoID := env.seedCodexInfra(t)
		aliasID := env.seedLinkedSubscription(t, userID, "codex-judge", codexToken("access"), codexToken("refresh"))
		target := env.seedCodexRun(t, userID, workerID, repoID)
		judge := env.seedCodexJudgeRun(t, userID, workerID, target)
		svc := New(env.q, env.box, testParams())
		svc.SetVault(env.vlt)
		if err := svc.FreezeCodexBinding(env.ctx, userID, judge, aliasID, codexAuthModeSubscription); err != nil {
			t.Fatal(err)
		}
		payload, err := svc.assembleClaim(env.ctx, store.Worker{ID: workerID, UserID: userID}, mustRun(t, env, judge))
		if err != nil {
			t.Fatal(err)
		}
		if payload.Config.CodexCatalogContext != nil {
			t.Fatalf("judge claim carries a catalog context: %s", *payload.Config.CodexCatalogContext)
		}
	})
}

func (e catalogEnv) dedicatedPool(t *testing.T, conns int32) *pgxpool.Pool {
	t.Helper()
	pool, err := store.OpenPool(e.ctx, os.Getenv("UZI_TEST_DATABASE_URL"), func(cfg *pgxpool.Config) { cfg.MaxConns = conns })
	if err != nil {
		t.Fatalf("open dedicated pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func (e catalogEnv) writerService(t *testing.T, pool *pgxpool.Pool) *Service {
	t.Helper()
	svc := New(e.q, e.box, testParams())
	svc.SetTxBeginner(pool)
	return svc
}

func catalogModel(i int) string { return fmt.Sprintf("model-%03d", i) }

func catalogRowFor(user uuid.UUID, model string) codexCatalogRow {
	return codexCatalogRow{UserID: user, ContextID: codexCatalogContext(user, uuid.Nil, codexAuthModeAPIKey, 1, ""), Model: model, RuntimeVersion: "0.164.0", DefaultEffort: "low"}
}

func (e catalogEnv) models(t *testing.T, user uuid.UUID) []string {
	t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT model FROM codex_catalog_defaults WHERE user_id = $1 ORDER BY model`, user)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// 64 concurrent reports for distinct models of one user, each on its own connection, leave
// exactly 32 rows.
func TestCodexCatalogRetentionConcurrentWritersLiveDB(t *testing.T) {
	e := setupCatalog(t, false)
	const n = 64
	svc := e.writerService(t, e.dedicatedPool(t, n))
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- svc.recordCodexCatalogRowTx(e.ctx, catalogRowFor(e.userID, catalogModel(i)))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("writer: %v", err)
		}
	}
	if got := e.rows(t); got != 32 {
		t.Fatalf("rows after %d concurrent writers = %d, want 32", n, got)
	}
}

func (e catalogEnv) seedRow(t *testing.T, user uuid.UUID, model string, age time.Duration) {
	t.Helper()
	e.exec(`INSERT INTO codex_catalog_defaults (user_id, context_id, model, runtime_version, default_effort, reported_at)
	        VALUES ($1, $2, $3, '0.164.0', 'low', now() - make_interval(secs => $4))`,
		user, codexCatalogContext(user, uuid.Nil, codexAuthModeAPIKey, 1, ""), model, age.Seconds())
}

func TestCodexCatalogRetentionAgeAndIsolationLiveDB(t *testing.T) {
	e := setupCatalog(t, false)
	svc := e.writerService(t, e.dedicatedPool(t, 4))

	// Another user's rows, including more than 32 and expired ones, are untouched by this user's write.
	other := e.seedUser(t)
	for i := 0; i < 40; i++ {
		age := time.Duration(i) * time.Hour
		if i%10 == 0 {
			age = 45 * 24 * time.Hour
		}
		e.seedRow(t, other, catalogModel(i), age)
	}
	otherBefore := e.models(t, other)

	e.seedRow(t, e.userID, "old-31d", 31*24*time.Hour)
	e.seedRow(t, e.userID, "fresh-29d", 29*24*time.Hour)
	if err := svc.recordCodexCatalogRowTx(e.ctx, catalogRowFor(e.userID, "new")); err != nil {
		t.Fatal(err)
	}
	if got := e.models(t, e.userID); !slices.Equal(got, []string{"fresh-29d", "new"}) {
		t.Fatalf("models after the write = %v, want the 31-day-old row pruned", got)
	}
	if after := e.models(t, other); !slices.Equal(after, otherBefore) {
		t.Fatalf("another user's rows changed: %d before, %d after", len(otherBefore), len(after))
	}
}

// A transaction that began before 32 others committed keeps its own row: the upsert stamps
// statement_timestamp(), not the transaction start, so the writer is never the oldest.
func TestCodexCatalogRetentionLateWriterKeepsItsRowLiveDB(t *testing.T) {
	e := setupCatalog(t, false)
	pool := e.dedicatedPool(t, 8)
	svc := e.writerService(t, pool)
	tx, err := pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if err := setUsageWriteIsolation(e.ctx, tx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		if err := svc.recordCodexCatalogRowTx(e.ctx, catalogRowFor(e.userID, catalogModel(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.writeCodexCatalogRow(e.ctx, store.New(tx), catalogRowFor(e.userID, "late"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := e.models(t, e.userID)
	if len(got) != 32 || !slices.Contains(got, "late") {
		t.Fatalf("models = %v (%d), want 32 rows including the late writer's", got, len(got))
	}
	if slices.Contains(got, catalogModel(0)) {
		t.Fatalf("the oldest committed row survived: %v", got)
	}
}

func TestCodexCatalogEffortSetMatchesCheckLiveDB(t *testing.T) {
	e := setupCatalog(t, false)
	var def string
	if err := e.pool.QueryRow(e.ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'codex_catalog_defaults_effort_check'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	var inList []string
	for _, m := range regexp.MustCompile(`'([a-z]+)'`).FindAllStringSubmatch(def, -1) {
		inList = append(inList, m[1])
	}
	want := slices.Clone(codexCatalogEfforts)
	sort.Strings(want)
	sort.Strings(inList)
	if !slices.Equal(want, inList) {
		t.Fatalf("effort set %v != CHECK IN-list %v (%s)", want, inList, def)
	}
}

// retentionMode selects the barrier protocol of runCatalogRetentionBarriers.
type retentionMode int

const (
	// retentionLocked proves the per-user lock: one writer reaches the post-upsert hook, the other
	// N-1 are provably waiting on the advisory lock, and writers then pass the hook one at a time.
	retentionLocked retentionMode = iota
	// retentionLockFree is the mutation-proof protocol: every writer reaches the post-upsert hook
	// before any prune runs, so it can only complete when the lock is not serializing the writers.
	// It is run only by the mutant test (build tag catalogmutant) against a scratch export from
	// which the LockCodexCatalogDefaults call has been removed.
	retentionLockFree
)

const catalogBarrierDeadline = 90 * time.Second

func awaitBarrier(t *testing.T, name string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(catalogBarrierDeadline):
		t.Fatalf("barrier %q not reached within %s", name, catalogBarrierDeadline)
	}
}

// runCatalogRetentionBarriers drives n writers (one connection each) through the hook protocol of
// mode and returns the final row count, the largest number of writers observed between the
// post-upsert hook and the end of their transaction, and the models in the order they reached the
// post-upsert hook.
func runCatalogRetentionBarriers(t *testing.T, e catalogEnv, n int, mode retentionMode) (rows int, maxInFlight int32, arrival []string) {
	t.Helper()
	svc := e.writerService(t, e.dedicatedPool(t, int32(n))) //nolint:gosec // small test constant
	arrivedA := make(chan struct{}, n)
	arrivedB := make(chan struct{}, n)
	releaseA := make(chan struct{}, n)
	releaseB := make(chan struct{}, n)
	var once sync.Once
	releaseAll := func() { once.Do(func() { close(releaseA); close(releaseB) }) }
	t.Cleanup(releaseAll)

	var inFlight, maxSeen atomic.Int32
	var mu sync.Mutex
	svc.codexCatalogHooks = &codexCatalogTestHooks{
		afterUpsert: func(row codexCatalogRow) {
			cur := inFlight.Add(1)
			for {
				m := maxSeen.Load()
				if cur <= m || maxSeen.CompareAndSwap(m, cur) {
					break
				}
			}
			mu.Lock()
			arrival = append(arrival, row.Model)
			mu.Unlock()
			arrivedA <- struct{}{}
			<-releaseA
		},
		beforeCommit: func() {
			// Leave the in-flight count here, still under the catalog lock: decrementing after the
			// call returns would run after COMMIT, when the next writer may already hold the lock.
			inFlight.Add(-1)
			if mode == retentionLockFree {
				arrivedB <- struct{}{}
				<-releaseB
			}
		},
	}
	errs := make(chan error, n)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- svc.recordCodexCatalogRowTx(e.ctx, catalogRowFor(e.userID, catalogModel(i)))
		}()
	}
	go func() { wg.Wait(); close(done) }()

	switch mode {
	case retentionLocked:
		awaitBarrier(t, "first writer at the post-upsert hook", arrivedA)
		e.awaitAdvisoryWaiters(t, n-1)
		for i := 0; i < n; i++ {
			releaseA <- struct{}{}
			if i < n-1 {
				awaitBarrier(t, fmt.Sprintf("writer %d of %d at the post-upsert hook", i+2, n), arrivedA)
			}
		}
	case retentionLockFree:
		for i := 0; i < n; i++ {
			awaitBarrier(t, fmt.Sprintf("writer %d of %d at the post-upsert hook (lock-free mode)", i+1, n), arrivedA)
		}
		for i := 0; i < n; i++ {
			releaseA <- struct{}{}
		}
		for i := 0; i < n; i++ {
			awaitBarrier(t, fmt.Sprintf("writer %d of %d at the pre-commit hook (lock-free mode)", i+1, n), arrivedB)
		}
		for i := 0; i < n; i++ {
			releaseB <- struct{}{}
		}
	}
	awaitBarrier(t, "all writers finished", done)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("writer: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return e.rows(t), maxSeen.Load(), slices.Clone(arrival)
}

// awaitAdvisoryWaiters polls pg_locks until exactly want ungranted advisory lock requests exist
// for this user's catalog lock key and one is granted. It polls on a deadline; there is no fixed sleep.
func (e catalogEnv) awaitAdvisoryWaiters(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(catalogBarrierDeadline)
	var waiting, granted int
	for {
		err := e.pool.QueryRow(e.ctx, `SELECT count(*) FILTER (WHERE NOT granted)::int, count(*) FILTER (WHERE granted)::int
			FROM pg_locks WHERE locktype = 'advisory' AND objsubid = 2
			  AND classid = $1::bigint::oid AND objid = hashtext($2::text)::oid`,
			int64(store.CodexCatalogLockClass), e.userID.String()).Scan(&waiting, &granted)
		if err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if waiting == want && granted == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("barrier %q not reached within %s: %d waiting, %d granted advisory requests", fmt.Sprintf("%d writers waiting on the catalog lock", want), catalogBarrierDeadline, waiting, granted)
		}
		select {
		case <-e.ctx.Done():
			t.Fatal("context done")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// With the per-user lock, at most one writer is ever between the post-upsert hook and its commit
// even though 63 others are waiting, and the final count is 32.
func TestCodexCatalogRetentionLockSerializesWritersLiveDB(t *testing.T) {
	e := setupCatalog(t, false)
	const n = 64
	rows, maxInFlight, arrival := runCatalogRetentionBarriers(t, e, n, retentionLocked)
	if maxInFlight != 1 {
		t.Fatalf("writers between the post-upsert hook and commit peaked at %d, want 1", maxInFlight)
	}
	if rows != 32 {
		t.Fatalf("rows = %d, want 32", rows)
	}
	// Writers were released one at a time in arrival order, so the survivors are the last 32.
	want := slices.Clone(arrival[n-32:])
	sort.Strings(want)
	if got := e.models(t, e.userID); !slices.Equal(got, want) {
		t.Fatalf("surviving models = %v, want the last 32 arrivals %v", got, want)
	}
}
