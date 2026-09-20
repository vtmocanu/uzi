package main

// PRD #1251 M3 — tier-2 escalation band. When the vault is locked AND ≥1 of the viewer's OWN
// runs is parked on it, the quiet M2 tier-1 hint escalates to a steady amber "needs-you" band
// on the same one optional line (mutually exclusive with the hint). The band is scoped to the
// viewer's own runs on the admin/factory board (D11), never counts another user's locked vault,
// degrades back to the tier-1 hint when run-health is off or nothing is parked (R6), and shows a
// COUNT — never the raw HealthReason — so it cannot inject via the count path (D7).

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// serverVaultLockedReason is the EXACT server string reasonVaultLocked produces
// (api/internal/workersvc/health.go). It is UNEXPORTED there and cmd/uzi must not import the
// server stack, so it is pinned here as a fixture: a reword upstream that no longer contains the
// matched substring trips TestVaultBandMatchesServerReasonString below (R3).
const serverVaultLockedReason = "your vault is locked, so this run can't start"

// vaultBoardModel drives a fresh board to the given vault-lock state holding runs, on either the
// own board (admin=false) or the admin/factory board (admin=true), threading the viewer identity
// selfEmail off the whoami reply (used for M3 admin-board own-run scoping, D11).
func vaultBoardModel(t *testing.T, admin bool, selfEmail string, locked bool, runs []apitypes.RunListItemDTO) tuiModel {
	t.Helper()
	m := tuiTestModel(t, nil, "")
	if admin {
		m = press(t, m, keyAdmin)
	}
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, admin: admin, runs: runs})
	m = next.(tuiModel)
	next, _ = m.Update(vaultStatusMsg{user: apitypes.UserDTO{Email: selfEmail}, locked: locked})
	return next.(tuiModel)
}

// vaultParkedRun is a run parked with the given HealthReason (and optional owner email for the
// admin board). CurrentActivity is deliberately nil so no selected-row second line is reserved,
// keeping boardCapacity arithmetic isolated to the vault line.
func vaultParkedRun(id, reason, owner string) apitypes.RunListItemDTO {
	r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{
		ID: id, Kind: "issue", Status: "queued", Health: "waiting_worker", HealthReason: sptr(reason),
	}}
	if owner != "" {
		r.OwnerEmail = sptr(owner)
	}
	return r
}

// TestVaultBandMatchesServerReasonString pins the coupling between our lowercased substring and
// the exact unexported server reason (R3): if the substring is ever narrowed so the real reason
// no longer matches, the count would silently go 0 — this trips first.
func TestVaultBandMatchesServerReasonString(t *testing.T) {
	if !strings.Contains(strings.ToLower(serverVaultLockedReason), vaultLockedReasonSubstr) {
		t.Fatalf("vaultLockedReasonSubstr %q no longer matches the server reason %q — the tier-2 count would silently be 0", vaultLockedReasonSubstr, serverVaultLockedReason)
	}
}

// TestVaultBandEscalatesWhenOwnRunParked — locked + ≥1 own parked run renders the amber band
// (with the count and copy), replacing the tier-1 hint they share the one line with.
func TestVaultBandEscalatesWhenOwnRunParked(t *testing.T) {
	m := vaultBoardModel(t, false, "me@x.io", true, []apitypes.RunListItemDTO{
		vaultParkedRun("run-parked", serverVaultLockedReason, ""),
	})
	line := m.vaultIndicatorLine()
	plain := stripANSI(line)
	for _, want := range []string{"VAULT LOCKED", "1 run parked", "unlock in the web app to resume"} {
		if !strings.Contains(plain, want) {
			t.Errorf("escalated band missing %q; got stripped %q", want, plain)
		}
	}
	// Regression pin: the band must NOT name a CLI vault-unlock command — there is none, unlock is
	// a web/API-only surface, so the copy points at the web app instead.
	if strings.Contains(plain, "uzi vault unlock") {
		t.Errorf("band names the non-existent `uzi vault unlock` CLI command; got %q", plain)
	}
	// The band and the tier-1 hint are mutually exclusive on the one line: the faint lock glyph
	// is gone once the band shows.
	if strings.Contains(plain, "🔒") {
		t.Errorf("escalated band still carries the tier-1 lock glyph; got %q", plain)
	}
	// The amber fill (the ONE needs-you surface) is painted in a colour terminal — same fg/bg/bold
	// convention as the detail attentionBanner, so a probe segment must appear verbatim.
	if probe := paintSeg(m.pal.bandFg, m.pal.amber, true, "▌ VAULT LOCKED"); !strings.Contains(line, probe) {
		t.Errorf("band is not painted with the amber fill; want fragment %q in %q", probe, line)
	}
	// The whole board render carries the band.
	if out := stripANSI(m.View().Content); !strings.Contains(out, "VAULT LOCKED") || !strings.Contains(out, "1 run parked") {
		t.Errorf("board render does not show the escalated vault band:\n%s", out)
	}
}

