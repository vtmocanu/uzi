package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ForgeBuilder builds a forge driver from a stored (encrypted) connection.
// *forgesvc.Service satisfies it — the same seam privcheck, selfimprove and
// runlifecycle already use. workersvc cannot import forgesvc directly (forgesvc
// already imports workersvc, a cycle), so the composite forge-write operations
// lifted out of the handlers (PRD #191 Decision 8) reach the forge through this
// narrow interface, wired via SetForges in main.go.
type ForgeBuilder interface {
	ForgeForConnection(forgeType, baseURL string, tokenCiphertext []byte) (forge.Forge, error)
}

// RepoGuard is the #66 default-branch guardrail (privcheck.Service satisfies it).
// workersvc holds it as a narrow interface (not *privcheck.Service) to keep the
// dependency one-way. Nil ⇒ the service-layer gate (D1 layer 2) is skipped; layer 3
// (the claim backstop, M6) is the security net, so a wiring gap does not fail all
// runs. main.go always wires it via SetRepoGuard.
type RepoGuard interface {
	GuardRepo(ctx context.Context, in privcheck.GuardInput) privcheck.GuardResult
}

// CreatedIssue is the forge issue a confirmed proposal created — the subset the
// callers (web handler, Slack card) render. Kept local so the return type does not
// leak forge.Issue across the service boundary.
type CreatedIssue struct {
	IID    int64
	WebURL string
	Title  string
}

// Composite-op sentinels (PRD #191 M1). The caller (web handler, Slack card) maps
// each to a surface status. The two forge-call sentinels wrap the driver's
// already-redacted error so the caller can surface it verbatim; the build sentinel
// is opaque (a decryption/config failure is not the user's to read).
var (
	// ErrForgesUnavailable means SetForges was never wired — a misconfiguration, not
	// a user error.
	ErrForgesUnavailable = errors.New("forge builder not configured")
	// ErrProposalRepoGone: the proposal's target repo is no longer owned/available.
	ErrProposalRepoGone = errors.New("the proposal's target repo is no longer available")
	// ErrForgeBuild: the forge client could not be built (token undecryptable, key
	// rotated, connection misconfigured). Opaque → 500.
	ErrForgeBuild = errors.New("could not build a forge client for the connection")
	// ErrForgeIssueWrite: the forge rejected CreateIssue. Wraps the redacted driver
	// error → 502.
	ErrForgeIssueWrite = errors.New("could not create the issue on the forge")
	// ErrForgeIssueRead: the forge rejected GetIssue. Wraps the redacted driver error
	// → 502.
	ErrForgeIssueRead = errors.New("could not read the issue from the forge")
)

// guardDefaultBranch is the #66 service-layer guardrail (D1 layer 2), the ONE
// shared helper the PAT-bearing run-create paths (issue lane, CI-fix,
// self-improve, and scheduled prompt) call right after fetching the repo with
// GetRepoForUser. It runs the live, fail-closed guard and returns a
// *GuardrailBlockedError (carrying the block-finding messages for the 422 body)
// when the bot can reach the default branch or that could not be verified.
//
// A nil guard is a no-op: see the RepoGuard doc — the claim backstop (M6, layer 3)
// is the security net, so a wiring gap never fails all runs. Overridden comes from
// the live guardrail_override_reason column (M8): a non-NULL reason means the admin
// per-repo override is active, so GuardRepo downgrades the waivable findings
// post-evaluation — never protection_unreadable (D8/D3).
func (s *Service) guardDefaultBranch(ctx context.Context, row store.GetRepoForUserRow) error {
	if s.guard == nil {
		return nil // see RepoGuard doc: layer 3 is the net
	}
	res := s.guard.GuardRepo(ctx, privcheck.GuardInput{
		ForgeType:       row.ForgeType,
		BaseURL:         row.BaseUrl,
		TokenCiphertext: row.TokenCiphertext,
		Repo: privcheck.Repo{
			ID:             row.ID.String(),
			Path:           row.PathWithNamespace,
			ForgeProjectID: row.ForgeProjectID,
			DefaultBranch:  row.DefaultBranch.String,
		},
		// Live per-repo override (M8): NULL reason ⇒ no override.
		Overridden: row.GuardrailOverrideReason.Valid,
	})
	if res.Blocked {
		return &GuardrailBlockedError{Findings: res.BlockMessages()}
	}
	return nil
}

