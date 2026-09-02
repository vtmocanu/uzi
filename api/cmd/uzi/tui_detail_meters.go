package main

// Crew-rail spend and rate-limit meters for the run detail view (PRD #1009 M3).

import (
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// renderSpend is the crew-rail SPEND block (PRD #650): a run's rolled-up cost headline over an
// in/out/cache token breakdown, sitting directly above the ACCOUNTS block (railRateMeters) — the
// "what it cost" beside the "which account paid it" (PRD #623). Omitted entirely for a nil Usage
// (pre-#40 / unclaimed run). Budgeted whole-block-or-nothing against the remaining rail height,
// exactly like railRateMeters' account entries, because joinColumns clamps the rail by dropping
// its BOTTOM lines — a half-drawn SPEND (header, no cache line) must never render.
//
// The token split mirrors the web usage panel's aggregates (web/src/lib/runUsage.ts): "in" is the
// web's `fresh` (InputTokens + CacheCreationTokens), "cache" is `cached` (CacheReadTokens), so
// in + cache == the web "Tokens in" figure, and the cache% is cache's share of that — the exact
// cacheDisplayPct semantics (clamped [1,99]; 100 only when fresh==0, 0 only when no cache reads).
func (m tuiModel) renderSpend(usedRows int) string {
	u := m.detail.run.Usage
	if u == nil {
		return ""
	}
	total := "—" // subscription-auth $0
	if u.CostUSD > 0 {
		total = fmtCostCents(u.CostUSD)
	}
	fresh := u.InputTokens + u.CacheCreationTokens
	pct := cacheDisplayPct(u.InputTokens, u.CacheReadTokens, u.CacheCreationTokens)
	head := m.pal.faint.Render("SPEND") + "  " + lipgloss.NewStyle().Foreground(m.pal.tungsten).Render(total)
	inOut := m.pal.faint.Render("in " + fmtTokens(fresh) + "  out " + fmtTokens(u.OutputTokens))
	cache := m.pal.faint.Render("cache " + fmtTokens(u.CacheReadTokens) + " " + itoa(pct) + "%")
	lines := []string{head, inOut, cache}
	// Whole-block-or-nothing: the -1 is the blank "\n\n" separator the caller prepends (same budget
	// arithmetic railRateMeters uses).
	if len(lines) > m.transcriptViewport()-usedRows-1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// railRateMeters renders the stacked per-account rate-limit block for the crew rail, or ""
// when the selection is empty. It appends WHOLE account entries only while they fit within
// the remaining rail height (transcriptViewport() minus usedRows minus the blank separator
// the caller adds), because joinColumns clamps the rail to the transcript height by dropping
// its BOTTOM lines one at a time — an uncapped block would leave a half-drawn entry (label +
// 5h, no 7d). Dropping whole entries keeps every visible entry complete. Reuses rateWindowCell
// so the bar/percent/tone and the nil-window "-" are identical to the board strip.
//
// Deploy-ordering note (#519): the meters populate only when GET /api/me/settings and
// /api/me/rate-limits answer over the CLI uzc_ Bearer token. /me/settings GET moved to
// RequireUser in #519; against a server that predates that the settings fetch 401s (error
// swallowed) and this falls back to default-token-only, exactly as the board strip does. No
// server change here.
func (m tuiModel) railRateMeters(now time.Time, usedRows int) string {
	shown, showLabel := m.selectedRateMeters()

	// PRD #623: force-show + highlight the account THIS run is spending, as the first
	// ACCOUNTS entry, even when it is deselected in settings. This fold runs BEFORE the
	// empty-check below (the M1 trap): when the run's account is deselected and nothing
	// else is selected, selectedRateMeters returns (nil, false) and an early return here
	// would drop the ACCOUNTS block entirely — exactly the deselected-account case this
	// PRD exists to fix. Detail-only: railRateMeters is not shared with the board strip,
	// so selectedRateMeters/boardRateLimitStrip stay untouched.
	runID := m.detail.run.AnthropicSecretID
	if runID != nil {
		// Move-to-front if already shown (the common path — the run's account is often
		// IsDefault, which is always in shown; a bare prepend without the remove would
		// double-list it).
		foundIdx := -1
		for i, t := range shown {
			if t.SecretID == *runID {
				foundIdx = i
				break
			}
		}
		if foundIdx >= 0 {
			runTok := shown[foundIdx]
			shown = append(shown[:foundIdx], shown[foundIdx+1:]...)
			shown = append([]apitypes.TokenRateLimitDTO{runTok}, shown...)
		} else {
			// Not selected — force-show it. Prefer a real rate-limit row (any Status, even
			// non-"ok") so its windows render; else synthesize a label-only entry so the
			// account name still shows with "5h -"/"7d -".
			var runTok apitypes.TokenRateLimitDTO
			hasTok := false
			for _, t := range m.rateLimits {
				if t.SecretID == *runID {
					runTok = t
					hasTok = true
					break
				}
			}
			if !hasTok {
				runTok = apitypes.TokenRateLimitDTO{SecretID: *runID, Label: strOr(m.detail.run.AnthropicSecretLabel, "")}
			}
			shown = append([]apitypes.TokenRateLimitDTO{runTok}, shown...)
		}
	}

	if len(shown) == 0 {
		return ""
	}
	// budget is the rail height left below the content already built; the -1 is the blank
	// separator the caller prepends via "\n\n" before this block.
	budget := m.transcriptViewport() - usedRows - 1

	// Each account entry is a \n-joined string of an optional faint label eyebrow + the two
	// window cells; entries are added whole while they fit under the ACCOUNTS header.
	const headerRow = 1
	var fitted []string
	accumulated := 0
	for _, t := range shown {
		var lines []string
		// The run's account label renders UNCONDITIONALLY (even when showLabel is false —
		// a single-account run must still show its highlighted name) in tungsten normal
		// weight; siblings keep faint and still obey showLabel. rows is computed from the
		// lines actually built, so the always-present run label is already in the budget.
		isRun := runID != nil && t.SecretID == *runID
		if isRun {
			lines = append(lines, lipgloss.NewStyle().Foreground(m.pal.tungsten).Render(m.renderer.Plain(t.Label, laneRailWidth)))
		} else if showLabel {
			lines = append(lines, m.pal.faint.Render(m.renderer.Plain(t.Label, laneRailWidth)))
		}
		lines = append(lines,
			m.rateWindowCell("5h", t.Limits.FiveHour, railRateBarWidth, 4, now),
			m.rateWindowCell("7d", t.Limits.SevenDay, railRateBarWidth, 4, now),
		)
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
	return m.pal.faint.Render("ACCOUNTS") + "\n" + strings.Join(fitted, "\n")
}
