package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/egressprofile"
	"github.com/vtmocanu/uzi/api/internal/fetchctl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// errIsolatedClaimRefused wraps errCredentialUnavailable (a terminal claim failure) for a
// profile-bound run that cannot be claimed as one: the Codex harness (Decision 10; the
// schema's CHECK already forbids it, this is the fail-closed backstop), a judge run (its
// claim is assembled on a separate lane that has no isolation), or an api without a
// transaction source to mint the credential with.
var errIsolatedClaimRefused = fmt.Errorf("%w: profile-bound run cannot be claimed", errCredentialUnavailable)

// errIsolatedLaneMismatch wraps errCredentialUnavailable (terminal) for a claim whose run and
// worker sit on different sides of the isolated lane (PRD #1906 M5, Decision 9): a
// profile-bound run on a worker without workers.isolated_lane, or an unbound run on a lane
// worker. ClaimRun's two-way clause never returns such a pair; this is the Go backstop.
var errIsolatedLaneMismatch = fmt.Errorf("%w: run and worker are on different sides of the isolated lane", errCredentialUnavailable)

// isolateClaim turns an assembled claim into a profile-bound research claim (PRD #1906
// M3, Decisions 5, 7, 10 and 12). In one transaction it snapshots the run's site list at
// its FIRST claim (a later claim keeps that snapshot, so an admin edit never changes a
// claimed run), mints or rotates the run's fetch credential under this claim's generation,
// and releases any reservation an earlier claim left open. Then stripForIsolation clears
// the forge credential and identity, the repo, the agents, skills and tool packages, and
// the forge- or repo-derived context (its comment lists exactly what, and what it keeps).
// The Codex block is never attached (assembleClaim refuses a Codex profile-bound run
// before building it).
//
// A mint that matches no row means the run left this claim (cancelled or reclaimed)
// between ClaimRun and here: errRunVanished, and nothing is delivered.
func (s *Service) isolateClaim(ctx context.Context, run store.Run, payload *ClaimPayload) error {
	if s.txBeginner == nil {
		return fmt.Errorf("%w: no transaction source to mint the fetch credential", errIsolatedClaimRefused)
	}
	token, hash, err := fetchctl.GenerateCredential()
	if err != nil {
		return err
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)

	if err := snapshotEgressProfile(ctx, q, run); err != nil {
		return err
	}
	raw, err := q.GetRunEgressSnapshot(ctx, run.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errRunVanished
	}
	if err != nil {
		return err
	}
	snap, err := fetchctl.ParseSnapshot(raw)
	if err != nil {
		return err
	}
	n, err := q.MintRunFetchCredential(ctx, store.MintRunFetchCredentialParams{
		RunID: run.ID, TokenHash: hash, ClaimGeneration: run.ClaimGeneration,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return errRunVanished
	}
	if _, err := q.ReleasePriorGenerationFetchReservations(ctx, store.ReleasePriorGenerationFetchReservationsParams{
		RunID: run.ID, ClaimGeneration: run.ClaimGeneration,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	stripForIsolation(payload)
	payload.IsolatedFetch = &ClaimIsolatedFetch{Credential: token, Profile: snap.Profile, Hosts: snap.Entries}
	return nil
}

// snapshotEgressProfile writes runs.egress_snapshot from the bound profile's EFFECTIVE
// entries, only when the run has none yet.
func snapshotEgressProfile(ctx context.Context, q *store.Queries, run store.Run) error {
	if len(run.EgressSnapshot) > 0 {
		return nil
	}
	p, err := q.GetEgressProfileByID(ctx, uuid.UUID(run.EgressProfileID.Bytes))
	if err != nil {
		// ON DELETE RESTRICT keeps a referenced profile, so a miss is a store defect.
		return fmt.Errorf("load egress profile: %w", err)
	}
	b, err := json.Marshal(fetchctl.Snapshot{
		Profile: p.Name,
		Entries: egressprofile.EffectiveEntries(p.Hosts, p.MultiPublisherOverride),
	})
	if err != nil {
		return err
	}
	return q.SetRunEgressSnapshotOnce(ctx, store.SetRunEgressSnapshotOnceParams{ID: run.ID, Snapshot: b})
}

// stripForIsolation clears the parts of an assembled claim that give a profile-bound run a
// forge, a repository, an agent roster or a tool beyond the research runner's fixed set:
// the repo and every forge credential and identity (and the Codex block), the agents,
// skills and tool packages, the devbox opt-in, and the forge- or repo-derived context (the
// issue's comment thread, review comments, CI pipeline, the branch and base branch, the PR
// description, self-improve targets and open MRs).
//
// It is not a whitelist. What it leaves is what the research runner reads
// (agent/src/isolated-runner.ts: run_id, claim_generation, last_seq, issue_title,
// issue_description, the Anthropic token, config's timeout, model and effort, and
// isolated_fetch) plus run bookkeeping the runner ignores (kind, status, the plan, session
// and resume fields, the milestones and flags): none of it is a credential, a forge
// handle or a tool. A new claim field that is one must be cleared here.
func stripForIsolation(p *ClaimPayload) {
	p.Repo = ClaimRepo{}
	p.IssueComments = nil
	p.Branch = nil
	p.BaseBranch = nil
	p.Secrets.ForgePAT = ""
	p.Secrets.ForgeUsername = ""
	p.Secrets.Codex = nil
	p.Agents = []ClaimAgent{}
	p.Skills = []ClaimSkill{}
	p.SkillsDropped = []ClaimSkillDrop{}
	p.Config.ToolPackages = nil
	p.Config.RepoDevboxOptIn = false
	p.PrDescription = nil
	p.InflightTargets = nil
	p.SelfImproveOpenMRs = nil
	p.SelfImproveDogfood = false
	p.KnownImproveUziTargets = nil
	p.ReviewComments = nil
	p.Pipeline = nil
}
