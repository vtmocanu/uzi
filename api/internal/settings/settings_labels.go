package settings

// This file holds the run-label accessors, their length-cap const and their
// validators/helpers (PRD #1021 M2, split verbatim from settings.go).

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

// AutopilotLabel returns the configured autopilot label. Falls back to
// DefaultAutopilotLabel.
func (c *Cache) AutopilotLabel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyAutopilotLabel)
}

// UziLabel returns the configured run-eligibility label (PRD #764): the single
// gate deciding whether an issue is uzi's to run. Falls back to DefaultUziLabel
// ("uzi").
func (c *Cache) UziLabel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyUziLabel)
}

// FindingLabel returns the configured incidental-finding marker label (PRD #333 D5),
// the server-mandated tag every filed finding issue carries. Falls back to
// DefaultFindingLabel ("agent-found"). A single label validated by the Decision-8
// label rules, like the other single-label keys.
func (c *Cache) FindingLabel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyFindingLabel)
}

// maxLabelLen is Decision 8's length cap (runes, not bytes).
const maxLabelLen = 64

// ValidateLabel checks a single label value against Decision 8's per-value
// rules: non-empty, at most 64 characters, and no comma (GitLab's label-list
// separator). It does not trim: a value with surrounding whitespace would not
// match the same label on the forge, so a whitespace-only value is rejected as
// empty and the caller is expected to send the exact label string.
func ValidateLabel(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be empty")
	}
	if utf8.RuneCountInString(value) > maxLabelLen {
		return errors.New("must be at most 64 characters")
	}
	if strings.ContainsRune(value, ',') {
		return errors.New("must not contain a comma")
	}
	return nil
}

// LabelChanged reports whether any submitted setting that affects which issues a
// board shows actually changed value: a board-filtering label (uzi_label or
// autopilot_label) in updates whose value differs from committed. The settings PUT
// uses it to decide whether to force a full repo resync. Only those two keys
// re-filter a board, so the check is a whitelist — every other key
// (default_theme presentation-only, the PRD #25 slack keys) is ignored, and a
// secret key's plaintext never participates. An idempotent write (same value)
// returns false, matching the prior "only resync on a real change".
func LabelChanged(committed, updates map[string]string) bool {
	for k, v := range updates {
		if k != KeyUziLabel && k != KeyAutopilotLabel {
			continue
		}
		if committed[k] != v {
			return true
		}
	}
	return false
}

// reservedSelfImproveLabel is the self-improve tracker's label. Its canonical home is
// schedsvc.SelfImproveTrackingLabel; settings cannot import schedsvc (schedsvc imports
// settings), so the value is mirrored here and pinned to the canonical one by an external
// test so the two cannot drift. See ValidateMerged.
const reservedSelfImproveLabel = "uzi-self-improve"

// ValidateMerged enforces the cross-key label rules on the effective post-update
// state (current values overlaid with the pending update), so a PUT touching one
// key is still checked against the others' stored values. PRD #764: the
// run-eligibility label (uzi_label) must be distinct from the autopilot label — an
// equal pair would autopilot every runnable issue, conflating "uzi's to run" with
// "skip the plan gate" — and from the finding label (see below). Each error names the
// key to change.
func ValidateMerged(merged map[string]string) error {
	if merged[KeyUziLabel] == merged[KeyAutopilotLabel] {
		return errors.New("uzi_label must differ from autopilot_label")
	}
	// PRD #764 hardening: uzi_label must also differ from finding_label, the marker uzi
	// stamps on issues it auto-files for incidental findings. If the two were equal, a
	// uzi-filed finding issue would carry the eligibility label, and an empty-selector
	// schedule (which defaults to [uzi_label]) could select and auto-run it. Before #764
	// a run ALSO needed a PRD link / PRDLESS / waiver — which finding issues lack — so
	// the collision was inert; #764 removed that backstop, so the distinctness that
	// guards eligibility must be enforced here. (Defaults uzi/agent-found are distinct.)
	if merged[KeyUziLabel] == merged[KeyFindingLabel] {
		return errors.New("uzi_label must differ from finding_label")
	}
	// Same class: uzi_label must not be the self-improve tracker's reserved label. The
	// tracker is uzi's own bookkeeping issue, deliberately non-runnable; if the
	// eligibility label equalled it, the board would offer "Start run" on the tracker and
	// CreateRun (which checks only uzi_label) would accept it. reservedSelfImproveLabel
	// mirrors schedsvc.SelfImproveTrackingLabel (settings cannot import schedsvc — a
	// cycle), pinned to the canonical value by TestReservedSelfImproveLabelMatchesSchedsvc.
	if merged[KeyUziLabel] == reservedSelfImproveLabel {
		return errors.New(`uzi_label must not be "uzi-self-improve" (the self-improve tracker's reserved label, which must never be runnable)`)
	}

	// PRD #602 M2: an ENABLED agent source must carry both a URL and a ref. This is
	// pure string logic on the merged post-update state — the allowlist half of the
	// URL check is the handler's job (ValidateMerged has no Config). It enforces the
	// enable-on atomicity ADR-0602 calls for: a partial write must never leave
	// enabled=true with an empty URL or ref (turning the feature on while pointing it
	// at nothing). Checked against the merged state so a single-key enable PUT is
	// still validated against the stored URL/ref.
	if merged[KeyAgentSourceEnabled] == "true" {
		if strings.TrimSpace(merged[KeyAgentSourceRepoURL]) == "" {
			return errors.New("agent_source_repo_url must be set when agent_source_enabled is true")
		}
		if strings.TrimSpace(merged[KeyAgentSourceRef]) == "" {
			return errors.New("agent_source_ref must be set when agent_source_enabled is true")
		}
	}
	return nil
}
