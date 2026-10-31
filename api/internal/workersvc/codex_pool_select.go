package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/codexselect"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// codex_pool_select.go is the I/O half of Codex account auto-selection (issue #3719): it
// gathers one codexselect.Candidate per pooled account inside the run-creation transaction
// and hands them to the pure codexselect.Select. createRunAtomic owns the lock order around
// it (shared secret-mutation lock, run INSERT, alias fence, freeze, account check).

// codexSelectMaxFenceAttempts bounds the fence's drop-and-reselect loop. The fence cannot fail
// under the shared secret-mutation lock, so the bound only caps a defensive path.
const codexSelectMaxFenceAttempts = 3

// codexPoolPick is the account and alias a selection bound, plus the identity tuple the
// post-freeze check compares against the run's frozen codex_account_key.
type codexPoolPick struct {
	AliasID            uuid.UUID
	AccountID          uuid.UUID
	ProviderUserID     string
	WorkspaceAccountID string
	Reason             codexselect.Reason
}

// selectCodexPoolAlias scores the user's pooled accounts and picks one. It re-reads the pool,
// readings, weekly reserve and in-flight counts on q, so it must run under the shared
// secret-mutation lock. Aliases in exclude are treated as not pooled. It returns
// errCodexPoolUnavailable when nothing is selectable.
//
// Per account: the alias is the lowest pooled, enabled, linked alias id; structural exclusions
// (see codexPoolRowStructural) drop the account; capacity exclusions (a fresh current reading
// with limit_reached or allowed=false, an admission cooldown still in force including its
// spread tail, a weekly-reserve hold on the alias) leave it reachable only via pool_exhausted.
// The in-flight count is CountCodexInFlightByAccount's nonterminal set (rationale in the SQL).
func (s *Service) selectCodexPoolAlias(ctx context.Context, q *store.Queries, userID uuid.UUID, exclude map[uuid.UUID]bool) (codexPoolPick, error) {
	now := s.selectionNow()
	rows, err := q.ListCodexPoolAliases(ctx, userID)
	if err != nil {
		return codexPoolPick{}, fmt.Errorf("codex pool select: list pool: %w", err)
	}

	type account struct {
		row store.ListCodexPoolAliasesRow // the lowest structural alias of the account
	}
	accounts := map[uuid.UUID]*account{}
	var order []uuid.UUID
	for _, r := range rows {
		if exclude[r.AliasID] || !codexPoolRowStructural(r) {
			continue
		}
		id := uuid.UUID(r.ProviderAccountID.Bytes)
		a, ok := accounts[id]
		if !ok {
			accounts[id] = &account{row: r}
			order = append(order, id)
			continue
		}
		if bytes.Compare(r.AliasID[:], a.row.AliasID[:]) < 0 {
			a.row = r
		}
	}
	if len(accounts) == 0 {
		return codexPoolPick{}, errCodexPoolUnavailable
	}

	readings, err := q.GetCodexAccountRateLimitsForUser(ctx, userID)
	if err != nil {
		return codexPoolPick{}, fmt.Errorf("codex pool select: read limits: %w", err)
	}
	byAccount := make(map[uuid.UUID]store.GetCodexAccountRateLimitsForUserRow, len(readings))
	for _, r := range readings {
		byAccount[r.ProviderAccountID] = r
	}
	inflightRows, err := q.CountCodexInFlightByAccount(ctx, userID)
	if err != nil {
		return codexPoolPick{}, fmt.Errorf("codex pool select: count in-flight: %w", err)
	}
	inflight := make(map[uuid.UUID]int, len(inflightRows))
	for _, r := range inflightRows {
		inflight[r.ProviderAccountID] = int(r.InFlight)
	}
	snap, err := buildReserveSnapshot(ctx, q, userID, now, s.reserveIntervals())
	if err != nil {
		return codexPoolPick{}, fmt.Errorf("codex pool select: %w", err)
	}
	interval := s.p.CodexUsagePollInterval
	// Vault state is read on the create transaction (q), never through the pool: a second
	// connection would wait on locks this transaction holds.
	vaultLocked := !s.codexVaultUnlocked(vault.WithStateReader(ctx, q), userID)

	cands := make([]codexselect.Candidate, 0, len(order))
	for _, id := range order {
		row := accounts[id].row
		c := codexselect.Candidate{AccountKey: id, AliasID: row.AliasID, Structural: true, InFlight: inflight[id]}

		rl, haveRL := byAccount[id]
		var reading codexselect.Reading
		meterFresh := false
		if haveRL {
			var lastSuccess, lastAttempt *time.Time
			if rl.LastSuccessAt.Valid {
				t := rl.LastSuccessAt.Time
				lastSuccess = &t
			}
			if rl.LastAttemptAt.Valid {
				t := rl.LastAttemptAt.Time
				lastAttempt = &t
			}
			meterFresh = usagereserve.CodexMeterStatus(interval, vaultLocked, rl.ReauthRequired, lastSuccess, lastAttempt, now) == usagereserve.CodexMeterFresh
			// NULL buckets are a reading the D13 fence hides (not current): no reading.
			if lastSuccess != nil && len(rl.Buckets) > 0 {
				var buckets []apitypes.CodexRateLimitBucketDTO
				// An undecodable body is "no reading": it can only make the score unknown.
				if json.Unmarshal(rl.Buckets, &buckets) == nil {
					reading = codexselect.Reading{Present: true, Buckets: buckets, LastSuccessAt: lastSuccess}
				}
			}
		}
		c.Score, c.ResetAt, c.ScoreKnown = codexselect.Score(reading, meterFresh, interval, now)

		// Capacity. ReleaseAt is the latest known per-cause release time; any cause whose time
		// is unknown makes the whole release unknown.
		var release *time.Time
		releaseKnown := true
		addRelease := func(t *time.Time) {
			if t == nil {
				releaseKnown = false
				return
			}
			if release == nil || t.After(*release) {
				release = t
			}
		}
		if meterFresh && reading.Present && codexselect.LimitReached(reading.Buckets) {
			c.CapacityExcluded = true
			addRelease(c.ResetAt)
		}
		if row.ThrottleEffectiveExpiry.Valid {
			until := row.ThrottleEffectiveExpiry.Time
			if row.ThrottleSpreadWidthMs.Valid {
				until = until.Add(time.Duration(row.ThrottleSpreadWidthMs.Int32) * time.Millisecond)
			}
			if until.After(now) {
				c.CapacityExcluded = true
				addRelease(&until)
			}
		}
		if snap.codexBlocked(row.AliasID) {
			c.CapacityExcluded = true
			if v := snap.codex[row.AliasID]; v.State == usagereserve.Held {
				addRelease(v.ResetsAt)
			} else {
				addRelease(nil)
			}
		}
		c.ReleaseAt, c.ReleaseKnown = release, releaseKnown && release != nil
		cands = append(cands, c)
	}

	out, err := codexselect.Select(cands, s.p.Autoselect, now)
	if err != nil {
		return codexPoolPick{}, err
	}
	if !out.Picked {
		return codexPoolPick{}, errCodexPoolUnavailable
	}
	row := accounts[out.AccountKey].row
	return codexPoolPick{
		AliasID:            out.AliasID,
		AccountID:          out.AccountKey,
		ProviderUserID:     row.ProviderUserID.String,
		WorkspaceAccountID: row.WorkspaceAccountID.String,
		Reason:             out.Reason,
	}, nil
}

