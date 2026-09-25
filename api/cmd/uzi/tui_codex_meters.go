package main

// Codex per-account rate-limit meters (PRD #1209 M3), drawn beside the Claude meters on
// BOTH the board strip (boardCodexAccountsSeg) and the detail rail (railCodexRateMeters).
// The selection mirrors the Claude side (selectedRateMeters): the default account plus
// sidebar_codex_account_ids, but keyed on AccountID and with "readable" = a status that
// carries a reading (fresh or stale). A stale reading is shown DIMMED. Aliases and bucket
// display names are provider-/user-authored and drawn through m.renderer.Plain (D7);
// "Aliases" and "DisplayName" are registered in d7UntrustedFields.
//
// PRD #1653 (D-T2/D-T3): every drawn account is named (a lone account included), each window
// is labelled by its reported length (codexWindowLabel: "5h", "7d", "3h", "?" when unknown),
// and the account's main bucket (codexMainBucketID) draws no bucket name of its own.

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// codexMainBucketID is the stable id of a Codex account's top-level rate_limit bucket. It
// mirrors the unexported codexauth.codexMainBucketID (api/internal/codexauth/usage.go), which
// builds that bucket with an empty display name, so the bucket-label fallback would otherwise
// draw the literal id "codex" beside the account. PRD #1653 D-T3: this bucket draws no name;
// every other bucket keeps its name before its windows.
const codexMainBucketID = "codex"

// codexWindowLabel is the compact window label derived from a window's reported
// limit_window_seconds, mirroring the web formatCodexWindowLabel
// (web/src/lib/codexRateLimits.ts): whole days → "Nd", whole hours → "Nh", whole minutes →
// "Nm", else "Ns". A missing or non-positive length reads "?" (PRD #1653 amendment: the web
// says "window", which the 26-col rail cannot fit). Derived from a number, so no Plain needed.
func codexWindowLabel(secs *int64) string {
	if secs == nil || *secs <= 0 {
		return "?"
	}
	s := *secs
	switch {
	case s%86400 == 0:
		return strconv.FormatInt(s/86400, 10) + "d"
	case s%3600 == 0:
		return strconv.FormatInt(s/3600, 10) + "h"
	case s%60 == 0:
		return strconv.FormatInt(s/60, 10) + "m"
	default:
		return strconv.FormatInt(s, 10) + "s"
	}
}

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
// Codex meters, shared by the board strip, the detail rail and the rail's fold floor so the
// surfaces cannot disagree on which accounts show. readable = a status carrying a reading
// (fresh or stale); shown = readable filtered by (IsDefault || AccountID ∈
// sidebarCodexAccountIds), deduped to one meter per account (an account both default and
// listed appears once). Empty selection => nil. There is no label flag: every drawn Codex
// account is named (PRD #1653 D-T2).
func (m tuiModel) selectedCodexRateMeters() (shown []apitypes.CodexAccountRateLimitDTO) {
	readable := make([]apitypes.CodexAccountRateLimitDTO, 0, len(m.codexRateLimits))
	for _, a := range m.codexRateLimits {
		if codexReadable(a.Status) {
			readable = append(readable, a)
		}
	}
	if len(readable) == 0 {
		return nil
	}
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
	return shown
}