// TestVaultBandFallsBackToTier1WhenNoneParked — locked with a 0 count (run-health off, or nothing
// parked on the vault) degrades to the M2 tier-1 faint hint, never the band (R6).
func TestVaultBandFallsBackToTier1WhenNoneParked(t *testing.T) {
	m := vaultBoardModel(t, false, "me@x.io", true, []apitypes.RunListItemDTO{
		vaultParkedRun("run-other", "waiting for a free worker", ""),
	})
	plain := stripANSI(m.vaultIndicatorLine())
	if !strings.Contains(plain, "🔒 vault locked") {
		t.Errorf("locked-but-nothing-parked must show the tier-1 hint (R6); got %q", plain)
	}
	if strings.Contains(plain, "VAULT LOCKED") || strings.Contains(plain, "parked") {
		t.Errorf("tier-1 fallback wrongly rendered the escalation band; got %q", plain)
	}
}

// TestVaultBandAdminScopingToOwnRuns — D11 cross-tenant safety: on the admin/factory board the
// count includes ONLY the viewer's own runs, and is suppressed entirely when the viewer identity
// is unknown, so the band never counts or blames another user's locked vault.
func TestVaultBandAdminScopingToOwnRuns(t *testing.T) {
	const self, other = "me@x.io", "stranger@x.io"

	// (a) Admin board, parked run owned by ANOTHER user: excluded, band suppressed → tier-1 hint.
	m := vaultBoardModel(t, true, self, true, []apitypes.RunListItemDTO{
		vaultParkedRun("r-other", serverVaultLockedReason, other),
	})
	if got := m.ownParkedOnVaultCount(); got != 0 {
		t.Errorf("another user's parked run must not be counted on the admin board; count = %d", got)
	}
	if plain := stripANSI(m.vaultIndicatorLine()); strings.Contains(plain, "VAULT LOCKED") {
		t.Errorf("admin band must not blame another user's locked vault; got %q", plain)
	}

	// (b) Admin board, parked run owned by the VIEWER: counted, band shows.
	m = vaultBoardModel(t, true, self, true, []apitypes.RunListItemDTO{
		vaultParkedRun("r-mine", serverVaultLockedReason, self),
	})
	if got := m.ownParkedOnVaultCount(); got != 1 {
		t.Errorf("the viewer's own parked run must be counted on the admin board; count = %d", got)
	}
	if plain := stripANSI(m.vaultIndicatorLine()); !strings.Contains(plain, "1 run parked") {
		t.Errorf("admin board with the viewer's own parked run must show the band; got %q", plain)
	}

	// (c) Admin board, viewer identity unknown (selfEmail == "", whoami failed): suppressed even
	// with a parked own-looking run present — never risk attributing a stranger's lock.
	m = vaultBoardModel(t, true, "", true, []apitypes.RunListItemDTO{
		vaultParkedRun("r-mine", serverVaultLockedReason, self),
	})
	if got := m.ownParkedOnVaultCount(); got != 0 {
		t.Errorf("with an unknown viewer identity the admin band must be suppressed; count = %d", got)
	}
	if plain := stripANSI(m.vaultIndicatorLine()); strings.Contains(plain, "VAULT LOCKED") {
		t.Errorf("admin band must be suppressed when the viewer identity is unknown; got %q", plain)
	}
}

