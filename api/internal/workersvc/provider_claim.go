package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// provider_claim.go is PRD #4320 module 5 ("Claim binding and worker capability"): how a
// PROVIDER-BOUND run's claim is built (one `provider` object, never a vendor credential) and how a
// later claim of a bound run is refused when its frozen binding was revoked (D9: revocation applies
// at the next actual claim, never inside a held claim).

// Hold reasons of a refused provider claim. Both park the run `paused` with hold_reason set to the
// literal. provider_destination_not_allowed has a promoter arm (the URL may be allowlisted again);
// provider_binding_stale has none (the frozen key or revision changed: a new run picks up the
// current key).
const (
	holdProviderDestinationNotAllowed = "provider_destination_not_allowed"
	holdProviderBindingStale          = "provider_binding_stale"
	// holdProviderModelUnmapped is the typed terminal reason of a provider JUDGE or JOB claim whose
	// resolved model the frozen provider's map no longer carries (PRD #4322 D-F, M1b). It is never
	// a park reason: finishRunClaim settles a judge's or job's provider refusal as a failed run.
	holdProviderModelUnmapped = "provider_model_unmapped"
)

// errProviderLaneClaimRefused is the claim-assembly refusal of a provider-bound run on a lane that
// has no provider support yet (cross-check, isolated, Codex job or chat; Claude and Codex judges are
// supported since PRD #4322 M1a and #4574 M1, and a Claude job since M1b), or on a worker lacking the lane's capability. It wraps
// errCredentialUnavailable, so finishRunClaim fails the claim terminally exactly as it does for the
// other refused-lane shapes (errIsolatedClaimRefused), never delivering anything.
var errProviderLaneClaimRefused = fmt.Errorf("%w: %w", errCredentialUnavailable, ErrProviderLaneUnsupported)

// providerClaimRefusal is the typed claim-time refusal of a provider-bound run's frozen binding.
// Reason is the hold_reason literal (holdProviderDestinationNotAllowed or holdProviderBindingStale).
// errors.Is matches the exported sentinels, so callers and tests can classify it.
type providerClaimRefusal struct {
	Reason string
	Detail string
}

func (e *providerClaimRefusal) Error() string {
	if e.Detail == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Detail
}

func (e *providerClaimRefusal) Is(target error) bool {
	switch target {
	case ErrProviderBindingStale:
		return e.Reason == holdProviderBindingStale
	case ErrProviderDestinationNotAllowed:
		return e.Reason == holdProviderDestinationNotAllowed
	case ErrProviderModelUnmapped:
		return e.Reason == holdProviderModelUnmapped
	}
	return false
}

func staleProviderRefusal(detail string) *providerClaimRefusal {
	return &providerClaimRefusal{Reason: holdProviderBindingStale, Detail: detail}
}

