package settings

// This file holds the agent-source sync accessors, their bounds consts and their
// write-time validators (PRD #1021 M2, split verbatim from settings.go).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// AgentSourceEnabled reports whether the agent-source reconcile loop is enabled
// (PRD #602 M3). Strict "true"/"false" with a false fallback, like JudgeEnabled
// — a malformed value never silently starts cloning an external repo.
func (c *Cache) AgentSourceEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyAgentSourceEnabled)
}

// AgentSourceRepoURL returns the configured https clone URL (PRD #602 M3), or ""
// when unconfigured. The value is stored verbatim; the reconcile re-checks it
// against the SSRF allowlist at the clone seam (TOCTOU defense).
func (c *Cache) AgentSourceRepoURL(ctx context.Context) (string, error) {
	return c.get(ctx, KeyAgentSourceRepoURL)
}

// AgentSourceRef returns the pinned tag/SHA or floating branch to clone (PRD #602
// M3), or "" to track the source's default branch.
func (c *Cache) AgentSourceRef(ctx context.Context) (string, error) {
	return c.get(ctx, KeyAgentSourceRef)
}

// AgentSourceFolder returns the repo-relative subfolder the reconcile reads role
// files from (PRD #702 M1). An empty/unset (or whitespace-only) value resolves to
// DefaultAgentSourceFolder (".claude/agents"), so existing installs read the same
// subtree as before. A configured value is returned with any single trailing slash
// trimmed ("product-agents/" → "product-agents"), the normalization that guarantees
// tree.Tree always receives a clean path.
func (c *Cache) AgentSourceFolder(ctx context.Context) (string, error) {
	v, err := c.get(ctx, KeyAgentSourceFolder)
	if strings.TrimSpace(v) == "" {
		return DefaultAgentSourceFolder, err
	}
	return strings.TrimSuffix(v, "/"), err
}

// AgentSourceInterval returns the reconcile cadence (PRD #602 M3). Stored as a Go
// duration string ("1h"); a missing or unparseable value falls back to the
// compiled-in default, and a sub-minute value is floored at 1m so a bad row can
// never make the loop hammer the source (the same floor validateAgentSourceInterval
// enforces at write time).
func (c *Cache) AgentSourceInterval(ctx context.Context) (time.Duration, error) {
	v, err := c.get(ctx, KeyAgentSourceInterval)
	d, perr := time.ParseDuration(v)
	if perr != nil || d <= 0 {
		d, _ = time.ParseDuration(DefaultAgentSourceInterval)
	}
	if d < agentSourceIntervalMin {
		d = agentSourceIntervalMin
	}
	return d, err
}

// AgentSourceCredential returns the private-repo clone token in plaintext (PRD #602
// M3), or "" for a public repo. Same precedence as SlackBotToken: an ENV overlay
// wins, else the sealed DB row is opened with the box. Errors carry no plaintext.
func (c *Cache) AgentSourceCredential(ctx context.Context) (string, error) {
	return c.secret(ctx, KeyAgentSourceCredential)
}

// AgentSourceCredentialConfigured reports whether a private-repo clone token is set
// from any source (PRD #602 M4), without exposing it — the only thing the admin GET
// ever reveals about the sealed credential (mirrors the Slack token's configured bit).
func (c *Cache) AgentSourceCredentialConfigured(ctx context.Context) (bool, error) {
	m, err := c.snapshot(ctx)
	if err != nil {
		return false, err
	}
	return c.configured(KeyAgentSourceCredential, m), nil
}

// AgentSourceLastAppliedSHA returns the fetched SHA of the snapshot last applied to
// agent_templates (PRD #602 M4), or "" when nothing has been applied yet. Apply
// compares the currently-staged snapshot's SHA against this to decide "pending":
// pending == a staged snapshot exists AND its fetched_sha != this value.
func (c *Cache) AgentSourceLastAppliedSHA(ctx context.Context) (string, error) {
	return c.get(ctx, KeyAgentSourceLastAppliedSHA)
}