// fenceCodexPoolAlias is the consent and link fence: a share lock on the picked alias row and a
// re-read of what the pick relied on (owner, kind, pool consent, enabled, linked, the scored
// account). A pick that fails is dropped and the pool re-selected from a fresh read, at most
// codexSelectMaxFenceAttempts times; when nothing remains it returns errCodexPoolUnavailable,
// never a credential-disabled error. Retrying holds several alias share locks, which cannot
// deadlock because the caller holds the shared secret-mutation lock that every multi-row
// writer takes exclusively.
func (s *Service) fenceCodexPoolAlias(ctx context.Context, q *store.Queries, userID uuid.UUID, pick codexPoolPick) (codexPoolPick, error) {
	dropped := map[uuid.UUID]bool{}
	for attempt := 0; ; attempt++ {
		ok, err := codexPoolPickHolds(ctx, q, userID, pick)
		if err != nil {
			return codexPoolPick{}, err
		}
		if ok {
			return pick, nil
		}
		dropped[pick.AliasID] = true
		if attempt+1 >= codexSelectMaxFenceAttempts {
			return codexPoolPick{}, errCodexPoolUnavailable
		}
		pick, err = s.selectCodexPoolAlias(ctx, q, userID, dropped)
		if err != nil {
			return codexPoolPick{}, err
		}
	}
}

// codexPoolAccountUsable is the account-state half of structural usability: a present account
// that is neither quarantined nor awaiting re-authentication.
func codexPoolAccountUsable(coord pgtype.Text, reauth pgtype.Bool) bool {
	return coord.Valid && coord.String != codexCoordQuarantined && reauth.Valid && !reauth.Bool
}

func codexPoolPickHolds(ctx context.Context, q *store.Queries, userID uuid.UUID, pick codexPoolPick) (bool, error) {
	row, err := q.LockCodexPoolAliasForSelection(ctx, store.LockCodexPoolAliasForSelectionParams{ID: pick.AliasID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("codex pool select: fence: %w", err)
	}
	// Quarantine and reauth are not covered by the shared secret-mutation lock (the refresher
	// writes them without it), so they are re-read here, on the same statement as the lock.
	return row.Kind == store.KindCodexAuth && row.AutoEligible && row.Enabled &&
		row.Status.Valid && row.Status.String == codexStatusLinked &&
		row.ProviderAccountID.Valid && uuid.UUID(row.ProviderAccountID.Bytes) == pick.AccountID &&
		codexPoolAccountUsable(row.CoordState, row.ReauthRequired), nil
}

// checkFrozenCodexAccount compares the run's frozen codex_account_key with the identity tuple
// of the account that was scored. A mismatch is a defensive invariant failure (the shared lock
// makes it unreachable): it refuses, and the caller's rollback leaves no run.
func (s *Service) checkFrozenCodexAccount(ctx context.Context, q *store.Queries, userID, runID uuid.UUID, pick codexPoolPick) error {
	want, err := codexAccountKey(pick.ProviderUserID, pick.WorkspaceAccountID)
	if err != nil {
		return fmt.Errorf("codex pool select: encode account key: %w", err)
	}
	run, err := q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: userID})
	if err != nil {
		return fmt.Errorf("codex pool select: read frozen account: %w", err)
	}
	if !run.CodexAccountKey.Valid || run.CodexAccountKey.String != want {
		return errCodexPoolUnavailable
	}
	return nil
}

// selectionNow is the single instant one selection decides at.
func (s *Service) selectionNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
