package weeklywarn

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TxBeginner opens the transactions of the observation and claim. *pgxpool.Pool satisfies it.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Publisher runs the post-commit half of a notification. *notifysvc.Service satisfies it.
type Publisher interface {
	PublishPersisted(ctx context.Context, row store.Notification, n notifysvc.Notification)
}

// Blocks builds the owner-only weekly-limit blocks of runs. *weeklylimit.Loader satisfies it.
type Blocks interface {
	ForRuns(ctx context.Context, ownerID uuid.UUID, runs []store.Run) map[uuid.UUID]*apitypes.WeeklyLimitDTO
}

// AffectedRuns is the DM's view of the runs on one credential: Total is every non-terminal run (the
// claim's own predicate, uncapped) and Newest at most maxRunLines of them, newest first.
type AffectedRuns struct {
	Total  int
	Newest []store.Run
}

// RunSource reads the non-terminal runs of one owner on one credential (an Anthropic secret id for
// harness "claude", a Codex provider account id for "codex"). The default runs the queries that share
// their predicate with the claim's HasNonTerminalRun queries.
type RunSource func(ctx context.Context, ownerID, credentialID uuid.UUID, harness string) (AffectedRuns, error)

// Config wires a Warner. Pool, Queries and Publisher are required; a nil Blocks sends a DM without
// the per-run lines.
type Config struct {
	Pool      TxBeginner
	Queries   *store.Queries
	Publisher Publisher
	Blocks    Blocks
	Runs      RunSource
	// ClaudeInterval and CodexInterval are the pollers' configured cadences: the freshness rule of the
	// validity gate (a reading older than three of them is stale), the same value the reserve uses.
	ClaudeInterval, CodexInterval time.Duration
	// Now is the clock of the validity gate; time.Now when nil.
	Now    func() time.Time
	Logger *slog.Logger
}

// Warner owns the weekly_limit_alerts ledger and the warning notification. The pollers call
// ObserveAnthropicWeekly and ObserveCodexWeekly after a written reading; both are best effort and
// never fail the poller.
type Warner struct {
	pool           TxBeginner
	q              *store.Queries
	pub            Publisher
	blocks         Blocks
	runs           RunSource
	claudeInterval time.Duration
	codexInterval  time.Duration
	now            func() time.Time
	logger         *slog.Logger

	// persist inserts the notifications row inside the claim transaction. It is notifysvc.PersistTx;
	// a test replaces it to inject a failing insert.
	persist func(ctx context.Context, q notifysvc.NotificationInserter, n notifysvc.Notification) (store.Notification, error)
}

