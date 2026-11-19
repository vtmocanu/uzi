package workersvc

// claim_run_env.go: the repository variables a claim delivers (PRD #4416 M2).
//
// assembleClaim reads the effective injection set and the receipted secret versions, opens their
// values through the vault seam and attaches them to ClaimSecrets (run_env, run_env_redact), keeping
// the (var_id, rev) snapshot it assembled from in an unexported field that is never serialized.
// finishRunClaimTx then takes the owner's users-row fence, recomputes the same key lists and
// requeues the claim when they differ, so a change committed between the two is never half
// delivered; on a match it appends the delivery receipts in the same transaction as the commit.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var (
	// errRunEnvCapabilityMissing: the run receives variables and the claiming worker lacks
	// run_env_v1. Transient like errCustomModelCapabilityMissing: the run returns to queued for a
	// capable worker. ClaimRun's SQL clause makes this the first-variable race only.
	errRunEnvCapabilityMissing = errors.New("worker lacks run_env_v1 for a run that receives repository variables")
	// errRunEnvHistoryUnavailable: a secret version the run holds a receipt for is missing, or a
	// version could not be opened. The run parks (hold run_env_history_unavailable) instead of
	// running with a value the worker cannot mask.
	errRunEnvHistoryUnavailable = errors.New("a secret version this run holds a receipt for is missing or cannot be opened")
	// errRunEnvStale: the finish transaction's recompute differs from the assembled snapshot.
	// Transient: the run is requeued and the next claim assembles the current set.
	errRunEnvStale = errors.New("repository variables changed while the claim was assembled")
	// errRunEnvBounds: the set the rows describe is outside the worker decoder's bounds. The
	// store's write limits keep a valid set inside them, so this is a defense; it is terminal
	// (provisioning_failed) because a retry would assemble the same set.
	errRunEnvBounds = errors.New("run environment exceeds the worker decoder bounds")
)

// Bounds the worker's decoder enforces (agent/src/run-env-claim.ts); the claim must stay inside
// them or the worker rejects the whole claim.
const (
	runEnvClaimMaxVars         = 50
	runEnvClaimMaxValueBytes   = 65536
	runEnvClaimMaxTotalBytes   = 262144
	runEnvClaimMaxRedact       = 250
	runEnvClaimMaxRedactBytes  = 2359296
	runEnvClaimMinSecretLength = runenv.MinSecretLen
)

// runEnvReader is the assembly's read surface; *store.Queries satisfies it (the optional-interface
// idiom of this package, so a unit fake that models no variables is not forced to implement it).
type runEnvReader interface {
	ListRunEnvEffectiveForRun(ctx context.Context, arg store.ListRunEnvEffectiveForRunParams) ([]store.ListRunEnvEffectiveForRunRow, error)
	ListRunEnvRedactForRun(ctx context.Context, arg store.ListRunEnvRedactForRunParams) ([]store.ListRunEnvRedactForRunRow, error)
}

var _ runEnvReader = (*store.Queries)(nil)

// runEnvKey is the identity of one version.
type runEnvKey struct {
	VarID uuid.UUID
	Rev   int32
}

// runEnvRedactKey is a receipted secret version outside the delivered set and whether its row
// still exists: a version deleted between assembly and finish changes the key list.
type runEnvRedactKey struct {
	runEnvKey
	Present bool
}

// runEnvDelivery is one delivered variable, kept for the receipt written at finish.
type runEnvDelivery struct {
	runEnvKey
	Scope  string
	Name   string
	Secret bool
}

// runEnvSnapshot is the (var_id, rev) state a payload was assembled from. It lives only in memory.
type runEnvSnapshot struct {
	effective []runEnvKey
	redact    []runEnvRedactKey
	delivered []runEnvDelivery
}

func compareRunEnvKey(a, b runEnvKey) int {
	if c := slices.Compare(a.VarID[:], b.VarID[:]); c != 0 {
		return c
	}
	switch {
	case a.Rev < b.Rev:
		return -1
	case a.Rev > b.Rev:
		return 1
	}
	return 0
}

func compareRunEnvRedactKey(a, b runEnvRedactKey) int {
	if c := compareRunEnvKey(a.runEnvKey, b.runEnvKey); c != 0 {
		return c
	}
	switch {
	case !a.Present && b.Present:
		return -1
	case a.Present && !b.Present:
		return 1
	}
	return 0
}

func (a *runEnvSnapshot) equal(effective []runEnvKey, redact []runEnvRedactKey) bool {
	slices.SortFunc(effective, compareRunEnvKey)
	slices.SortFunc(redact, compareRunEnvRedactKey)
	return slices.Equal(a.effective, effective) && slices.Equal(a.redact, redact)
}

func runEnvRepoParam(run store.Run) pgtype.UUID { return run.RepoID }

func runEnvRepoUUID(run store.Run) uuid.UUID {
	if !run.RepoID.Valid {
		return uuid.Nil
	}
	return uuid.UUID(run.RepoID.Bytes)
}

