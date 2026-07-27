// Package usagepoller is the per-TOKEN Claude rate-limit poller (PRD #53, repointed
// from per-user by #104 M5), a self-improve/privcheck-shaped engine: a Boot pass at
// start plus an interval ticker, 0 disables it, wired in main.go under the background
// WaitGroup. Each tick it lists every anthropic_token in the factory (one row per
// TOKEN, not per user), opens each via the shared vault path (secretopen), asks
// Anthropic (usage endpoint first, ~1-token header probe as fallback — D2), and
// upserts one gauge row per TOKEN (D4). The token never leaves this process; only
// percentages + reset epochs are stored.
//
// Failure semantics are copied from cc-statusline (D5): a malformed response never
// overwrites the last good row; a definitive HTTP refusal with no usable fallback
// arms a fixed 15-minute PER-TOKEN backoff (in-process, no knob, keyed by
// user_secret_id since PRD #104 M5 — this said "per-user" until PRD #111 and had
// been wrong since the gauge was repointed at tokens) so a refusing credential is
// not hammered every interval; a transport error just waits for the
// next tick. A vault-locked (dek-sealed) owner is skipped and their last reading
// kept (D3); a master-sealed token opens regardless of lock state — both handled
// by secretopen's dispatch, so this engine needs no separate vault gate.
package usagepoller

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/anthropic"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretopen"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// defaultMaxConcurrency bounds how many users are polled in parallel per tick.
const defaultMaxConcurrency = 4

// backoffDuration is the fixed per-TOKEN backoff after a definitive HTTP refusal
// with no usable fallback (D5). In-process and no knob by design: a restart just
// retries once.
//
// Per-token, not per-user, and the distinction is the whole point of the map: one
// refusing credential must not silence its owner's other meters (see inBackoff,
// where the map is keyed by secret id). This said "per-user" until PRD #111 M4.
const backoffDuration = 15 * time.Minute

// pokeBuffer bounds the poke channel; a full buffer drops the signal (the next
// tick covers the user anyway).
const pokeBuffer = 64

// Store is the query surface the engine needs. *store.Queries satisfies it.
// ListAnthropicTokensToPoll returns each TOKEN's id, owner, ciphertext +
// sealed_with so the tick opens them in one pass (no per-row re-fetch);
// GetDefaultUserSecretID backs the single-user poke path, which polls the token a
// save just touched.
type Store interface {
	ListAnthropicTokensToPoll(ctx context.Context) ([]store.ListAnthropicTokensToPollRow, error)
	GetDefaultUserSecretID(ctx context.Context, arg store.GetDefaultUserSecretIDParams) (uuid.UUID, error)
	GetUserSecretCiphertext(ctx context.Context, arg store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error)
	UpsertRateLimits(ctx context.Context, arg store.UpsertRateLimitsParams) error
}

// TokenOpener opens a user's Anthropic token via the vault path (PRD #53 D1/D3).
// *secretopen.Opener satisfies it in production; tests inject a fake. OpenSealed is
// the tick path (row already fetched); Open is the poke path (single-user lookup).
// Both return secretopen.ErrVaultLocked for a locked dek-sealed vault (skip, keep
// the last reading) and ErrNoSecret/ErrUndecryptable when the token is
// gone/undecryptable.
type TokenOpener interface {
	Open(ctx context.Context, userID uuid.UUID, kind string) ([]byte, error)
	OpenSealed(userID uuid.UUID, kind, sealedWith string, ciphertext []byte) ([]byte, error)
}

// Client reads rate-limit state from Anthropic. *anthropic.Client satisfies it.
type Client interface {
	Usage(ctx context.Context, token []byte) (anthropic.Reading, error)
	ProbeHeaders(ctx context.Context, token []byte) (anthropic.Reading, error)
}

// Engine is the rate-limit poller.
type Engine struct {
	store    Store
	opener   TokenOpener
	client   Client
	interval time.Duration
	probe    bool
	maxConc  int
	now      func() time.Time
	logger   *slog.Logger

	// mu guards backoff (written by concurrent per-token goroutines within a tick).
	// Keyed by SECRET id since M5, so a refusing credential backs off only itself.
	mu      sync.Mutex
	backoff map[uuid.UUID]time.Time

	// poke carries poke-on-token-save signals (D3b): a user whose token was just
	// saved is polled out-of-band so meters appear in seconds, not a full interval.
	poke chan uuid.UUID
}

