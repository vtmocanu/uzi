package settings

// This file holds the trusted review-bot allowlist (issue #2347): its strict accessor,
// the entry parser shared with the write-time validator, and the instance matcher.

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// maxTrustedBots bounds the allowlist so a hand-edited row cannot grow without limit.
const maxTrustedBots = 50

// TrustedBot is one allowlist entry: a forge instance (normalized https base URL) and the
// stable forge user id of a review bot on it.
type TrustedBot struct {
	BaseURL     string
	ForgeUserID int64
}

// MrReviewTrustedBots returns the parsed trusted review-bot allowlist. An absent or empty
// value yields an empty slice. It is a STRICT read: a store error is returned, including on
// a warm cache whose refresh failed (no stale-on-error), so the caller fails closed (the
// watcher skips the repo's tick, the on-demand rework answers 409) instead of trusting a
// list it could not re-read; an admin's removal of a bot must not outlive a failed refresh.
// An unparseable entry in a hand-edited row is skipped: write-time validation is the real gate.
func (c *Cache) MrReviewTrustedBots(ctx context.Context) ([]TrustedBot, error) {
	if v, ok := c.env[KeyMrReviewTrustedBots]; ok && v != "" {
		bots, _ := parseTrustedBots(v)
		return bots, nil
	}
	m, err := c.strictSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	v := c.effective(KeyMrReviewTrustedBots, m)
	bots, _ := parseTrustedBots(v)
	return bots, nil
}

// parseTrustedBots splits the stored value into entries, returning the valid ones and the
// first problem found (nil when the whole value is valid).
func parseTrustedBots(value string) ([]TrustedBot, error) {
	out := []TrustedBot{}
	var firstErr error
	seen := map[TrustedBot]bool{}
	for _, tok := range strings.Split(value, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		b, err := parseTrustedBot(tok)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if seen[b] {
			if firstErr == nil {
				firstErr = fmt.Errorf("duplicate entry %q", tok)
			}
			continue
		}
		seen[b] = true
		out = append(out, b)
	}
	if len(out) > maxTrustedBots && firstErr == nil {
		firstErr = fmt.Errorf("at most %d entries are allowed", maxTrustedBots)
	}
	return out, firstErr
}

func parseTrustedBot(tok string) (TrustedBot, error) {
	base, idText, ok := strings.Cut(tok, "#")
	if !ok {
		return TrustedBot{}, fmt.Errorf("entry %q must look like <base_url>#<forge_user_id>", tok)
	}
	norm, err := NormalizeTrustedBotBaseURL(base)
	if err != nil {
		return TrustedBot{}, fmt.Errorf("entry %q: %w", tok, err)
	}
	if norm != base {
		return TrustedBot{}, fmt.Errorf("entry %q: base URL must be written as %q", tok, norm)
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != idText {
		return TrustedBot{}, fmt.Errorf("entry %q: forge user id must be a positive integer", tok)
	}
	return TrustedBot{BaseURL: norm, ForgeUserID: id}, nil
}

// validateTrustedBots is the write-time gate: empty is valid; every entry must be
// "<normalized https base URL>#<positive forge user id>", with no duplicates and at most
// maxTrustedBots entries. Like the docker allowlist it needs an explicit Validate case: the
// default branch's label rules reject the comma and '#'.
func validateTrustedBots(value string) error {
	_, err := parseTrustedBots(value)
	return err
}

// NormalizeTrustedBotBaseURL mirrors config.NormalizeForgeBaseURL (https only, scheme and
// host lower-cased, path dropped). It is a copy because config imports this package;
// TestNormalizeTrustedBotBaseURLMatchesConfig in the config package pins the two together.
func NormalizeTrustedBotBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("base URL %q must use https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("base URL %q has no host", raw)
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// canonicalInstance maps the GitHub API host onto the web host: the GitHub driver treats
// https://api.github.com and https://github.com as one instance (forge/github.go,
// isDefaultGitHubBase), so an entry written for either must match a connection on either.
// The https default port is treated as absent on both sides, because a connection base URL
// stored via config.NormalizeForgeBaseURL keeps an explicit :443 while an entry may omit it
// (write validation still accepts both spellings). Other ports stay distinct instances.
func canonicalInstance(normalized string) string {
	normalized = strings.TrimSuffix(normalized, ":443")
	if normalized == "https://api.github.com" {
		return "https://github.com"
	}
	return normalized
}

// TrustedBotMatches reports whether a comment authored by forgeUserID on a connection with
// the given base URL is a trusted review bot. Only the stable id and the normalized
// instance count: never a login, never a "[bot]" suffix. A base URL that does not normalize
// matches nothing.
func TrustedBotMatches(bots []TrustedBot, connectionBaseURL string, forgeUserID int64) bool {
	if forgeUserID <= 0 || len(bots) == 0 {
		return false
	}
	norm, err := NormalizeTrustedBotBaseURL(connectionBaseURL)
	if err != nil {
		return false
	}
	inst := canonicalInstance(norm)
	for _, b := range bots {
		if b.ForgeUserID == forgeUserID && canonicalInstance(b.BaseURL) == inst {
			return true
		}
	}
	return false
}