// codexAccountLabel draws a Codex account's identity — its aliases joined, or the account id
// when it carries no alias — capped to width. It carries no default badge (PRD #1653
// amendment): the label is always drawn now, and the suffix cost columns the strip and the
// 26-col rail cannot spare. The aliases are USER-authored and drawn through renderer.Plain
// (D7); an all-control-byte alias set scrubs to "" and falls back to the (safe) account id.
// "Aliases" is in d7UntrustedFields.
func (m tuiModel) codexAccountLabel(a apitypes.CodexAccountRateLimitDTO, width int) string {
	name := m.renderer.Plain(strings.Join(a.Aliases, ", "), width)
	if strings.TrimSpace(name) == "" {
		name = m.renderer.Plain(a.AccountID, width)
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
// rateWindowCell): a faint window-length label (codexWindowLabel from the window's own
// limit_window_seconds, PRD #1653 D-T3; never a fixed primary/secondary slot letter), a
// tone-coloured mini bar filled proportional to used_percent, then the server-rounded NN% text
// plus an optional reset countdown. A nil window (label "?") or one with no reading draws a
// faint `label —` — never 0, so partial/unknown stays distinct from a genuine zero. dim mutes
// the bar to faint for a stale account, so a stale reading reads as present-but-aged; the NN%
// text is always painted so the signal survives an Ascii/NO_COLOR profile that strips the tone.
func (m tuiModel) codexRateWindowCell(w *apitypes.CodexRateLimitWindowDTO, barW int, dim bool, now time.Time) string {
	label := "?"
	if w != nil {
		label = codexWindowLabel(w.LimitWindowSeconds)
	}
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

// boardCodexAccountsSeg builds the board strip's Codex ACCOUNTS section — one segment per shown
// account, joined with the 3-space token gap — and returns "" when nothing is selected. It
// carries NO provider tag: the single faint lowercase "codex" provider tag is added by the layout
// code (boardMeterLayout) on every Codex-bearing line, so the Codex section mirrors the Claude
// side, which likewise has no group prefix. The old hardcoded "Codex " group prefix is gone (PRD
// 1519 M3): it was redundant chrome and, when a user aliased an account "codex", read as a literal
// "Codex codex" duplication. Each account segment carries a per-group accent bar ▎ tinted by its
// peak window pct (alarm ≥ rateDangerPct, faint otherwise), the account label (ALWAYS, PRD #1653
// D-T2, a lone account included), and its buckets' length-labelled windows; a stale account is
// drawn dimmed. Aliases/bucket names ride renderer.Plain (D7), inside boardCodexAccountSeg.
func (m tuiModel) boardCodexAccountsSeg(now time.Time) string {
	shown := m.selectedCodexRateMeters()
	if len(shown) == 0 {
		return ""
	}
	accts := make([]string, 0, len(shown))
	for _, a := range shown {
		accts = append(accts, m.boardCodexAccountSeg(a, now))
	}
	return strings.Join(accts, "   ")
}

// boardCodexAccountSeg renders one Codex account's board-strip segment:
// `▎<label> [<bucket name> ]<len> <bar> NN%  <len> <bar> NN%`. The label is always drawn; the
// main bucket (codexMainBucketID) draws no bucket name, any other bucket keeps its name before
// its windows (PRD #1653 D-T2/D-T3). An absent secondary window is not drawn.
func (m tuiModel) boardCodexAccountSeg(a apitypes.CodexAccountRateLimitDTO, now time.Time) string {
	seg := paintSeg(m.pal.faintC, nil, false, m.codexAccountLabel(a, 16)+" ")
	dim := a.Status == "stale"
	bucketSegs := make([]string, 0, len(a.Buckets))
	for _, b := range a.Buckets {
		bs := ""
		if b.ID != codexMainBucketID {
			bs = paintSeg(m.pal.faintC, nil, false, m.codexBucketLabel(b, 12)+" ")
		}
		bs += m.codexRateWindowCell(b.Primary, rateBarWidth, dim, now)
		if b.Secondary != nil {
			bs += "  " + m.codexRateWindowCell(b.Secondary, rateBarWidth, dim, now)
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

// codexRailLabelCols is the window-label width railRateBarWidth was budgeted for: the rail row
// is `<label> <bar> NNN% <countdown>` = label + 1 + bar + 5 + 1 + 6 (the widest shortDuration of
// a sub-100-day reset, "29d23h") within laneRailWidth, which leaves label + bar = 13 = 3 + 10.
const codexRailLabelCols = 3

// codexRailWindowLine renders one Codex window as a detail-rail row. A length label wider than
// codexRailLabelCols (an odd length such as "3601s", or "365d") shrinks the bar
// by the excess — never below one cell — so the percent and the reset countdown stay intact;
// the final clampVisual is only a backstop for a pathological label or countdown and bounds the
// row to laneRailWidth either way.
func (m tuiModel) codexRailWindowLine(w *apitypes.CodexRateLimitWindowDTO, dim bool, now time.Time) string {
	barW := railRateBarWidth
	if w != nil {
		if extra := visualWidth(codexWindowLabel(w.LimitWindowSeconds)) - codexRailLabelCols; extra > 0 {
			barW = max(1, barW-extra)
		}
	}
	return clampVisual(m.codexRateWindowCell(w, barW, dim, now), laneRailWidth)
}

// railCodexRateMeters renders the detail crew rail's stacked per-account Codex block under
// a "CODEX" header, or "" when the selection is empty. It appends WHOLE account entries
// only while they fit the remaining rail height (mirroring railRateMeters), so joinColumns'
// bottom-line clamp never leaves a half-drawn account. Each entry is the account label
// eyebrow (always, PRD #1653 D-T2), then per bucket a name eyebrow (skipped for the main
// codexMainBucketID bucket, D-T3) and one length-labelled line per present window. A stale
// account is drawn dimmed via codexRateWindowCell. Reuses codexRateWindowCell so the
// bar/percent/tone/reset and the no-reading "—" match the board strip. railCodexFloorRows
// mirrors this row math exactly.
func (m tuiModel) railCodexRateMeters(now time.Time, usedRows int) string {
	shown := m.selectedCodexRateMeters()
	if len(shown) == 0 {
		return ""
	}
	budget := m.transcriptViewport() - usedRows - 1
	const headerRow = 1
	var fitted []string
	accumulated := 0
	for _, a := range shown {
		dim := a.Status == "stale"
		lines := []string{m.pal.faint.Render(m.codexAccountLabel(a, laneRailWidth))}
		for _, b := range a.Buckets {
			if b.ID != codexMainBucketID {
				lines = append(lines, m.pal.faint.Render(m.codexBucketLabel(b, laneRailWidth)))
			}
			lines = append(lines, m.codexRailWindowLine(b.Primary, dim, now))
			if b.Secondary != nil {
				lines = append(lines, m.codexRailWindowLine(b.Secondary, dim, now))
			}
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