// TestVaultBandScopesOnRunSetProvenanceNotToggle — D11 transient cross-tenant safety. keyAdmin
// flips admin→own IMMEDIATELY on keypress but does NOT clear the stale ALL-USERS run set, which
// stays resident until the async own-runs ListRuns reply lands. The owner-filter must scope on the
// PROVENANCE of the held run set (runsAdmin), not the live toggle, so during that window a
// stranger's vault-locked run is never counted into the viewer's band. Reddens if the count is
// scoped on m.board.admin.
func TestVaultBandScopesOnRunSetProvenanceNotToggle(t *testing.T) {
	const self, other = "me@x.io", "stranger@x.io"

	// Admin board holding ONLY a stranger's parked-on-vault run; the viewer's own vault is locked
	// but none of the viewer's own runs are parked → count 0, tier-1 hint (not the band).
	m := vaultBoardModel(t, true, self, true, []apitypes.RunListItemDTO{
		vaultParkedRun("r-other", serverVaultLockedReason, other),
	})
	if got := m.ownParkedOnVaultCount(); got != 0 {
		t.Fatalf("precondition: a stranger's parked run must not count on the admin board; count = %d", got)
	}
	if plain := stripANSI(m.vaultIndicatorLine()); strings.Contains(plain, "VAULT LOCKED") {
		t.Fatalf("precondition: the admin band must not show for a stranger's run; got %q", plain)
	}

	// Toggle a → own view WITHOUT delivering the own-runs reply: admin flips to false immediately,
	// but the stale admin run set (the stranger's run) is still resident and runsAdmin is still true.
	m = press(t, m, keyAdmin)
	if m.board.admin {
		t.Fatalf("the keyAdmin toggle did not flip off the admin board")
	}
	// The stranger's stale admin-set run must STILL not be counted during this window: the filter
	// scopes on the run set's provenance, not the live toggle. This reddens if it keys on m.board.admin.
	if got := m.ownParkedOnVaultCount(); got != 0 {
		t.Errorf("a stranger's stale admin run was miscounted into the viewer's band during the admin→own toggle window; count = %d, want 0", got)
	}
	if plain := stripANSI(m.vaultIndicatorLine()); strings.Contains(plain, "VAULT LOCKED") {
		t.Errorf("the vault band was wrongly shown during the admin→own toggle window; got %q", plain)
	}
	// The vault is still locked, so the tier-1 hint stays (the band suppressed, not the whole line).
	if plain := stripANSI(m.vaultIndicatorLine()); !strings.Contains(plain, "🔒 vault locked") {
		t.Errorf("the tier-1 hint should still show while the vault is locked and the band is suppressed; got %q", plain)
	}
}

// TestVaultBandSuppressedWhenViewerIdentityUnknown — pins the selfEmail=="" identity-unknown
// early-return guard INDEPENDENTLY of the per-run OwnerEmail mismatch. An admin-provenance run set
// holds a run whose OwnerEmail is a pointer to the EMPTY string with the viewer identity unknown
// (selfEmail == ""): without the guard, EqualFold("", "") would match and count the run, wrongly
// showing the band. Reddens if the selfEmail=="" early return is removed.
func TestVaultBandSuppressedWhenViewerIdentityUnknown(t *testing.T) {
	r := vaultParkedRun("r-empty-owner", serverVaultLockedReason, "")
	r.OwnerEmail = sptr("") // explicit empty-string owner (distinct from nil): EqualFold("", "") matches
	m := vaultBoardModel(t, true, "", true, []apitypes.RunListItemDTO{r})

	if got := m.ownParkedOnVaultCount(); got != 0 {
		t.Errorf("with an unknown viewer identity the band must be suppressed even for an empty-owner run; count = %d, want 0", got)
	}
	if plain := stripANSI(m.vaultIndicatorLine()); strings.Contains(plain, "VAULT LOCKED") {
		t.Errorf("the band must be suppressed when the viewer identity is unknown; got %q", plain)
	}
}

// TestVaultBandCountsOnlyMatchingOwnRuns — the count reflects only runs whose HealthReason matches
// the vault-locked substring (case-insensitive): a different reason and a nil reason do not count.
func TestVaultBandCountsOnlyMatchingOwnRuns(t *testing.T) {
	m := vaultBoardModel(t, false, "me@x.io", true, []apitypes.RunListItemDTO{
		vaultParkedRun("r1", serverVaultLockedReason, ""),
		vaultParkedRun("r2", "waiting for a free worker", ""),                     // different reason — not counted
		{RunDTO: apitypes.RunDTO{ID: "r3", Kind: "issue", Status: "running"}},     // nil HealthReason — not counted
		vaultParkedRun("r4", "Your VAULT IS LOCKED, so this run can't start", ""), // upper-case — counted (case-insensitive)
	})
	if got := m.ownParkedOnVaultCount(); got != 2 {
		t.Errorf("count must include only vault-locked HealthReasons (case-insensitive); got %d, want 2", got)
	}
	if plain := stripANSI(m.vaultIndicatorLine()); !strings.Contains(plain, "2 runs parked") {
		t.Errorf("band must read the plural count; got %q", plain)
	}
}

