package workersvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Repo variable (PRD #4416 M2) claim errors. All three are matched by identity in
// finishRunClaim / finishRunClaimTx.
var (
	// errRunEnvHistoryUnavailable: a secret version the run holds a receipt for (or a live value)
	// cannot be assembled (its row is gone, or it does not open under its sealing context), so the
	// redaction list would be incomplete. The claim parks the run on run_env_history_unavailable.
	errRunEnvHistoryUnavailable = errors.New("repo variable history is unavailable")
	// errRunEnvCapabilityMissing: the claiming worker lacks run_env_v1 while the run has variables
	// to deliver. Transient: the run returns to queued for a capable worker.
	errRunEnvCapabilityMissing = errors.New("worker does not support repo variables (run_env_v1)")
	// errRunEnvStale: the effective or redact version set changed between assembly and the claim's
	// finish. Transient: the run returns to queued and nothing was delivered.
	errRunEnvStale = errors.New("repo variables changed during claim assembly")
	// errRunEnvSnapshotMissing: a delivered run-env lane payload carries no assembly record. It is a bug
	// guard, never a transient condition, and the claim is not delivered.
	errRunEnvSnapshotMissing = errors.New("run-env lane claim has no assembly record")
)

// holdRunEnvHistoryUnavailable is the hold reason ParkRunEnvHistoryUnavailableRun writes.
const holdRunEnvHistoryUnavailable = "run_env_history_unavailable"

// RunEnvDelivered reports whether a run receives repo variables: exactly the lead lane
// (leadcfg.LeadLane), mirrored in SQL by fn_run_env_lane, which the claim gate and run health read.
func RunEnvDelivered(kind string, reviewTarget, egressBound bool) bool {
	return leadcfg.LeadLane(kind, reviewTarget, egressBound)
}

func runEnvDeliveredForRun(r store.Run) bool {
	return RunEnvDelivered(r.Kind, r.ReviewTargetRunID.Valid, r.EgressProfileID.Valid)
}

// runEnvDelivery names one (var_id, rev) a claim delivers; it carries no value.
type runEnvDelivery struct {
	varID  uuid.UUID
	rev    int32
	scope  string
	name   string
	secret bool
}

// runEnvSnapshot is the assembly metadata of one claim: which versions were put in the payload.
// It is unexported and tagged json:"-" on ClaimPayload, so it is never serialized. The finish
// transaction compares it with a recomputation under the owner fence and, on a match, writes one
// receipt per delivered version.
type runEnvSnapshot struct {
	live   []runEnvDelivery
	redact []runEnvDelivery
}

// runEnvReader is the read surface of the run-env assembly; *store.Queries satisfies it.
type runEnvReader interface {
	ListRunEnvEffectiveForRun(ctx context.Context, runID uuid.UUID) ([]store.ListRunEnvEffectiveForRunRow, error)
	ListRunEnvRedactForRun(ctx context.Context, runID uuid.UUID) ([]store.ListRunEnvRedactForRunRow, error)
}

type runEnvKey struct {
	varID uuid.UUID
	rev   int32
}

func (k runEnvKey) less(o runEnvKey) bool {
	if c := bytes.Compare(k.varID[:], o.varID[:]); c != 0 {
		return c < 0
	}
	return k.rev < o.rev
}

func sortedRunEnvKeys(d []runEnvDelivery) []runEnvKey {
	out := make([]runEnvKey, 0, len(d))
	for _, x := range d {
		out = append(out, runEnvKey{x.varID, x.rev})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].less(out[j]) })
	return out
}

