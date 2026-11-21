package workersvc

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// RunEnvDelivered reports whether a run of this shape may receive run environment variables
// (PRD #4416 M2): the lead kinds that execute repo code, never a task-review run and never an
// isolated-lane run. It is leadcfg.LeadLane by definition; the SQL mirror is fn_run_env_lane,
// and a LiveDB pin test asserts the two agree for every run kind.
func RunEnvDelivered(kind string, reviewTarget, egressBound bool) bool {
	return leadcfg.LeadLane(kind, reviewTarget, egressBound)
}

// runEnvEntry is one delivered or redact-only version as the snapshot records it. It carries
// no value: the snapshot lives on the assembled claim and is compared and written as receipts.
type runEnvEntry struct {
	VarID  uuid.UUID
	Rev    int32
	Scope  string
	Name   string
	Secret bool
}

func (e runEnvEntry) key() runEnvKey { return runEnvKey{VarID: e.VarID, Rev: e.Rev} }

type runEnvKey struct {
	VarID uuid.UUID
	Rev   int32
}

// runEnvSnapshot is the (var_id, rev) snapshot of both claim lists. effective is everything
// delivered in run_env; redact is the secret versions delivered in run_env_redact. capable is
// whether the claiming worker advertised run_env_v1 when the claim was assembled.
type runEnvSnapshot struct {
	effective []runEnvEntry
	redact    []runEnvEntry
	capable   bool
}

// attachRunEnv fills payload.Secrets.RunEnv and RunEnvRedact for a run that receives run
// environment variables, and records the snapshot finishRunClaimTx re-checks. It returns
// errVaultLocked (requeue) for a locked vault, errRunEnvCapabilityMissing (requeue) when the
// gate predicate holds on a worker without run_env_v1, and errRunEnvHistoryUnavailable (park)
// when a receipted version is missing, swapped or cannot be opened.
func (s *Service) attachRunEnv(ctx context.Context, wkr store.Worker, run store.Run, payload *ClaimPayload) error {
	if !RunEnvDelivered(run.Kind, run.ReviewTargetRunID.Valid, run.EgressProfileID.Valid) {
		return nil
	}
	capable := slices.Contains(wkr.ProtocolCapabilities, capability.RunEnvV1)
	eff, err := s.q.ListRunEnvEffectiveForRun(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("run env effective: %w", err)
	}
	red, err := s.q.ListRunEnvRedactForRun(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("run env redact: %w", err)
	}
	snap := &runEnvSnapshot{capable: capable}
	payload.runEnv = snap
	// The gate predicate of ClaimRun: an enabled live repo variable, or a secret receipt.
	// A redact row exists only for a secret receipt, and a secret receipt that is not redact
	// is in the effective set, so the two lists decide it.
	if len(eff) == 0 && len(red) == 0 {
		return nil
	}
	if !capable {
		return errRunEnvCapabilityMissing
	}
	for _, row := range eff {
		scope := runenv.ScopeUser
		var repoID *uuid.UUID
		if row.RepoID.Valid {
			scope = runenv.ScopeRepo
			id := uuid.UUID(row.RepoID.Bytes)
			repoID = &id
		}
		kind, aerr := runenv.AADFor(row.AadVersion, row.UserID, scope, repoID, row.Secret, row.Name, row.VarID, row.Rev)
		if aerr != nil {
			return fmt.Errorf("%w: %w", errRunEnvHistoryUnavailable, aerr)
		}
		plain, oerr := secretopen.OpenSealed(s.vlt, s.box, row.UserID, kind, row.SealedWith, row.Ciphertext)
		if oerr != nil {
			return mapRunEnvOpenError(oerr)
		}
		entry := runEnvEntry{VarID: row.VarID, Rev: row.Rev, Scope: scope, Name: row.Name, Secret: row.Secret}
		snap.effective = append(snap.effective, entry)
		payload.Secrets.RunEnv = append(payload.Secrets.RunEnv, ClaimRunEnvVar{
			VarID: row.VarID.String(), Scope: scope, Rev: row.Rev, Name: row.Name,
			Value: string(plain), Secret: row.Secret, Description: row.Description,
		})
	}
	seen := make(map[runEnvKey]bool, len(red))
	for _, row := range red {
		k := runEnvKey{VarID: row.VarID, Rev: row.Rev}
		if seen[k] {
			continue
		}
		seen[k] = true
		// Missing version, a name that no longer matches the receipt, or a row that is not a
		// secret: the history cannot be masked completely.
		if !row.RowID.Valid || !row.RowName.Valid || row.RowName.String != row.Name ||
			!row.RowSecret.Valid || !row.RowSecret.Bool || !row.SealedWith.Valid ||
			!row.AadVersion.Valid || !row.RowUserID.Valid {
			return errRunEnvHistoryUnavailable
		}
		scope := runenv.ScopeUser
		var repoID *uuid.UUID
		if row.RowRepoID.Valid {
			scope = runenv.ScopeRepo
			id := uuid.UUID(row.RowRepoID.Bytes)
			repoID = &id
		}
		owner := uuid.UUID(row.RowUserID.Bytes)
		// The AAD binds the owner, scope, repo, secret flag, name, var id and rev (v2), so a
		// ciphertext copied from another version of the variable fails to open here.
		kind, aerr := runenv.AADFor(row.AadVersion.Int16, owner, scope, repoID, true, row.RowName.String, row.VarID, row.Rev)
		if aerr != nil {
			return fmt.Errorf("%w: %w", errRunEnvHistoryUnavailable, aerr)
		}
		plain, oerr := secretopen.OpenSealed(s.vlt, s.box, owner, kind, row.SealedWith.String, row.Ciphertext)
		if oerr != nil {
			return mapRunEnvOpenError(oerr)
		}
		snap.redact = append(snap.redact, runEnvEntry{VarID: row.VarID, Rev: row.Rev, Scope: row.Scope, Name: row.Name, Secret: true})
		payload.Secrets.RunEnvRedact = append(payload.Secrets.RunEnvRedact, ClaimRunEnvRedact{
			VarID: row.VarID.String(), Scope: row.Scope, Rev: row.Rev, Name: row.Name, Value: string(plain),
		})
	}
	return nil
}

