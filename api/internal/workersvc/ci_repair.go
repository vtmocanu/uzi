package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/ciartifacts"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Default-branch CI repair (PRD #2865): the in-transaction episode machinery behind the
// scheduler's ci_watch target. The scheduler discovers a red execution OUTSIDE any database lock
// (it talks to the forge); every write it then makes (a started run, a halt, a green reset)
// goes through this file, which re-validates the schedule configuration and the episode row
// under locks before writing anything.
//
// Lock order for every start, halt and reset transaction:
//
//	pinned secret (share) -> repo (share) -> schedule (update) -> episode (update) -> run insert
//
// The order matters in both directions. Secret deletion locks the secret and then the schedules
// that pin it, so taking the schedule before the pinned secret would cycle with it. Repo
// deletion locks the repo and cascades to its schedules, so taking the schedule before the repo
// would cycle with that. The episode row is locked after the schedule it belongs to, in a
// second statement so it reads a fresh snapshot (see ci_watch.sql).

var (
	// ErrCIWatchConfigChanged: the schedule or its repo no longer match what the fire read
	// (edited, disabled, repointed, default branch changed) or the schedule is gone. Nothing
	// was written. The scheduler records the benign ci_config_changed skip.
	ErrCIWatchConfigChanged = errors.New("ci_watch: schedule configuration changed since the fire read it")
	// ErrCIWatchCapacity: the lifetime 1024-key admission limit of the schedule is reached and
	// the workflow key is not yet admitted.
	ErrCIWatchCapacity = errors.New("ci_watch: episode capacity reached for this schedule")
	// ErrCIWatchStale: the observation is older than the episode's watermark (or the state it
	// was decided on no longer holds). Nothing was written.
	ErrCIWatchStale = errors.New("ci_watch: stale observation")
	// ErrCIWatchAlreadyHandled: the identical observation was already acted on.
	ErrCIWatchAlreadyHandled = errors.New("ci_watch: execution already handled")
	// errCIRepairRequiresTx: the repair create needs one transaction covering the locks, the run
	// insert and the episode write; with no transaction there is no safe path.
	errCIRepairRequiresTx = errors.New("ci repair run creation requires an atomic transaction")
)

// CIWatchHaltedError reports that the episode is halted, or that the loop guard trips for the
// observation, under the row lock. Reason is the halted_reason vocabulary: "no_progress" or
// "attempt_cap". Create does not persist the halt (it rolls back); the caller follows with
// TransitionCIWatchEpisode to record it.
type CIWatchHaltedError struct{ Reason string }

func (e *CIWatchHaltedError) Error() string { return "ci_watch: episode halted: " + e.Reason }

// Halt reasons stored in ci_watch_episodes.halted_reason.
const (
	CIHaltNoProgress = "no_progress"
	CIHaltAttemptCap = "attempt_cap"
)

// CIRepairConfigFloor is the server-side set of CI-config path globs a default-branch repair
// run may never push a change to. CreateCIRepairRun always unions it into the run's
// ci_config_paths, whatever the operator configured.
// It is the same floor the automatic CI-fix guard always carries.
var CIRepairConfigFloor = config.CIAutofixConfigFloor

