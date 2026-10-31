package main

import (
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/codexselect"
)

// codexAliasCell renders the CODEX_ALIAS row of `uzi run get` (issue #3719): the run's
// snapshotted alias label plus WHY that alias was chosen. A Codex run with no recorded
// reason (created before auto-selection) reads "not recorded", never a guessed mode.
func codexAliasCell(r apitypes.RunDTO) string {
	label := cellText(*r.CodexSecretLabel)
	if r.CodexSecretID == nil {
		label += " (deleted)"
	}
	if r.CodexSelectReason == nil || *r.CodexSelectReason == "" {
		return label + " — not recorded"
	}
	return label + " — " + codexSelectReasonText(codexselect.Reason(*r.CodexSelectReason))
}

// codexSelectReasonText is EXHAUSTIVE over codexselect.AllReasons(), pinned by
// TestCodexSelectReasonTextCoversEveryReason. An UNRECOGNISED reason prints as itself
// (through cellText, since it is not from this binary's closed set): the CLI is versioned
// separately from the API, so a newer server can ship a reason this binary has never heard
// of, and inventing a rendering for it or dropping it would be worse.
func codexSelectReasonText(reason codexselect.Reason) string {
	switch reason {
	case codexselect.ReasonDefault:
		return "default"
	case codexselect.ReasonPoolEmpty:
		return "default (auto mode, but no login is pooled)"
	case codexselect.ReasonAuto:
		return "auto (most limit headroom)"
	case codexselect.ReasonBestOfPool:
		return "auto (best of pool; every login was under the headroom floor)"
	case codexselect.ReasonPoolStale:
		return "auto (no usage reading yet; first pooled login)"
	case codexselect.ReasonPoolExhausted:
		return "auto (every login at capacity; soonest to release)"
	}
	return cellText(string(reason))
}

// codexReasonTag is the short form the TUI rail draws beside the run's Codex alias
// label. It returns "" for no reason. Like codexSelectReasonText it covers the closed set;
// an unrecognised value is returned verbatim for the caller to sanitize (it is not from
// this binary's closed set).
func codexReasonTag(reason *string) string {
	if reason == nil || *reason == "" {
		return ""
	}
	switch codexselect.Reason(*reason) {
	case codexselect.ReasonDefault:
		return "default"
	case codexselect.ReasonPoolEmpty:
		return "pool empty"
	case codexselect.ReasonAuto:
		return "auto"
	case codexselect.ReasonBestOfPool:
		return "best of pool"
	case codexselect.ReasonPoolStale:
		return "pool stale"
	case codexselect.ReasonPoolExhausted:
		return "pool exhausted"
	}
	return *reason
}