// runEnvLane reports whether the run receives variables: exactly the lead lane.
func runEnvLane(run store.Run) bool {
	return leadcfg.RunEnvDelivered(run.Kind, run.ReviewTargetRunID.Valid, run.EgressProfileID.Valid)
}

// openRunEnvValue opens one sealed value under the context its aad_version selects. A locked
// vault is errVaultLocked (a requeue); any other failure is errRunEnvHistoryUnavailable.
func (s *Service) openRunEnvValue(userID uuid.UUID, aad string, sealedWith string, ciphertext []byte) (string, error) {
	if sealedWith == store.SealedWithMaster {
		return "", errRunEnvHistoryUnavailable // the table never admits a nil-AAD seal
	}
	plain, err := secretopen.OpenSealed(s.vlt, s.box, userID, aad, sealedWith, ciphertext)
	if err != nil {
		if errors.Is(err, secretopen.ErrVaultLocked) {
			return "", errVaultLocked
		}
		return "", errRunEnvHistoryUnavailable
	}
	v := string(plain)
	for i := range plain {
		plain[i] = 0
	}
	return v, nil
}

// attachRunEnv assembles run_env and run_env_redact for a run that receives variables. It returns
// nil and leaves the payload alone for any other run, and for a lead-lane run with nothing to
// deliver (an empty snapshot is still recorded, so the finish recompute catches a first variable).
func (s *Service) attachRunEnv(ctx context.Context, wkr store.Worker, run store.Run, payload *ClaimPayload) error {
	if !runEnvLane(run) {
		return nil
	}
	rd, ok := s.q.(runEnvReader)
	if !ok {
		return nil
	}
	eff, err := rd.ListRunEnvEffectiveForRun(ctx, store.ListRunEnvEffectiveForRunParams{UserID: run.UserID, RepoID: runEnvRepoParam(run)})
	if err != nil {
		return err
	}
	red, err := rd.ListRunEnvRedactForRun(ctx, store.ListRunEnvRedactForRunParams{RunID: run.ID, UserID: run.UserID, RepoID: runEnvRepoUUID(run)})
	if err != nil {
		return err
	}
	snap := &runEnvSnapshot{}
	payload.runEnv = snap
	if len(eff) == 0 && len(red) == 0 {
		return nil
	}
	// The gate predicate every mirror evaluates: an enabled live repo variable, or a secret receipt.
	if !slices.Contains(wkr.ProtocolCapabilities, capability.RunEnvV1) {
		return errRunEnvCapabilityMissing
	}
	repoID := run.RepoID
	var repo *uuid.UUID
	if repoID.Valid {
		id := uuid.UUID(repoID.Bytes)
		repo = &id
	}
	var vars []ClaimRunEnvVar
	var redact []ClaimRunEnvRedact
	var totalValue, redactBytes int
	for _, row := range eff {
		// A name denied after the row was saved is skipped, never an error: the finish recompute
		// applies the same skip, so the two lists still agree. A secret so skipped is still masked
		// (it falls into the redact list below, keyed by its receipt, when one exists).
		if runenv.ValidateName(row.Name) != nil {
			continue
		}
		value, err := s.openRunEnvValue(run.UserID, runenv.AADFor(runenv.Row{AADVersion: row.AadVersion, RepoID: repo, Secret: row.Secret, Name: row.Name, VarID: row.VarID, Rev: row.Rev}), row.SealedWith, row.Ciphertext)
		if err != nil {
			return err
		}
		vars = append(vars, ClaimRunEnvVar{VarID: row.VarID, Scope: runenv.ScopeRepo, Rev: row.Rev, Name: row.Name, Value: value, Secret: row.Secret, Description: row.Description})
		snap.effective = append(snap.effective, runEnvKey{row.VarID, row.Rev})
		snap.delivered = append(snap.delivered, runEnvDelivery{runEnvKey{row.VarID, row.Rev}, runenv.ScopeRepo, row.Name, row.Secret})
		totalValue += len(value)
		if len(value) > runEnvClaimMaxValueBytes || (row.Secret && runenv.JSLength(value) < runEnvClaimMinSecretLength) {
			return fmt.Errorf("%w: a value is outside its bounds", errRunEnvBounds)
		}
	}
	effectiveNames := make(map[runEnvKey]bool, len(eff))
	for _, row := range eff {
		if runenv.ValidateName(row.Name) == nil {
			effectiveNames[runEnvKey{row.VarID, row.Rev}] = true
		}
	}
	for _, row := range red {
		key := runEnvKey{row.VarID, row.Rev}
		if row.Effective && effectiveNames[key] {
			continue // delivered in run_env
		}
		// The version must exist, be the very (var_id, rev) the receipt names and the secret
		// the receipt recorded: anything else is missing history, not a value to guess at.
		if !row.RowID.Valid || !row.RowVarID.Valid || uuid.UUID(row.RowVarID.Bytes) != row.VarID ||
			!row.RowRev.Valid || row.RowRev.Int32 != row.Rev || !row.RowSecret.Valid || !row.RowSecret.Bool ||
			!row.RowName.Valid || row.RowName.String != row.ReceiptName {
			return errRunEnvHistoryUnavailable
		}
		var rowRepo *uuid.UUID
		if row.RowRepoID.Valid {
			id := uuid.UUID(row.RowRepoID.Bytes)
			rowRepo = &id
		}
		value, err := s.openRunEnvValue(run.UserID, runenv.AADFor(runenv.Row{AADVersion: row.RowAadVersion.Int16, RepoID: rowRepo, Secret: true, Name: row.RowName.String, VarID: row.VarID, Rev: row.Rev}), row.RowSealedWith.String, row.RowCiphertext)
		if err != nil {
			return err
		}
		redact = append(redact, ClaimRunEnvRedact{VarID: row.VarID, Scope: row.Scope, Rev: row.Rev, Name: row.ReceiptName, Value: value})
		snap.redact = append(snap.redact, runEnvRedactKey{key, true})
		redactBytes += len(value)
		if len(value) > runEnvClaimMaxValueBytes || runenv.JSLength(value) < runEnvClaimMinSecretLength {
			return fmt.Errorf("%w: a value is outside its bounds", errRunEnvBounds)
		}
	}
	if len(vars) > runEnvClaimMaxVars || totalValue > runEnvClaimMaxTotalBytes ||
		len(redact) > runEnvClaimMaxRedact || redactBytes > runEnvClaimMaxRedactBytes {
		return fmt.Errorf("%w: %d variables (%d bytes), %d redact entries (%d bytes)", errRunEnvBounds, len(vars), totalValue, len(redact), redactBytes)
	}
	slices.SortFunc(snap.effective, compareRunEnvKey)
	slices.SortFunc(snap.redact, compareRunEnvRedactKey)
	payload.Secrets.RunEnv = vars
	payload.Secrets.RunEnvRedact = redact
	return nil
}