// New builds a Warner.
func New(cfg Config) *Warner {
	w := &Warner{
		pool: cfg.Pool, q: cfg.Queries, pub: cfg.Publisher, blocks: cfg.Blocks, runs: cfg.Runs,
		claudeInterval: cfg.ClaudeInterval, codexInterval: cfg.CodexInterval,
		now: cfg.Now, logger: cfg.Logger, persist: notifysvc.PersistTx,
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.logger == nil {
		w.logger = slog.Default()
	}
	if w.runs == nil {
		w.runs = w.listRuns
	}
	return w
}

func (w *Warner) listRuns(ctx context.Context, ownerID, credentialID uuid.UUID, harness string) (AffectedRuns, error) {
	var (
		total int64
		runs  []store.Run
		err   error
	)
	if harness == "codex" {
		if total, err = w.q.CountNonTerminalRunsForCodexAccount(ctx, store.CountNonTerminalRunsForCodexAccountParams{UserID: ownerID, AccountID: credentialID}); err != nil {
			return AffectedRuns{}, err
		}
		runs, err = w.q.ListNonTerminalRunsForCodexAccount(ctx, store.ListNonTerminalRunsForCodexAccountParams{UserID: ownerID, AccountID: credentialID, MaxRows: maxRunLines})
	} else {
		if total, err = w.q.CountNonTerminalRunsForAnthropicSecret(ctx, store.CountNonTerminalRunsForAnthropicSecretParams{UserID: ownerID, SecretID: credentialID}); err != nil {
			return AffectedRuns{}, err
		}
		runs, err = w.q.ListNonTerminalRunsForAnthropicSecret(ctx, store.ListNonTerminalRunsForAnthropicSecretParams{UserID: ownerID, SecretID: credentialID, MaxRows: maxRunLines})
	}
	if err != nil {
		return AffectedRuns{}, err
	}
	return AffectedRuns{Total: int(total), Newest: runs}, nil
}

// credential is one reading's identity and the ledger accessors that differ between the two vendors.
// next is the pure state machine over the locked row; trigger is the reading's headline window.
type credential struct {
	userID uuid.UUID
	// credID is the Anthropic secret id or the Codex provider account id the reading belongs to.
	credID  uuid.UUID
	key     string
	harness string
	obs     time.Time
	trigger usagereserve.WeeklyReading
	// label is the credential's display label when the run blocks name none; set by current.
	label string

	next    func(Row) Next
	seed    func(ctx context.Context, q *store.Queries) error
	lock    func(ctx context.Context, q *store.Queries) (store.WeeklyLimitAlert, error)
	current func(ctx context.Context, q *store.Queries) (bool, error)
	hasRun  func(ctx context.Context, q *store.Queries) (bool, error)
	// fence attaches the credential authority fence to the Slack render.
	fence func(r *notifysvc.SlackRender)
}

// ObserveAnthropicWeekly handles one written Anthropic reading. syncedAt is the exact instant the
// fenced meter write persisted (microsecond precision); rev is the enablement revision it landed at.
func (w *Warner) ObserveAnthropicWeekly(ctx context.Context, userID, secretID uuid.UUID, rev int64, pct *int16, resetsAt *time.Time, syncedAt time.Time) {
	if c := w.anthropicCredential(userID, secretID, rev, pct, resetsAt, syncedAt); c != nil {
		w.process(ctx, c)
	}
}

// anthropicCredential runs step 0 (the whole-reading validity gate, a pure function of the in-memory
// reading and the injected clock) and returns the reading's credential, or nil when the reading is
// rejected: a rejected reading opens no transaction, takes no lock and writes no row.
func (w *Warner) anthropicCredential(userID, secretID uuid.UUID, rev int64, pct *int16, resetsAt *time.Time, syncedAt time.Time) *credential {
	obs := syncedAt.UTC().Truncate(time.Microsecond)
	// Step 0, before any transaction: the whole-reading validity gate. ClaudeWeekly, never
	// EvaluateClaude, whose nil threshold reports OK (margin-only users have none).
	wr := usagereserve.ClaudeWeekly(usagereserve.ClaudeReading{
		Present: true, SevenDayPct: pct, SevenDayResetsAt: resetsAt, SyncedAt: &obs,
	}, w.claudeInterval, w.now())
	if !wr.OK {
		return nil
	}
	win := ClaudeWindow{Reset: wr.ResetsAt, Pct: wr.Pct}
	fence := notifysvc.CredentialFence{SecretID: secretID, Kind: store.KindAnthropicToken, EnablementRev: rev}
	c := &credential{
		userID: userID, credID: secretID, key: workersvc.AnthropicCredentialKey(secretID), harness: "claude", obs: obs, trigger: wr,
		next: func(r Row) Next { return nextClaude(r, obs, win) },
		seed: func(ctx context.Context, q *store.Queries) error {
			return q.SeedWeeklyLimitAlertAnthropic(ctx, store.SeedWeeklyLimitAlertAnthropicParams{UserID: userID, SecretID: secretID})
		},
		lock: func(ctx context.Context, q *store.Queries) (store.WeeklyLimitAlert, error) {
			return q.LockWeeklyLimitAlertAnthropic(ctx, store.LockWeeklyLimitAlertAnthropicParams{UserID: userID, SecretID: secretID})
		},
		hasRun: func(ctx context.Context, q *store.Queries) (bool, error) {
			return q.HasNonTerminalRunForAnthropicSecret(ctx, store.HasNonTerminalRunForAnthropicSecretParams{UserID: userID, SecretID: secretID})
		},
		fence: func(r *notifysvc.SlackRender) { f := fence; r.Credential = &f },
	}
	c.current = func(ctx context.Context, q *store.Queries) (bool, error) {
		row, err := q.GetSecretEnablement(ctx, store.GetSecretEnablementParams{ID: secretID, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		c.label = row.Label
		return fence.Current(row), nil
	}
	return c
}

// ObserveCodexWeekly handles one written Codex reading. fence is the account authority the fenced
// write observed; buckets and lastSuccessAt are the stored reading and its exact persisted instant.
func (w *Warner) ObserveCodexWeekly(ctx context.Context, fence notifysvc.CodexAccountFence, buckets []apitypes.CodexRateLimitBucketDTO, lastSuccessAt time.Time) {
	if c := w.codexCredential(fence, buckets, lastSuccessAt); c != nil {
		w.process(ctx, c)
	}
}

// codexCredential is anthropicCredential's Codex twin.
func (w *Warner) codexCredential(fence notifysvc.CodexAccountFence, buckets []apitypes.CodexRateLimitBucketDTO, lastSuccessAt time.Time) *credential {
	obs := lastSuccessAt.UTC().Truncate(time.Microsecond)
	cw, ok := codexGate(fence, buckets, obs, w.codexInterval, w.now())
	if !ok {
		return nil
	}
	view := CodexView{Windows: cw.Windows, Dup: duplicateBuckets(buckets)}
	trig, _ := view.Trigger()
	account := fence.AccountID
	c := &credential{
		userID: fence.OwnerID, credID: account, key: workersvc.CodexCredentialKey(account), harness: "codex", obs: obs, trigger: trig,
		label: "Codex account",
		next:  func(r Row) Next { return nextCodex(r, obs, view) },
		seed: func(ctx context.Context, q *store.Queries) error {
			return q.SeedWeeklyLimitAlertCodex(ctx, store.SeedWeeklyLimitAlertCodexParams{UserID: fence.OwnerID, AccountID: account})
		},
		lock: func(ctx context.Context, q *store.Queries) (store.WeeklyLimitAlert, error) {
			return q.LockWeeklyLimitAlertCodex(ctx, store.LockWeeklyLimitAlertCodexParams{UserID: fence.OwnerID, AccountID: account})
		},
		current: func(ctx context.Context, q *store.Queries) (bool, error) {
			ok, err := q.IsCodexWeeklyWarningCurrent(ctx, store.IsCodexWeeklyWarningCurrentParams(fence.Params()))
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return ok, err
		},
		hasRun: func(ctx context.Context, q *store.Queries) (bool, error) {
			return q.HasNonTerminalRunForCodexAccount(ctx, store.HasNonTerminalRunForCodexAccountParams{UserID: fence.OwnerID, AccountID: account})
		},
		fence: func(r *notifysvc.SlackRender) {
			f := fence
			r.DeliveryTag, r.CodexAccount = notifysvc.DeliveryTagCodexWeeklyWarning, &f
		},
	}
	return c
}

// codexGate is step 0 for a Codex reading, a pure function of the reading, the fence and the clock:
// CodexWeekly directly, so a malformed, NaN, Inf or negative weekly window anywhere in the reading, a
// stale reading, no weekly window and a pre-reset best window all reject it, as does an invalid fence.
func codexGate(fence notifysvc.CodexAccountFence, buckets []apitypes.CodexRateLimitBucketDTO, obs time.Time, interval time.Duration, now time.Time) (usagereserve.CodexWeeklyReading, bool) {
	cw := usagereserve.CodexWeekly(usagereserve.CodexReading{Present: true, Buckets: buckets, LastSuccessAt: &obs}, interval, now)
	return cw, cw.OK && fence.Valid()
}

// duplicateBuckets is the set of bucket ids carried by more than one bucket of the reading.
func duplicateBuckets(buckets []apitypes.CodexRateLimitBucketDTO) map[string]bool {
	seen := map[string]int{}
	for _, b := range buckets {
		seen[b.ID]++
	}
	dup := map[string]bool{}
	for id, n := range seen {
		if n > 1 {
			dup[id] = true
		}
	}
	return dup
}

// process runs the two transactions for one accepted-by-the-gate reading.
func (w *Warner) process(ctx context.Context, c *credential) {
	next, ok := w.observe(ctx, c)
	if !ok || next.Warned || !next.PeriodOK {
		return
	}
	w.claim(ctx, c, next)
}

func thresholdOf(set store.GetUserUsageReserveRow) *int16 {
	if !set.WeeklyReservePct.Valid {
		return nil
	}
	v := set.WeeklyReservePct.Int16
	return &v
}

func tsz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func rowOf(r store.WeeklyLimitAlert) Row {
	row := Row{AnchorMulti: r.AnchorMulti, Warned: r.Warned}
	if r.AnchorReset.Valid {
		t := r.AnchorReset.Time.UTC()
		row.AnchorReset = &t
	}
	if r.AnchorPct.Valid {
		v := r.AnchorPct.Float64
		row.AnchorPct = &v
	}
	if r.AnchorBucket.Valid {
		v := r.AnchorBucket.String
		row.AnchorBucket = &v
	}
	return row
}

// observe is the observation transaction: it records the reading in the ledger and commits. It returns
// the state the reading left the row in, and false when the reading was dropped (a stale credential
// fence, or an equal or older observation instant) or the transaction failed.
func (w *Warner) observe(ctx context.Context, c *credential) (Next, bool) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.logger.Warn("weeklywarn: begin observation", "error", err)
		return Next{}, false
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The shared secret-mutation lock first: every parent deleter and Codex generation bump takes it
	// exclusively before touching the credential, so the fence check below cannot go stale in-flight.
	if err := store.LockSecretMutationShared(ctx, tx, c.userID); err != nil {
		w.logger.Warn("weeklywarn: lock observation", "error", err)
		return Next{}, false
	}
	q := w.q.WithTx(tx)
	current, err := c.current(ctx, q)
	if err != nil {
		w.logger.Warn("weeklywarn: observation fence", "error", err)
		return Next{}, false
	}
	if !current {
		return Next{}, false
	}
	if err := c.seed(ctx, q); err != nil {
		w.logger.Warn("weeklywarn: seed ledger row", "error", err)
		return Next{}, false
	}
	row, err := c.lock(ctx, q)
	if err != nil {
		w.logger.Warn("weeklywarn: lock ledger row", "error", err)
		return Next{}, false
	}
	next := c.next(rowOf(row))
	arg := store.ApplyWeeklyLimitObservationParams{
		ID: row.ID, ObservedAt: tsz(c.obs), AnchorMulti: next.AnchorMulti, Warned: next.Warned,
	}
	if next.AnchorReset != nil {
		arg.AnchorReset = tsz(*next.AnchorReset)
	}
	if next.AnchorPct != nil {
		arg.AnchorPct = pgtype.Float8{Float64: *next.AnchorPct, Valid: true}
	}
	if next.AnchorBucket != nil {
		arg.AnchorBucket = pgtype.Text{String: *next.AnchorBucket, Valid: true}
	}
	if _, err := q.ApplyWeeklyLimitObservation(ctx, arg); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) { // ErrNoRows: the strict fence dropped an equal or older instant
			w.logger.Warn("weeklywarn: apply observation", "error", err)
		}
		return Next{}, false
	}
	if err := tx.Commit(ctx); err != nil {
		w.logger.Warn("weeklywarn: commit observation", "error", err)
		return Next{}, false
	}
	return next, true
}

