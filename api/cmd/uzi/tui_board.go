package main

import (
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// The board: a live list of runs, refreshed on a ListRuns poll (D3 — the board is
// list-level, so a socket per run to keep a counter fresh is disproportionate).

type boardState struct {
	runs   []apitypes.RunListItemDTO
	cursor int
	err    error

	// admin is the `[a]` factory-wide toggle. adminDenied records that the token
	// lacks the scope, so the refusal renders as a line rather than being retried.
	admin       bool
	adminDenied bool

	// runsAdmin is the PROVENANCE of the currently-resident run set: the admin-ness of the
	// board whose accepted ListRuns reply is held in runs. It is set ONLY when a reply is
	// accepted (in apply), so it lags the live admin toggle during the window after keyAdmin
	// flips admin but before the matching reply lands — runs still holds the PREVIOUS board's
	// set then. The vault band's cross-tenant owner-filter (ownParkedOnVaultCount, D11) scopes on
	// THIS, never on the live toggle, so a stranger's parked run in a stale admin set is never
	// miscounted into the viewer's band during that window. Defaults false, matching the
	// non-admin Init fetch.
	runsAdmin bool

	// reqSeq / waitID / tickGen implement the request-generation guard (PRD #1130 M1 D2).
	//
	// reqSeq is the monotonic id minted for each board fetch (startBoardReq bumps it). waitID
	// is the id of the request the model is currently waiting on: waitID == 0 means idle, and a
	// boardTickMsg issues a new periodic poll ONLY while idle — that is the in-flight guard. A
	// boardRunsMsg is honoured only when its reqID == waitID, so an older reply that resolves
	// after a newer request was minted (bubbletea delivers replies in completion order, not
	// request order) is dropped instead of clearing the newer request's guard — the
	// out-of-order defence. User r/a and exit-to-board go through startBoardReq too: they mint
	// a fresh id (so they are never gated) and, because that id becomes the new waitID, any
	// periodic reply still in flight is superseded rather than mistaken for theirs.
	//
	// tickGen is the tick-chain generation. The reply re-arms the next tick and bumps tickGen;
	// a boardTickMsg whose gen != tickGen belongs to a superseded chain (e.g. a tick left
	// pending when a manual refresh reply already re-armed) and is dropped, so exactly one tick
	// chain is live. newTUIModel seeds all three to count the Init fetch as in flight (reqSeq
	// 1 / waitID 1 / tickGen 1).
	reqSeq  uint64
	waitID  uint64
	tickGen uint64

	// errStreak is the consecutive-failure counter driving the tick backoff (PRD #1130 M3 D4):
	// each consecutive failed board poll widens the reschedule interval via boardTickInterval,
	// and the first success resets it to 0 so the cadence snaps back to the 2s base. Updated
	// inside apply, so it honours the same admin-mismatch early return (a stale reply leaves it
	// unchanged).
	errStreak int

	filtering bool
	filter    string

	// hideDone is the `[h]` toggle: drop terminal runs (completed/failed/cancelled) from
	// the board, keeping the active + needs-you set. A pure client-side view over the runs
	// already fetched, so toggling it needs no refetch. On the admin board it is a no-op —
	// AdminListRuns already returns non-terminal runs only.
	hideDone bool

	// scroll is the index of the first visible run row: the board windows the run list to
	// the terminal height so the header and footer stay on screen even with hundreds of runs.
	scroll int
}

func newBoardState() boardState { return boardState{} }

func (b *boardState) apply(msg boardRunsMsg) {
	// Ignore a reply for a view the user has since toggled away from, or a stale
	// admin reply would overwrite the own-runs list.
	if msg.admin != b.admin {
		return
	}
	if msg.err != nil {
		if b.admin {
			// A uzc_ token has no admin scope; the toggle is refused, not retried
			// (D8: cleanly refused, never a crash).
			b.admin = false
			b.adminDenied = true
		}
		b.err = msg.err
		// A genuine failed poll for the board this reply belongs to: widen the backoff
		// (PRD #1130 M3 D4). Placed after the admin-mismatch early return, so a stale reply
		// leaves the streak untouched. The admin-refusal sub-branch above is still an error
		// reply for the own board that follows, so incrementing here is correct for it too.
		b.errStreak++
		return
	}
	b.err = nil
	b.errStreak = 0
	b.runs = msg.runs
	// Record the accepted set's provenance. This is the single site runs are accepted (the
	// error path above leaves runs — and so runsAdmin — untouched), and msg.admin == b.admin
	// is guaranteed by the early return, so this is exactly the admin-ness of the held runs.
	b.runsAdmin = msg.admin
	b.clampCursor()
}

func (b *boardState) clampCursor() {
	n := len(b.visible())
	if b.cursor >= n {
		b.cursor = n - 1
	}
	if b.cursor < 0 {
		b.cursor = 0
	}
}

// visible applies the `/` filter, then orders the survivors into the three triage bands
// (NEEDS YOU → ON THE FLOOR → DONE). The cursor indexes this list, so the selectable order
// IS the visual order and navigation walks the bands top to bottom. The filter is the
// user's own text, matched against sanitized cell text so a control byte in a title cannot
// affect what matches — and against BOTH the raw status and the human status word, so a
// user can filter by either "awaiting_approval" or "plan gate".
func (b *boardState) visible() []apitypes.RunListItemDTO {
	base := b.runs
	if b.hideDone {
		kept := make([]apitypes.RunListItemDTO, 0, len(base))
		for _, r := range base {
			if terminalRunStatuses[r.Status] {
				continue
			}
			kept = append(kept, r)
		}
		base = kept
	}
	if strings.TrimSpace(b.filter) != "" {
		q := strings.ToLower(strings.TrimSpace(b.filter))
		out := make([]apitypes.RunListItemDTO, 0, len(base))
		for _, r := range base {
			// The effective status (the enum the user sees — "planning", not the raw "running"
			// #321 hides) plus the human word, so a user can filter by either "awaiting_approval"
			// or "plan gate". The truly-raw r.Status is deliberately NOT included: it would let
			// "running" match a planning run again, the exact thing #321 fixed.
			word := runStateWord(r)
			hay := strings.ToLower(strings.Join([]string{
				r.ID, r.Kind, effectiveRunStatus(r.Status, r.IsPlanning, r.IsRevising),
				word, r.Health, cellText(runTitle(r.RunDTO)),
			}, " "))
			if strings.Contains(hay, q) {
				out = append(out, r)
			}
		}
		base = out
	}
	return bandOrder(base)
}

// The three triage bands, in fixed top-to-bottom order.
const (
	bandNeedsYou = iota // awaiting_approval + awaiting_input + awaiting_followup + a Codex relogin_required hold + a credential_disabled hold — the only rows a human must act on
	bandFloor           // everything non-terminal not in NEEDS YOU (running/claimed/queued/planning/limit_wait/pool_wait/recovery_wait, stalled)
	bandDone            // terminal: completed/failed/cancelled
	numBands
)

var bandNames = [numBands]string{"NEEDS YOU", "ON THE FLOOR", "DONE"}

// runBand places a run in its triage band from its status (plus is_revising).
//
// issue #750: a run mid-"revise" replan keeps status == awaiting_approval but is NOT the
// user's turn — the agent is re-planning (~90s) and will re-gate itself — so it drops to
// ON THE FLOOR instead of sitting in NEEDS YOU. is_revising is NOT status-gated
// server-side, so the awaiting_approval check is applied here (mirroring effectiveRunStatus).
func runBand(status string, isRevising bool) int {
	if status == "awaiting_approval" && isRevising {
		return bandFloor
	}
	switch status {
	case "awaiting_approval", "awaiting_input", "awaiting_followup":
		// awaiting_followup (PRD #517) is the user's turn — an interactive task parked for
		// its next follow-up — so it belongs in NEEDS YOU alongside the other two parks.
		return bandNeedsYou
	}
	if terminalRunStatuses[status] {
		return bandDone
	}
	return bandFloor
}

// runBandOf is runBand over a list row, plus the one band decision the status cannot make
// alone: a run held on its Codex account that needs the owner to re-log in (PRD #1590
// relogin_required) is the owner's turn, so it bands into NEEDS YOU. Every other Codex hold
// action resolves without the owner and stays ON THE FLOOR. A run held on credential_disabled
// (PRD #1732 D14) resumes only on the owner's Enable or token switch, so it is NEEDS YOU too. Every band decision on the board
// (the ordering, the eyebrow counts, the row's title ink) goes through here so they agree.
func runBandOf(r apitypes.RunListItemDTO) int {
	if codexReloginHold(r.RunDTO) || isCredentialDisabledHold(r.RunDTO) || isWorkerRecoveryExhausted(r.RunDTO) {
		return bandNeedsYou
	}
	return runBand(r.Status, r.IsRevising)
}

// runStateWord is the human status word the board row draws for r (runStateToken's word),
// which the `/` filter matches alongside the effective status.
func runStateWord(r apitypes.RunListItemDTO) string {
	if codexReloginHold(r.RunDTO) {
		return codexReloginWord
	}
	if isCredentialDisabledHold(r.RunDTO) {
		return credDisabledWord
	}
	_, word := stateGlyphWord(r.Status, r.Health, r.IsPlanning, r.IsRevising, r.LandingState, strOr(r.RecoveryWaitCause, ""))
	return word
}

// bandOrder concatenates NEEDS YOU → ON THE FLOOR → DONE. Active bands keep the
// server's order; DONE uses the web archive's newest-finish-first order (#2098),
// preserving server order when both the finish instant and status rank tie.
func bandOrder(runs []apitypes.RunListItemDTO) []apitypes.RunListItemDTO {
	var buckets [numBands][]apitypes.RunListItemDTO
	for _, r := range runs {
		b := runBandOf(r)
		buckets[b] = append(buckets[b], r)
	}
	slices.SortStableFunc(buckets[bandDone], compareDoneRuns)
	out := make([]apitypes.RunListItemDTO, 0, len(runs))
	for b := 0; b < numBands; b++ {
		out = append(out, buckets[b]...)
	}
	return out
}

func compareDoneRuns(a, b apitypes.RunListItemDTO) int {
	aTime, bTime := a.UpdatedAt, b.UpdatedAt
	if a.FinishedAt != nil {
		aTime = *a.FinishedAt
	}
	if b.FinishedAt != nil {
		bTime = *b.FinishedAt
	}
	if cmp := bTime.Compare(aTime); cmp != 0 {
		return cmp
	}
	return doneStatusRank(a.Status) - doneStatusRank(b.Status)
}

// Keep aligned with PAST_STATUS_RANK in web/src/pages/RunsList.tsx.
func doneStatusRank(status string) int {
	switch status {
	case "failed":
		return 0
	case "cancelled":
		return 1
	case "completed":
		return 2
	default:
		return 3
	}
}

func (b *boardState) selected() (apitypes.RunListItemDTO, bool) {
	v := b.visible()
	if b.cursor < 0 || b.cursor >= len(v) {
		return apitypes.RunListItemDTO{}, false
	}
	return v[b.cursor], true
}

func (m tuiModel) boardKey(k string) (tea.Model, tea.Cmd) {
	// Filter input mode swallows ordinary keys so typing "a" filters rather than
	// toggling the admin board.
	if m.board.filtering {
		switch k {
		case keyEnter, keyEsc:
			m.board.filtering = false
			if k == keyEsc {
				m.board.filter = ""
			}
			m.board.clampCursor()
		case "backspace":
			if n := len([]rune(m.board.filter)); n > 0 {
				m.board.filter = string([]rune(m.board.filter)[:n-1])
			}
			m.board.clampCursor()
		default:
			if k == keySpaceName {
				k = " "
			}
			if len([]rune(k)) == 1 {
				m.board.filter += k
				m.board.clampCursor()
			}
		}
		m.board.scroll = m.syncedScrollAt(m.boardScrollCapacity())
		return m, nil
	}

	if d := motionDelta(k); d != 0 {
		m.board.cursor += d
		m.board.clampCursor()
		m.board.scroll = m.syncedScrollAt(m.boardScrollCapacity())
		return m, nil
	}

	switch k {
	case keyFilter:
		m.board.filtering = true
		return m, nil
	case keyRefresh:
		// User-initiated fetch (PRD #1130 M1 D1): NEVER gated on the in-flight guard — a
		// keypress is intent and always issues a fetch. startBoardReq mints a fresh id that
		// becomes the new waitID, so the next periodic tick does not stack a second poll on top
		// of this one and any periodic reply still in flight is superseded (its stale reqID is
		// dropped); this reply's own reqID clears the guard.
		return m, tea.Batch((&m).startBoardReq(), m.fetchRateLimitsCmd(), m.fetchCodexRateLimitsCmd(), (&m).startSelfUsageReq(), m.fetchSettingsCmd(), m.fetchVaultCmd(), (&m).startWorkersReq())
	case keyAdmin:
		m.board.admin = !m.board.admin
		m.board.adminDenied = false
		m.board.cursor = 0
		m.board.scroll = 0
		// Same as keyRefresh (D1): always fetch, minting a fresh id via startBoardReq so the next
		// tick does not stack and any pre-toggle periodic reply is superseded. The subsequent
		// boardRunsMsg for the new admin value carries the matching reqID and clears the guard.
		return m, tea.Batch((&m).startBoardReq(), (&m).startSelfUsageReq(), (&m).startWorkersReq())
	case keyHideDone:
		// No-op on the admin board: AdminListRuns already returns non-terminal runs only, so
		// hiding "finished" runs would change no rows — flipping the label there reads as a
		// broken toggle. Ignore it rather than toggling a hidden state.
		if m.board.admin {
			return m, nil
		}
		// Client-side view over the already-fetched runs, so no refetch. Reset the cursor and
		// scroll since the visible list changes shape under it.
		m.board.hideDone = !m.board.hideDone
		m.board.cursor = 0
		m.board.scroll = 0
		m.board.clampCursor()
		return m, nil
	case keyEnter, keyRight:
		// enter or → (right) opens the selected run — → is the natural "drill in" that pairs
		// with ← (left) backing out of the run detail (detailKey).
		sel, ok := m.board.selected()
		if !ok {
			return m, nil
		}
		m.fromSplit = m.splitDrawn()
		m.view = viewDetail
		m.detail = newDetailState(sel.ID)
		// A fresh session generation per drill-in: a reply still in flight from a previous
		// session on the SAME run (exit, reopen) passes the runID guard and is rejected on this.
		m.detailGen++
		m.detail.gen = m.detailGen
		return m, tea.Batch(m.loadRunCmd(sel.ID), m.loadTailCmd(sel.ID), m.openStreamCmd(sel.ID))
	case keyTab, keyViewWorkers:
		m.setListView(viewWorkers)
		return m, nil
	case keyViewPulls:
		return m.gotoPulls()
	case keyViewCI:
		// 4 jumps straight to the forge `ci` list (PRD #1255 M4b).
		return m.gotoCI()
	}
	return m, nil
}

// tabStrip builds the wordmark + the floor · workers · pulls · ci tab strip shared by the board
// and the forge views (PRD #1255 D1): the active screen's tab is tungsten-bold, the rest
// faint. The marked tab and admin relabel are explicit so a split header can
// mark the focused pane independently. The board's admin sub-mode relabels its own tab "active runs"
// (AdminListRuns returns non-terminal runs only), keeping the board's wordmark promise.
// The per-repo forge screens name the scoped repo (D2) after the tabs; the board is
// cross-repo and names none.
func (m tuiModel) tabStrip(admin bool, marked tuiView, repoSuffix bool) string {
	floorLabel := "floor"
	if admin {
		floorLabel = "active runs"
	}
	tabs := []struct {
		label  string
		active bool
	}{
		{floorLabel, marked == viewBoard},
		{"workers", marked == viewWorkers},
		{"pulls", marked == viewPulls},
		{"ci", marked == viewCI},
	}
	out := m.pal.title.Render("▚▚ uzi") + m.pal.faint.Render(" · ")
	for i, t := range tabs {
		if i > 0 {
			out += m.pal.faint.Render("  ")
		}
		if t.active {
			label := t.label
			out += m.pal.title.Render(label)
		} else {
			out += m.pal.faint.Render(t.label)
		}
	}
	if repoSuffix {
		if repo, ok := m.currentRepo(); ok {
			// PathWithNamespace is forge-authored (D7) → renderer.Plain.
			out += m.pal.faint.Render("   " + m.renderer.Plain(repo.PathWithNamespace, 40))
		}
	}
	return out
}

// vaultLockedReasonSubstr is the lowercased marker matched on a run's HealthReason to count
// runs parked because the viewer's vault is locked (PRD #1251 M3, tier-2 escalation input,
// D10/R3). The full server string is reasonVaultLocked = "your vault is locked, so this run
// can't start" (api/internal/workersvc/health.go), but that is UNEXPORTED in workersvc and
// cmd/uzi (package main) must not import the server stack, so the count matches this lowercased
// substring instead. A reword upstream would break the count silently — mitigated by pinning
// the exact server string in a regression test (TestVaultBandMatchesServerReasonString); presence
// never depends on it (it comes from WhoamiVault, D10), so a reword degrades only the tier-2
// count, never whether the lock is shown.
const vaultLockedReasonSubstr = "vault is locked"

// ownParkedOnVaultCount counts the VIEWER'S OWN board runs parked because the vault is locked —
// the best-effort tier-2 escalation input (PRD #1251 M3, R3/R6). A run counts when its
// HealthReason contains vaultLockedReasonSubstr (case-insensitive); the reason is only a
// best-effort input (it is run-health-gated and collapses into the generic waiting_worker
// status, D10), so a run-health-off board simply counts 0 queued runs and the indicator stays
// at the M2 tier-1 hint. Issue #1766: a run also counts when it is a recovery_wait park with
// cause vault_locked (isVaultLockedPark), a typed signal that needs no run-health.
//
// It is computed over m.board.runs — the FULL loaded set, not the scrolled window — matching
// boardSummary's run-source convention, so the count is stable while the board scrolls.
//
// Scoping (D11): the owner-filter decision keys on the PROVENANCE of the currently-resident run
// set (m.board.runsAdmin — the admin-ness of the board whose accepted ListRuns reply is held),
// NOT the live admin toggle. On an own-provenance set ListRuns was owner-scoped, so every run is
// the viewer's own and all matching runs count. On an admin-provenance set AdminListRuns returned
// EVERY user's runs with their health reasons present (RunDTO.HealthReason rides unconditionally,
// run.go), so the count is scoped to runs the viewer OWNS via OwnerEmail — it never counts or
// blames another user's locked vault the admin cannot act on. Keying on provenance (not the toggle)
// closes the transient window after keyAdmin flips admin→own but before the own reply lands, when
// the stale ALL-USERS set is still resident: the owner-filter stays applied until the own set
// arrives, so a stranger's run is never counted into the viewer's band. If the viewer identity is
// unknown (selfEmail == "", whoami failed) the band is suppressed on an admin-provenance set
// (count 0) rather than risk attributing a stranger's lock to the admin.
func (m tuiModel) ownParkedOnVaultCount() int {
	admin := m.board.runsAdmin
	if admin && m.selfEmail == "" {
		return 0
	}
	n := 0
	for _, r := range m.board.runs {
		// Issue #1766: a run already in flight when the vault locked parks as recovery_wait
		// with cause vault_locked, whatever its health reason says; it is parked on the vault too.
		queuedOnVault := r.HealthReason != nil && strings.Contains(strings.ToLower(*r.HealthReason), vaultLockedReasonSubstr)
		if !queuedOnVault && !isVaultLockedPark(r.RunDTO) {
			continue
		}
		if admin && (r.OwnerEmail == nil || !strings.EqualFold(*r.OwnerEmail, m.selfEmail)) {
			continue
		}
		n++
	}
	return n
}

// vaultIndicatorLine is the vault-locked andon line (PRD #1251 M2/M3, D3/D4/D5): a STEADY (never
// blinking), non-dismissable line shown when the viewer's vault is locked, and "" otherwise — so
// it auto-clears on its own the moment WhoamiVault reports the vault unlocked (D3). It is a line
// of its OWN, adjacent to and never replacing the rate-limit strip (D5): the two are distinct
// signals and can be visible together.
//
// Two mutually-exclusive forms share this ONE optional line, so boardCapacity's single-row
// reservation (via vaultIndicatorLine() != "") stays correct without change:
//   - M3 tier-2 amber needs-you band, when ≥1 of the viewer's OWN runs is parked on the vault
//     (ownParkedOnVaultCount ≥ 1): the escalated, filled attention surface (vaultBand).
//   - M2 tier-1 quiet faint hint, when locked but nothing is parked (run-health off, or count 0):
//     the baseline informational hint (R6 — the lock still shows because WhoamiVault reports it).
//
// The signal is carried by the WORDS "vault locked", never colour alone (D4). Under
// colorprofile.Ascii / NO_COLOR the faint SGR is stripped downstream, so the ascii form drops
// the lock glyph for a plain "[locked]" marker that survives with no colour at all.
func (m tuiModel) vaultIndicatorLine() string {
	if !m.vaultLocked {
		return ""
	}
	if n := m.ownParkedOnVaultCount(); n >= 1 {
		return m.vaultBand(n)
	}
	if m.profile == colorprofile.Ascii {
		return " [locked] vault locked"
	}
	return " " + m.pal.faint.Render("🔒 vault locked")
}

// vaultBand is the M3 tier-2 amber needs-you band (PRD #1251 M3, D2/D8): the ONE filled surface,
// a full-width amber fill with near-bg ink (pal.amber / pal.bandFg), shown when the vault is
// locked AND n ≥ 1 of the viewer's own runs are parked on it. Steady (never blinking, D3),
// non-dismissable, and it auto-clears when the lock clears (vaultIndicatorLine returns "" then).
// It shows a COUNT, never the raw HealthReason, so no untrusted text reaches the frame (D7) — the
// hostile-render test proves the count path cannot inject. The copy aligns with
// vault_lock_notice.go and points the fix off-TUI, in the web app (there is no CLI
// vault-unlock command — unlock is a web/API surface only).
//
// It reuses the detail screen's attentionBanner fill+ink convention (paintSeg fg/bg, ▌ cap, full
// width padded to m.width) so the board's one filled surface reads identically to the detail's.
// Under colorprofile.Ascii the amber fill is stripped for a plain ascii-safe band whose WORDS
// ("VAULT LOCKED", "N run(s) parked", "unlock in the web app to resume") carry the signal with no colour (D4).
func (m tuiModel) vaultBand(n int) string {
	runsWord := "runs"
	if n == 1 {
		runsWord = "run"
	}
	count := itoa(n) + " " + runsWord + " parked"
	if m.profile == colorprofile.Ascii {
		return "[VAULT LOCKED] " + count + " - unlock in the web app to resume"
	}
	amber, fg := m.pal.amber, m.pal.bandFg
	seg := func(bold bool, s string) string { return paintSeg(fg, amber, bold, s) }
	left := seg(true, "▌ VAULT LOCKED") +
		seg(false, " · "+count+" — unlock in the web app to resume")
	return clampVisual(padSeg(left, m.width, amber), m.width)
}

func (m tuiModel) renderBoard() string {
	return m.renderBoardBody(m.height, true)
}

func (m tuiModel) renderBoardBody(height int, fullScreen bool) string {
	var sb strings.Builder
	rows := m.board.visible()

	// ONE per-frame meter snapshot with ONE now (PRD 1519 M4): boardMeterLayout decides
	// combined-vs-split once and returns the rendered header meter line(s). The SAME snapshot
	// feeds both the combined summary line and its row reservation, so the reserved
	// chrome can never disagree with what is drawn.
	var meters boardMeterLayout
	if fullScreen {
		meters = m.boardMeterLayout(time.Now())
	}

	// The wordmark is now a tab strip (PRD #1255 D1): ▚▚ uzi · floor  workers  pulls  ci, the active
	// tab bold. tabStrip relabels the floor tab "active runs" on the admin board (AdminListRuns
	// returns non-terminal runs only, so promising completed rows would be a claim the API
	// cannot satisfy).
	brand := m.boardTitle(fullScreen)

	// The display list injects non-selectable eyebrow + spacer lines around the run rows; the
	// cursor still indexes RUN rows only (via visible()), so selection/enter/clamp are unchanged.
	// The window keeps the selected run row on screen so the wordmark and footer never scroll off.
	items := m.buildBoardItems(rows)
	meterRows := len(meters.lines)
	if fullScreen {
		meterRows = len(m.boardMeterSummaryLines(meters.lines, m.boardSummary()))
	}
	capacity := m.boardCapacityAt(height, meterRows, fullScreen)
	selItem := selectedBoardItem(items, m.board.cursor)
	start, end := boardWindow(selItem, m.board.scroll, len(items), capacity)

	// Summary glyph cluster + position readout, right-aligned with account meters. Over the WHOLE board (not the
	// filtered view) so it stays a stable factory read while you filter.
	summary := m.boardSummary()
	if len(rows) > 0 {
		lo, hi := windowRunSpan(items, start, end)
		summary += m.pal.faint.Render(" · " + itoa(lo) + "–" + itoa(hi))
	}
	if fullScreen {
		for _, line := range m.workerFleetTitleLines(" " + brand) {
			sb.WriteString(line + "\n")
		}
	} else {
		sb.WriteString(clampVisual(" "+brand, m.width) + "\n")
	}
	// The viewer's own rate-limit meters, mirroring the web sidebar's selection (PRD #1209 M3 /
	// 1519 M4). boardMeterLayout adaptively renders the Claude and Codex meters on ONE combined
	// header line when they fit m.width, or on two lines (Claude, then Codex) when they do not.
	// The summary joins the last account line or gets a separate row; capacity reserves
	// that combined layout from the same meter snapshot.
	if fullScreen {
		for _, line := range m.boardMeterSummaryLines(meters.lines, summary) {
			sb.WriteString(line + "\n")
		}
	}
	// The tier-1 vault-locked hint (PRD #1251 M2, D5): its OWN line directly under the strip,
	// never replacing or hiding it — both are distinct signals and can show together. Its row
	// is reserved in boardCapacity via the SAME vaultIndicatorLine() check, so the two spots
	// cannot drift.
	if fullScreen {
		if vault := m.vaultIndicatorLine(); vault != "" {
			sb.WriteString(vault + "\n")
		}
		sb.WriteString("\n")
	}

	if fullScreen && m.board.adminDenied {
		sb.WriteString(clampVisual(m.pal.faint.Render(" the factory-wide board needs an admin (uza_) token — showing your runs"), m.width) + "\n")
	}
	if fullScreen && m.board.err != nil {
		sb.WriteString(clampVisual(m.pal.faint.Render(" could not refresh: "+fmtErr(m.board.err)), m.width) + "\n")
	}

	if len(rows) == 0 {
		sb.WriteString(m.boardEmptyState())
	} else {
		// The judge marker's sub-column widths are sized to the WHOLE visible list (not just the
		// window) so the columns do not jitter as you scroll. Every row flushes its marker to the
		// board's right edge.
		mc := m.boardMarkerCols(rows)
		for i := start; i < end; i++ {
			switch it := items[i]; it.kind {
			case biEyebrow:
				sb.WriteString(m.boardEyebrow(it) + "\n")
			case biRow:
				r := rows[it.runIdx]
				sel := it.runIdx == m.board.cursor
				sb.WriteString(m.boardRow(r, sel, mc) + "\n")
				// The selected row gains a variable-height second "now" line (D4). The window
				// math reserves its physical line via boardCapacity, so this never overflows.
				if sel {
					if sl := m.boardSecondLine(r); sl != "" {
						sb.WriteString(sl + "\n")
					}
				}
			default:
				sb.WriteString("\n")
			}
		}
	}

	if fullScreen {
		sb.WriteString(clampVisual(m.boardFooterLine(), m.width))
	}
	frame := sb.String()
	if fullScreen {
		if height <= 0 {
			return ""
		}
		lines := strings.Split(frame, "\n")
		if len(lines) > height {
			// Headers can exceed the viewport at tiny heights. Keep the footer,
			// while cropping the already capacity-budgeted body to its available rows.
			return strings.Join(append(lines[:height-1], lines[len(lines)-1]), "\n")
		}
	}
	return frame
}

// boardItem is one line of the board's display list: a band eyebrow, a run row, or a blank
// spacer between bands. Only biRow lines are selectable; runIdx indexes visible().
type boardItem struct {
	kind   int
	band   int // biEyebrow: which band
	count  int // biEyebrow: how many runs in it
	runIdx int // biRow: index into visible()
}

const (
	biEyebrow = iota
	biRow
	biSpacer
)

// buildBoardItems turns the (already band-ordered) visible run list into the flat display
// list: an eyebrow at each band boundary, a spacer between bands, one row per run. Cheap
// (no styling), so both the renderer and the key handler's syncedScroll can call it and agree
// on line positions.
func (m tuiModel) buildBoardItems(rows []apitypes.RunListItemDTO) []boardItem {
	var counts [numBands]int
	for _, r := range rows {
		counts[runBandOf(r)]++
	}
	items := make([]boardItem, 0, len(rows)+numBands*2)
	prevBand := -1
	for i, r := range rows {
		if b := runBandOf(r); b != prevBand {
			if prevBand != -1 {
				items = append(items, boardItem{kind: biSpacer})
			}
			items = append(items, boardItem{kind: biEyebrow, band: b, count: counts[b]})
			prevBand = b
		}
		items = append(items, boardItem{kind: biRow, runIdx: i})
	}
	return items
}

// selectedBoardItem is the display index of the row the cursor is on (0 if none).
func selectedBoardItem(items []boardItem, cursor int) int {
	for i, it := range items {
		if it.kind == biRow && it.runIdx == cursor {
			return i
		}
	}
	return 0
}

// windowRunSpan is the 1-based run position range [lo, hi] visible in items[start:end].
func windowRunSpan(items []boardItem, start, end int) (lo, hi int) {
	for i := start; i < end && i < len(items); i++ {
		if items[i].kind != biRow {
			continue
		}
		pos := items[i].runIdx + 1
		if lo == 0 || pos < lo {
			lo = pos
		}
		if pos > hi {
			hi = pos
		}
	}
	if lo == 0 {
		lo = 1
	}
	return lo, hi
}

// boardEyebrow is a faint CAPS band label + count; NEEDS YOU is amber, because it is the one
// band a human must act on.
func (m tuiModel) boardEyebrow(it boardItem) string {
	name := bandNames[it.band]
	if it.band == bandNeedsYou {
		return " " + lipgloss.NewStyle().Foreground(m.pal.amber).Bold(true).Render(name) + m.pal.faint.Render(" · "+itoa(it.count))
	}
	return " " + m.pal.faint.Render(name+" · "+itoa(it.count))
}

// boardFooter preserves the unabridged legend for version-readout selection.
func (m tuiModel) boardFooter() string {
	return m.renderFooterHints(m.boardFooterHints(), 0)
}

func (m tuiModel) boardFooterHints() []footerHint {
	parts := []footerHint{m.footerHint("enter/→", "open"), m.footerHint("/", "filter")}
	markedCost := !m.board.admin && m.selfUsageReady && (m.selfUsage.Last7SubscriptionRunCount > 0 || m.selfUsage.Last7UnreportedRunCount > 0)
	if markedCost {
		parts = append(parts, m.footerHint("q", "quit"), footerHint{text: m.pal.faint.Render("+ partial")})
	}
	if m.board.admin {
		parts = append(parts, m.footerHint("a", "my runs"))
	} else {
		parts = append(parts, m.footerHint("a", "factory"))
		if m.board.hideDone {
			parts = append(parts, m.footerHint("h", "show done"))
		} else {
			parts = append(parts, m.footerHint("h", "fold done"))
		}
	}
	parts = append(parts, m.footerHint("r", "refresh"), m.footerHint("?", "keys"))
	if !markedCost {
		parts = append(parts, m.footerHint("q", "quit"))
	}
	return parts
}

// boardFooterLine chooses the readout against the original legend, then fits
// structured hints in the suffix budget. Split notes only use leftover space.
func (m tuiModel) boardFooterLine() string {
	hints := m.boardFooterHints()
	original := m.boardFooter()
	if line, ok := m.restartFooterContent(original, hints); ok {
		return line
	}
	if !m.showVersion {
		return m.withSplitNote(m.fitFooterHints(hints, m.width))
	}
	const gap = 1
	readout := m.versionReadout()
	if visualWidth(original)+gap+visualWidth(readout) > m.width {
		readout = m.versionClientOnly()
	}
	// Shed all optional hints before shortening an unusually long client stamp.
	essentialW := visualWidth(m.renderFooterHints(hints, 6))
	if m.width >= 80 {
		readout = clampVisual(readout, max(0, m.width-essentialW-gap))
	} else {
		readout = clampVisual(readout, max(0, m.width))
	}
	rw := visualWidth(readout)
	help := m.fitFooterHints(hints, m.width-rw-gap)
	if candidate := m.withSplitNote(help); visualWidth(candidate)+gap+rw <= m.width {
		help = candidate
	}
	return padVisual(help, max(0, m.width-rw)) + readout
}

// restartFooter also serves the split caller, whose supplied keys are essential.
func (m tuiModel) restartFooter(help string) (string, bool) {
	return m.restartFooterContent(help, nil)
}

// restartHintText is the unstyled full restart hint shared by the board and
// split footers, so the wording cannot drift between them.
func (m tuiModel) restartHintText() string {
	installed := m.renderer.Plain(m.updatePrompt.installedVersion, 256)
	return "v" + strings.TrimPrefix(installed, "v") + " installed, restart uzi to use it"
}

// restartFooterContent reserves essential help before choosing the installed
// banner. Board hints shed within that budget; split hint selection stays intact.
func (m tuiModel) restartFooterContent(help string, hints []footerHint) (string, bool) {
	if !m.showVersion || m.updatePrompt.installedVersion == "" {
		return "", false
	}
	hint := m.restartHintText()
	width := max(0, m.width)
	essentialW := visualWidth(help)
	if hints != nil {
		essentialW = visualWidth(m.renderFooterHints(hints, 6))
	}
	if visualWidth(hint)+1+essentialW > width {
		hint = "restart uzi"
	}
	hint = clampVisual(hint, width)
	if m.profile != colorprofile.Ascii {
		hint = m.pal.faint.Render(hint)
	}
	hw := visualWidth(hint)
	budget := max(0, width-hw-1)
	if hints != nil {
		help = m.fitFooterHints(hints, budget)
	}
	if m.profile == colorprofile.Ascii {
		help = ansi.Strip(help)
	}
	left := clampVisual(help, budget)
	return padVisual(left, max(0, width-hw)) + hint, true
}

// versionReadout is the compact CLI-vs-server version line. The client version renders
// verbatim as this binary was stamped (v0.63.0, or `dev`); the server renders verbatim
// as it reports (bare 0.63.0), sanitized through cellText — it is attacker-controlled
// (GET /api/version) and serverVersion is in d7UntrustedFields. Steady colour only, no
// blink (SGR 5 is an accessibility problem, WCAG 2.2.2); red is reinforcement, the arrow
// and server number carry "behind" on their own — which is what survives NO_COLOR/Ascii.
func (m tuiModel) versionReadout() string {
	client := version
	srv := cellText(m.serverVersion)
	cmp, ok := uzicli.CompareServerVersion(client, srv)
	if !ok {
		// dev build, or no/invalid server version: client alone, neutral.
		return m.pal.faint.Render(client)
	}
	ascii := m.profile == colorprofile.Ascii
	switch {
	case cmp == 0:
		return m.pal.faint.Render(client) // in sync: one neutral version
	case cmp < 0: // CLI behind server: client red, faint arrow to server
		arrow, marker := " ⇢ ", ""
		if ascii {
			arrow, marker = " -> ", " (behind)"
		}
		red := lipgloss.NewStyle().Foreground(m.pal.alarm)
		return red.Render(client) + m.pal.faint.Render(arrow+srv+marker)
	default: // CLI ahead of server: neutral, never alarm
		arrow := " ⇠ "
		if ascii {
			arrow = " <- "
		}
		return m.pal.faint.Render(client + arrow + srv)
	}
}

// versionClientOnly is the narrow-width fallback: the client version with no server
// suffix, still red when the CLI is behind so the alarm is not silently dropped.
func (m tuiModel) versionClientOnly() string {
	client := version
	if cmp, ok := uzicli.CompareServerVersion(client, cellText(m.serverVersion)); ok && cmp < 0 {
		return lipgloss.NewStyle().Foreground(m.pal.alarm).Render(client)
	}
	return m.pal.faint.Render(client)
}

const (
	boardIDWidth         = 8  // short run id (first 8 of the UUID)
	boardStatusWordWidth = 14 // status word cell; fits the 13-rune longest words ("recovery wait" / "needs landing", issue #1418) with a column of breathing room
	boardAgeWidth        = 4  // AGE cell (relAge, single-unit)
	boardMileWidth       = 9  // milestone micro-bar cell (up to boardMileCap ▰/▱ cells, or done/total | –/N text above that)
	boardMileCap         = 9  // above this many milestones the micro-bar falls back to N/M text
	boardOwnerWidth      = 20 // admin owner-email cell
	boardCredWidth       = 10 // credential cell: a 2-col tone-dot slot + up to 8 cols of token label
	boardCostWidth       = 6  // COST cell width (right-aligned; fits "$9999", "<$1", "—")
	boardTitleMax        = 60 // TITLE cap: long titles trim to a tidy column instead of running a wide terminal
	// boardMileMinWidth is the narrowest own board that shows the milestone micro-bar. Below it
	// the column is dropped (milestone progress is still on the run-detail view) so the fixed
	// prefix does not squeeze the title into an overflowing marker row (issue #379).
	// PROG (PRD #2602) is shown with MILES but its narrow cell comes out of the flexible TITLE
	// width, not out of this threshold: raising it would push MILES off the 100-col board (#379).
	boardMileMinWidth = 91
	// boardProgBarMinWidth is the narrowest own board that keeps the PROG bar; below it the cell
	// is the percent or a short flag alone, so the bar drops before the percent.
	boardProgBarMinWidth = boardMileMinWidth + boardProgWideWidth - boardProgNarrowWidth
	// boardCostMinWidth is the narrowest own board that keeps the COST column. It sits BELOW
	// boardMileMinWidth so COST (the higher-priority cost signal) is retained after the milestone
	// micro-bar drops on a narrowing terminal (PRD #650 M2; issue #379 invariant).
	boardCostMinWidth = 83
)

// boardShowMile reports whether the terminal is wide enough for the milestone micro-bar column.
// The admin board carries an extra owner cell, so its prefix is wider and the threshold shifts
// up by exactly that extra prefix — the column drops on a narrow admin board instead of clipping.
func (m tuiModel) boardShowMile() bool {
	min := boardMileMinWidth
	// The extra columns before TITLE (the admin owner cell, and the credential cell when
	// shown) push the mile threshold up by exactly their width, so a narrow board drops the
	// micro-bar instead of squeezing the title (issue #379). The milestone-free
	// prefix leaves only the owner + credential + cost deltas.
	min += boardRowPrefixWidth(m.board.admin, m.boardShowCred(), m.boardShowCost()) - boardRowPrefixWidth(false, false, false)
	return m.width >= min
}

// boardShowCost gates the own-board COST column on terminal width. The admin board has no
// per-run Usage on the wire (AdminListRuns attaches none), so it never shows the column —
// the same way it carries no judge marker. COST is retained down to boardCostMinWidth, BELOW
// boardMileMinWidth, so on a narrowing terminal the milestone micro-bar drops FIRST and COST
// (the higher-priority cost signal) drops only after it — the title is never the column cut.
func (m tuiModel) boardShowCost() bool {
	if m.board.admin {
		return false
	}
	min := boardCostMinWidth
	// The extra columns before TITLE (the credential cell when shown) push the threshold up by
	// exactly their width; cost is false in both terms so it cancels, only the cred delta survives.
	min += boardRowPrefixWidth(false, m.boardShowCred(), false) - boardRowPrefixWidth(false, false, false)
	return m.width >= min
}

// boardShowCred reserves one aligned credential column when either harness has
// multiple credentials. The admin board always shows it.
func (m tuiModel) boardShowCred() bool {
	return m.board.admin || m.tokenCount > 1 || m.codexCredentialCount > 1
}

// boardShowRunCred gates the cell by this run's harness, so a second Codex
// credential never exposes the sole Claude credential (and vice versa).
func (m tuiModel) boardShowRunCred(r apitypes.RunListItemDTO) bool {
	if m.board.admin {
		return true
	}
	switch r.Harness {
	case "codex":
		return m.codexCredentialCount > 1
	case "claude", "":
		return m.tokenCount > 1
	default:
		return false
	}
}

// boardMeterLayout is the ONE per-frame snapshot of the header rate-limit meter line(s) (PRD 1519
// M4). It decides — ONCE per frame, with ONE now — whether the Claude and Codex meters share a
// single combined line or fall back to two, and returns the rendered line string(s). len(lines) is
// the physical row count the meters occupy (0, 1, or 2). Rendering and capacity combine this
// SAME snapshot with the run summary, reserving any summary fallback row as well, so the
// reserved chrome agrees with what is drawn: a countdown/reset-in text can change visual width at a reset boundary between two
// now values and flip the combined-vs-split decision, so deriving the layout twice (two nows) is banned.
type boardMeterLayout struct{ lines []string }

// boardCodexProviderTag is the single faint LOWERCASE provider tag "codex " (the provider tag of the
// PRD 1519 D3 matrix; PRD #1653 D-T1 keeps it as the TUI's only provider mark — no glyph, Claude
// untagged). It is a hardcoded literal, so it needs no renderer.Plain; only the user-authored account
// aliases and bucket names (inside boardCodexAccountsSeg) go through Plain (D7). The tag and the
// per-account label are SEMANTICALLY DISTINCT and BOTH render even when an account alias literally
// equals "codex": that yields the "codex" provider tag PLUS that account's own "codex" label, which is
// correct — NOT the old redundant hardcoded prefix. The tag is never suppressed to match an alias.
func (m tuiModel) boardCodexProviderTag() string {
	return paintSeg(m.pal.faintC, nil, false, "codex ")
}

// boardMeterLayout builds the snapshot (PRD 1519 D3/D4, PRD #1653 D-T4). claudeStr is the Claude
// section (" " + the 3-space-joined per-token segments); codexAccounts is the Codex ACCOUNTS section
// with NO provider tag; the tag (boardCodexProviderTag, "codex ") is placed by THIS layout code, not
// by the sections.
//
// Tag-placement rule (invariant: at most one provider-level token ever renders per line):
//   - neither section present → 0 lines.
//   - Claude only → the Claude line; no tag (there is no Codex section).
//   - Codex only → one line " " + tag + codexAccounts — under Ascii/NoTTY the accent-bar tint is
//     stripped and the ▎ glyph is identical for both providers, so the tag is the provider signal.
//   - both, and the combined line fits m.width → one line claudeStr + gap + tag + codexAccounts;
//     exactly one tag, so the provider stays unambiguous under Ascii/NoTTY.
//   - both, and it does NOT fit → two lines (Claude, then " " + tag + codexAccounts). PRD #1653 D-T4
//     supersedes PRD 1519's omission of the tag here: the Codex windows are now labelled by length
//     ("5h"/"7d"), the same vocabulary as Claude's, so without the tag nothing on the split line says
//     Codex. A line wider than m.width is clipped at the right edge (D-T5).
//
// The Claude↔Codex section gap on the combined line is the same 3-space token gap the sections use
// internally; the per-account accent bar ▎ still delimits the account groups.
func (m tuiModel) boardMeterLayout(now time.Time) boardMeterLayout {
	claudeSegs := m.boardClaudeMeterSegs(now)
	codexAccounts := m.boardCodexAccountsSeg(now)
	hasClaude := len(claudeSegs) > 0
	hasCodex := codexAccounts != ""
	switch {
	case !hasClaude && !hasCodex:
		return boardMeterLayout{nil}
	case !hasCodex: // Claude only — no Codex section, so no provider tag.
		claudeStr := " " + strings.Join(claudeSegs, "   ")
		return boardMeterLayout{[]string{clampVisual(claudeStr, m.width)}}
	case !hasClaude: // Codex only — one tag.
		return boardMeterLayout{[]string{clampVisual(" "+m.boardCodexProviderTag()+codexAccounts, m.width)}}
	default: // both
		claudeStr := " " + strings.Join(claudeSegs, "   ")
		combined := claudeStr + "   " + m.boardCodexProviderTag() + codexAccounts
		if visualWidth(combined) <= m.width {
			return boardMeterLayout{[]string{clampVisual(combined, m.width)}} // exactly one tag
		}
		// Fallback: two lines; the Codex line keeps the tag (PRD #1653 D-T4).
		return boardMeterLayout{[]string{
			clampVisual(claudeStr, m.width),
			clampVisual(" "+m.boardCodexProviderTag()+codexAccounts, m.width),
		}}
	}
}

// boardCapacity is how many display lines fit between the wordmark block and the footer at the
// current terminal height. It is the zero-arg form for callers that do not already hold a meter
// snapshot (syncedScroll, tests): it derives the meter row count from a fresh boardMeterLayout.
// Rendering counts the combined meter/summary layout from its own snapshot so the reserved
// rows match what it draws.
func (m tuiModel) boardCapacity() int {
	return m.boardCapacityWith(len(m.boardMeterSummaryLines(m.boardMeterLayout(time.Now()).lines, m.boardSummary())))
}

// boardCapacityWith is boardCapacity given the number of header meter rows the caller is drawing
// (including any run-summary fallback). It counts the same chrome renderBoard draws: the wordmark
// line, the blank below it, the footer (3), the meter rows, plus the optional adminDenied, error,
// vault-hint, and selected-row second lines. Tiny full-screen viewports crop the body to keep the footer.
func (m tuiModel) boardCapacityWith(meterLines int) int {
	return m.boardCapacityAt(m.height, meterLines, true)
}

func (m tuiModel) boardCapacityAt(height, meterLines int, fullScreen bool) int {
	chrome := 1 // pane title, filter and summary
	if fullScreen {
		chrome += 2 // blank line and footer
		chrome += len(m.workerFleetTitleLines(" "+m.boardTitle(true))) - 1
		if m.board.adminDenied {
			chrome++
		}
		if m.board.err != nil {
			chrome++
		}
		// The adaptive rate-limit meter line(s) (PRD 1519 M4): reserve exactly the row count the caller
		// is drawing from its snapshot, so the combined-vs-split decision cannot drift by a line.
		chrome += meterLines
	}
	// The tier-1 vault-locked hint (PRD #1251 M2) is its own line under the strip when shown;
	// reserve one row for it the SAME way (calling vaultIndicatorLine, not re-deriving the
	// show-condition) so the row window and renderBoard's layout cannot drift by a line.
	if fullScreen && m.vaultIndicatorLine() != "" {
		chrome++
	}
	// The selected row's variable-height second "now" line (D4) reserves one physical line, so
	// the row window shows one fewer item and the extra line never pushes the footer off screen.
	// This is the ONE variable-height row the board's selection/scroll math must tolerate.
	if r, ok := m.board.selected(); ok && m.boardShowSecondLine(r) {
		chrome++
	}
	c := height - chrome
	if c < 1 {
		c = 1
	}
	return c
}

// boardWindow is the [start, end) slice of the display list to draw, scrolled so the selected
// line stays on screen. Stateless: it takes the last scroll offset and returns a corrected one
// via start, so it is safe to call from both the key handler (to persist) and the renderer.
func boardWindow(cursor, scroll, n, capacity int) (int, int) {
	if capacity < 1 {
		capacity = 1
	}
	if n <= capacity {
		return 0, n
	}
	start := scroll
	if start < 0 {
		start = 0
	}
	if cursor < start {
		start = cursor
	}
	if cursor >= start+capacity {
		start = cursor - capacity + 1
	}
	if maxStart := n - capacity; start > maxStart {
		start = maxStart
	}
	if start < 0 {
		start = 0
	}
	return start, start + capacity
}

// syncedScrollAt keeps the selected row visible at the drawn list pane's capacity.
// The key handler persists it so in-window navigation does not shift the viewport.
func (m tuiModel) syncedScrollAt(capacity int) int {
	items := m.buildBoardItems(m.board.visible())
	sel := selectedBoardItem(items, m.board.cursor)
	start, _ := boardWindow(sel, m.board.scroll, len(items), capacity)
	return start
}

// boardSummary is the top-right glyph cluster: ⚑ N · ✎ N · ➤ N · ⚿ N · ⊘ N · ▲ N · <total> runs.
// Zero-count segments are dropped, so a healthy factory reads simply "N runs". Computed
// over m.board.runs so it does not shrink under a filter. Every park in the NEEDS YOU
// band gets a segment — awaiting_followup (PRD #517) alongside awaiting_approval and
// awaiting_input, and the Codex relogin_required hold (PRD #1590, ⚿) — so no run that is
// the owner's turn is invisible in the summary line. The credential_disabled hold (PRD #1732,
// ⊘) is counted the same way.
func (m tuiModel) boardSummary() string {
	approvals, inputs, followups, relogins, credOff, warn := 0, 0, 0, 0, 0, 0
	for _, r := range m.board.runs {
		if codexReloginHold(r.RunDTO) {
			relogins++
		}
		if isCredentialDisabledHold(r.RunDTO) {
			credOff++
		}
		switch r.Status {
		case "awaiting_approval":
			// issue #750: a run mid-"revise" replan keeps status == awaiting_approval but is
			// NOT the user's turn (the agent is re-planning and will re-gate itself), so it must
			// not inflate the ⚑ counter — mirror runBand's !isRevising gate so the cluster count
			// matches the NEEDS YOU band header.
			if !r.IsRevising {
				approvals++
			}
		case "awaiting_input":
			inputs++
		case "awaiting_followup":
			followups++
		}
		// Match the recovery row's token: health frozen while parked needs no attention.
		if r.Status != statusRecoveryWait && stalledHealth[r.Health] {
			warn++
		}
	}
	var segs []string
	if approvals > 0 {
		segs = append(segs, paintSeg(m.pal.amber, nil, false, "⚑ "+itoa(approvals)))
	}
	if inputs > 0 {
		segs = append(segs, paintSeg(m.pal.amber, nil, false, "✎ "+itoa(inputs)))
	}
	if followups > 0 {
		segs = append(segs, paintSeg(m.pal.amber, nil, false, "➤ "+itoa(followups)))
	}
	if relogins > 0 {
		segs = append(segs, paintSeg(m.pal.amber, nil, false, codexReloginGlyph+" "+itoa(relogins)))
	}
	if credOff > 0 {
		segs = append(segs, paintSeg(m.pal.amber, nil, false, credDisabledGlyph+" "+itoa(credOff)))
	}
	if warn > 0 {
		segs = append(segs, paintSeg(m.pal.stall, nil, false, "▲ "+itoa(warn)))
	}
	// The server's seven-day aggregate includes runs beyond the board's row limit.
	if !m.board.admin && m.selfUsageReady {
		if total, ok := boardUsageCost(m.selfUsage); ok {
			segs = append(segs, lipgloss.NewStyle().Foreground(m.pal.tungsten).Render(m.renderer.Plain(total, 40)))
		}
	}
	segs = append(segs, m.pal.faint.Render(itoa(len(m.board.runs))+" runs"))
	return strings.Join(segs, m.pal.faint.Render(" · "))
}

// boardTitle is shared by rendering and capacity so filter chrome cannot hide a row.
func (m tuiModel) boardTitle(full bool) string {
	brand := ""
	if full {
		brand = m.tabStrip(m.board.admin, m.view, false)
	}
	if m.board.hideDone && !m.board.admin {
		brand += m.pal.faint.Render("   active only")
	}
	if m.board.filter != "" || m.board.filtering {
		brand += m.pal.faint.Render("   /" + cellText(m.board.filter))
		if m.board.filtering {
			brand += m.pal.title.Render("▌")
		}
	}
	return brand
}

// Reserve the widest possible page range so gaining a row cannot make the
// summary wrap and change the capacity that produced that range.
func (m tuiModel) boardSummaryReserve() string {
	summary := m.boardSummary()
	if n := len(m.board.visible()); n > 0 {
		summary += m.pal.faint.Render(" · " + itoa(n) + "–" + itoa(n))
	}
	return summary
}
func (m tuiModel) boardMeterSummaryLines(meters []string, summary string) []string {
	lines := append([]string(nil), meters...)
	if len(lines) > 0 {
		last := len(lines) - 1
		if m.width-visualWidth(lines[last])-visualWidth(m.boardSummaryReserve()) >= 2 {
			lines[last] = padVisual(lines[last], m.width-visualWidth(summary)) + summary
			return lines
		}
	}
	return append(lines, padVisual("", max(0, m.width-visualWidth(summary)))+clampVisual(summary, m.width))
}
func (m tuiModel) boardWindowSummary(capacity int) string {
	rows := m.board.visible()
	summary := m.boardSummary()
	if len(rows) > 0 {
		items := m.buildBoardItems(rows)
		start, end := boardWindow(selectedBoardItem(items, m.board.cursor), m.board.scroll, len(items), capacity)
		lo, hi := windowRunSpan(items, start, end)
		summary += m.pal.faint.Render(" · " + itoa(lo) + "–" + itoa(hi))
	}
	return summary
}
