package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/mod/semver"

	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_catalog_default.go is the Codex catalog default reasoning effort seam (issue #3251): the
// claim's opaque catalog context, the worker's report (RecordCodexCatalogDefault), and the read
// (CodexCatalogDefault) the settings cell calls. Rows are per user, keyed by a context hash of the
// credential revision a claim actually released, and bounded to 32 rows and 30 days per user.

// Catalog default states (CatalogDefault.State).
const (
	CatalogStateKnown   = "known"
	CatalogStateUnknown = "unknown"
	CatalogStateVaries  = "varies"
)

const (
	codexCatalogCtxDomain = "uzi-codex-catalog-ctx-v1"
	// codexCatalogMaxAge mirrors the 30-day half of the retention bound; the SQL carries both halves
	// (32 rows, 30 days: queries/codex_catalog_defaults.sql), pinned by the LiveDB retention tests.
	codexCatalogMaxAge        = 30 * 24 * time.Hour
	maxCodexCatalogModelBytes = 100
	maxCodexCatalogVersion    = 32
)

var (
	// ErrCodexCatalogInvalid is a report the server refuses as malformed or for a run that is not a
	// Codex run → 400 invalid_catalog_default.
	ErrCodexCatalogInvalid = errors.New("invalid codex catalog default report")
	// ErrStaleCatalogContext is a report whose echoed context is not the one the run's frozen
	// binding, or the alias's current state, yields (the credential was replaced or re-linked
	// since the claim) → 409 stale_context.
	ErrStaleCatalogContext = errors.New("codex catalog context is stale")
)

// codexCatalogEfforts is the stored effort set: the Codex wire subset, mirrored by the table's
// codex_catalog_defaults_effort_check (TestCodexCatalogEffortSetMatchesCheckLiveDB pins them).
var codexCatalogEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

var codexCatalogContextRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CodexCatalogDefaultRequest is the body of POST /api/worker/runs/{id}/codex-catalog-default. The
// handler decodes it with DisallowUnknownFields, so a field a newer worker sends must be declared
// here in the release BEFORE that worker ships.
type CodexCatalogDefaultRequest struct {
	ClaimGeneration *int64 `json:"claim_generation,omitempty"`
	CatalogContext  string `json:"catalog_context"`
	Model           string `json:"model"`
	RuntimeVersion  string `json:"runtime_version"`
	DefaultEffort   string `json:"default_effort"`
}

// codexCatalogContext derives the opaque catalog context: lowercase hex SHA-256 of the domain
// string and the five identity parts, each separated by a NUL byte. It carries identifiers and
// counters only, never credential material; hashing gives a fixed opaque value, not secrecy.
// accountKey is empty for an api_key binding. The subscription refresh generation is deliberately
// not part of it (a refresh rotates the token of the same account, and the catalog is account
// metadata).
func codexCatalogContext(userID, secretID uuid.UUID, authMode string, materialRevision int64, accountKey string) string {
	h := sha256.New()
	parts := []string{codexCatalogCtxDomain, userID.String(), secretID.String(), authMode, strconv.FormatInt(materialRevision, 10), accountKey}
	for i, p := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// claimCodexCatalogContext is the context a claim ships (ClaimConfig.CodexCatalogContext): only
// for a Codex run on the lead lane (the executor whose probe feeds the report), and only when the
// frozen binding is complete. A subscription run with no frozen account key has no context.
func claimCodexCatalogContext(run store.Run) *string {
	if run.Harness != harnessCodex || !leadcfg.LeadLane(run.Kind, run.ReviewTargetRunID.Valid, run.EgressProfileID.Valid) {
		return nil
	}
	ctxID, ok := frozenCodexCatalogContext(run)
	if !ok {
		return nil
	}
	return &ctxID
}

// frozenCodexCatalogContext derives the context from the run's frozen binding columns.
func frozenCodexCatalogContext(run store.Run) (string, bool) {
	if !run.CodexSecretID.Valid || !run.CodexAuthMode.Valid || !run.CodexMaterialRevision.Valid {
		return "", false
	}
	accountKey := ""
	switch run.CodexAuthMode.String {
	case codexAuthModeAPIKey:
	case codexAuthModeSubscription:
		if !run.CodexAccountKey.Valid {
			return "", false
		}
		accountKey = run.CodexAccountKey.String
	default:
		return "", false
	}
	return codexCatalogContext(run.UserID, uuid.UUID(run.CodexSecretID.Bytes), run.CodexAuthMode.String, run.CodexMaterialRevision.Int64, accountKey), true
}

// codexCatalogAliasStore is the read surface for the context an alias would freeze now. Both
// *store.Queries and a transaction-bound one satisfy it.
type codexCatalogAliasStore interface {
	GetCodexCredentialState(ctx context.Context, arg store.GetCodexCredentialStateParams) (store.CodexCredentialState, error)
	GetCodexProviderAccountByID(ctx context.Context, arg store.GetCodexProviderAccountByIDParams) (store.CodexProviderAccount, error)
}

// currentCodexCatalogContext derives the context an alias would carry if a run froze its binding
// now: the alias's current material revision and, for a subscription, the account key
// freezeCodexBinding would freeze. ok is false when no complete context exists (no state row, or
// an unlinked subscription).
func currentCodexCatalogContext(ctx context.Context, q codexCatalogAliasStore, userID, secretID uuid.UUID, authMode string) (string, bool, error) {
	st, err := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{UserSecretID: secretID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("codex catalog: read alias state: %w", err)
	}
	accountKey := ""
	switch authMode {
	case codexAuthModeAPIKey:
	case codexAuthModeSubscription:
		if !st.ProviderAccountID.Valid {
			return "", false, nil
		}
		acct, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: userID, ID: uuid.UUID(st.ProviderAccountID.Bytes)})
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("codex catalog: read provider account: %w", err)
		}
		accountKey, err = codexAccountKey(acct.ProviderUserID, acct.WorkspaceAccountID)
		if err != nil {
			return "", false, fmt.Errorf("codex catalog: encode account key: %w", err)
		}
	default:
		return "", false, nil
	}
	return codexCatalogContext(userID, secretID, authMode, st.MaterialRevision, accountKey), true, nil
}

