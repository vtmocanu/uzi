package main

// Codex per-account rate-limit meters (PRD #1209 M3), drawn beside the Claude meters on
// BOTH the board strip (boardCodexMeterSeg) and the detail rail (railCodexRateMeters).
// The selection mirrors the Claude side (selectedRateMeters): the default account plus
// sidebar_codex_account_ids, but keyed on AccountID and with "readable" = a status that
// carries a reading (fresh or stale). A stale reading is shown DIMMED. Aliases and bucket
// display names are provider-/user-authored and drawn through m.renderer.Plain (D7);
// "Aliases" and "DisplayName" are registered in d7UntrustedFields.

import (
	"slices"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// codexReadable reports whether a Codex account status carries a usable reading (PRD
// #1209 M3): fresh or stale. Every other status in the closed set (no_subscription,
// pending, no_reading, vault_locked, credential_action_required, polling_disabled) has no
// reading to draw, so its account is hidden. Status is a closed enum branched on here, so
// it is deliberately NOT a d7UntrustedFields entry.
func codexReadable(status string) bool {
	return status == "fresh" || status == "stale"
}

// codexRateWindowPct rounds a Codex window's used_percent to a whole percent, reporting
// false when the window is absent OR carries no reading (used_percent null) — the
// partial/unknown case a caller renders as "—", never 0.
func codexRateWindowPct(w *apitypes.CodexRateLimitWindowDTO) (int, bool) {
	if w == nil || w.UsedPercent == nil {
		return 0, false
	}
	return int(*w.UsedPercent + 0.5), true
}

// codexAccountPeakPct is the max used_percent across an account's bucket windows (primary
// and secondary), the analog of tokenPeakPct: it tints the per-account accent bar. An
// account with no reading at all returns the sentinel -1 (stays faint).
func codexAccountPeakPct(a apitypes.CodexAccountRateLimitDTO) int {
	peak := -1
	for _, b := range a.Buckets {
		for _, w := range []*apitypes.CodexRateLimitWindowDTO{b.Primary, b.Secondary} {
			if pct, ok := codexRateWindowPct(w); ok && pct > peak {
				peak = pct
			}
		}
	}
	return peak
}

// selectedCodexRateMeters applies the board/sidebar selection to the model's per-account
// Codex meters, shared by the board strip and the detail rail so the two surfaces cannot
// disagree on which accounts show OR on showLabel. readable = a status carrying a reading
// (fresh or stale); showLabel is keyed off the READABLE count (>1), not the shown count;
// shown = readable filtered by (IsDefault || AccountID ∈ sidebarCodexAccountIds), deduped
// to one meter per account (an account both default and listed appears once). Empty
// selection => (nil, false).
func (m tuiModel) selectedCodexRateMeters() (shown []apitypes.CodexAccountRateLimitDTO, showLabel bool) {
	readable := make([]apitypes.CodexAccountRateLimitDTO, 0, len(m.codexRateLimits))
	for _, a := range m.codexRateLimits {
		if codexReadable(a.Status) {
			readable = append(readable, a)
		}
	}
	if len(readable) == 0 {
		return nil, false
	}
	showLabel = len(readable) > 1
	shown = make([]apitypes.CodexAccountRateLimitDTO, 0, len(readable))
	seen := make(map[string]bool, len(readable))
	for _, a := range readable {
		if !a.IsDefault && !slices.Contains(m.sidebarCodexAccountIds, a.AccountID) {
			continue
		}
		if seen[a.AccountID] {
			continue // dedup: one meter per account
		}
		seen[a.AccountID] = true
		shown = append(shown, a)
	}
	return shown, showLabel
}

// codexAccountLabel draws a Codex account's identity — its aliases joined, default-badged,
// or the account id when it carries no alias — capped to width. The aliases are
// USER-authored and drawn through renderer.Plain (D7); an all-control-byte alias set
// scrubs to "" and falls back to the (safe) account id. "Aliases" is in d7UntrustedFields.
func (m tuiModel) codexAccountLabel(a apitypes.CodexAccountRateLimitDTO, width int) string {
	name := m.renderer.Plain(strings.Join(a.Aliases, ", "), width)
	if strings.TrimSpace(name) == "" {
		name = m.renderer.Plain(a.AccountID, width)
	}
	if a.IsDefault {
		name += " (default)"
	}
	return name
}

// codexBucketLabel draws a bucket's human label — its display name, or its stable id when
// it carries none — capped to width. DisplayName is provider-authored free text drawn
// through renderer.Plain (D7); an all-control-byte name scrubs to "" and falls back to the
// (safe) id. "DisplayName" is in d7UntrustedFields.
func (m tuiModel) codexBucketLabel(b apitypes.CodexRateLimitBucketDTO, width int) string {
	name := m.renderer.Plain(b.DisplayName, width)
	if strings.TrimSpace(name) == "" {
		name = m.renderer.Plain(b.ID, width)
	}
	return name
}

// codexRateWindowCell renders one Codex bucket window as `label <bar> NN%` (mirroring
// rateWindowCell): a faint label, a tone-coloured mini bar filled proportional to
// used_percent, then the server-rounded NN% text plus an optional reset countdown. A nil
// window (or one with no reading) draws a faint `label —` — never 0, so partial/unknown
// stays distinct from a genuine zero. dim mutes the bar to faint for a stale account, so a
// stale reading reads as present-but-aged; the NN% text is always painted so the signal
// survives an Ascii/NO_COLOR profile that strips the tone.
func (m tuiModel) codexRateWindowCell(label string, w *apitypes.CodexRateLimitWindowDTO, barW int, dim bool, now time.Time) string {
	pct, ok := codexRateWindowPct(w)
	if !ok {
		return paintSeg(m.pal.faintC, nil, false, label+" —")
	}
	filled, empty := rateBarParts(pct, barW)
	tone := m.rateTone(pct)
	if dim {
		tone = m.pal.faintC
	}
	pctSeg := " " + itoa(pct) + "%"
	// Reset countdown after the percent, when Codex reported one. Prefer the seconds-until
	// form; fall back to the absolute epoch. shortDuration clamps a past reset to "0s".
	switch {
	case w.ResetAfterSeconds != nil:
		pctSeg += " " + shortDuration(time.Duration(*w.ResetAfterSeconds)*time.Second)
	case w.ResetAt != nil:
		pctSeg += " " + shortDuration(time.Duration(*w.ResetAt-now.Unix())*time.Second)
	}
	return paintSeg(m.pal.faintC, nil, false, label+" ") +
		paintSeg(tone, nil, false, filled) +
		paintSeg(m.pal.faintC, nil, false, empty) +
		paintSeg(m.pal.faintC, nil, false, pctSeg)
}

// boardCodexMeterSeg renders the board strip's Codex section — a faint "Codex" provider
// label so the two providers never look merged, then one segment per shown account. ""
// when nothing is selected. Each account carries a per-group accent bar ▎ tinted by its
// peak window pct (alarm ≥ rateDangerPct, faint otherwise), an optional account label
// (when showLabel), and its buckets' primary/secondary windows. A stale account is drawn
// dimmed. The whole board strip is width-clamped downstream, so this rides one line.
func (m tuiModel) boardCodexMeterSeg(now time.Time) string {
	shown, showLabel := m.selectedCodexRateMeters()
	if len(shown) == 0 {
		return ""
	}
	accts := make([]string, 0, len(shown))
	for _, a := range shown {
		accts = append(accts, m.boardCodexAccountSeg(a, showLabel, now))
	}
	return paintSeg(m.pal.faintC, nil, false, "Codex ") + strings.Join(accts, "   ")
}

// boardCodexAccountSeg renders one Codex account's board-strip segment: the accent bar,
// the optional account label, and each bucket's `<name> P <bar> NN%  S <bar> NN%` windows.
func (m tuiModel) boardCodexAccountSeg(a apitypes.CodexAccountRateLimitDTO, showLabel bool, now time.Time) string {
	seg := ""
	if showLabel {
		seg = paintSeg(m.pal.faintC, nil, false, m.codexAccountLabel(a, 16)+" ")
	}
	dim := a.Status == "stale"
	bucketSegs := make([]string, 0, len(a.Buckets))
	for _, b := range a.Buckets {
		bs := paintSeg(m.pal.faintC, nil, false, m.codexBucketLabel(b, 12)+" ")
		bs += m.codexRateWindowCell("P", b.Primary, rateBarWidth, dim, now)
		if b.Secondary != nil {
			bs += "  " + m.codexRateWindowCell("S", b.Secondary, rateBarWidth, dim, now)
		}
		bucketSegs = append(bucketSegs, bs)
	}
	seg += strings.Join(bucketSegs, "   ")
	accent := m.pal.faintC
	if codexAccountPeakPct(a) >= rateDangerPct {
		accent = m.pal.alarm
	}
	return paintSeg(accent, nil, false, "▎") + seg
}

// railCodexRateMeters renders the detail crew rail's stacked per-account Codex block under
// a "CODEX" header, or "" when the selection is empty. It appends WHOLE account entries
// only while they fit the remaining rail height (mirroring railRateMeters), so joinColumns'
// bottom-line clamp never leaves a half-drawn account. Each entry is an optional account
// label eyebrow, then per bucket a name eyebrow + a `P`/`S` window line each. A stale
// account is drawn dimmed via codexRateWindowCell. Reuses codexRateWindowCell so the
// bar/percent/tone/reset and the nil-window "—" match the board strip.
func (m tuiModel) railCodexRateMeters(now time.Time, usedRows int) string {
	shown, showLabel := m.selectedCodexRateMeters()
	if len(shown) == 0 {
		return ""
	}
	budget := m.transcriptViewport() - usedRows - 1
	const headerRow = 1
	var fitted []string
	accumulated := 0
	for _, a := range shown {
		dim := a.Status == "stale"
		var lines []string
		if showLabel {
			lines = append(lines, m.pal.faint.Render(m.codexAccountLabel(a, laneRailWidth)))
		}
		for _, b := range a.Buckets {
			lines = append(lines, m.pal.faint.Render(m.codexBucketLabel(b, laneRailWidth)))
			lines = append(lines, m.codexRateWindowCell("P", b.Primary, railRateBarWidth, dim, now))
			if b.Secondary != nil {
				lines = append(lines, m.codexRateWindowCell("S", b.Secondary, railRateBarWidth, dim, now))
			}
		}
		if len(lines) == 0 {
			continue
		}
		entry := strings.Join(lines, "\n")
		rows := len(lines)
		if headerRow+accumulated+rows > budget {
			break
		}
		fitted = append(fitted, entry)
		accumulated += rows
	}
	if len(fitted) == 0 {
		return ""
	}
	return m.pal.faint.Render("CODEX") + "\n" + strings.Join(fitted, "\n")
}
