package workersvc

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgecred"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ErrForgeCredentialUnavailable deliberately carries no stored or upstream credential.
var ErrForgeCredentialUnavailable = errors.New("forge credential unavailable")

// openGitCredential is the only way a decrypted connection becomes a git credential.
// App private keys stay in the API; callers receive installation tokens only.
func (s *Service) openGitCredential(ctx context.Context, kind, baseURL string, ciphertext []byte) (string, time.Time, error) {
	plain, err := forgecred.Open(s.box, ciphertext)
	if err != nil {
		return "", time.Time{}, ErrForgeCredentialUnavailable
	}
	token, expiry, err := forge.ResolveGitCredentialWithExpiry(ctx, forge.Type(kind), baseURL, string(plain), 20*time.Second)
	if err != nil {
		return "", time.Time{}, ErrForgeCredentialUnavailable
	}
	return token, expiry, nil
}

func forgeCredentialRunAllowed(run store.Run, worker store.Worker, generation int64, cancelled bool) bool {
	if generation <= 0 || run.ClaimGeneration != generation || cancelled || run.ClaimReleasedAt.Valid ||
		run.UserID != worker.UserID || !run.WorkerID.Valid || uuid.UUID(run.WorkerID.Bytes) != worker.ID ||
		!run.RepoID.Valid || run.EgressProfileID.Valid {
		return false
	}
	switch run.Status {
	case "claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup":
		return true
	default:
		return false
	}
}

// RefreshForgeCredential derives connection and repository solely from a live,
// worker-owned claim. Authority is checked again after the external token exchange.
func (s *Service) RefreshForgeCredential(ctx context.Context, worker store.Worker, runID uuid.UUID, generation int64) (string, time.Time, error) {
	return s.refreshForgeCredential(ctx, worker, runID, generation, s.openGitCredential)
}

func (s *Service) refreshForgeCredential(ctx context.Context, worker store.Worker, runID uuid.UUID, generation int64, resolve func(context.Context, string, string, []byte) (string, time.Time, error)) (string, time.Time, error) {
	if _, quarantined := s.ResidueQuarantineFor(worker.ID); quarantined {
		return "", time.Time{}, ErrForgeCredentialUnavailable
	}
	run, cancelled, err := s.runOwnershipRead(ctx, runID, worker)
	if err != nil || !forgeCredentialRunAllowed(run, worker, generation, cancelled) {
		return "", time.Time{}, ErrRunNotOwned
	}
	rc, err := s.q.GetRunClaimContext(ctx, runID)
	if err != nil {
		return "", time.Time{}, ErrForgeCredentialUnavailable
	}
	// Reapply the existing run-lane guard before releasing another write credential.
	if s.guard != nil {
		result := s.guard.GuardRepo(ctx, privcheck.GuardInput{
			ForgeType: rc.ForgeType, BaseURL: rc.BaseUrl, TokenCiphertext: rc.TokenCiphertext,
			Repo:       privcheck.Repo{ID: uuid.UUID(run.RepoID.Bytes).String(), Path: rc.RepoPath, ForgeProjectID: rc.ForgeProjectID, DefaultBranch: rc.DefaultBranch.String},
			Overridden: rc.GuardrailOverrideReason.Valid,
		})
		if result.Blocked {
			return "", time.Time{}, ErrForgeCredentialUnavailable
		}
	}
	token, expiry, err := resolve(ctx, rc.ForgeType, rc.BaseUrl, rc.TokenCiphertext)
	if err != nil || expiry.IsZero() {
		return "", time.Time{}, ErrForgeCredentialUnavailable
	}
	if _, quarantined := s.ResidueQuarantineFor(worker.ID); quarantined {
		return "", time.Time{}, ErrForgeCredentialUnavailable
	}
	again, cancelled, err := s.runOwnershipRead(ctx, runID, worker)
	if err != nil || !forgeCredentialRunAllowed(again, worker, generation, cancelled) {
		return "", time.Time{}, ErrRunNotOwned
	}
	current, err := s.q.GetRunClaimContext(ctx, runID)
	if err != nil || current.ForgeType != rc.ForgeType || current.BaseUrl != rc.BaseUrl || current.ForgeProjectID != rc.ForgeProjectID || !bytes.Equal(current.TokenCiphertext, rc.TokenCiphertext) {
		return "", time.Time{}, ErrForgeCredentialUnavailable
	}
	return token, expiry, nil
}

func forgeExpiryPointer(expiry time.Time) *time.Time {
	if expiry.IsZero() {
		return nil
	}
	return &expiry
}

// gitCredentialUsername separates machine HTTP authentication from commit/PR attribution.
func gitCredentialUsername(kind, username, token string) string {
	if kind == string(forge.TypeGitHub) && strings.HasPrefix(token, "ghs_") {
		return "x-access-token"
	}
	return username
}