// validateCodexCatalogRequest checks everything a report carries before any read or write. The
// returned error never quotes a worker-supplied value.
func validateCodexCatalogRequest(req CodexCatalogDefaultRequest) error {
	if req.ClaimGeneration == nil || *req.ClaimGeneration < 1 {
		return ErrMissingClaimGeneration
	}
	if !codexCatalogContextRE.MatchString(req.CatalogContext) {
		return fmt.Errorf("%w: catalog_context", ErrCodexCatalogInvalid)
	}
	if n := len(req.Model); n < 1 || n > maxCodexCatalogModelBytes || !utf8.ValidString(req.Model) || strings.ContainsRune(req.Model, 0) {
		return fmt.Errorf("%w: model", ErrCodexCatalogInvalid)
	}
	if n := len(req.RuntimeVersion); n < 1 || n > maxCodexCatalogVersion || !semver.IsValid("v"+req.RuntimeVersion) {
		return fmt.Errorf("%w: runtime_version", ErrCodexCatalogInvalid)
	}
	if !slices.Contains(codexCatalogEfforts, req.DefaultEffort) {
		return fmt.Errorf("%w: default_effort", ErrCodexCatalogInvalid)
	}
	return nil
}

// codexCatalogRow is one stored report.
type codexCatalogRow struct {
	UserID         uuid.UUID
	ContextID      string
	Model          string
	RuntimeVersion string
	DefaultEffort  string
}

// codexCatalogTestHooks are test seams (nil in production) inside the locked write: afterUpsert
// runs after the upsert and before the prune, beforeCommit after the prune and the post step,
// both while the per-user lock is held.
type codexCatalogTestHooks struct {
	afterUpsert  func(codexCatalogRow)
	beforeCommit func()
}