// TestVaultBandKeepsRateLimitStrip — D5: the band and the rate-limit strip are DISTINCT signals on
// their OWN lines and show together; the band never consumes or hides the strip.
func TestVaultBandKeepsRateLimitStrip(t *testing.T) {
	m := stripModel(t, []apitypes.TokenRateLimitDTO{okMeter("sec-personal", "personal", true, 33, 61)}, nil)
	next, _ := m.Update(boardRunsMsg{reqID: m.board.waitID, runs: []apitypes.RunListItemDTO{
		vaultParkedRun("r1", serverVaultLockedReason, ""),
	}})
	m = next.(tuiModel)
	next, _ = m.Update(vaultStatusMsg{user: apitypes.UserDTO{Email: "me@x.io"}, locked: true})
	m = next.(tuiModel)

	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "33%") {
		t.Errorf("the rate-limit strip was hidden when the vault band showed (D5 broken):\n%s", out)
	}
	if !strings.Contains(out, "VAULT LOCKED") {
		t.Errorf("the escalation band is missing while the strip renders:\n%s", out)
	}
	var stripLine, bandLine = -1, -1
	for i, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "33%") {
			stripLine = i
		}
		if strings.Contains(ln, "VAULT LOCKED") {
			bandLine = i
		}
	}
	if stripLine < 0 || bandLine < 0 || stripLine == bandLine {
		t.Errorf("strip (line %d) and vault band (line %d) must be present on distinct lines:\n%s", stripLine, bandLine, out)
	}
}

// TestBoardCapacityReservesVaultBandLine — the row window reserves EXACTLY one fewer content row
// whether the band or the tier-1 hint is showing, mirroring how the rate-limit strip is accounted,
// so the accounting cannot drift between the two mutually-exclusive forms of the one line.
func TestBoardCapacityReservesVaultBandLine(t *testing.T) {
	parked := []apitypes.RunListItemDTO{vaultParkedRun("r1", serverVaultLockedReason, "")}
	other := []apitypes.RunListItemDTO{vaultParkedRun("r1", "waiting for a free worker", "")}

	// Same run set, only the vault state differs, so the band line is the sole chrome delta.
	unlockedWithParked := vaultBoardModel(t, false, "me@x.io", false, parked)
	band := vaultBoardModel(t, false, "me@x.io", true, parked) // locked + parked → band
	hint := vaultBoardModel(t, false, "me@x.io", true, other)  // locked, nothing parked → tier-1 hint

	capUnlocked := unlockedWithParked.boardCapacity()
	if capBand := band.boardCapacity(); capBand != capUnlocked-1 {
		t.Errorf("boardCapacity: band = %d, unlocked = %d; the band must reserve exactly one row", capBand, capUnlocked)
	}
	if capHint := hint.boardCapacity(); capHint != capUnlocked-1 {
		t.Errorf("boardCapacity: hint = %d, unlocked = %d; the tier-1 hint must reserve exactly one row", capHint, capUnlocked)
	}
	if band.boardCapacity() != hint.boardCapacity() {
		t.Errorf("band (%d) and hint (%d) must reserve the same single row so the accounting cannot drift", band.boardCapacity(), hint.boardCapacity())
	}
}

// TestVaultBandAsciiFallback — under colorprofile.Ascii the amber fill is stripped and the band's
// WORDS carry the signal with NO colour (D4).
func TestVaultBandAsciiFallback(t *testing.T) {
	m := vaultBoardModel(t, false, "me@x.io", true, []apitypes.RunListItemDTO{
		vaultParkedRun("r1", serverVaultLockedReason, ""),
	})
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)

	line := m.vaultIndicatorLine()
	for _, want := range []string{"VAULT LOCKED", "1 run parked", "unlock in the web app to resume"} {
		if !strings.Contains(line, want) {
			t.Errorf("ascii band missing the signal word %q; got %q", want, line)
		}
	}
	// Regression pin: the ascii band must not name a CLI vault-unlock command either (there is none).
	if strings.Contains(line, "uzi vault unlock") {
		t.Errorf("ascii band names the non-existent `uzi vault unlock` CLI command; got %q", line)
	}
	// No SGR/colour under Ascii: the words alone carry it.
	if strings.ContainsRune(line, '\x1b') {
		t.Errorf("ascii band still emits an SGR escape; got %q", line)
	}
	if out := stripANSI(m.View().Content); !strings.Contains(out, "VAULT LOCKED") {
		t.Errorf("ascii board does not show the ascii-safe vault band:\n%s", out)
	}
}