// New builds an Engine. interval is the poll cadence; the caller only starts the
// engine when interval > 0 (0 disables it). probe is UZI_USAGE_PROBE. logger may
// be nil.
func New(st Store, opener TokenOpener, client Client, interval time.Duration, probe bool, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		store:    st,
		opener:   opener,
		client:   client,
		interval: interval,
		probe:    probe,
		maxConc:  defaultMaxConcurrency,
		now:      time.Now,
		logger:   logger,
		backoff:  make(map[uuid.UUID]time.Time),
		poke:     make(chan uuid.UUID, pokeBuffer),
	}
}

// Poke requests an out-of-band poll for one user (D3b). Non-blocking: it drops the
// signal when the buffer is full (the next tick covers the user regardless), so a
// caller — the token-save handler — never blocks on the poller.
func (e *Engine) Poke(userID uuid.UUID) {
	select {
	case e.poke <- userID:
	default:
	}
}

// Boot runs one immediate pass at start so a user who saved a token while the API
// was down gets a reading promptly, not one interval later. Non-fatal on failure.
func (e *Engine) Boot(ctx context.Context) { e.tickAll(ctx) }

// Run blocks until ctx is cancelled, polling every interval and servicing pokes.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	e.logger.Info("usage poller started", "interval", e.interval.String(), "probe", e.probe)
	for {
		select {
		case <-ctx.Done():
			e.logger.Info("usage poller stopped")
			return
		case userID := <-e.poke:
			// A freshly saved token: ignore any prior backoff (the new credential may
			// work where the old refused) and poll just that user's DEFAULT token via a
			// single-user lookup-open (the row wasn't part of a bulk list here).
			//
			// The poke identity stays the USER, not the token, because the only poker is
			// the kind-path save (handler/secrets.go), which rotates the default and has
			// no token id to offer. A poke therefore refreshes one meter, not all of the
			// user's — the rest are covered by the next tick, which is the same latency
			// they had before this feature existed.
			e.pokeUser(ctx, userID)
		case <-ticker.C:
			e.tickAll(ctx)
		}
	}
}

// tickAll polls every anthropic_token in the factory once, with bounded concurrency
// and a per-tick deadline of one interval so a pile-up of slow calls can't run past
// the next tick. A per-TOKEN failure is logged and skipped inside pollToken.
//
// The fan-out is one goroutine per TOKEN, not per user, so a user holding three
// credentials occupies three slots of maxConc. That is deliberate (poll cost scales
// with token count, not user count — R3) and it is what the semaphore bounds.
func (e *Engine) tickAll(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, e.interval)
	defer cancel()

	rows, err := e.store.ListAnthropicTokensToPoll(tickCtx)
	if err != nil {
		e.logger.Error("usage poller: list tokens", "error", err)
		return
	}

	sem := make(chan struct{}, e.maxConc)
	var wg sync.WaitGroup
	for _, row := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func(row store.ListAnthropicTokensToPollRow) {
			defer wg.Done()
			defer func() { <-sem }()
			// Open from the already-fetched row (no per-row re-fetch, no N+1). The
			// ciphertext stays in this goroutine and is never logged.
			e.pollToken(tickCtx, row.UserID, row.ID, false, func() ([]byte, error) {
				return e.opener.OpenSealed(row.UserID, store.KindAnthropicToken, row.SealedWith, row.Ciphertext)
			})
		}(row)
	}
	wg.Wait()
}

// pokeUser polls the token a just-saved credential landed on: the user's DEFAULT,
// which is what the kind-path save rotates or creates. Resolving the id first is
// what lets the reading be written against the right gauge row now that the gauge
// is per-token — writing it against "the user" is no longer expressible.
//
// A user with no default (no token at all, or the transient no-default state D12
// describes) has nothing to poll, which is not an error worth logging on a path
// triggered by a delete-then-poke race.
func (e *Engine) pokeUser(ctx context.Context, userID uuid.UUID) {
	secretID, err := e.store.GetDefaultUserSecretID(ctx, store.GetDefaultUserSecretIDParams{
		UserID: userID,
		Kind:   store.KindAnthropicToken,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			e.logger.Error("usage poller: resolve default token for poke", "user", userID.String(), "error", err)
		}
		return
	}
	e.pollToken(ctx, userID, secretID, true, func() ([]byte, error) {
		return e.opener.Open(ctx, userID, store.KindAnthropicToken)
	})
}

