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

// isolateClaim turns an assembled claim into a profile-bound research claim (PRD #1906
// M3, Decisions 5, 7, 10 and 12). In one transaction it snapshots the run's site list at
// its FIRST claim (a later claim keeps that snapshot, so an admin edit never changes a
// claimed run), mints or rotates the run's fetch credential under this claim's generation,
// and releases any reservation an earlier claim left open. Then it strips everything a
// research run must not have: the forge credential and identity, the repo, the agents,
// skills and tool packages, and every forge- or memory-derived extra. The Codex block is
// never attached (assembleClaim refuses a Codex profile-bound run before building it).
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

// stripForIsolation removes from a claim everything a profile-bound run must not carry.
func stripForIsolation(p *ClaimPayload) {
	p.Repo = ClaimRepo{}
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