// ConfirmProposalForUser executes a proposed issue on the forge (PRD #191 Decision
// 8): the ONLY path that turns a pending proposal into a real forge issue, lifted
// out of the web handler so the Slack proposal card can call the identical
// claim/forge/settle flow. Forge-first via the caller's OWN connection, owner-scoped
// through the chat run.
//
// Claim-first: the proposal atomically moves pending -> confirming BEFORE the forge
// write, so of two concurrent confirms exactly one reaches CreateIssue (the other
// gets ErrProposalNotPending). Every failure AFTER the claim reverts the row to
// pending so the user can retry or dismiss. The final settle (confirming ->
// confirmed) is log-only on error because the issue already exists — reverting then
// would orphan a real forge issue.
func (s *Service) ConfirmProposalForUser(ctx context.Context, userID, runID, propID uuid.UUID) (CreatedIssue, error) {
	if s.forges == nil {
		return CreatedIssue{}, ErrForgesUnavailable
	}

	claim, err := s.ClaimProposalForConfirm(ctx, userID, runID, propID)
	if err != nil {
		// ErrProposalNotFound / ErrProposalNotPending flow straight to the caller's
		// lookup-error mapping; nothing was claimed, so there is nothing to revert.
		return CreatedIssue{}, err
	}

	// Load the target repo + its connection PAT (the user must still own it).
	repo, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: claim.RepoID, UserID: userID})
	if err != nil {
		s.revertProposal(ctx, propID)
		if errors.Is(err, pgx.ErrNoRows) {
			return CreatedIssue{}, ErrProposalRepoGone
		}
		return CreatedIssue{}, err
	}

	f, err := s.forges.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		s.revertProposal(ctx, propID)
		return CreatedIssue{}, fmt.Errorf("%w: %v", ErrForgeBuild, err)
	}

	var labels []string
	if len(claim.Labels) > 0 {
		if err := json.Unmarshal(claim.Labels, &labels); err != nil {
			labels = nil
		}
	}

	created, err := f.CreateIssue(ctx, repo.ForgeProjectID, claim.Title, claim.Description, labels)
	if err != nil {
		s.revertProposal(ctx, propID)
		// err is already PAT-redacted by the driver.
		return CreatedIssue{}, fmt.Errorf("%w: %v", ErrForgeIssueWrite, err)
	}

	// Settle confirming -> confirmed with the created iid. This row is ours (we hold
	// the claim), so a non-nil error here is unexpected; the issue WAS created, so log
	// and surface it rather than revert.
	if err := s.ConfirmProposal(ctx, propID, created.IID); err != nil {
		slog.Error("confirm proposal: mark confirmed after issue creation",
			"proposal", propID.String(), "issue_iid", created.IID, "error", err)
	}
	return CreatedIssue{IID: created.IID, WebURL: created.WebURL, Title: created.Title}, nil
}

// revertProposal best-effort returns a claimed ('confirming') proposal to pending
// after a post-claim failure; a revert error is logged but never changes the error
// the caller surfaces for the underlying failure.
func (s *Service) revertProposal(ctx context.Context, propID uuid.UUID) {
	if err := s.RevertProposalToPending(ctx, propID); err != nil {
		slog.Error("confirm proposal: revert to pending", "proposal", propID.String(), "error", err)
	}
}

// DismissProposalForUser dismisses a pending proposal, ownership-checked through the
// owning chat run (PRD #191 M4): the Slack Dismiss button's counterpart to the web
// handler's GetChatProposal + DismissProposal. It NEVER touches the forge. Returns the
// lookup sentinels (ErrProposalNotFound → not yours/gone, ErrProposalNotPending →
// already resolved) unchanged.
func (s *Service) DismissProposalForUser(ctx context.Context, userID, runID, propID uuid.UUID) error {
	if _, err := s.GetChatProposal(ctx, userID, runID, propID); err != nil {
		return err
	}
	return s.DismissProposal(ctx, propID)
}

// StartRunForUser queues an agent run for an issue (PRD #191 M1): the forge
// GetIssue + CreateRun composite lifted out of the web CreateRun handler so the
// Slack start-run card can start a run identically. The issue description is
// snapshotted from the forge (the source of truth) at queue time.
//
// Eligibility is the single uzi_label gate CreateRun enforces (PRD #764 M1); this
// composite no longer computes any PRD-link bypass. It returns the CreateRun
// sentinels unchanged (ErrNotPRDIssue, ErrActiveRunExists, …) plus the forge
// sentinels above.
func (s *Service) StartRunForUser(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, waitOnLimit *bool, seed *SeededPlan) (store.Run, error) {
	if s.forges == nil {
		return store.Run{}, ErrForgesUnavailable
	}
	repo, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRepoNotFound
		}
		return store.Run{}, err
	}
	f, err := s.forges.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		return store.Run{}, fmt.Errorf("%w: %v", ErrForgeBuild, err)
	}
	issue, err := f.GetIssue(ctx, repo.ForgeProjectID, issueIID)
	if err != nil {
		// err is already PAT-redacted by the driver.
		return store.Run{}, fmt.Errorf("%w: %v", ErrForgeIssueRead, err)
	}
	return s.CreateRun(ctx, userID, repo.ID, issueIID, issue.Description, waitOnLimit, seed)
}

// StartRunForUserByPath is StartRunForUser keyed by the human repo PATH the chat
// surfaces expose (PRD #191 M5): the start-run card carries repo_path (what list_runs
// shows), not the internal repo id. It resolves the path to the user's own repo id
// (ErrRepoNotFound for an unknown/foreign path) and delegates. Same gate, same
// sentinels as the web start button.
func (s *Service) StartRunForUserByPath(ctx context.Context, userID uuid.UUID, repoPath string, issueIID int64, waitOnLimit *bool, seed *SeededPlan) (store.Run, error) {
	repoID, err := s.q.GetRepoIDByPathForUser(ctx, store.GetRepoIDByPathForUserParams{Path: repoPath, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRepoNotFound
		}
		return store.Run{}, err
	}
	return s.StartRunForUser(ctx, userID, repoID, issueIID, waitOnLimit, seed)
}