// mapRunEnvOpenError maps a secretopen failure to the claim lane's domain errors: a locked
// vault requeues, anything that cannot be opened parks the run.
func mapRunEnvOpenError(err error) error {
	if errors.Is(err, secretopen.ErrVaultLocked) {
		return errVaultLocked
	}
	return fmt.Errorf("%w: a value could not be opened", errRunEnvHistoryUnavailable)
}

// runEnvFinishQueries is the statement surface of the finish-time recompute and receipt write.
type runEnvFinishQueries interface {
	ListRunEnvEffectiveKeysForRun(ctx context.Context, runID uuid.UUID) ([]store.ListRunEnvEffectiveKeysForRunRow, error)
	ListRunEnvRedactKeysForRun(ctx context.Context, runID uuid.UUID) ([]store.ListRunEnvRedactKeysForRunRow, error)
	UpsertRunEnvReceipt(ctx context.Context, arg store.UpsertRunEnvReceiptParams) error
}

// checkRunEnv is finishRunClaimTx's step under the owner's users-row share lock: it recomputes
// both lists and compares them with the assembly snapshot. A non-nil error return is a
// database failure (the transaction rolls back); a non-nil decision is a classification
// (errRunEnvStale, errRunEnvCapabilityMissing, errRunEnvHistoryUnavailable) and nothing is
// delivered.
func checkRunEnv(ctx context.Context, q runEnvFinishQueries, locked store.Run, snap *runEnvSnapshot) (decision error, err error) {
	eff, err := q.ListRunEnvEffectiveKeysForRun(ctx, locked.ID)
	if err != nil {
		return nil, fmt.Errorf("run env effective keys: %w", err)
	}
	red, err := q.ListRunEnvRedactKeysForRun(ctx, locked.ID)
	if err != nil {
		return nil, fmt.Errorf("run env redact keys: %w", err)
	}
	for _, r := range red {
		if !r.Present {
			return errRunEnvHistoryUnavailable, nil
		}
	}
	if !runEnvKeysMatch(snap.effective, eff, func(r store.ListRunEnvEffectiveKeysForRunRow) runEnvKey {
		return runEnvKey{VarID: r.VarID, Rev: r.Rev}
	}) || !runEnvKeysMatch(snap.redact, red, func(r store.ListRunEnvRedactKeysForRunRow) runEnvKey {
		return runEnvKey{VarID: r.VarID, Rev: r.Rev}
	}) {
		return errRunEnvStale, nil
	}
	if (len(eff) > 0 || len(red) > 0) && !snap.capable {
		return errRunEnvCapabilityMissing, nil
	}
	return nil, nil
}

// writeRunEnvReceipts appends one receipt per version the claim delivers in run_env (plain
// and secret) at this claim generation, in the finish transaction that commits the claim. The
// redact-only versions already hold receipts from the claims that delivered them; they are
// not rewritten, so the generation-scoped run page lists only what this claim injected.
func writeRunEnvReceipts(ctx context.Context, q runEnvFinishQueries, locked store.Run, snap *runEnvSnapshot) error {
	for _, e := range snap.effective {
		if err := q.UpsertRunEnvReceipt(ctx, store.UpsertRunEnvReceiptParams{
			RunID: locked.ID, UserID: locked.UserID, VarID: e.VarID, Rev: e.Rev,
			Scope: e.Scope, Name: e.Name, Secret: e.Secret, Generation: locked.ClaimGeneration,
		}); err != nil {
			return fmt.Errorf("run env receipt: %w", err)
		}
	}
	return nil
}

// runEnvKeysMatch compares a snapshot list with a recomputed one as sets of (var_id, rev).
func runEnvKeysMatch[T any](snap []runEnvEntry, now []T, key func(T) runEnvKey) bool {
	if len(snap) != len(now) {
		return false
	}
	want := make(map[runEnvKey]bool, len(snap))
	for _, e := range snap {
		want[e.key()] = true
	}
	for _, r := range now {
		if !want[key(r)] {
			return false
		}
	}
	return true
}