// openRunEnvSealed opens one sealed run-env value under the sealing context its row version selects.
// A locked vault is errVaultLocked (a transient requeue); a value that does not open, or a row that
// cannot name its context, is errRunEnvHistoryUnavailable. No error carries the value.
func (s *Service) openRunEnvSealed(userID uuid.UUID, aadVersion int, scope string, repoID pgtype.UUID, secret bool, name string, varID uuid.UUID, rev int32, sealedWith string, ciphertext []byte) (string, error) {
	var repo *uuid.UUID
	if repoID.Valid {
		u := uuid.UUID(repoID.Bytes)
		repo = &u
	}
	aad, err := runenv.AADFor(aadVersion, scope, repo, secret, name, varID, int(rev))
	if err != nil {
		return "", errRunEnvHistoryUnavailable
	}
	plain, err := secretopen.OpenSealed(s.vlt, s.box, userID, aad, sealedWith, ciphertext)
	switch {
	case err == nil:
		return string(plain), nil
	case errors.Is(err, secretopen.ErrVaultLocked):
		return "", errVaultLocked
	default:
		return "", errRunEnvHistoryUnavailable
	}
}

// assembleRunEnv fills payload.Secrets.RunEnv and RunEnvRedact for a lead-lane run and records the
// delivered (var_id, rev) lists in payload.runEnv. Other runs are left untouched. It runs after the
// Codex credential block, so every error is wrapped with the minted identity (crossCheckAssemblyError).
func (s *Service) assembleRunEnv(ctx context.Context, wkr store.Worker, run store.Run, payload *ClaimPayload) error {
	if !runEnvDeliveredForRun(run) {
		return nil
	}
	rd := s.q // Store embeds runEnvReader: the step cannot be skipped on a store that lacks the reads
	live, err := rd.ListRunEnvEffectiveForRun(ctx, run.ID)
	if err != nil {
		return err
	}
	redactRows, err := rd.ListRunEnvRedactForRun(ctx, run.ID)
	if err != nil {
		return err
	}
	snap := &runEnvSnapshot{}
	payload.runEnv = snap
	if len(live) == 0 && len(redactRows) == 0 {
		return nil
	}
	if !slices.Contains(wkr.ProtocolCapabilities, capability.RunEnvV1) {
		return crossCheckAssemblyError(payload, errRunEnvCapabilityMissing)
	}
	seen := map[runEnvKey]bool{}
	for _, row := range live {
		k := runEnvKey{row.VarID, row.Rev}
		if seen[k] {
			continue
		}
		seen[k] = true
		value, err := s.openRunEnvSealed(run.UserID, int(row.AadVersion), row.Scope, row.RepoID, row.Secret, row.Name, row.VarID, row.Rev, row.SealedWith, row.Ciphertext)
		if err != nil {
			return crossCheckAssemblyError(payload, err)
		}
		payload.Secrets.RunEnv = append(payload.Secrets.RunEnv, ClaimRunEnvVar{
			VarID: row.VarID, Scope: row.Scope, Rev: int(row.Rev), Name: row.Name,
			Value: value, Secret: row.Secret, Description: row.Description,
		})
		snap.live = append(snap.live, runEnvDelivery{varID: row.VarID, rev: row.Rev, scope: row.Scope, name: row.Name, secret: row.Secret})
	}
	for _, row := range redactRows {
		k := runEnvKey{row.VarID, row.Rev}
		if seen[k] {
			continue
		}
		seen[k] = true
		if row.Ciphertext == nil || !row.SealedWith.Valid || !row.AadVersion.Valid {
			return crossCheckAssemblyError(payload, errRunEnvHistoryUnavailable)
		}
		value, err := s.openRunEnvSealed(run.UserID, int(row.AadVersion.Int16), row.Scope, row.RepoID, true, row.Name, row.VarID, row.Rev, row.SealedWith.String, row.Ciphertext)
		if err != nil {
			return crossCheckAssemblyError(payload, err)
		}
		payload.Secrets.RunEnvRedact = append(payload.Secrets.RunEnvRedact, ClaimRunEnvRedact{
			VarID: row.VarID, Scope: row.Scope, Rev: int(row.Rev), Name: row.Name, Value: value,
		})
		snap.redact = append(snap.redact, runEnvDelivery{varID: row.VarID, rev: row.Rev, scope: row.Scope, name: row.Name, secret: true})
	}
	return nil
}