// runEnvFinishQueries is the statement surface of the finish transaction's recompute and receipts.
type runEnvFinishQueries interface {
	ListRunEnvEffectiveKeysForRun(ctx context.Context, arg store.ListRunEnvEffectiveKeysForRunParams) ([]store.ListRunEnvEffectiveKeysForRunRow, error)
	ListRunEnvRedactKeysForRun(ctx context.Context, arg store.ListRunEnvRedactKeysForRunParams) ([]store.ListRunEnvRedactKeysForRunRow, error)
	UpsertRunEnvReceipt(ctx context.Context, arg store.UpsertRunEnvReceiptParams) error
}

// runEnvSnapshotCurrent recomputes both key lists under the owner fence and reports whether they
// still equal the snapshot the payload was assembled from. A payload without a snapshot (a lane
// that receives no variables) is trivially current.
func runEnvSnapshotCurrent(ctx context.Context, q runEnvFinishQueries, locked store.Run, payload *ClaimPayload) (bool, error) {
	if payload == nil || payload.runEnv == nil {
		return true, nil
	}
	eff, err := q.ListRunEnvEffectiveKeysForRun(ctx, store.ListRunEnvEffectiveKeysForRunParams{UserID: locked.UserID, RepoID: runEnvRepoParam(locked)})
	if err != nil {
		return false, err
	}
	red, err := q.ListRunEnvRedactKeysForRun(ctx, store.ListRunEnvRedactKeysForRunParams{RunID: locked.ID, UserID: locked.UserID, RepoID: runEnvRepoUUID(locked)})
	if err != nil {
		return false, err
	}
	var effective []runEnvKey
	delivered := make(map[runEnvKey]bool, len(eff))
	for _, row := range eff {
		if runenv.ValidateName(row.Name) != nil {
			continue // the same skip as the assembly
		}
		k := runEnvKey{row.VarID, row.Rev}
		effective = append(effective, k)
		delivered[k] = true
	}
	var redact []runEnvRedactKey
	for _, row := range red {
		k := runEnvKey{row.VarID, row.Rev}
		if row.Effective && delivered[k] {
			continue
		}
		redact = append(redact, runEnvRedactKey{k, row.Present})
	}
	return payload.runEnv.equal(effective, redact), nil
}

// recordRunEnvReceipts appends one receipt per delivered version (plain ones too: the run page
// lists the names it received). Same transaction as the claim's commit.
func recordRunEnvReceipts(ctx context.Context, q runEnvFinishQueries, locked store.Run, payload *ClaimPayload) error {
	if payload == nil || payload.runEnv == nil {
		return nil
	}
	for _, d := range payload.runEnv.delivered {
		if err := q.UpsertRunEnvReceipt(ctx, store.UpsertRunEnvReceiptParams{
			RunID: locked.ID, UserID: locked.UserID, VarID: d.VarID, Rev: d.Rev,
			Scope: d.Scope, Name: d.Name, Secret: d.Secret, ClaimGeneration: locked.ClaimGeneration,
		}); err != nil {
			return err
		}
	}
	return nil
}