// isProviderClaimRefusal reports whether err is (or wraps) a providerClaimRefusal.
func isProviderClaimRefusal(err error) (*providerClaimRefusal, bool) {
	var r *providerClaimRefusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// ClaimProviderBinding is the one `provider` object a provider-bound claim carries instead of an
// Anthropic OAuth token (PRD #4320 module 5). It is the frozen binding of the run: the provider
// revision the run was created against, the destination (BaseURL: the operator allowlist entry the
// URL matched), and the gateway key the run froze at its enablement revision. Never logged: it
// carries the usable gateway key. The worker's decoder refuses a claim carrying both this and an
// Anthropic token (m6); assertClaimSecretsExclusive is the server twin.
type ClaimProviderBinding struct {
	ProviderID string             `json:"provider_id"`
	Revision   int32              `json:"revision"`
	Engine     string             `json:"engine"`
	Protocol   string             `json:"protocol"`
	BaseURL    string             `json:"base_url"`
	AuthStyle  string             `json:"auth_style"`
	Credential string             `json:"credential"`
	ModelMap   providers.ModelMap `json:"model_map"`
}

// errClaimSecretsNotExclusive is the builder's refusal of a payload carrying a provider binding
// together with any vendor credential.
var errClaimSecretsNotExclusive = errors.New("claim payload carries a provider binding together with a vendor credential")

// assertClaimSecretsExclusive is the single exclusivity assertion every claim builder (issue run,
// chat, judge, job, cross-check) calls on the secrets it is about to deliver: a provider binding
// excludes the Anthropic token and the Codex block. A provider claim sends `provider` and an empty
// anthropic_oauth_token; a vendor claim never carries `provider`.
func assertClaimSecretsExclusive(s ClaimSecrets) error {
	if s.Provider != nil && (s.AnthropicOAuthToken != "" || s.Codex != nil) {
		return errClaimSecretsNotExclusive
	}
	return nil
}

// providerClaimReader is the read surface the claim-time binding load needs; *store.Queries
// satisfies it (the optional-interface idiom of this package, so unit fakes that do not model
// providers are not forced to implement it: a provider-bound run on such a store fails closed).
type providerClaimReader interface {
	GetModelProviderRevisionForOwner(ctx context.Context, arg store.GetModelProviderRevisionForOwnerParams) (store.GetModelProviderRevisionForOwnerRow, error)
	GetProviderBindingKeyByID(ctx context.Context, arg store.GetProviderBindingKeyByIDParams) (store.GetProviderBindingKeyByIDRow, error)
}

// providerBindingLocker is the in-transaction surface of the finisher's locked re-check: the key
// row FOR SHARE NOWAIT plus the revision read. A transaction-bound *store.Queries satisfies it.
type providerBindingLocker interface {
	LockProviderBindingKeyForShareNowait(ctx context.Context, arg store.LockProviderBindingKeyForShareNowaitParams) (store.LockProviderBindingKeyForShareNowaitRow, error)
	LockProviderBindingRevisionForKeyShareNowait(ctx context.Context, arg store.LockProviderBindingRevisionForKeyShareNowaitParams) (store.LockProviderBindingRevisionForKeyShareNowaitRow, error)
}

// providerKeyFacts are the key-row fields the binding check reads.
type providerKeyFacts struct {
	Kind          string
	ProviderID    pgtype.UUID
	Disabled      bool
	EnablementRev int64
}

// frozenProviderKeyID returns the run's frozen key secret id, or false when the binding is not the
// fully set user_key shape (an incomplete or unknown-mode binding is stale, never delivered).
func frozenProviderKeyID(run store.Run) (uuid.UUID, bool) {
	if !run.ModelProviderID.Valid || !run.ModelProviderRevision.Valid || !run.ModelProviderKeySecretID.Valid ||
		!run.ModelProviderKeyRev.Valid || !run.ModelProviderKeyMode.Valid || run.ModelProviderKeyMode.String != providerKeyModeUser {
		return uuid.Nil, false
	}
	return uuid.UUID(run.ModelProviderKeySecretID.Bytes), true
}

// providerBindingRefusal is the pure judgement of a run's frozen binding against what is committed
// now. It returns "" when the binding is intact, else the hold reason. A retired provider is NOT a
// refusal (retiring stops new selections; bound runs keep their frozen revision). Precedence: a
// broken binding (revision or key gone, key disabled, rotated or replaced) is provider_binding_stale,
// final for this run; only an intact binding whose frozen URL is no longer on the operator allowlist
// is provider_destination_not_allowed, the one refusal the promoter can undo.
func (s *Service) providerBindingRefusal(run store.Run, rev *store.GetModelProviderRevisionForOwnerRow, key *providerKeyFacts) string {
	if _, ok := frozenProviderKeyID(run); !ok || rev == nil || key == nil {
		return holdProviderBindingStale
	}
	pid := uuid.UUID(run.ModelProviderID.Bytes)
	if key.Kind != providerKeyKind(run.Harness) || !key.ProviderID.Valid || uuid.UUID(key.ProviderID.Bytes) != pid ||
		key.Disabled || key.EnablementRev != run.ModelProviderKeyRev.Int64 {
		return holdProviderBindingStale
	}
	var models providers.ModelMap
	if rev.Engine != run.Harness || rev.Revision != run.ModelProviderRevision.Int32 ||
		uuid.UUID(rev.ProviderID) != pid || json.Unmarshal(rev.ModelMap, &models) != nil ||
		providers.ValidateProfile(run.Harness, rev.Protocol, rev.AuthStyle, models) != nil {
		return holdProviderBindingStale
	}
	if _, ok := s.providerAllow(rev.BaseUrl); !ok {
		return holdProviderDestinationNotAllowed
	}
	return ""
}

// lockedProviderRefusal is the finisher's in-transaction re-check (D9), run under the exact-claim
// run lock already held: it takes the frozen key row FOR SHARE NOWAIT (a concurrent rotation,
// disable or delete either committed first and is seen here, or waits until this claim decision
// commits; a held row is 55P03, which rolls the transaction back for the bounded retry instead of
// blocking under the run lock), then reads the frozen revision FOR KEY SHARE NOWAIT too (a blocking
// revision lock can form a lock cycle with a provider delete, which cascades the revision and key
// rows in either order: 40P01, which the finishers do not retry; NOWAIT makes it 55P03, which they do), and
// returns the refusal reason or "" when intact. A missing key row or revision is stale. A
// not-provider-bound run returns "".
func (s *Service) lockedProviderRefusal(ctx context.Context, q any, locked store.Run) (string, error) {
	if !providerBoundRun(locked) {
		return "", nil
	}
	keyID, ok := frozenProviderKeyID(locked)
	if !ok {
		return holdProviderBindingStale, nil
	}
	pl, ok := q.(providerBindingLocker)
	if !ok {
		return "", fmt.Errorf("%w: claim transaction cannot re-check the provider binding", errCredentialUnavailable)
	}
	k, err := pl.LockProviderBindingKeyForShareNowait(ctx, store.LockProviderBindingKeyForShareNowaitParams{ID: keyID, UserID: locked.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return holdProviderBindingStale, nil
	}
	if err != nil {
		return "", err
	}
	rev, err := pl.LockProviderBindingRevisionForKeyShareNowait(ctx, store.LockProviderBindingRevisionForKeyShareNowaitParams{
		ProviderID: uuid.UUID(locked.ModelProviderID.Bytes), Revision: locked.ModelProviderRevision.Int32, OwnerUserID: locked.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return holdProviderBindingStale, nil
	}
	if err != nil {
		return "", err
	}
	facts := providerKeyFacts{Kind: k.Kind, ProviderID: k.ProviderID, Disabled: k.DisabledAt.Valid, EnablementRev: k.EnablementRev}
	revRow := store.GetModelProviderRevisionForOwnerRow(rev)
	return s.providerBindingRefusal(locked, &revRow, &facts), nil
}

// assembleProviderBinding builds the `provider` object of a provider-bound run's claim from the
// run's FROZEN binding, never the owner's current provider source (same-run continuation: the owner
// may have cleared or changed the source since). It judges the binding first (a stale or
// not-allowlisted binding is a typed *providerClaimRefusal, nothing opened), then opens the frozen
// key through the vault by id and kind (owner-scoped; a key of another kind or owner is refused),
// and loads the frozen revision. It touches no vendor credential: it never calls claimSecretID,
// openWithAutoRetry, recordRunCredential or SetRunAnthropicSecret, so runs.anthropic_secret_id and
// anthropic_rejected_at stay untouched, and the claiming worker's anthropic_bind_mode is ignored.
//
// The claiming worker must advertise its engine provider capability (checked before any read).
//
// The finisher re-checks the same binding under locks (lockedProviderRefusal) after this open: a
// rotation between the open and the delivery bumps enablement_rev, so the locked re-check refuses it
// and the possibly re-keyed payload is never delivered.
func (s *Service) assembleProviderBinding(ctx context.Context, wkr store.Worker, run store.Run) (*ClaimProviderBinding, error) {
	// Require the complete engine protocol before any binding read or vault open.
	if !workerSupportsProvider(wkr, run.Harness, run.Kind) {
		return nil, errProviderLaneClaimRefused
	}
	keyID, ok := frozenProviderKeyID(run)
	if !ok {
		return nil, staleProviderRefusal("the run's provider binding is incomplete")
	}
	rd, ok := s.q.(providerClaimReader)
	if !ok {
		return nil, fmt.Errorf("%w: the store cannot read provider bindings", errCredentialUnavailable)
	}
	pid := uuid.UUID(run.ModelProviderID.Bytes)
	rev, err := rd.GetModelProviderRevisionForOwner(ctx, store.GetModelProviderRevisionForOwnerParams{
		ProviderID: pid, Revision: run.ModelProviderRevision.Int32, OwnerUserID: run.UserID,
	})
	var revp *store.GetModelProviderRevisionForOwnerRow
	switch {
	case err == nil:
		revp = &rev
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("provider claim: read revision: %w", err)
	}
	key, err := rd.GetProviderBindingKeyByID(ctx, store.GetProviderBindingKeyByIDParams{ID: keyID, UserID: run.UserID})
	var keyp *providerKeyFacts
	switch {
	case err == nil:
		keyp = &providerKeyFacts{Kind: key.Kind, ProviderID: key.ProviderID, Disabled: key.DisabledAt.Valid, EnablementRev: key.EnablementRev}
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("provider claim: read key: %w", err)
	}
	if reason := s.providerBindingRefusal(run, revp, keyp); reason != "" {
		return nil, &providerClaimRefusal{Reason: reason}
	}
	entry, _ := s.providerAllow(rev.BaseUrl) // allowlisted: providerBindingRefusal just proved it
	var mm providers.ModelMap
	if err := json.Unmarshal(rev.ModelMap, &mm); err != nil || providers.ValidateModelMapForEngine(run.Harness, mm) != nil {
		return nil, fmt.Errorf("%w: the bound provider revision has an invalid model map", errCredentialUnavailable)
	}
	plain, err := secretopen.OpenByIDOfKind(ctx, s.q, s.vlt, s.box, run.UserID, keyID, providerKeyKind(run.Harness))
	switch {
	case err == nil:
	case errors.Is(err, secretopen.ErrVaultLocked):
		return nil, errVaultLocked
	case errors.Is(err, secretopen.ErrNoSecret):
		return nil, staleProviderRefusal("the bound key no longer exists")
	case errors.Is(err, secretopen.ErrUndecryptable):
		return nil, fmt.Errorf("%w: the provider key could not be decrypted", errCredentialUnavailable)
	default:
		return nil, err
	}
	return &ClaimProviderBinding{
		ProviderID: pid.String(),
		Revision:   rev.Revision,
		Engine:     run.Harness,
		Protocol:   rev.Protocol,
		BaseURL:    entry,
		AuthStyle:  rev.AuthStyle,
		Credential: string(plain),
		ModelMap:   mm,
	}, nil
}

// workerSupportsProvider checks only the advertised protocol for the frozen engine.
// Wake admission and claim assembly share this conjunction without reading credentials. A Claude
// judge additionally needs claude_provider_judge_v1 (PRD #4322 M1a), a Codex judge
// codex_provider_judge_v1 (PRD #4574 M1), and a Claude job claude_provider_job_v1 (PRD #4322 M1b),
// each mirroring the SQL claim clause.
func workerSupportsProvider(w store.Worker, engine, kind string) bool {
	switch engine {
	case harnessClaude:
		return slices.Contains(w.ProtocolCapabilities, capability.ClaudeProviderV1) &&
			(kind != runkind.Judge || slices.Contains(w.ProtocolCapabilities, capability.ClaudeProviderJudgeV1)) &&
			(kind != runkind.Job || slices.Contains(w.ProtocolCapabilities, capability.ClaudeProviderJobV1))
	case harnessCodex:
		return slices.Contains(w.ProtocolCapabilities, capability.CodexProviderV1) &&
			slices.Contains(w.ProtocolCapabilities, capability.CodexHarnessV1) &&
			slices.Contains(w.ProtocolCapabilities, capability.CodexRuntimeV2) &&
			slices.Contains(w.ProtocolCapabilities, capability.CodexRuntimeV3) &&
			(kind != runkind.Judge || slices.Contains(w.ProtocolCapabilities, capability.CodexProviderJudgeV1))
	default:
		return false
	}
}

// promotionRunBinding is the run-shaped view of the frozen provider binding the promoter's locked
// read (LockCredentialDisabledRunForPromotion) carries, so the provider arms need no second read of
// the run row. Only the binding columns (plus identity) are set.
func promotionRunBinding(r store.LockCredentialDisabledRunForPromotionRow) store.Run {
	return store.Run{
		ID: r.ID, UserID: r.UserID, Harness: r.Harness,
		ModelProviderID: r.ModelProviderID, ModelProviderRevision: r.ModelProviderRevision,
		ModelProviderKeyMode: r.ModelProviderKeyMode, ModelProviderKeySecretID: r.ModelProviderKeySecretID,
		ModelProviderKeyRev: r.ModelProviderKeyRev,
	}
}

// providerHoldRefusal is the promoter's decision for a run held on provider_destination_not_allowed:
// "" when the run's own frozen binding is intact and its frozen URL is allowlisted again (re-queue),
// holdProviderDestinationNotAllowed while the URL is still off the list (stay held), and
// holdProviderBindingStale when the binding broke while parked (frozen key or revision changed: the
// caller rewrites the hold, whose "resumes automatically" copy is false then). Run under the owner's
// secret mutation lock (so a rotation or disable serializes against this decision); the revision read
// takes FOR KEY SHARE (GetModelProviderRevisionForOwner) and the key read is plain. The claim
// re-checks under locks anyway. A retired provider does not block (retiring keeps bound
// runs). Any other hold reason never reaches here.
func (s *Service) providerHoldRefusal(ctx context.Context, q *store.Queries, run store.Run) (string, error) {
	if !providerBoundRun(run) {
		return holdProviderDestinationNotAllowed, nil
	}
	keyID, ok := frozenProviderKeyID(run)
	if !ok {
		return holdProviderBindingStale, nil
	}
	var revp *store.GetModelProviderRevisionForOwnerRow
	rev, err := q.GetModelProviderRevisionForOwner(ctx, store.GetModelProviderRevisionForOwnerParams{
		ProviderID: uuid.UUID(run.ModelProviderID.Bytes), Revision: run.ModelProviderRevision.Int32, OwnerUserID: run.UserID,
	})
	switch {
	case err == nil:
		revp = &rev
	case !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("provider requirement: read revision: %w", err)
	}
	var keyp *providerKeyFacts
	key, err := q.GetProviderBindingKeyByID(ctx, store.GetProviderBindingKeyByIDParams{ID: keyID, UserID: run.UserID})
	switch {
	case err == nil:
		keyp = &providerKeyFacts{Kind: key.Kind, ProviderID: key.ProviderID, Disabled: key.DisabledAt.Valid, EnablementRev: key.EnablementRev}
	case !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("provider requirement: read key: %w", err)
	}
	return s.providerBindingRefusal(run, revp, keyp), nil
}
