package forgesvc

// projectsync_share.go holds the GitHub Projects v2 board-access surface (PRD #557):
// board visibility (get/set) and write-only collaborator sharing (share/unshare).

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// GetVisibility reads the linked board's current `public` flag from GitHub (PRD
// #557 M2). Unlike the write paths it runs projectSyncResolve (instance-flag gate,
// GitHub-only, forge build, ProjectBoardSyncer assertion) but NOT the scope preflight:
// this is the lazy read issued on every Board-access panel open (D4), and the extra
// f.TokenInfo introspection round-trip only buys a cleaner 422 — a scope-missing token
// fails the visibility query itself, so the preflight is dropped from this hot path
// (issue #569 finding #2). It then reads the link row for the board's node id and issues
// a single `node(...ProjectV2{public})` query. A repo with no link row surfaces
// pgx.ErrNoRows verbatim (handler → 404).
//
// A stale/deleted board node id makes graphqlDo wrap GitHub's NOT_FOUND with
// forge.ErrGitHubUserNotFound (that wrap is applied to EVERY NOT_FOUND, not just a
// user lookup). We deliberately do NOT translate it here: only the resolve step in
// ShareWithUser/Unshare maps to ErrProjectSyncUserNotFound (422). So a stale board
// propagates as a generic error → the handler's default 500, exactly as intended.
func (s *ProjectSyncService) GetVisibility(ctx context.Context, repoID uuid.UUID) (bool, error) {
	_, syncer, _, err := s.projectSyncResolve(ctx, repoID)
	if err != nil {
		return false, err
	}
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		// pgx.ErrNoRows passes through so the handler can 404 ("not sync-enabled").
		return false, err
	}
	return syncer.GetProjectV2Visibility(ctx, link.ProjectNodeID)
}

// RepoOwnerType reports whether the repo's owner is a GitHub User or Organization
// (PRD #576 M1, F-G), for the sync panel's Provision feasibility nudge. It runs the
// shared projectSyncPreamble (instance-flag gate, GitHub-only, forge build,
// ProjectBoardSyncer assertion, scope preflight), resolves the repo's owner login via
// RepoSlug, then issues a single `repositoryOwner(login){ __typename }` query. Unlike
// GetVisibility it needs NO link row — the nudge is for a not-yet-linked repo — so it
// never reads github_project_links. Errors (a bad slug, an unexpected __typename)
// propagate to the handler's default 500; the frontend treats any failure as
// "unresolved" and falls back to showing both paths.
func (s *ProjectSyncService) RepoOwnerType(ctx context.Context, repoID uuid.UUID) (forge.ProjectV2OwnerType, error) {
	repo, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
		return "", err
	}
	owner, _, err := syncer.RepoSlug(ctx, repo.ForgeProjectID)
	if err != nil {
		return "", fmt.Errorf("project sync: resolve repo slug: %w", err)
	}
	return syncer.ResolveRepositoryOwnerType(ctx, owner)
}

// SetVisibility writes the linked board's `public` flag on GitHub (PRD #557 M2) via
// `updateProjectV2`. As a WRITE it runs the full projectSyncPreamble (including the
// scope preflight), unlike GetVisibility which drops the preflight (issue #569 finding
// #2) — so a scope-missing token is rejected here with ErrProjectSyncMissingScope but
// reads through on GetVisibility. The link-row read is the same. As with GetVisibility,
// a stale board node id propagates as a generic error (→ 500), not
// ErrProjectSyncUserNotFound — the not-found translation is scoped to the username
// resolve step in ShareWithUser/Unshare, never the visibility path.
func (s *ProjectSyncService) SetVisibility(ctx context.Context, repoID uuid.UUID, public bool) error {
	_, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
		return err
	}
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	return syncer.SetProjectV2Visibility(ctx, link.ProjectNodeID, public)
}

// ShareWithUser grants a GitHub user READER access to the linked board (PRD #557
// M2): resolve the login to its node id, then set the collaborator role to READER
// via `updateProjectV2Collaborators` (upsert — a duplicate grant is a no-op success).
//
// D6 scoping: the not-found translation is applied ONLY to the ResolveUserNodeID
// return value. graphqlDo wraps EVERY GitHub NOT_FOUND with
// forge.ErrGitHubUserNotFound — including one from a stale board node id on the
// visibility path — so a blanket errors.Is over the whole method would mis-map an
// unrelated 500 to a 422. Here the only NOT_FOUND that can reach this errors.Is is a
// bad username, which is the one case that is user-actionable → ErrProjectSyncUserNotFound
// (422). Every other resolve error (transient/permission) propagates → default 500.
func (s *ProjectSyncService) ShareWithUser(ctx context.Context, repoID uuid.UUID, username string) error {
	_, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
		return err
	}
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	userID, err := syncer.ResolveUserNodeID(ctx, username)
	if err != nil {
		if errors.Is(err, forge.ErrGitHubUserNotFound) {
			return ErrProjectSyncUserNotFound
		}
		return err
	}
	return syncer.SetProjectV2Collaborator(ctx, link.ProjectNodeID, userID, forge.RoleReaderCollaborator)
}

// Unshare revokes a GitHub user's access to the linked board (PRD #557 M2): resolve
// the login, then set the collaborator role to NONE. The username not-found mapping
// is scoped to the resolve step exactly as in ShareWithUser (see its D6 note); the
// only behavioral difference is the role set — NONE instead of READER.
func (s *ProjectSyncService) Unshare(ctx context.Context, repoID uuid.UUID, username string) error {
	_, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
		return err
	}
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	userID, err := syncer.ResolveUserNodeID(ctx, username)
	if err != nil {
		if errors.Is(err, forge.ErrGitHubUserNotFound) {
			return ErrProjectSyncUserNotFound
		}
		return err
	}
	return syncer.SetProjectV2Collaborator(ctx, link.ProjectNodeID, userID, forge.RoleNoneCollaborator)
}
