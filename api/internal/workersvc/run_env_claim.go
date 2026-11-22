package workersvc

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
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
	governing, leadLane := runEnvGoverningRun(run)
	if governing == uuid.Nil {
		return nil
	}
	capable := slices.Contains(wkr.ProtocolCapabilities, capability.RunEnvV1)
	if !leadLane {
		return s.attachCrossCheckRedact(ctx, run, governing, capable, payload)
	}
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
		// The run's owner is the only owner a value is opened for: a row of another owner is
		// never delivered, whatever the query returned.
		if row.UserID != run.UserID {
			return errRunEnvHistoryUnavailable
		}
		scope := runenv.ScopeUser
		var repoID *uuid.UUID
		if row.RepoID.Valid {
			scope = runenv.ScopeRepo
			id := uuid.UUID(row.RepoID.Bytes)
			repoID = &id
		}
		kind, aerr := runenv.AADFor(row.AadVersion, run.UserID, scope, repoID, row.Secret, row.Name, row.VarID, row.Rev)
		if aerr != nil {
			return fmt.Errorf("%w: %w", errRunEnvHistoryUnavailable, aerr)
		}
		plain, oerr := secretopen.OpenSealed(s.vlt, s.box, run.UserID, kind, row.SealedWith, row.Ciphertext)
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
		plain, err := s.openRunEnvRedact(run.UserID, runEnvRedactRow{
			VarID: row.VarID, Rev: row.Rev, Name: row.Name, RowID: row.RowID, Ciphertext: row.Ciphertext,
			SealedWith: row.SealedWith, AadVersion: row.AadVersion, RowSecret: row.RowSecret,
			RowRepoID: row.RowRepoID, RowUserID: row.RowUserID, RowName: row.RowName,
		})
		if err != nil {
			return err
		}
		snap.redact = append(snap.redact, runEnvEntry{VarID: row.VarID, Rev: row.Rev, Scope: row.Scope, Name: row.Name, Secret: true})
		payload.Secrets.RunEnvRedact = append(payload.Secrets.RunEnvRedact, ClaimRunEnvRedact{
			VarID: row.VarID.String(), Scope: row.Scope, Rev: row.Rev, Name: row.Name, Value: plain,
		})
	}
	return nil
}

// runEnvGoverningRun answers which run's receipts govern what this run's output must mask: a
// lead-lane run is governed by itself (leadLane true), a cross-check child by the lead it checks
// (its target_run_id), and every other run by none (uuid.Nil). A cross-check child is never given
// run_env and writes no receipts of its own: its checker reads the lead's code and output, so it
// must mask what the lead was given without being given a value itself.
func runEnvGoverningRun(run store.Run) (governing uuid.UUID, leadLane bool) {
	switch {
	case RunEnvDelivered(run.Kind, run.ReviewTargetRunID.Valid, run.EgressProfileID.Valid):
		return run.ID, true
	case run.Kind == runkind.CrossCheck && run.TargetRunID.Valid:
		return uuid.UUID(run.TargetRunID.Bytes), false
	}
	return uuid.Nil, false
}

// errCheckerRunEnvUnavailable ends a cross-check child as checker_unavailable (a non-pass that
// forces a human gate) when the lead's secret history cannot be masked in the checker's output:
// the claiming worker lacks run_env_v1, or a receipted version is missing or cannot be opened.
// It wraps errCheckerPinUnavailable so the claim finish decides the check lead-first; it
// deliberately does not wrap errRunEnvHistoryUnavailable, which would park the child instead.
var errCheckerRunEnvUnavailable = fmt.Errorf("%w: the lead's run environment cannot be masked", errCheckerPinUnavailable)