// CIObservation is the identity of one workflow execution as a fire saw it.
type CIObservation struct {
	RunID     int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ciPGTime maps a time to the column value; the zero time is stored as NULL.
func ciPGTime(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func ciFromPG(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

// CIWatermark reads the episode's persisted watermark; ok is false when none was recorded.
func CIWatermark(ep store.CiWatchEpisode) (CIObservation, bool) {
	if !ep.ObservedRunID.Valid {
		return CIObservation{}, false
	}
	return CIObservation{
		RunID:     ep.ObservedRunID.Int64,
		CreatedAt: ciFromPG(ep.ObservedCreatedAt),
		UpdatedAt: ciFromPG(ep.ObservedUpdatedAt),
	}, true
}

// ciMicro truncates to the column precision so a value read back from Postgres compares equal
// to the forge timestamp it was written from.
func ciMicro(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// ciUsesIDOrder reports whether the forge orders executions by immutable execution ID alone.
// Forgejo does (its timestamps never reorder distinct executions); GitHub and GitLab order by
// (CreatedAt, ID).
func ciUsesIDOrder(forgeType string) bool { return forgeType == "forgejo" }

// CIObservationOrder compares the creation order of two DISTINCT executions: negative when a
// is older than b, positive when newer, zero when they are the same execution (same ID).
func CIObservationOrder(forgeType string, a, b CIObservation) int {
	if a.RunID == b.RunID {
		return 0
	}
	if !ciUsesIDOrder(forgeType) {
		ac, bc := ciMicro(a.CreatedAt), ciMicro(b.CreatedAt)
		switch {
		case ac.Before(bc):
			return -1
		case ac.After(bc):
			return 1
		}
	}
	if a.RunID < b.RunID {
		return -1
	}
	return 1
}

// CIObservationNewer reports whether a should replace b as the newest observation of a
// workflow: it orders after b, or is the same execution with a later UpdatedAt (a re-run
// revision). The discovery step and the lock-time freshness check share this ordering.
func CIObservationNewer(forgeType string, a, b CIObservation) bool {
	if a.RunID == b.RunID {
		return ciMicro(a.UpdatedAt).After(ciMicro(b.UpdatedAt))
	}
	return CIObservationOrder(forgeType, a, b) > 0
}

// CIFreshness classifies an observation against an episode watermark.
type CIFreshness int

const (
	// CIFresh: the observation orders after the watermark, or is a later revision of the same
	// execution (a re-run). It may be acted on.
	CIFresh CIFreshness = iota
	// CIAlreadyHandled: identical to the watermark in all three components.
	CIAlreadyHandled
	// CIStale: older than the watermark.
	CIStale
)

// ClassifyCIObservation compares obs with the episode's watermark. A missing watermark means
// every observation is fresh.
func ClassifyCIObservation(forgeType string, obs CIObservation, ep store.CiWatchEpisode) CIFreshness {
	wm, ok := CIWatermark(ep)
	if !ok {
		return CIFresh
	}
	if obs.RunID == wm.RunID {
		switch o, w := ciMicro(obs.UpdatedAt), ciMicro(wm.UpdatedAt); {
		case o.After(w):
			return CIFresh
		case o.Equal(w):
			return CIAlreadyHandled
		default:
			return CIStale
		}
	}
	if CIObservationOrder(forgeType, obs, wm) > 0 {
		return CIFresh
	}
	return CIStale
}

// ciFreshnessErr maps a non-fresh classification to its sentinel (nil when fresh).
func ciFreshnessErr(f CIFreshness) error {
	switch f {
	case CIAlreadyHandled:
		return ErrCIWatchAlreadyHandled
	case CIStale:
		return ErrCIWatchStale
	default:
		return nil
	}
}

// CIWatchFingerprint digests every schedule and repo field a ci_watch fire reads, so the
// lock-time recheck can tell whether the configuration the fire decided on still holds.
// resolvedBody is the base instruction the fire resolved (the prompt, or the catalog body of a
// default job); it is part of the digest so a body change is a config change too.
func CIWatchFingerprint(sched store.RunSchedule, defaultBranch pgtype.Text, resolvedBody string) string {
	secret := ""
	if sched.CredentialOverrideSecretID.Valid {
		secret = uuid.UUID(sched.CredentialOverrideSecretID.Bytes).String()
	}
	mr := "inherit"
	if sched.MrReworkEnabled.Valid {
		mr = fmt.Sprint(sched.MrReworkEnabled.Bool)
	}
	fp := struct {
		RepoID                string  `json:"repo_id"`
		Target                string  `json:"target"`
		CIArtifacts           string  `json:"ci_artifacts"`
		CIWorkflows           string  `json:"ci_workflows"`
		Origin                string  `json:"origin"`
		CatalogSlug           *string `json:"catalog_slug"`
		Prompt                *string `json:"prompt"`
		Guidance              *string `json:"guidance"`
		ResolvedBody          string  `json:"resolved_body"`
		AutoApprove           bool    `json:"auto_approve"`
		WaitOnLimit           bool    `json:"wait_on_limit"`
		MrRework              string  `json:"mr_rework"`
		Model                 *string `json:"model"`
		OverrideSubagentModel bool    `json:"override_subagent_model"`
		Harness               *string `json:"harness"`
		CredentialMode        *string `json:"credential_override_mode"`
		CredentialSecretID    string  `json:"credential_override_secret_id"`
		Enabled               bool    `json:"enabled"`
		DefaultBranch         *string `json:"default_branch"`
	}{
		RepoID: sched.RepoID.String(), Target: sched.Target, CIArtifacts: string(sched.CiArtifacts), CIWorkflows: string(sched.CiWorkflows),
		Origin: sched.Origin, CatalogSlug: textPtrFP(sched.CatalogSlug), Prompt: textPtrFP(sched.Prompt),
		Guidance: textPtrFP(sched.Guidance), ResolvedBody: resolvedBody,
		AutoApprove: sched.AutoApprove, WaitOnLimit: sched.WaitOnLimit, MrRework: mr,
		Model: textPtrFP(sched.Model), OverrideSubagentModel: sched.OverrideSubagentModel,
		Harness: textPtrFP(sched.Harness), CredentialMode: textPtrFP(sched.CredentialOverrideMode),
		CredentialSecretID: secret, Enabled: sched.Enabled, DefaultBranch: textPtrFP(defaultBranch),
	}
	raw, err := json.Marshal(fp)
	if err != nil {
		// Unreachable for this fixed struct; a fingerprint that cannot be computed must never
		// match, so fail closed with a value no digest equals.
		return "unmarshalable"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func textPtrFP(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

// ciWatchQueries is the slice of the generated queries the episode transactions use. It is
// satisfied by a transaction-bound *store.Queries.
type ciWatchQueries interface {
	LockRepoForCIWatch(ctx context.Context, id uuid.UUID) (pgtype.Text, error)
	LockRunScheduleForUser(ctx context.Context, arg store.LockRunScheduleForUserParams) (store.RunSchedule, error)
	EnsureCIWatchEpisode(ctx context.Context, arg store.EnsureCIWatchEpisodeParams) (store.EnsureCIWatchEpisodeRow, error)
	LockCIWatchEpisode(ctx context.Context, arg store.LockCIWatchEpisodeParams) (store.CiWatchEpisode, error)
	UpdateCIWatchEpisode(ctx context.Context, arg store.UpdateCIWatchEpisodeParams) (store.CiWatchEpisode, error)
	StartCIWatchEpisode(ctx context.Context, arg store.StartCIWatchEpisodeParams) (store.CiWatchEpisode, error)
	ResetCIWatchEpisode(ctx context.Context, arg store.ResetCIWatchEpisodeParams) (store.CiWatchEpisode, error)
}

// CIWatchTarget identifies one workflow episode and carries the configuration fingerprint the
// fire computed before it talked to the forge.
type CIWatchTarget struct {
	UserID     uuid.UUID
	RepoID     uuid.UUID
	ScheduleID uuid.UUID
	// WorkflowKey is the episode key (forge.WorkflowRun.WorkflowKey).
	WorkflowKey string
	// ForgeType selects the freshness ordering (forgejo orders by ID alone).
	ForgeType string
	// Observation is the execution the transition acts on.
	Observation CIObservation
	// Fingerprint and ResolvedBody are the fire's configuration snapshot (CIWatchFingerprint).
	Fingerprint  string
	ResolvedBody string
}

// recheckCIWatchConfig takes the repo share lock and then the schedule lock and verifies the
// fire's configuration fingerprint against the locked rows. A missing repo or schedule is the
// same benign outcome as a changed one.
func recheckCIWatchConfig(ctx context.Context, q ciWatchQueries, t CIWatchTarget) error {
	branch, err := q.LockRepoForCIWatch(ctx, t.RepoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCIWatchConfigChanged
		}
		return err
	}
	sched, err := q.LockRunScheduleForUser(ctx, store.LockRunScheduleForUserParams{ID: t.ScheduleID, UserID: t.UserID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCIWatchConfigChanged
		}
		return err
	}
	if CIWatchFingerprint(sched, branch, t.ResolvedBody) != t.Fingerprint {
		return ErrCIWatchConfigChanged
	}
	return nil
}

// ensureAndLockCIWatchEpisode admits the key (creating its row when absent and room remains),
// then locks the row in a second statement so it reads a fresh snapshot.
func ensureAndLockCIWatchEpisode(ctx context.Context, q ciWatchQueries, t CIWatchTarget) (store.CiWatchEpisode, error) {
	if _, err := q.EnsureCIWatchEpisode(ctx, store.EnsureCIWatchEpisodeParams{
		WorkflowKey: t.WorkflowKey, ScheduleID: t.ScheduleID, UserID: t.UserID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.CiWatchEpisode{}, ErrCIWatchCapacity
		}
		return store.CiWatchEpisode{}, err
	}
	ep, err := q.LockCIWatchEpisode(ctx, store.LockCIWatchEpisodeParams{
		ScheduleID: t.ScheduleID, UserID: t.UserID, WorkflowKey: t.WorkflowKey,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.CiWatchEpisode{}, ErrCIWatchConfigChanged
		}
		return store.CiWatchEpisode{}, err
	}
	return ep, nil
}

// CILoopGuard evaluates the loop guard against the locked row. The cap takes precedence over
// no-progress (the same order as the CI auto-fix poller): at the end of the budget the message
// must say the limit was reached. "" means the guard lets the attempt through.
func CILoopGuard(ep store.CiWatchEpisode, signature string, maxAttempts int) string {
	if int(ep.Attempts) >= maxAttempts {
		return CIHaltAttemptCap
	}
	if ep.Attempts >= 1 && ep.LastSignature.Valid && ep.LastSignature.String == signature {
		return CIHaltNoProgress
	}
	return ""
}

// CIWatchStart is the episode half of CreateCIRepairRun.
type CIWatchStart struct {
	CIWatchTarget
	// Signature is FailureSignature of the original bounded snapshot.
	Signature string
	// MaxAttempts is the attempt cap (CI_AUTOFIX_MAX_ATTEMPTS).
	MaxAttempts int
}

// startCIWatchEpisodeLocked is the before-insert half of a start: config recheck, admission,
// episode lock and the re-evaluation of freshness, halt state and the loop guard against the
// locked row.
func startCIWatchEpisodeLocked(ctx context.Context, q ciWatchQueries, st CIWatchStart) error {
	if err := recheckCIWatchConfig(ctx, q, st.CIWatchTarget); err != nil {
		return err
	}
	ep, err := ensureAndLockCIWatchEpisode(ctx, q, st.CIWatchTarget)
	if err != nil {
		return err
	}
	if err := ciFreshnessErr(ClassifyCIObservation(st.ForgeType, st.Observation, ep)); err != nil {
		return err
	}
	if ep.HaltedReason.Valid {
		return &CIWatchHaltedError{Reason: ep.HaltedReason.String}
	}
	if reason := CILoopGuard(ep, st.Signature, st.MaxAttempts); reason != "" {
		return &CIWatchHaltedError{Reason: reason}
	}
	return nil
}

// CIRepairRunParams are the inputs of CreateCIRepairRun: a prompt run's fields plus the
// episode start.
type CIRepairRunParams struct {
	UserID, RepoID, ScheduleID uuid.UUID
	Title, Description         string
	AutoApprove, WaitOnLimit   bool
	MrReworkEnabled            *bool
	Model                      *string
	OverrideSubagentModel      bool
	CredOverride               *CredentialOverride
	Explicit                   *Harness
	// CIConfigPaths are the configured defaults (and the project's own CI config path); the
	// server-side floor CIRepairConfigFloor is unioned in regardless.
	CIConfigPaths    []string
	ArtifactManifest *ciartifacts.Manifest
	ArtifactInputs   []ciartifacts.Input
	Episode          CIWatchStart
}

// CreateCIRepairRun queues the guarded kind='prompt' run for a red default-branch execution
// and records the episode attempt in the SAME transaction (PRD #2865). It shares its body with
// CreatePromptRun (createPromptRunShared) and adds, inside the create transaction:
//
//   - after the pinned-secret share lock and before the run INSERT: the repo share lock, the
//     schedule lock with the configuration fingerprint recheck, episode admission, the episode
//     lock and the freshness/halt/loop-guard recheck against the locked row (any failure
//     aborts with nothing written: ErrCIWatchConfigChanged, ErrCIWatchCapacity,
//     ErrCIWatchStale, ErrCIWatchAlreadyHandled or *CIWatchHaltedError);
//   - after the INSERT: StartCIWatchEpisode (attempts+1, signature and watermark advance).
//
// The run carries ci_config_guard and the floor-unioned ci_config_paths. Any error rolls the
// run and the episode write back together. It fails closed without a transaction.
func (s *Service) CreateCIRepairRun(ctx context.Context, p CIRepairRunParams) (store.Run, error) {
	if s.txBeginner == nil {
		return store.Run{}, errCIRepairRequiresTx
	}
	if _, ok := s.q.(*store.Queries); !ok {
		return store.Run{}, errCIRepairRequiresTx
	}
	paths := MergeCIConfigPaths(append(append([]string{}, CIRepairConfigFloor...), p.CIConfigPaths...), "")
	asCI := func(q Store) (ciWatchQueries, error) {
		cq, ok := q.(ciWatchQueries)
		if !ok {
			return nil, errCIRepairRequiresTx
		}
		return cq, nil
	}
	return s.createPromptRunShared(ctx, promptRunInput{
		userID: p.UserID, repoID: p.RepoID, scheduleID: p.ScheduleID, title: p.Title, prompt: p.Description,
		autoApprove: p.AutoApprove, waitOnLimit: p.WaitOnLimit, mrReworkEnabled: p.MrReworkEnabled, model: p.Model,
		overrideSubagentModel: p.OverrideSubagentModel, credOverride: p.CredOverride, explicit: p.Explicit,
	}, &promptRunGuard{
		ciConfigPaths: paths,
		before: func(ctx context.Context, q Store) error {
			cq, err := asCI(q)
			if err != nil {
				return err
			}
			return startCIWatchEpisodeLocked(ctx, cq, p.Episode)
		},
		after: func(ctx context.Context, q Store, run *store.Run) error {
			txq, ok := q.(*store.Queries)
			if !ok {
				return errCIRepairRequiresTx
			}
			if err := s.admitCIArtifacts(ctx, txq, run, p.ArtifactManifest, p.ArtifactInputs); err != nil {
				return err
			}
			cq, err := asCI(q)
			if err != nil {
				return err
			}
			if s.ciRepairAfterInsert != nil {
				if err := s.ciRepairAfterInsert(run.ID); err != nil {
					return err
				}
			}
			ob := p.Episode.Observation
			if _, err := cq.StartCIWatchEpisode(ctx, store.StartCIWatchEpisodeParams{
				ObservedRunID:     ob.RunID,
				ObservedCreatedAt: ciPGTime(ob.CreatedAt),
				ObservedUpdatedAt: ciPGTime(ob.UpdatedAt),
				LastSignature:     pgtype.Text{String: p.Episode.Signature, Valid: true},
				ScheduleID:        p.Episode.ScheduleID,
				WorkflowKey:       p.Episode.WorkflowKey,
				UserID:            p.Episode.UserID,
			}); err != nil {
				return err
			}
			return nil
		},
	})
}

// CIWatchTransition is a halt or a green reset, run in its own transaction.
type CIWatchTransition struct {
	CIWatchTarget
	// Reset selects a green reset; otherwise the transition is a halt.
	Reset bool
	// HaltReason (CIHaltNoProgress or CIHaltAttemptCap), Signature and MaxAttempts describe the
	// guard decision a halt was made on; they are re-evaluated against the locked row.
	HaltReason  string
	Signature   string
	MaxAttempts int
}

// TransitionCIWatchEpisode performs one halt or green reset atomically:
//
//   - both take the repo share lock, the schedule lock and the configuration recheck first;
//   - a halt admits the key if needed (ErrCIWatchCapacity at the limit), locks the row,
//     rechecks freshness and that the loop-guard condition still holds, and writes the halt
//     with its parameters built from the LOCKED row (the signature and attempt count are kept;
//     only the halt reason and the watermark change);
//   - a reset NEVER creates a row: with none there is nothing to reset and it returns nil, so
//     green workflows never use up the admission slots. With a row it requires a fresh
//     observation (a stale green leaves a newer halt and attempt count untouched) and clears
//     the loop history.
//
// A failed recheck writes nothing and returns the sentinel.
func (s *Service) TransitionCIWatchEpisode(ctx context.Context, tr CIWatchTransition) error {
	if s.txBeginner == nil {
		return errCIRepairRequiresTx
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err := transitionCIWatchEpisodeLocked(ctx, q, tr); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func transitionCIWatchEpisodeLocked(ctx context.Context, q ciWatchQueries, tr CIWatchTransition) error {
	if err := recheckCIWatchConfig(ctx, q, tr.CIWatchTarget); err != nil {
		return err
	}
	ob := tr.Observation
	if tr.Reset {
		ep, err := q.LockCIWatchEpisode(ctx, store.LockCIWatchEpisodeParams{
			ScheduleID: tr.ScheduleID, UserID: tr.UserID, WorkflowKey: tr.WorkflowKey,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // no row: nothing to reset, and a green never admits a key
			}
			return err
		}
		if err := ciFreshnessErr(ClassifyCIObservation(tr.ForgeType, ob, ep)); err != nil {
			return err
		}
		_, err = q.ResetCIWatchEpisode(ctx, store.ResetCIWatchEpisodeParams{
			ObservedRunID: ob.RunID, ObservedCreatedAt: ciPGTime(ob.CreatedAt), ObservedUpdatedAt: ciPGTime(ob.UpdatedAt),
			ScheduleID: tr.ScheduleID, WorkflowKey: tr.WorkflowKey, UserID: tr.UserID,
		})
		return err
	}
	ep, err := ensureAndLockCIWatchEpisode(ctx, q, tr.CIWatchTarget)
	if err != nil {
		return err
	}
	if err := ciFreshnessErr(ClassifyCIObservation(tr.ForgeType, ob, ep)); err != nil {
		return err
	}
	// The decision the fire made from an unlocked read must still hold on the locked row.
	// Halting a row that has since moved on (a reset, a different signature) would be wrong,
	// so it is a benign stale outcome and the next fire re-decides.
	if !ep.HaltedReason.Valid && CILoopGuard(ep, tr.Signature, tr.MaxAttempts) != tr.HaltReason {
		return ErrCIWatchStale
	}
	reason := tr.HaltReason
	if ep.HaltedReason.Valid {
		reason = ep.HaltedReason.String
	}
	_, err = q.UpdateCIWatchEpisode(ctx, store.UpdateCIWatchEpisodeParams{
		LastHandledRunID:  pgtype.Int8{Int64: ob.RunID, Valid: true},
		ObservedCreatedAt: ciPGTime(ob.CreatedAt),
		ObservedUpdatedAt: ciPGTime(ob.UpdatedAt),
		ObservedRunID:     pgtype.Int8{Int64: ob.RunID, Valid: true},
		LastSignature:     ep.LastSignature,
		Attempts:          ep.Attempts,
		HaltedReason:      pgtype.Text{String: reason, Valid: true},
		ScheduleID:        tr.ScheduleID,
		WorkflowKey:       tr.WorkflowKey,
		UserID:            tr.UserID,
	})
	return err
}
