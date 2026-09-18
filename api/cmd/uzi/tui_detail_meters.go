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

// railCredentialLine is the crew rail's per-run token-choice + pending-switch BLOCK (PRD #1247 M8):
// up to TWO physical lines — a token line naming the override the owner chose for this run, and a
// SEPARATE switch line for any held-state switch in flight. It is drawn INDEPENDENTLY of the
// ACCOUNTS block's isRun branch (railRateMeters), so a QUEUED, unclaimed run — which has no
// AnthropicSecretID and therefore no ACCOUNTS entry at all — STILL shows its override/switch. "" when
// the run inherits the worker binding with no pending switch (credential_override null AND
// credential_switch null), so the common case adds no line (and no stray blank row).
//
// Two lines, not one: joinColumns clamps every rail line to laneRailWidth (26), and the packed
// one-line form ("token: <label> (run-pinned) · switch requested") is always wider than that, so the
// clamp TRUNCATED the pending-switch marker — the transient, attention-worthy signal the line exists
// to surface — off the tail. Rendering the switch on its OWN line (mirroring renderMilestones'
// eyebrow→continuation-line precedent) keeps it inside the rail, fully rendered. Each line is
// self-capped to laneRailWidth by renderer.Plain (token line) or is a short server enum (switch
// line), so neither depends on the joinColumns clamp to fit.
//
// usedRows budgets each line whole-line-or-nothing against the remaining rail height, the same
// arithmetic renderSpend/railRateMeters use (the -1 is the blank "\n\n" separator the caller
// prepends). renderLaneRail draws the block LAST, below ACCOUNTS, so this budget never steals a row
// from a PRD #1257-protected block above it — the lines are best-effort like a sibling ACCOUNTS
// entry, not part of the protected set railAutoFolded counts. Priority under pressure: when only ONE
// row remains and a switch IS pending, the switch line wins (the transient signal is worth surfacing
// over the steady-state token choice); otherwise the token line shows. When two rows fit, both show,
// token first.
func (m tuiModel) railCredentialLine(usedRows int) string {
	token := m.railCredentialTokenLine()
	sw := m.railCredentialSwitchLine()
	if token == "" && sw == "" {
		return ""
	}
	budget := m.transcriptViewport() - usedRows - 1
	if budget < 1 {
		return ""
	}
	switch {
	case token != "" && sw != "":
		if budget >= 2 {
			return token + "\n" + sw
		}
		// One row left: prioritise the transient switch marker over the steady token choice.
		return sw
	case sw != "":
		return sw
	default:
		return token
	}
}

// railCredentialTokenLine builds the faint "token: <choice>" rail line, or "" when the run carries no
// credential override. The WHOLE line rides renderer.Plain at laneRailWidth so the USER-AUTHORED
// override label (D7) is sanitized and the line is capped to the rail width in one place; a long
// label may push "(run-pinned)" past the cap and lose it, which is acceptable — the label is the
// priority on the token line. The mode words are server enums (auto/default) rendered honestly (an
// unrecognised value still rides Plain rather than reaching the frame raw, via railOverrideMode).
func (m tuiModel) railCredentialTokenLine() string {
	co := m.detail.run.CredentialOverride
	if co == nil {
		return ""
	}
	return m.pal.faint.Render(m.renderer.Plain("token: "+m.railOverrideMode(co), laneRailWidth))
}

// railCredentialSwitchLine builds the faint pending-switch rail line ("switch requested" / "switch
// released") on its OWN line, so the marker fully renders inside laneRailWidth rather than being
// truncated off the token line. "" when no switch is in flight. The switch word is a server enum
// rendered honestly; an unrecognised value rides renderer.Plain rather than reaching the frame raw.
func (m tuiModel) railCredentialSwitchLine() string {
	sw := m.detail.run.CredentialSwitch
	if sw == nil || *sw == "" {
		return ""
	}
	switch *sw {
	case "requested":
		return m.pal.faint.Render("switch requested")
	case "released":
		return m.pal.faint.Render("switch released")
	default:
		return m.pal.faint.Render("switch " + m.renderer.Plain(*sw, laneRailWidth))
	}
}

// railOverrideMode renders a per-run credential override's mode for the crew rail: a pinned override
// names its snapshotted label + the run-pinned reason ("<label> (run-pinned)"), auto/default name the
// mode. The label rides renderer.Plain (D7 — it is the CredentialOverride.Label the "Label" guard
// entry covers). A pinned override whose label is gone (deleted token) drops to "run-pinned"; an
// unrecognised mode rides Plain too rather than reaching the frame raw.
func (m tuiModel) railOverrideMode(co *apitypes.CredentialOverrideDTO) string {
	switch co.Mode {
	case "pinned":
		if co.Label != nil && *co.Label != "" {
			return m.renderer.Plain(*co.Label, laneRailWidth) + " (run-pinned)"
		}
		return "run-pinned"
	case "auto":
		return "auto"
	case "default":
		return "default"
	default:
		return m.renderer.Plain(co.Mode, laneRailWidth)
	}
}