// RecordCodexCatalogDefault persists one worker report. Ownership, the claim-generation
// requirement and the fence follow RecordRunUsage; the report must also echo the context both the
// run's frozen binding and the alias's current state yield, so a report is attributed only to the
// credential revision its claim released and never to a later one.
//
// ONE READ COMMITTED transaction, statements in this order: the shared secret-mutation lock
// (credential replacement bumps material_revision under the exclusive one, so it cannot commit
// between the current-state check and the commit), the per-user catalog lock, the run row read
// (FOR KEY SHARE), the early fence and both context checks, the upsert, the prune, and the locked
// fence recheck as the last statement before commit. Lock order: secret (shared), catalog, run row.
func (s *Service) RecordCodexCatalogDefault(ctx context.Context, wkr store.Worker, runID uuid.UUID, req CodexCatalogDefaultRequest) error {
	if err := validateCodexCatalogRequest(req); err != nil {
		return err
	}
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return err
	}
	if run.Harness != harnessCodex || run.UserID != wkr.UserID {
		return fmt.Errorf("%w: run", ErrCodexCatalogInvalid)
	}
	if s.txBeginner == nil {
		return fmt.Errorf("codex catalog report unavailable: no tx beginner wired for run %s", runID)
	}
	gen := req.ClaimGeneration
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	if err := setUsageWriteIsolation(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	// A no-op after a successful Commit; on every early return it releases both advisory locks.
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.LockSecretMutationShared(ctx, tx, run.UserID); err != nil {
		return err
	}
	row := codexCatalogRow{UserID: run.UserID, ContextID: req.CatalogContext, Model: req.Model, RuntimeVersion: req.RuntimeVersion, DefaultEffort: req.DefaultEffort}
	pre := func(q *store.Queries) error {
		// Reconciliation holds the run FOR UPDATE through audit insertion; KEY SHARE serializes
		// that audit without blocking ordinary release/reclaim UPDATEs (see RecordRunUsage).
		var lockedRun store.Run
		err := tx.QueryRow(ctx, `SELECT claim_generation, claim_released_at FROM runs
			WHERE id=$1 AND worker_id=$2 FOR KEY SHARE`, runID, wkr.ID).
			Scan(&lockedRun.ClaimGeneration, &lockedRun.ClaimReleasedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleClaim
		}
		if err != nil {
			return err
		}
		reconciled, err := reconciliationExists(ctx, tx, runID)
		if err != nil {
			return err
		}
		if reconciled {
			if err := validateReconciledGeneration(lockedRun, gen); err != nil {
				return err
			}
		}
		live, err := q.RunUsageFenceLive(ctx, store.RunUsageFenceLiveParams{RunID: runID, ClaimGeneration: pgconv.Int8Ptr(gen)})
		if err != nil {
			return err
		}
		if !live {
			return ErrStaleClaim
		}
		frozen, ok := frozenCodexCatalogContext(run)
		if !ok || frozen != req.CatalogContext {
			return ErrStaleCatalogContext
		}
		current, ok, err := currentCodexCatalogContext(ctx, q, run.UserID, uuid.UUID(run.CodexSecretID.Bytes), run.CodexAuthMode.String)
		if err != nil {
			return err
		}
		if !ok || current != req.CatalogContext {
			return ErrStaleCatalogContext
		}
		return nil
	}
	post := func(q *store.Queries) error {
		// Locked recheck as the LAST statement before Commit, as RecordRunUsage: a release or
		// reclaim that committed after the early read is seen here and discards the write.
		live, err := q.RunUsageFenceLiveLocked(ctx, store.RunUsageFenceLiveLockedParams{RunID: runID, WorkerID: pgconv.UUID(wkr.ID), ClaimGeneration: pgconv.Int8Ptr(gen)})
		if err != nil {
			return err
		}
		if !live {
			return ErrStaleClaim
		}
		return nil
	}
	if err := s.writeCodexCatalogRow(ctx, store.New(tx), row, pre, post); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// writeCodexCatalogRow is the one locked write, run on a transaction-bound q: the per-user catalog
// lock as its own statement, the optional pre step (the fences), the upsert, the prune, the
// optional post step. The lock serializes every writer for the user, so each prune sees every
// previously committed row and the user has at most 32 rows (none older than 30 days as of this
// write) after commit. The writer's own row survives its prune: the upsert stamps
// statement_timestamp(), not the transaction start.
func (s *Service) writeCodexCatalogRow(ctx context.Context, q *store.Queries, row codexCatalogRow, pre, post func(*store.Queries) error) error {
	if err := q.LockCodexCatalogDefaults(ctx, row.UserID); err != nil {
		return err
	}
	if pre != nil {
		if err := pre(q); err != nil {
			return err
		}
	}
	if err := q.UpsertCodexCatalogDefault(ctx, store.UpsertCodexCatalogDefaultParams{
		UserID: row.UserID, ContextID: row.ContextID, Model: row.Model, RuntimeVersion: row.RuntimeVersion, DefaultEffort: row.DefaultEffort,
	}); err != nil {
		return err
	}
	if h := s.codexCatalogHooks; h != nil && h.afterUpsert != nil {
		h.afterUpsert(row)
	}
	if err := q.PruneCodexCatalogDefaults(ctx, row.UserID); err != nil {
		return err
	}
	if post != nil {
		if err := post(q); err != nil {
			return err
		}
	}
	if h := s.codexCatalogHooks; h != nil && h.beforeCommit != nil {
		h.beforeCommit()
	}
	return nil
}

// recordCodexCatalogRowTx is the locked write in its own transaction, without a run or a fence.
// Production reaches the write only through RecordCodexCatalogDefault; this exists so the
// retention bound can be proven without a claimed run.
func (s *Service) recordCodexCatalogRowTx(ctx context.Context, row codexCatalogRow) error {
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setUsageWriteIsolation(ctx, tx); err != nil {
		return err
	}
	if err := s.writeCodexCatalogRow(ctx, store.New(tx), row, nil, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CatalogDefault is the catalog default effort a new Codex run would see for a model.
type CatalogDefault struct {
	State          string // CatalogStateKnown, CatalogStateUnknown or CatalogStateVaries
	Effort         string
	Model          string
	RuntimeVersion string
	ObservedAt     time.Time
}

// codexCatalogReadStore is the read surface CodexCatalogDefault needs beyond the resolver's.
type codexCatalogReadStore interface {
	harnessResolverStore
	codexCatalogAliasStore
	ListFreshCodexCatalogDefaults(ctx context.Context, arg store.ListFreshCodexCatalogDefaultsParams) ([]store.ListFreshCodexCatalogDefaultsRow, error)
}

// CodexCatalogDefault answers what the Codex catalog's default reasoning effort would be for model
// on the credential a new run of this user would bind, as last reported within 30 days. It reads
// no lock. Unknown covers: no usable Codex credential, an unlinked subscription, account pool mode
// (the alias is only picked inside the creation transaction, so no credential can be predicted),
// and no fresh report for the current credential revision.
func (s *Service) CodexCatalogDefault(ctx context.Context, userID uuid.UUID, model string) (CatalogDefault, error) {
	unknown := CatalogDefault{State: CatalogStateUnknown}
	q, ok := s.q.(codexCatalogReadStore)
	if !ok {
		return unknown, errHarnessStoreUnavailable
	}
	choice, usable, err := s.resolveUsableCodexCredential(ctx, userID, q)
	if err != nil {
		return unknown, err
	}
	if !usable || choice.Select == codexChoicePool || choice.SecretID == uuid.Nil {
		return unknown, nil
	}
	ctxID, ok, err := currentCodexCatalogContext(ctx, q, userID, choice.SecretID, choice.AuthMode)
	if err != nil {
		return unknown, err
	}
	if !ok {
		return unknown, nil
	}
	rows, err := q.ListFreshCodexCatalogDefaults(ctx, store.ListFreshCodexCatalogDefaultsParams{UserID: userID, ContextID: ctxID, Model: model})
	if err != nil {
		return unknown, fmt.Errorf("codex catalog: list defaults: %w", err)
	}
	out := selectCatalogDefault(rows, time.Now())
	if out.State != CatalogStateUnknown {
		out.Model = model
	}
	return out, nil
}

// selectCatalogDefault picks the label from the fresh rows of one context and model: the highest
// runtime version by semver (ties by latest report), "varies" when any other version's effort
// differs, unknown when no row is fresh. A row older than 30 days is ignored, and so is one whose
// version does not parse (the writer never stores one). Model is left for the caller to fill.
func selectCatalogDefault(rows []store.ListFreshCodexCatalogDefaultsRow, now time.Time) CatalogDefault {
	var best *store.ListFreshCodexCatalogDefaultsRow
	fresh := make([]store.ListFreshCodexCatalogDefaultsRow, 0, len(rows))
	for _, r := range rows {
		if !r.ReportedAt.Valid || r.ReportedAt.Time.Before(now.Add(-codexCatalogMaxAge)) || !semver.IsValid("v"+r.RuntimeVersion) {
			continue
		}
		fresh = append(fresh, r)
	}
	for i := range fresh {
		r := &fresh[i]
		if best == nil {
			best = r
			continue
		}
		cmp := semver.Compare("v"+r.RuntimeVersion, "v"+best.RuntimeVersion)
		if cmp > 0 || (cmp == 0 && r.ReportedAt.Time.After(best.ReportedAt.Time)) {
			best = r
		}
	}
	if best == nil {
		return CatalogDefault{State: CatalogStateUnknown}
	}
	out := CatalogDefault{State: CatalogStateKnown, Effort: best.DefaultEffort, RuntimeVersion: best.RuntimeVersion, ObservedAt: best.ReportedAt.Time}
	for _, r := range fresh {
		if semver.Compare("v"+r.RuntimeVersion, "v"+best.RuntimeVersion) != 0 && r.DefaultEffort != best.DefaultEffort {
			out.State = CatalogStateVaries
			break
		}
	}
	return out
}