// pollToken polls ONE token, applying the D2 (usage-first, probe fallback) and D5
// (fail-closed / backoff) rules. open resolves that token via the vault path (bulk
// OpenSealed on the tick, single-user Open on the poke); ignoreBackoff is set on the
// poke path so a just-saved credential is polled even if the one it replaced was
// backed off.
//
// Backoff is keyed on the TOKEN since M5, not the user: one refusing credential
// must not silence its owner's other meters, which is precisely the case this
// feature exists to support (a throttled subscription alongside a working console
// key).
func (e *Engine) pollToken(ctx context.Context, userID, secretID uuid.UUID, ignoreBackoff bool, open func() ([]byte, error)) {
	if ignoreBackoff {
		e.clearBackoff(secretID)
	} else if e.inBackoff(secretID) {
		return
	}

	token, err := open()
	if err != nil {
		switch {
		case errors.Is(err, secretopen.ErrVaultLocked):
			// D3: locked dek-sealed vault — skip, keep the last reading (marked stale
			// server-side later). No backoff: the block clears on the next unlock.
			return
		case errors.Is(err, secretopen.ErrNoSecret), errors.Is(err, secretopen.ErrUndecryptable):
			// Token vanished or is undecryptable mid-tick — skip, no backoff.
			return
		default:
			e.logger.Error("usage poller: open token", "user", userID.String(), "error", err)
			return
		}
	}
	defer wipe(token)

	reading, err := e.client.Usage(ctx, token)
	if err == nil {
		e.upsert(ctx, userID, secretID, reading)
		e.clearBackoff(secretID)
		return
	}

	var aerr *anthropic.Error
	switch {
	case !errors.As(err, &aerr) || aerr.Kind == anthropic.KindTransport:
		// Transport failure — transient, just wait for the next tick (no backoff).
		return
	case aerr.Kind == anthropic.KindMalformed:
		// A 2xx we couldn't parse — fail closed: keep the last good row, no backoff.
		return
	}

	// KindHTTP: the usage endpoint definitively refused this credential (observed:
	// 429 for setup-tokens). Fall back to the ~1-token header probe (D2).
	if !e.probe {
		e.setBackoff(secretID) // no usable fallback → back off (D5)
		return
	}
	preading, perr := e.client.ProbeHeaders(ctx, token)
	if perr == nil {
		e.upsert(ctx, userID, secretID, preading)
		e.clearBackoff(secretID)
		return
	}
	// The probe also failed (refused, transport, or malformed) — no usable fallback,
	// so back off to avoid hammering a persistently refusing credential (D5).
	e.setBackoff(secretID)
}

// upsert overwrites ONE TOKEN's gauge row (PRD #53 D4, repointed by #104 M5).
// synced_at is stamped now.
func (e *Engine) upsert(ctx context.Context, userID, secretID uuid.UUID, r anthropic.Reading) {
	if err := e.store.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
		UserSecretID:     secretID,
		UserID:           userID,
		FiveHourPct:      pgInt2(r.FiveHour.Pct),
		FiveHourResetsAt: pgTimePtr(r.FiveHour.ResetsAt),
		SevenDayPct:      pgInt2(r.SevenDay.Pct),
		SevenDayResetsAt: pgTimePtr(r.SevenDay.ResetsAt),
		Source:           pgText(r.Source),
		SyncedAt:         pgTime(e.now().UTC()),
	}); err != nil {
		// The token id is safe to log — it is a row identifier, never the credential.
		e.logger.Error("usage poller: upsert", "user", userID.String(), "secret", secretID.String(), "error", err)
	}
}

// The backoff map is keyed by SECRET id since M5 (it was user id under PRD #53's
// per-user gauge): one refusing credential must not silence its owner's other
// meters. secretID names the token being backed off, not its owner.
func (e *Engine) inBackoff(secretID uuid.UUID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	until, ok := e.backoff[secretID]
	return ok && e.now().Before(until)
}

func (e *Engine) setBackoff(secretID uuid.UUID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.backoff[secretID] = e.now().Add(backoffDuration)
}

func (e *Engine) clearBackoff(secretID uuid.UUID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.backoff, secretID)
}

func pgInt2(v int) pgtype.Int2              { return pgtype.Int2{Int16: int16(v), Valid: true} }
func pgTime(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
func pgText(s string) pgtype.Text           { return pgtype.Text{String: s, Valid: true} }

func pgTimePtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// wipe best-effort zeroizes the token after use (matching the vault's own hygiene;
// Go gives no guarantee the bytes weren't already copied by the runtime).
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
