package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// Held-work publication (issue #2545): EVERY forge call it makes, step A's fetch and push, step
// B's proof listing, the reconcile listing and the retention delete, takes its connection from
// heldRemote. There is no other path to the credential, so the SSRF gate cannot be skipped by a
// caller that forgets it, and the worker never holds a forge credential for this.

// ErrHeldRemoteGone means the run, its repo or its forge connection no longer exists, so no forge
// call can ever be brokered for it again.
var ErrHeldRemoteGone = errors.New("held publication: the run, its repo or its forge connection is gone")

// HeldForge is the server-derived forge connection of one run's held publication. The PAT is
// decrypted only after the SSRF gate passed; it never enters a log, an error or a DTO.
type HeldForge struct {
	CloneURL string
	Username string
	PAT      string
}

// ListOptions is the ListRefTips connection for f.
func (f HeldForge) ListOptions() pushbroker.ListRefsOptions {
	return pushbroker.ListRefsOptions{CloneURL: f.CloneURL, Username: f.Username, PAT: f.PAT}
}

// DeleteOptions is the compare-and-swap delete of ref at expectedTip over f. A held ref is never
// deleted unconditionally (pushbroker.Delete refuses an empty tip).
func (f HeldForge) DeleteOptions(ref, expectedTip string) pushbroker.DeleteOptions {
	return pushbroker.DeleteOptions{
		CloneURL: f.CloneURL, Username: f.Username, PAT: f.PAT, Ref: ref, ExpectedOldTip: expectedTip,
	}
}

// ApplyTo fills o's connection fields from f.
func (f HeldForge) ApplyTo(o *pushbroker.HeldPackOptions) {
	o.CloneURL, o.Username, o.PAT = f.CloneURL, f.Username, f.PAT
}

// HeldRemoteFunc derives the forge connection for runID. recovery.Service takes one for step B
// (so the proof listing goes through the same gate as step A's push): inject
// (*Service).HeldRemote().
type HeldRemoteFunc func(ctx context.Context, runID uuid.UUID) (HeldForge, error)

// HeldRemote returns the injectable form of heldRemote.
func (s *Service) HeldRemote() HeldRemoteFunc { return s.heldRemote }

// heldRemote derives the clone URL, bot username and PAT for runID through forgeForRetention,
// the same server-side derivation Publish, retention and salvage use: GetRunClaimContext, the
// SSRF gate (forgeBaseURLAllowed) on both the connection's base URL and the dialed clone host,
// BEFORE the PAT is decrypted by box.Open. A missing gate or box is an error checked first,
// never fail-open. A run, repo or connection that is gone is ErrHeldRemoteGone; any other
// problem is a retryable error whose text is already safe to persist.
func (s *Service) heldRemote(ctx context.Context, runID uuid.UUID) (HeldForge, error) {
	if s.forgeBaseURLAllowed == nil {
		return HeldForge{}, errors.New("held publication: forge base URL allowlist is not configured")
	}
	if s.box == nil {
		return HeldForge{}, errors.New("held publication: secret box is not configured")
	}
	f, problem, gone := s.forgeForRetention(ctx, runID)
	switch {
	case gone:
		return HeldForge{}, ErrHeldRemoteGone
	case problem != "":
		return HeldForge{}, errors.New("held publication: " + problem)
	}
	return HeldForge{CloneURL: f.cloneURL, Username: f.username, PAT: f.pat}, nil
}
