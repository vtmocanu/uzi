package settings

// This file holds the upstream-release-check accessors, their bounds consts and
// their write-time validators (PRD #1021 M2, split verbatim from settings.go).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ReleaseCheckEnabled reports whether the upstream-release check is enabled (PRD
// #836 M1): the master gate — when off the api never calls github.com. Stored as
// "true"/"false"; any other value falls back to the compiled-in default (true), the
// same junk-tolerance as SlackEnabled but defaulting ON, so a malformed value never
// silently disables the check.
func (c *Cache) ReleaseCheckEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyReleaseCheckEnabled)
}

// ReleaseCheckBannerEnabled reports whether the intrusive escalation banner is
// enabled (PRD #836 M1). Independent of the master gate: it governs only the banner
// (the pip and admin card do not depend on it). Same junk-tolerant defaulting ON.
func (c *Cache) ReleaseCheckBannerEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyReleaseCheckBannerEnabled)
}

// ReleaseCheckInterval returns the release-check poll cadence (PRD #836 M1). Stored
// as a Go duration string ("6h"); a missing or unparseable value falls back to the
// compiled-in default, and a sub-minute value is floored at releaseCheckIntervalMin
// so a bad row can never make the loop hammer github.com (the same floor
// validateReleaseCheckInterval enforces at write time).
func (c *Cache) ReleaseCheckInterval(ctx context.Context) (time.Duration, error) {
	v, err := c.get(ctx, KeyReleaseCheckInterval)
	d, perr := time.ParseDuration(v)
	if perr != nil || d <= 0 {
		d, _ = time.ParseDuration(DefaultReleaseCheckInterval)
	}
	if d < releaseCheckIntervalMin {
		d = releaseCheckIntervalMin
	}
	return d, err
}

// ReleaseCheckToken returns the OPTIONAL upstream-check GitHub token in plaintext
// (PRD #836 M1), or "" when unconfigured (the unauthenticated path). Same secret
// precedence as SlackBotToken: an ENV overlay wins, else the sealed DB row is opened
// with the box. Errors carry no plaintext.
func (c *Cache) ReleaseCheckToken(ctx context.Context) (string, error) {
	return c.secret(ctx, KeyReleaseCheckToken)
}

// ReleaseStatus is the engine-managed remote-release facts the release-check Runner
// persists (PRD #836 M1) and the derivation + admin panel read. Every field is
// stored as an app_setting by the Runner; an absent key reads as "". "Update
// available"/"far behind"/"security" are DERIVED from these plus the running version,
// never stored — see releasecheck.UpdateAvailable / FarBehind / Security.
type ReleaseStatus struct {
	LatestTag   string
	LatestName  string
	Body        string
	NotesURL    string
	PublishedAt string
	CheckedAt   string
	// BannerSnoozeTag is the release tag the escalation banner was snoozed for (PRD
	// #836 M6), or "" when never snoozed. "banner_snoozed" is derived: it is true iff
	// this equals LatestTag, so a newer release auto-clears the snooze.
	BannerSnoozeTag string
}

// ReleaseStatus reads the six engine-managed release-fact keys in one snapshot pass
// (PRD #836 M1). Best-effort: a snapshot error returns the zero status alongside the
// error so a best-effort caller can still render an empty panel.
func (c *Cache) ReleaseStatus(ctx context.Context) (ReleaseStatus, error) {
	m, err := c.snapshot(ctx)
	if err != nil {
		return ReleaseStatus{}, err
	}
	return ReleaseStatus{
		LatestTag:       c.effective(KeyReleaseLatestTag, m),
		LatestName:      c.effective(KeyReleaseLatestName, m),
		Body:            c.effective(KeyReleaseLatestBody, m),
		NotesURL:        c.effective(KeyReleaseNotesURL, m),
		PublishedAt:     c.effective(KeyReleasePublishedAt, m),
		CheckedAt:       c.effective(KeyReleaseCheckedAt, m),
		BannerSnoozeTag: c.effective(KeyReleaseBannerSnoozeTag, m),
	}, nil
}

// releaseCheckIntervalMin is the upstream-release-check cadence floor (PRD #836 M1):
// a sub-minute poll is a fat-finger, and GitHub's unauthenticated 60 req/hr budget
// makes hammering the endpoint pointless as well as rude.
const releaseCheckIntervalMin = time.Minute

// validateReleaseCheckInterval is the write-time gate for the release-check cadence
// (PRD #836 M1), mirroring validateAgentSourceInterval: a valid Go duration string
// ("6h", "1h") that parses to at least releaseCheckIntervalMin. The floor stops a
// bad value from making the poll loop hammer github.com.
func validateReleaseCheckInterval(value string) error {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return errors.New(`must be a duration like "6h"`)
	}
	if d < releaseCheckIntervalMin {
		return errors.New("must be at least 1m")
	}
	return nil
}

// maxReleaseCheckTokenLen caps the sealed release-check token (PRD #836 M1). Like
// the agent-source credential it is generous: a GitHub fine-grained PAT
// (github_pat_...) is ~93 chars, well over the 64-char label cap the ValidateLabel
// default branch would otherwise impose.
const maxReleaseCheckTokenLen = 1024

// validateReleaseCheckToken is the write-time gate for the OPTIONAL upstream-check
// GitHub token (PRD #836 M1), the same shape as validateAgentSourceCredential: a
// single opaque token, non-empty, no whitespace/control characters, generously
// capped. The error never echoes the value (a token must not appear in a message).
func validateReleaseCheckToken(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("token must not be empty")
	}
	if utf8.RuneCountInString(value) > maxReleaseCheckTokenLen {
		return fmt.Errorf("token must be at most %d characters", maxReleaseCheckTokenLen)
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("token must not contain whitespace or control characters")
		}
	}
	return nil
}