// claim builds the DM and then runs the claim transaction: under the row lock it re-checks that this reading is still the
// newest observation and the period is still unwarned, then sets warned and inserts the notifications
// row together. The Slack publish happens only after the commit.
func (w *Warner) claim(ctx context.Context, c *credential, next Next) {
	// Outside any transaction and before any lock: the cheap pre-filter, then the DM content (the
	// owner's run listing and the weekly-limit blocks read the pool). The content is advisory: the claim
	// transaction re-checks every gate under the row lock, so a stale line can only mis-word a DM that
	// is still due, and no pool read happens while the advisory or row locks are held.
	set, err := w.q.GetUserUsageReserve(ctx, c.userID)
	if err != nil {
		w.logger.Warn("weeklywarn: read warning settings", "error", err)
		return
	}
	pre := usagereserve.EvaluateNear(thresholdOf(set), int(set.WeeklyWarningMarginPts), c.trigger)
	if !Claimable(next, c.obs, pre, int(set.WeeklyWarningMarginPts)) {
		return
	}
	n := w.build(ctx, c, pre)
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.logger.Warn("weeklywarn: begin claim", "error", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.LockSecretMutationShared(ctx, tx, c.userID); err != nil {
		w.logger.Warn("weeklywarn: lock claim", "error", err)
		return
	}
	q := w.q.WithTx(tx)
	row, err := c.lock(ctx, q)
	if err != nil { // no row: the credential was deleted since the observation
		if !errors.Is(err, pgx.ErrNoRows) {
			w.logger.Warn("weeklywarn: lock claim row", "error", err)
		}
		return
	}
	if row.Warned || !row.ObservedAt.Valid || !row.ObservedAt.Time.Equal(c.obs) {
		return // a newer observation won, or the period is already claimed
	}
	set, err = q.GetUserUsageReserve(ctx, c.userID)
	if err != nil {
		w.logger.Warn("weeklywarn: claim settings", "error", err)
		return
	}
	margin := int(set.WeeklyWarningMarginPts)
	near := usagereserve.EvaluateNear(thresholdOf(set), margin, c.trigger)
	if !Claimable(next, c.obs, near, margin) {
		return
	}
	current, err := c.current(ctx, q)
	if err != nil || !current {
		return
	}
	// The run check is last: it is the only condition that reads another table.
	has, err := c.hasRun(ctx, q)
	if err != nil || !has {
		return
	}
	if _, err := q.ClaimWeeklyLimitWarning(ctx, store.ClaimWeeklyLimitWarningParams{ID: row.ID, ObservedAt: tsz(c.obs)}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			w.logger.Warn("weeklywarn: claim", "error", err)
		}
		return
	}
	persisted, err := w.persist(ctx, q, n)
	if err != nil { // rolls the claim back: warned stays false and the next near reading retries
		w.logger.Warn("weeklywarn: insert notification", "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		w.logger.Warn("weeklywarn: commit claim", "error", err)
		return
	}
	w.pub.PublishPersisted(ctx, persisted, n)
}