// AgentSourceStatus is the engine-managed sync/apply status the admin panel reads
// (PRD #602 M4). Every field is stored as an app_setting by the reconcile job
// (last-sync-*) or by Apply (last-applied-*); an absent key reads as "". CountsJSON
// is the raw {"staged":N,"changed":N,"failed":N} blob so the handler can surface it
// without this package importing a counts type.
type AgentSourceStatus struct {
	LastSyncAt     string
	LastSyncSHA    string
	LastSyncStatus string
	LastSyncError  string
	CountsJSON     string
	LastAppliedAt  string
	LastAppliedSHA string
	// Remote facts persisted by the PRD #702 M4 update-check (engine-managed); an
	// absent key reads as "". "Update available" is DERIVED from these + live config,
	// never stored — see agentsource.DeriveUpdate.
	LatestRef       string
	RemoteTipSHA    string
	UpdateCheckedAt string
}

// AgentSourceStatus reads the engine-managed last-sync + last-applied status keys in
// one snapshot pass (PRD #602 M4). Best-effort: a snapshot error returns the zero
// status alongside the error so a best-effort caller can still render an empty panel.
func (c *Cache) AgentSourceStatus(ctx context.Context) (AgentSourceStatus, error) {
	m, err := c.snapshot(ctx)
	if err != nil {
		return AgentSourceStatus{}, err
	}
	return AgentSourceStatus{
		LastSyncAt:      c.effective(KeyAgentSourceLastSyncAt, m),
		LastSyncSHA:     c.effective(KeyAgentSourceLastSyncSHA, m),
		LastSyncStatus:  c.effective(KeyAgentSourceLastSyncStatus, m),
		LastSyncError:   c.effective(KeyAgentSourceLastSyncError, m),
		CountsJSON:      c.effective(KeyAgentSourceLastSyncCounts, m),
		LastAppliedAt:   c.effective(KeyAgentSourceLastAppliedAt, m),
		LastAppliedSHA:  c.effective(KeyAgentSourceLastAppliedSHA, m),
		LatestRef:       c.effective(KeyAgentSourceLatestRef, m),
		RemoteTipSHA:    c.effective(KeyAgentSourceRemoteTipSHA, m),
		UpdateCheckedAt: c.effective(KeyAgentSourceUpdateCheckedAt, m),
	}, nil
}

// maxAgentSourceCredentialLen caps the sealed clone token (PRD #602 M2). It is
// generous on purpose: a GitHub fine-grained PAT (github_pat_...) is ~93 chars,
// well over the 64-char label cap that KeyAgentSourceCredential would otherwise
// inherit from the ValidateLabel default branch, so a legitimate token must not
// be rejected at write. It is a single-line opaque token, never a multi-line key.
const maxAgentSourceCredentialLen = 1024

// validateAgentSourceCredential is the write-time gate for the sealed private-repo
// clone token (PRD #602 M2). Unlike the label default branch it does NOT cap at 64
// chars or ban commas — a real PAT is longer and may contain them. It rejects an
// empty/whitespace-only value, any control character, and any internal whitespace
// (a clone token is a single opaque token with no spaces), and caps a generous
// length. The error never echoes the value.
func validateAgentSourceCredential(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("credential must not be empty")
	}
	if utf8.RuneCountInString(value) > maxAgentSourceCredentialLen {
		return fmt.Errorf("credential must be at most %d characters", maxAgentSourceCredentialLen)
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("credential must not contain whitespace or control characters")
		}
	}
	return nil
}

// agentSourceIntervalMin is the reconcile-cadence floor (PRD #602 M2): a sub-minute
// interval would let the reconcile loop hammer the source, so the write is rejected.
const agentSourceIntervalMin = time.Minute

// maxAgentSourceRefLen caps the git ref length (PRD #602 M2). A tag/branch/SHA is
// short; the bound only catches a runaway paste.
const maxAgentSourceRefLen = 256

// validateAgentSourceInterval is the write-time gate for the agent-source reconcile
// cadence (PRD #602 M2): a Go duration string ("1h", "15m") that parses to at least
// agentSourceIntervalMin. Unlike selfimprove_interval (validateDuration, positive
// only) it enforces a 1-minute floor so a fat-fingered sub-minute value cannot make
// the reconcile loop hammer the source.
func validateAgentSourceInterval(value string) error {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return errors.New(`must be a duration like "1h"`)
	}
	if d < agentSourceIntervalMin {
		return errors.New("must be at least 1m")
	}
	return nil
}