// attachCrossCheckRedact fills payload.Secrets.RunEnvRedact for a cross-check child with every
// secret version the target (lead) run holds a receipt for, and nothing else: never run_env, and
// no snapshot, so the finish records no receipts for the child. A locked vault is the claim
// lane's transient requeue (errVaultLocked).
func (s *Service) attachCrossCheckRedact(ctx context.Context, run store.Run, target uuid.UUID, capable bool, payload *ClaimPayload) error {
	rows, err := s.q.ListRunEnvSecretReceiptVersionsForRun(ctx, target)
	if err != nil {
		return fmt.Errorf("run env redact receipts: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	if !capable {
		return errCheckerRunEnvUnavailable
	}
	for _, row := range rows {
		plain, err := s.openRunEnvRedact(run.UserID, runEnvRedactRow{
			VarID: row.VarID, Rev: row.Rev, Name: row.Name, RowID: row.RowID, Ciphertext: row.Ciphertext,
			SealedWith: row.SealedWith, AadVersion: row.AadVersion, RowSecret: row.RowSecret,
			RowRepoID: row.RowRepoID, RowUserID: row.RowUserID, RowName: row.RowName,
		})
		switch {
		case errors.Is(err, errVaultLocked):
			return err
		case err != nil:
			return errCheckerRunEnvUnavailable
		}
		payload.Secrets.RunEnvRedact = append(payload.Secrets.RunEnvRedact, ClaimRunEnvRedact{
			VarID: row.VarID.String(), Scope: row.Scope, Rev: row.Rev, Name: row.Name, Value: plain,
		})
	}
	return nil
}

// runEnvRedactRow is a receipt with the owner's stored row, as both redact reads return it.
// The row columns are NULL (invalid) when no row of the run's owner holds the version.
type runEnvRedactRow struct {
	VarID      uuid.UUID
	Rev        int32
	Name       string
	RowID      pgtype.UUID
	Ciphertext []byte
	SealedWith pgtype.Text
	AadVersion pgtype.Int2
	RowSecret  pgtype.Bool
	RowRepoID  pgtype.UUID
	RowUserID  pgtype.UUID
	RowName    pgtype.Text
}

// openRunEnvRedact opens one receipted secret version for masking. The value is opened for the
// RUN's owner only, with a context built from that owner (never from the row): a missing
// version, a name that no longer matches the receipt, a row that is not a secret or one of
// another owner means the history cannot be masked completely (errRunEnvHistoryUnavailable).
// The AAD binds the owner, scope, repo, secret flag, name, var id and rev (v2), so a ciphertext
// copied from another version of the variable fails to open here.
func (s *Service) openRunEnvRedact(owner uuid.UUID, row runEnvRedactRow) (string, error) {
	if !row.RowID.Valid || !row.RowName.Valid || row.RowName.String != row.Name ||
		!row.RowSecret.Valid || !row.RowSecret.Bool || !row.SealedWith.Valid ||
		!row.AadVersion.Valid || !row.RowUserID.Valid || uuid.UUID(row.RowUserID.Bytes) != owner {
		return "", errRunEnvHistoryUnavailable
	}
	scope := runenv.ScopeUser
	var repoID *uuid.UUID
	if row.RowRepoID.Valid {
		scope = runenv.ScopeRepo
		id := uuid.UUID(row.RowRepoID.Bytes)
		repoID = &id
	}
	kind, aerr := runenv.AADFor(row.AadVersion.Int16, owner, scope, repoID, true, row.RowName.String, row.VarID, row.Rev)
	if aerr != nil {
		return "", fmt.Errorf("%w: %w", errRunEnvHistoryUnavailable, aerr)
	}
	plain, oerr := secretopen.OpenSealed(s.vlt, s.box, owner, kind, row.SealedWith.String, row.Ciphertext)
	if oerr != nil {
		return "", mapRunEnvOpenError(oerr)
	}
	return string(plain), nil
}

// ErrRunEnvVaultLocked, ErrRunEnvHistoryUnavailable and ErrRunEnvRedactClosed are the redact
// maintenance route's typed refusals: the owner's vault is locked, a receipted version is missing
// or cannot be opened, or the requested run is past the point the route answers for it (a
// terminal run with no open custody hold, no capture preparing or uploading, no held publication
// in flight and no terminal preservation bound or publishing, or a cross-check child that is
// terminal or whose check is no longer open, with the same exceptions). None carries a value.
var (
	ErrRunEnvVaultLocked        = errors.New("run environment redact: vault locked")
	ErrRunEnvHistoryUnavailable = errors.New("run environment redact: history unavailable")
	ErrRunEnvRedactClosed       = errors.New("run environment redact: closed for this run")
)

// RunEnvRedact answers the redact maintenance route (PRD #4416 M2): every secret version the
// governing run (runEnvGoverningRun: the run itself in the lead lane, the lead for a cross-check
// child) was ever delivered, opened for the run's owner, so a worker can mask the run's output
// after the claim that delivered them. Protected is false, with no versions, when the run has no
// governing run or the governing run has no secret receipt. The run must be the worker's user's
// and one this worker holds or last held (runs.worker_id), or one whose open recovery custody
// hold this worker took (the worker recovering a run another claim has since moved); anything
// else is ErrRunNotOwned. When versions would be returned, the requested run must still be open
// (RunEnvRedactOpen), else ErrRunEnvRedactClosed. The route is read-only, so there is no
// generation or repo check. The result carries plaintext secrets: the caller must never log or
// cache it.
func (s *Service) RunEnvRedact(ctx context.Context, wkr store.Worker, runID uuid.UUID) (RunEnvRedactResponse, error) {
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: wkr.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return RunEnvRedactResponse{}, ErrRunNotOwned
	}
	if err != nil {
		return RunEnvRedactResponse{}, fmt.Errorf("run env redact run: %w", err)
	}
	if run.WorkerID != pgconv.UUID(wkr.ID) {
		// An open custody hold the worker took on this run also authorizes it; an
		// inventory-guarded hold additionally requires the worker still to be its live holder.
		held, err := s.q.WorkerHoldsOpenCustodyForRun(ctx, store.WorkerHoldsOpenCustodyForRunParams{
			RunID: runID, UserID: wkr.UserID, WorkerID: wkr.ID,
		})
		if err != nil {
			return RunEnvRedactResponse{}, fmt.Errorf("run env redact custody: %w", err)
		}
		if !held {
			return RunEnvRedactResponse{}, ErrRunNotOwned
		}
	}
	governing, _ := runEnvGoverningRun(run)
	if governing == uuid.Nil {
		return RunEnvRedactResponse{Protected: false}, nil
	}
	rows, err := s.q.ListRunEnvSecretReceiptVersionsForRun(ctx, governing)
	if err != nil {
		return RunEnvRedactResponse{}, fmt.Errorf("run env redact receipts: %w", err)
	}
	if len(rows) == 0 {
		return RunEnvRedactResponse{Protected: false}, nil
	}
	open, err := s.q.RunEnvRedactOpen(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunEnvRedactResponse{}, ErrRunNotOwned
	}
	if err != nil {
		return RunEnvRedactResponse{}, fmt.Errorf("run env redact open: %w", err)
	}
	if !open {
		return RunEnvRedactResponse{}, ErrRunEnvRedactClosed
	}
	out := RunEnvRedactResponse{Protected: true, Versions: make([]ClaimRunEnvRedact, 0, len(rows))}
	for _, row := range rows {
		plain, err := s.openRunEnvRedact(run.UserID, runEnvRedactRow{
			VarID: row.VarID, Rev: row.Rev, Name: row.Name, RowID: row.RowID, Ciphertext: row.Ciphertext,
			SealedWith: row.SealedWith, AadVersion: row.AadVersion, RowSecret: row.RowSecret,
			RowRepoID: row.RowRepoID, RowUserID: row.RowUserID, RowName: row.RowName,
		})
		switch {
		case errors.Is(err, errVaultLocked):
			return RunEnvRedactResponse{}, ErrRunEnvVaultLocked
		case err != nil:
			return RunEnvRedactResponse{}, ErrRunEnvHistoryUnavailable
		}
		out.Versions = append(out.Versions, ClaimRunEnvRedact{
			VarID: row.VarID.String(), Scope: row.Scope, Rev: row.Rev, Name: row.Name, Value: plain,
		})
	}
	return out, nil
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

// runEnvReceiptReader is the receipt read behind the run page, satisfied by *store.Queries.
type runEnvReceiptReader interface {
	ListRunEnvReceiptsForRunGeneration(ctx context.Context, runID uuid.UUID) ([]store.ListRunEnvReceiptsForRunGenerationRow, error)
}

// InjectedRunEnv lists what the run's current claim generation delivered (names, scope and the
// secret flag from the delivery receipts; no value, no ciphertext, no vault key). The caller
// decides who may see it. A store without the receipt table reads as nothing delivered.
func (s *Service) InjectedRunEnv(ctx context.Context, runID uuid.UUID) ([]apitypes.InjectedEnvVarDTO, error) {
	rr, ok := s.q.(runEnvReceiptReader)
	if !ok {
		return nil, nil
	}
	rows, err := rr.ListRunEnvReceiptsForRunGeneration(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]apitypes.InjectedEnvVarDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, apitypes.InjectedEnvVarDTO{Name: r.Name, Scope: r.Scope, Secret: r.Secret})
	}
	return out, nil
}
