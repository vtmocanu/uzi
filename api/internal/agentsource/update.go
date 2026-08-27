package agentsource

import (
	"context"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// UpdateCheckResult summarizes one update-check pass (PRD #702 M4). Status is
// "disabled", "ok", or "error". On "ok" LatestRef is the newest IsValid semver tag on
// the source ("" when none) and RemoteTipSHA is the advertised tip of the configured
// ref; both are also persisted as engine settings. On "disabled"/"error" nothing is
// persisted and Message carries the (already PAT-scrubbed) reason.
type UpdateCheckResult struct {
	LatestRef    string
	RemoteTipSHA string
	Status       string
	Message      string
}

// shaHexLen is the length of a full git object id in hex; a ref of exactly this many
// hex digits is treated as an immutable SHA pin (Decision 6).
const shaHexLen = 40

// CheckForUpdate ls-remotes the CONFIGURED source (sealed credential) and persists the
// remote facts the GET/status derive against (PRD #702 M4). It mirrors Reconcile's
// structure: read config, SSRF-recheck, read the sealed credential, a SINGLE ref
// advertisement (no pack fetch), then persist. It never mutates agent_templates and
// never derives "update available" — that is DeriveUpdate's read-time job. The returned
// error is always nil today (every failure is recorded in the result Status/Message,
// not propagated); the signature matches the engine convention.
func (r *Reconciler) CheckForUpdate(ctx context.Context) (UpdateCheckResult, error) {
	rawURL, _ := r.settings.AgentSourceRepoURL(ctx)
	url := strings.TrimSpace(rawURL)
	if url == "" {
		return UpdateCheckResult{Status: "disabled", Message: "agent source is not configured"}, nil
	}

	// TOCTOU re-check: the env allowlist can have shrunk since the URL was stored, so
	// re-validate at the egress seam. On a miss, DO NOT ls-remote or persist.
	if !r.cfg.AgentSourceBaseURLAllowed(url) {
		return UpdateCheckResult{Status: statusError, Message: "source url is not on the AGENT_SOURCE_ALLOWED_BASE_URLS allowlist"}, nil
	}

	ref, _ := r.settings.AgentSourceRef(ctx)
	ref = strings.TrimSpace(ref)
	token, terr := r.settings.AgentSourceCredential(ctx)
	if terr != nil {
		// A decrypt failure is a misconfiguration, not a reason to reach anonymously
		// against a private repo. Record nothing and bail.
		return UpdateCheckResult{Status: statusError, Message: "could not read the source credential"}, nil
	}

	refs, lerr := r.lsRemote(ctx, CloneOptions{
		CloneURL:        url,
		Ref:             ref,
		Token:           token,
		RedirectAllowed: r.cfg.AgentSourceBaseURLAllowed,
	})
	if lerr != nil {
		// Unreachable / auth / off-allowlist redirect: the message is already PAT-scrubbed
		// by the ls-remote helper. Persist nothing (keep the last-good remote facts).
		return UpdateCheckResult{Status: statusError, Message: lerr.Error()}, nil
	}

	latest := pickLatestSemverTag(refs.Tags)
	tip := resolveRefTip(refs, ref)

	// Persist the remote facts (the recordStatus pattern). Best-effort; a write failure
	// is logged, never fatal.
	writes := []store.UpsertAppSettingParams{
		{Key: settings.KeyAgentSourceLatestRef, Value: latest},
		{Key: settings.KeyAgentSourceRemoteTipSHA, Value: tip},
		{Key: settings.KeyAgentSourceUpdateCheckedAt, Value: r.now().UTC().Format(time.RFC3339)},
	}
	for _, w := range writes {
		if _, err := r.store.UpsertAppSetting(ctx, w); err != nil {
			r.logger.Error("agentsource: persist update-check remote facts", "key", w.Key, "error", err)
		}
	}
	r.settings.Invalidate()

	return UpdateCheckResult{Status: statusOK, LatestRef: latest, RemoteTipSHA: tip}, nil
}

// resolveRefTip picks the advertised tip SHA of the configured ref: empty ref → HEAD;
// else a matching branch tip; else a matching tag; else HEAD (the source advertises no
// such ref, so fall back to its default-branch tip).
func resolveRefTip(refs RemoteRefs, ref string) string {
	if ref == "" {
		return refs.HeadSHA
	}
	if sha, ok := refs.Branches[ref]; ok {
		return sha
	}
	if sha, ok := refs.Tags[ref]; ok {
		return sha
	}
	return refs.HeadSHA
}

// DeriveUpdate computes "update available" at READ time (no egress) from the persisted
// remote facts + the live config, implementing Decision 6. It is pure and self-clearing:
// after a bump (ref := latestRef) tag-mode compares equal → false; after an apply
// (lastAppliedSHA := tip) branch-mode compares equal → false.
//
//   - SHA-pinned (ref is 40-hex): no signal (an immutable pin is intentionally frozen).
//   - tag-pinned (ref is a valid semver): available iff the newest source tag is
//     strictly semver-greater than the pin; latest names that tag.
//   - branch-pinned / empty: available iff the advertised tip differs from the applied
//     SHA ("moved"); latest is "" (a shallow ls-remote yields only tips, not history).
func DeriveUpdate(pinnedRef, lastAppliedSHA, latestRef, remoteTipSHA string) (available bool, latest string) {
	r := strings.TrimSpace(pinnedRef)

	if isSHAPin(r) {
		return false, ""
	}

	if cand := "v" + strings.TrimPrefix(r, "v"); semver.IsValid(cand) {
		// tag-pinned: newest source tag strictly greater than the pin.
		if latestRef != "" && semverNewer(latestRef, r) {
			return true, latestRef
		}
		return false, ""
	}

	// branch-pinned / empty: the advertised tip moved past what is running.
	if remoteTipSHA != "" && !strings.EqualFold(remoteTipSHA, lastAppliedSHA) {
		return true, ""
	}
	return false, ""
}

// isSHAPin reports whether ref is exactly a 40-hex git object id (an immutable pin).
func isSHAPin(ref string) bool {
	if len(ref) != shaHexLen {
		return false
	}
	for _, c := range ref {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// semverNewer reports whether tag a is a strictly-greater semver than tag b, guarding
// BOTH operands with the re-prefix + IsValid discipline (Decision 4): x/mod/semver
// treats a malformed version as equal (Compare returns 0), so an unguarded compare fails
// silently open. Either operand not being valid semver → false.
func semverNewer(a, b string) bool {
	ca := "v" + strings.TrimPrefix(a, "v")
	cb := "v" + strings.TrimPrefix(b, "v")
	if !semver.IsValid(ca) || !semver.IsValid(cb) {
		return false
	}
	return semver.Compare(ca, cb) > 0
}