// validateAgentSourceRepoURL is the write-time gate for the agent-source clone URL
// (PRD #602 M2). Empty is allowed — it is the "unconfigured" value (the feature is
// off by default and no canonical repo is pre-filled, ADR-0602). A non-empty value
// must be an absolute https URL with a host. The SEPARATE SSRF allowlist check
// against AGENT_SOURCE_ALLOWED_BASE_URLS is the HANDLER's job (Validate is a pure
// (key,value) function with no Config, and importing config here is a cycle), so
// this only enforces the format — https, absolute, non-empty host.
func validateAgentSourceRepoURL(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	// Reject surrounding whitespace rather than silently trimming: the value is
	// stored verbatim in a non-secret key, and a padded URL would be fed to go-git
	// at the M3 clone seam. Validate and the allowlist check both trim, so without
	// this the stored form could differ from the checked form.
	if value != strings.TrimSpace(value) {
		return errors.New("must not have leading or trailing whitespace")
	}
	u, err := url.Parse(value)
	if err != nil {
		return errors.New("must be a valid URL")
	}
	if u.Scheme != "https" {
		return errors.New("must use https")
	}
	if u.Host == "" {
		return errors.New("must include a host")
	}
	// Reject URL userinfo (https://token@host/...): agent_source_repo_url is a
	// NON-secret key, stored in cleartext and surfaced in the admin GET and (M3)
	// go-git clone error strings. A credential belongs in the sealed
	// agent_source_credential field, never in the URL.
	if u.User != nil {
		return errors.New("must not embed credentials in the URL; use the agent_source_credential field")
	}
	return nil
}

// validateAgentSourceRef is the write-time gate for the pinned git ref (PRD #602
// M2): a tag, branch, or SHA. Empty is allowed (unused while the URL is empty). A
// non-empty value must be a single token — no whitespace and no control characters,
// length-capped — mirroring the label-style single-token rule. A pinned tag/SHA is
// the recommended form and a bare branch is the floating opt-in; the two are not
// distinguished here.
func validateAgentSourceRef(value string) error {
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be whitespace only")
	}
	if utf8.RuneCountInString(value) > maxAgentSourceRefLen {
		return fmt.Errorf("must be at most %d characters", maxAgentSourceRefLen)
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("must not contain whitespace or control characters")
		}
	}
	return nil
}

// validateAgentSourceFolder is the write-time gate for the agent-source subfolder
// (PRD #702 M1, Decision 2). It selects a subtree of the already-cloned,
// already-allowlisted source repo, so it must be a CLEAN repo-relative subpath — it
// never reaches the network and adds no egress. Empty is allowed (it resolves to
// DefaultAgentSourceFolder at read time, so existing installs are unchanged). A
// non-empty value is rejected when it: is whitespace-only; exceeds the length cap;
// contains a control character; looks like a URL (contains "://") or a UNC path
// (leading "\\"); has a leading "/" (it is repo-relative, not absolute); carries a
// ".." path segment (which would escape the subtree); or has a scheme-like ":" in
// its first segment. A single trailing slash is ACCEPTED here (the Cache reader
// normalizes it away before tree.Tree) — this is validation, not a rewrite, so the
// illegal cases return an error rather than being silently cleaned.
func validateAgentSourceFolder(value string) error {
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be whitespace only")
	}
	if utf8.RuneCountInString(value) > maxAgentSourceRefLen {
		return fmt.Errorf("must be at most %d characters", maxAgentSourceRefLen)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return errors.New("must not contain control characters")
		}
	}
	if strings.Contains(value, "://") {
		return errors.New("must be a repo-relative path, not a URL")
	}
	if strings.HasPrefix(value, `\\`) {
		return errors.New("must be a repo-relative path, not a UNC path")
	}
	if strings.HasPrefix(value, "/") {
		return errors.New("must be repo-relative (no leading slash)")
	}
	// Analyze segments against the trailing-slash-normalized form (the reader trims
	// the same single trailing slash before tree.Tree). A ".." segment escapes the
	// subtree; a ":" in the first segment is a scheme/host smell.
	trimmed := strings.TrimSuffix(value, "/")
	for i, seg := range strings.Split(trimmed, "/") {
		if seg == ".." {
			return errors.New(`must not contain a ".." path segment`)
		}
		if i == 0 && strings.Contains(seg, ":") {
			return errors.New("must be a repo-relative path, not a scheme/host")
		}
	}
	return nil
}
