package main

import (
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"golang.org/x/mod/semver"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// The codex-style startup update prompt (PRD #1251 M1): a modal shown over the board at
// startup when a newer STABLE uzi release exists, offering `brew upgrade uzi-cli` for a
// Homebrew install or the release-notes link otherwise. It is built entirely from the
// existing andon palette/primitives (D8) and does NO CLI egress of its own — the data
// rides the same GET /api/version the footer skew banner already fetches (D6).

// updatePromptState is the modal's whole state, held on tuiModel.updatePrompt.
type updatePromptState struct {
	// showing is whether the modal is currently drawn (and capturing keys). shownThisSession
	// latches after the first eligible reply so the prompt fires at most once per `uzi tui`.
	showing          bool
	shownThisSession bool
	// sel is the highlighted choice index into updateChoices() (which varies by isBrew).
	sel int
	// latestVersion / latestName / latestNotesURL are the server-authored release facts copied
	// off buildInfoMsg.latest. They are UNTRUSTED (D7) and drawn only through cellText /
	// renderer.Plain; all three are registered in d7UntrustedFields.
	latestVersion  string
	latestName     string
	latestNotesURL string
	// security is Latest.Security: a security release renders as the amber andon band (D2).
	security bool
	// isBrew is whether this uzi-cli is a Homebrew install (detectBrewCmd → brewInfoMsg). It
	// gates the "Update now" action vs the info-only variant. brewKnown records whether the
	// detection probe has completed, so it runs at most once.
	isBrew    bool
	brewKnown bool
	// pendingUpgrade / upgradeArgv are the foreground-exit hand-off (D1): "Update now" sets
	// these and quits, and newTUICmd's RunE runs env.Brew(true, upgradeArgv...) after p.Run().
	pendingUpgrade bool
	upgradeArgv    []string
}

// maybeShowUpdatePrompt evaluates the show gate ONCE per session on the first eligible
// buildInfoMsg and, when it opens the modal, returns the brew-detection command so the
// brew/non-brew variant resolves without blocking the board. It returns nil (no command,
// no state change) whenever the prompt should not show.
//
// The gate mirrors the skew warning's discipline plus the PRD #1251 axis rule: the wire
// Latest is a PRESENCE signal only (nil ⇒ no check ran / feature disabled ⇒ nothing), and
// the CLI axis is recomputed LOCALLY — this binary vs latest — NEVER the wire
// update_available bool, which is the server's own version-vs-latest axis.
//
// The local compare is uzicli.CompareServerVersion, NOT releasecheck.UpdateAvailable: the
// latter lives in a package that also imports internal/store (the pgx server stack), and
// cmd/uzi must not depend on it (TestNoServerDeps). CompareServerVersion is the CLI-side
// twin — the same normSemver re-prefix + IsValid guard + semver.Compare the footer skew
// readout already uses — so "cmp < 0" (CLI strictly behind) is exactly UpdateAvailable's
// "latest strictly newer than running", IsValid-guarded so a `dev`/malformed version reads
// as "not behind" rather than a false prompt.
func (m *tuiModel) maybeShowUpdatePrompt(latest *apitypes.LatestReleaseDTO) tea.Cmd {
	if m.updatePrompt.shownThisSession {
		return nil
	}
	// Same gate as the footer readout/auto-probe: a session not allowed to probe (--demo,
	// direct test construction, --quiet, UZI_VERSION_CHECK=0, an unstamped `dev` build) never
	// prompts. skewCheck already folds in the stamped-build requirement.
	if !m.skewCheck || !m.showVersion {
		return nil
	}
	if latest == nil {
		return nil // no check has run, or the release-check feature is disabled → show nothing
	}
	// Never offer an -rc.N. Latest excludes prereleases upstream (releasecheck/client.go), so
	// this is defense in depth: even if a prerelease ever reached the wire, the prompt must not
	// point a user at one. This is also what makes the RC-guard regression test meaningful.
	if isPrereleaseTag(latest.Version) {
		return nil
	}
	// Recompute the CLI axis locally: is THIS binary behind the latest? IsValid-guarded, so a
	// `dev` or malformed version reads as "not behind" (never a false prompt).
	if cmp, ok := uzicli.CompareServerVersion(version, latest.Version); !ok || cmp >= 0 {
		return nil
	}
	if m.dismissedForVersion(latest.Version) {
		return nil
	}
	m.updatePrompt.showing = true
	m.updatePrompt.shownThisSession = true
	m.updatePrompt.sel = 0
	m.updatePrompt.latestVersion = latest.Version
	m.updatePrompt.latestName = latest.Name
	m.updatePrompt.latestNotesURL = latest.NotesURL
	m.updatePrompt.security = latest.Security
	if !m.updatePrompt.brewKnown {
		return m.detectBrewCmd()
	}
	return nil
}

// dismissedForVersion reports whether the user already chose "don't remind me" for tag v
// on this server (PRD #1251 M1). A nil store (test/demo path, or no home dir) reads as
// not-dismissed. The per-version keying means a NEWER release re-prompts even after an
// older one was dismissed.
func (m tuiModel) dismissedForVersion(v string) bool {
	if m.store == nil {
		return false
	}
	return m.store.DismissedUpdateTag(m.serverURL) == v
}

// detectBrewCmd runs the deterministic brew-detection probe off the injected seam (R1) and
// reports the result as a brewInfoMsg. It never blocks the board and, on any doubt, reports
// non-brew (the info variant, which is always safe — it just shows the manual command).
func (m tuiModel) detectBrewCmd() tea.Cmd {
	brew := m.brew
	return func() tea.Msg {
		return brewInfoMsg{isBrew: detectBrew(brew)}
	}
}

// detectBrew is the two-detector brew probe (R1), pure over the injected seam so it is
// deterministic offline. Detector 1: `brew list uzi-cli` exits 0 iff the formula is
// installed. Detector 2 (fallback): the running executable resolves under `brew --prefix`.
// A nil seam or any error at either step reads as NON-brew.
func detectBrew(brew func(foreground bool, args ...string) (string, error)) bool {
	if brew == nil {
		return false
	}
	if _, err := brew(false, "list", "uzi-cli"); err == nil {
		return true
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	out, err := brew(false, "--prefix")
	if err != nil {
		return false
	}
	prefix := strings.TrimSpace(out)
	if prefix == "" {
		return false
	}
	// Resolve symlinks on both sides: a brew-managed binary is typically symlinked out of the
	// Cellar, so a raw prefix-check would miss it. EvalSymlinks failures (e.g. a fake prefix in
	// a test, or a path that no longer exists) fall back to the raw value rather than crashing.
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	if resolved, rerr := filepath.EvalSymlinks(prefix); rerr == nil {
		prefix = resolved
	}
	return exe == prefix || strings.HasPrefix(exe, prefix+string(filepath.Separator))
}

// updateChoice is one selectable action in the modal.
type updateChoice int

const (
	updateChoiceUpdateNow updateChoice = iota // brew users only
	updateChoiceNotNow
	updateChoiceDismiss
)

// updateChoices is the ordered choice list for the current variant: a brew user gets the
// "Update now" action first; a non-brew user gets only "Not now" / "Don't remind me".
func (m tuiModel) updateChoices() []updateChoice {
	if m.updatePrompt.isBrew {
		return []updateChoice{updateChoiceUpdateNow, updateChoiceNotNow, updateChoiceDismiss}
	}
	return []updateChoice{updateChoiceNotNow, updateChoiceDismiss}
}

// updateChoiceLabel is the visible text for a choice. The dismiss label carries the
// UNTRUSTED latest version, sanitized through cellText (D7).
func (m tuiModel) updateChoiceLabel(c updateChoice) string {
	switch c {
	case updateChoiceUpdateNow:
		return "Update now  (brew upgrade uzi-cli)"
	case updateChoiceDismiss:
		return "Don't remind me for " + cellText(m.updatePrompt.latestVersion)
	default:
		return "Not now"
	}
}

// updatePromptKey handles a key while the modal is up (PRD #1251 M1). ↑/↓ (and j/k, matching
// the rest of the TUI) move the selection, enter selects, esc = not now (close, session
// only). Every other key is swallowed — the modal is modal. ctrl+c is handled upstream in
// handleKey, so quitting still works.
func (m tuiModel) updatePromptKey(k string) (tea.Model, tea.Cmd) {
	choices := m.updateChoices()
	switch k {
	case keyEsc:
		m.updatePrompt.showing = false
		return m, nil
	case keyUp, "k":
		if m.updatePrompt.sel > 0 {
			m.updatePrompt.sel--
		}
		return m, nil
	case keyDown, "j":
		if m.updatePrompt.sel < len(choices)-1 {
			m.updatePrompt.sel++
		}
		return m, nil
	case keyEnter:
		return m.selectUpdateChoice(choices)
	}
	return m, nil
}

// selectUpdateChoice performs the highlighted choice. "Update now" arms the foreground-exit
// upgrade and quits (D1); "Don't remind me" persists the per-version dismissal and closes;
// "Not now" just closes for the session (no persistence).
func (m tuiModel) selectUpdateChoice(choices []updateChoice) (tea.Model, tea.Cmd) {
	if m.updatePrompt.sel < 0 || m.updatePrompt.sel >= len(choices) {
		m.updatePrompt.showing = false
		return m, nil
	}
	switch choices[m.updatePrompt.sel] {
	case updateChoiceUpdateNow:
		m.updatePrompt.pendingUpgrade = true
		m.updatePrompt.upgradeArgv = []string{"upgrade", "uzi-cli"}
		m.updatePrompt.showing = false
		return m, tea.Quit
	case updateChoiceDismiss:
		// Best-effort like the skew cache: a read-only $HOME must not break the TUI, so the
		// error is swallowed. It still closes the modal for this session either way.
		if m.store != nil {
			_ = m.store.RecordDismissedUpdate(m.serverURL, m.updatePrompt.latestVersion)
		}
		m.updatePrompt.showing = false
		return m, nil
	default: // updateChoiceNotNow
		m.updatePrompt.showing = false
		return m, nil
	}
}

// renderUpdatePrompt draws the modal from the real palette/primitives (D8): a `▲` eyebrow
// (tungsten routine, amber band for a security release, D2), a `uzi <current> → <latest>`
// line (current faint, latest sage), a body line, the choices with the `▸`/tungsten/selBg
// selection convention, and a footer hint. Under colorprofile.Ascii every fill/tint is
// stripped and an ascii `->` arrow is used, so the word "Update available"/"Security update"
// and the arrow carry the signal without colour (D4).
func (m tuiModel) renderUpdatePrompt() string {
	ascii := m.profile == colorprofile.Ascii
	pal := m.pal

	// Eyebrow: the andon signal word. Amber band for a security release, quiet tungsten otherwise.
	eyebrowWord := "▲ Update available"
	if m.updatePrompt.security {
		eyebrowWord = "▲ Security update"
	}
	var eyebrow string
	switch {
	case ascii:
		eyebrow = eyebrowWord
	case m.updatePrompt.security:
		eyebrow = lipgloss.NewStyle().Background(pal.amber).Foreground(pal.bandFg).Bold(true).
			Render(" " + eyebrowWord + " ")
	default:
		eyebrow = pal.title.Render(eyebrowWord)
	}

	// Version line: uzi <current>  →  <latest>. latest is UNTRUSTED (cellText); current is the
	// trusted ldflags stamp, rendered like the footer readout.
	latest := cellText(m.updatePrompt.latestVersion)
	arrow := "  →  "
	if ascii {
		arrow = "  ->  "
	}
	var versionLine string
	if ascii {
		versionLine = "uzi " + version + arrow + latest
	} else {
		versionLine = "uzi " + pal.faint.Render(version) + arrow +
			lipgloss.NewStyle().Foreground(pal.sage).Render(latest)
	}

	// Body line: the release name when present (UNTRUSTED, renderer.Plain), else a
	// variant-specific sentence.
	bodyText := "A newer release is available."
	if m.updatePrompt.security {
		bodyText = "A security update is available."
	}
	if name := m.renderer.Plain(m.updatePrompt.latestName, updatePromptTextCap); name != "" {
		bodyText = name
	}
	bodyLine := bodyText
	if !ascii {
		bodyLine = pal.faint.Render(bodyText)
	}

	lines := []string{eyebrow, versionLine, bodyLine}

	// Non-brew info variant: the release-notes URL instead of an Update-now action (UNTRUSTED,
	// renderer.Plain). A brew user gets the action row instead, so the URL is omitted there.
	if !m.updatePrompt.isBrew {
		if url := m.renderer.Plain(m.updatePrompt.latestNotesURL, updatePromptTextCap); url != "" {
			notes := "Release notes: " + url
			if !ascii {
				notes = pal.faint.Render(notes)
			}
			lines = append(lines, notes)
		}
	}

	lines = append(lines, "") // blank spacer before the choices

	// Choices, padded to a common width so the selected row's warm bar spans the block.
	choices := m.updateChoices()
	choiceWidth := 0
	for _, c := range choices {
		if w := visualWidth(m.updateChoiceLabel(c)) + 2; w > choiceWidth { // +2 for the cursor cell
			choiceWidth = w
		}
	}
	for i, c := range choices {
		lines = append(lines, m.updateChoiceRow(c, i == m.updatePrompt.sel, ascii, choiceWidth))
	}

	lines = append(lines, "") // blank spacer before the hint

	hint := "↑/↓ move · enter select · esc = not now"
	if !ascii {
		hint = pal.faint.Render(hint)
	}
	lines = append(lines, hint)

	content := strings.Join(lines, "\n")
	if ascii {
		// No box chrome or fills under Ascii — the text/glyph carriers stand alone (D4).
		return content
	}
	box := pal.box
	if m.updatePrompt.security {
		box = box.BorderForeground(pal.amber)
	}
	return box.Render(content)
}

// updatePromptTextCap bounds an UNTRUSTED release string (name, notes URL) to a sane modal
// width before it is drawn through renderer.Plain — a hostile server cannot stretch the modal.
const updatePromptTextCap = 80

// updateChoiceRow renders one choice with the shared selection convention: a bold `▸`
// tungsten cursor on the selected row, its label tungsten on the warm selBg fill spanning
// the row; unselected rows are quiet default ink with no fill. Under Ascii the `▸` glyph
// carries selection with no colour/fill.
func (m tuiModel) updateChoiceRow(c updateChoice, selected, ascii bool, width int) string {
	label := m.updateChoiceLabel(c)
	if ascii {
		cursor := "  "
		if selected {
			cursor = "▸ "
		}
		return cursor + label
	}
	var bg color.Color
	if selected {
		bg = m.pal.selBg
	}
	var row string
	if selected {
		row = paintSeg(m.pal.tungsten, bg, true, "▸ ") + paintSeg(m.pal.tungsten, bg, false, label)
	} else {
		row = paintSeg(nil, bg, false, "  ") + paintSeg(nil, bg, false, label)
	}
	return padSeg(row, width, bg)
}

// isPrereleaseTag reports whether tag is a semver prerelease (e.g. v0.85.0-rc.1). It
// re-prefixes and IsValid-guards like semverNewer, so a malformed tag reads as NOT a
// prerelease (the show gate's UpdateAvailable check then rejects it on the compare instead).
func isPrereleaseTag(tag string) bool {
	v := "v" + strings.TrimPrefix(tag, "v")
	return semver.Prerelease(v) != ""
}

// runPendingUpgrade runs `brew upgrade uzi-cli` in the FOREGROUND after the TUI has exited
// (PRD #1251 M1 D1), so the from-source compile progress and any failure are visible and the
// stale running process is replaced by a rerun. It is extracted from RunE so a test can
// assert it calls the seam with foreground=true and argv ["upgrade","uzi-cli"] WITHOUT
// running brew. A nil seam is reported cleanly rather than panicking.
func runPendingUpgrade(env Env, argv []string) error {
	if env.Brew == nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "update: no brew command is available")
	}
	// Clear the alt-screen residue and tell the user what is about to happen. The escape is
	// built from a source escape at runtime (never a raw control byte in source). Write errors
	// are dropped explicitly: this is a best-effort UX line, and env.Brew is the real work.
	_, _ = fmt.Fprint(env.Stdout, "\x1b[H\x1b[2J")
	_, _ = fmt.Fprintln(env.Stdout, "Updating uzi-cli via Homebrew (this compiles from source)...")
	if _, err := env.Brew(true, argv...); err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "update failed: %v\n", err)
		return uzicli.Exitf(uzicli.ExitGeneric, "brew upgrade uzi-cli: %v", err)
	}
	_, _ = fmt.Fprintln(env.Stdout, "Update complete. Start the TUI again to use the new version.")
	return nil
}