// runEnvFinishQueries is the statement surface verifyRunEnvAtFinish needs; claimFinishQueries embeds it.
type runEnvFinishQueries interface {
	ListRunEnvEffectiveKeysForRun(ctx context.Context, runID uuid.UUID) ([]store.ListRunEnvEffectiveKeysForRunRow, error)
	ListRunEnvRedactKeysForRun(ctx context.Context, runID uuid.UUID) ([]store.ListRunEnvRedactKeysForRunRow, error)
	UpsertRunEnvReceipt(ctx context.Context, arg store.UpsertRunEnvReceiptParams) error
}

// verifyRunEnvAtFinish runs inside the claim's finish transaction, after the run lock and the owner
// share lock. It recomputes both version sets with the key-only queries; a difference from the
// assembly snapshot is errRunEnvStale (nothing was delivered). A non-empty set on a worker without
// run_env_v1 is errRunEnvCapabilityMissing. Otherwise it writes one receipt per delivered version,
// secret and plain, advancing last_claim_generation for a re-delivery.
func verifyRunEnvAtFinish(ctx context.Context, q runEnvFinishQueries, locked store.Run, payload *ClaimPayload, workerCaps []string) error {
	snap := payload.runEnv
	if snap == nil {
		// assembleClaim runs assembleRunEnv for every run in the lane, and stripForIsolation (which clears
		// the snapshot) only applies to profile-bound runs, which are outside the lane. A lane payload
		// without a snapshot was not assembled, so nothing it carries can be vouched for: fail the finish.
		return errRunEnvSnapshotMissing
	}
	liveKeys, err := q.ListRunEnvEffectiveKeysForRun(ctx, locked.ID)
	if err != nil {
		return err
	}
	redactKeys, err := q.ListRunEnvRedactKeysForRun(ctx, locked.ID)
	if err != nil {
		return err
	}
	cur := func(rows []runEnvKey) []runEnvKey {
		sort.Slice(rows, func(i, j int) bool { return rows[i].less(rows[j]) })
		return rows
	}
	var l, r []runEnvKey
	for _, k := range liveKeys {
		l = append(l, runEnvKey{k.VarID, k.Rev})
	}
	for _, k := range redactKeys {
		r = append(r, runEnvKey{k.VarID, k.Rev})
	}
	l, r = cur(l), cur(r)
	if !slices.Equal(l, sortedRunEnvKeys(snap.live)) || !slices.Equal(r, sortedRunEnvKeys(snap.redact)) {
		return errRunEnvStale
	}
	if len(l)+len(r) > 0 && !slices.Contains(workerCaps, capability.RunEnvV1) {
		return errRunEnvCapabilityMissing
	}
	for _, group := range [][]runEnvDelivery{snap.live, snap.redact} {
		for _, d := range group {
			if err := q.UpsertRunEnvReceipt(ctx, store.UpsertRunEnvReceiptParams{
				RunID: locked.ID, UserID: locked.UserID, VarID: d.varID, Rev: d.rev,
				Scope: d.scope, Name: d.name, Secret: d.secret, ClaimGeneration: locked.ClaimGeneration,
			}); err != nil {
				return fmt.Errorf("record run env receipt: %w", err)
			}
		}
	}
	return nil
}

// settleRunEnv is finishRunClaimTx's run-env step: verify the snapshot against a recomputation and
// write the receipts, for a delivered lead-lane payload.
func settleRunEnv(ctx context.Context, q claimFinishQueries, locked store.Run, payload *ClaimPayload, identity claimRecoveryIdentity) error {
	if payload == nil || !runEnvDeliveredForRun(locked) {
		return nil
	}
	return verifyRunEnvAtFinish(ctx, q, locked, payload, identity.protocolCaps)
}
