package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// pubRun builds a publication-phase forge park through the wire decoder, so the retry-key
// presence flag is set exactly as a real server response would set it.
func pubRun(t *testing.T, extra string) apitypes.RunDTO {
	t.Helper()
	var r apitypes.RunDTO
	wire := `{"id":"r1","kind":"issue","status":"recovery_wait","recovery_wait_cause":"forge_unreachable","recovery_wait_phase":"publication","forge_park_count":7,"forge_park_max":9` + extra + `}`
	if err := json.Unmarshal([]byte(wire), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func withForge(r apitypes.RunDTO, forge string) apitypes.RunDTO {
	r.ForgeType = forge
	return r
}

func withRunMR(r apitypes.RunDTO, iid int64) apitypes.RunDTO {
	r.MrIID = &iid
	return r
}

func TestPublicationParkLineTimedWait(t *testing.T) {
	now := time.Date(2030, 1, 2, 12, 0, 0, 0, time.Local)
	start := now.Add(-12 * time.Minute)
	retry := time.Date(2030, 1, 2, 15, 4, 0, 0, time.Local)
	mk := func(limit *int, intent *apitypes.PublicationIntentDTO) apitypes.RunDTO {
		r := pubRun(t, "")
		r.PublicationWaitStartedAt = &start
		r.PublicationWaitLimitSeconds = limit
		r.RecoveryRetryNotBefore = &retry
		r.PublicationIntent = intent
		return r
	}
	day := 24 * 3600
	mr := int64(42)
	for _, tc := range []struct {
		name string
		run  apitypes.RunDTO
		want string
	}{
		{"limit", mk(&day, nil), "publishing paused: forge unreachable; waiting 12m of 24h, retry at 15:04"},
		{"no limit", mk(nil, nil), "publishing paused: forge unreachable; waiting 12m, no limit, retry at 15:04"},
		{"mr known", mk(&day, &apitypes.PublicationIntentDTO{Branch: "agent/issue-1", MrIID: &mr}), "retry at 15:04 · MR !42"},
		{"mr known github", withForge(mk(&day, &apitypes.PublicationIntentDTO{Branch: "agent/issue-1", MrIID: &mr}), "github"), "retry at 15:04 · PR #42"},
		{"mr known forgejo", withForge(mk(&day, &apitypes.PublicationIntentDTO{Branch: "agent/issue-1", MrIID: &mr}), "forgejo"), "retry at 15:04 · PR #42"},
		{"run mr github", withForge(withRunMR(mk(&day, nil), mr), "github"), "retry at 15:04 · PR #42"},
		{"run mr gitlab", withRunMR(mk(&day, nil), mr), "retry at 15:04 · MR !42"},
		{"branch only", mk(&day, &apitypes.PublicationIntentDTO{Branch: "agent/issue-1"}), "retry at 15:04 · branch agent/issue-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := publicationParkLineAt(tc.run, now)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("got %q want substring %q", got, tc.want)
			}
			if strings.Contains(got, "7 of") || strings.Contains(got, "of 9") || strings.Contains(got, "unlimited") {
				t.Fatalf("publication park shows the forge park count: %q", got)
			}
		})
	}
	// Routed through the shared forge sentence the CLI and TUI surfaces call.
	r := mk(&day, nil)
	r.PublicationWaitStartedAt = ptrTime(time.Now().Add(-12 * time.Minute))
	if got := forgeParkLine(r); !strings.HasPrefix(got, "publishing paused: forge unreachable; waiting 12m of 24h") {
		t.Fatalf("forgeParkLine did not route a publication park: %q", got)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestPublicationHoldReasons(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   string
	}{
		{"publication_retry_exhausted", "the forge stayed unreachable for the whole 24h wait"},
		{"publication_custodian_unavailable", "the worker holding the unpublished source is gone; the work is local-only"},
		{"publication_model_required", "publishing needs model work a publication-only resume does not do"},
		{"publication_source_unavailable", "the unpublished work is not available"},
		{"publication_intent_mismatch", "the publication record did not match"},
		{"publication_from_the_future", `unrecognised hold reason "publication_from_the_future"`},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			r := pubRun(t, `,"recovery_retry_not_before":null,"publication_hold_reason":"`+tc.reason+`","publication_wait_limit_seconds":86400,"publication_intent":{"branch":"agent/issue-1","mr_iid":7,"original_head":"abc","candidate":null,"state":"x","enrollment_seq":1}`)
			if !isForgeOwnerHold(r) {
				t.Fatal("not an owner hold")
			}
			head := forgeParkLine(r)
			if head != "publishing held: "+tc.want+" · MR !7" {
				t.Fatalf("headline %q", head)
			}
			gh := r
			gh.ForgeType = "github"
			if head := forgeParkLine(gh); head != "publishing held: "+tc.want+" · PR #7" {
				t.Fatalf("github headline %q", head)
			}
			all := strings.Join(forgeRunLines(r), "\n")
			requireWorkerCopy(t, all, "uzi run resume r1", "starts a new publication-only attempt; no model work", "uzi run cancel r1")
			for _, banned := range []string{"7 of", "of 9", "Export", "export", "Counters", "will resume automatically"} {
				if strings.Contains(all, banned) {
					t.Fatalf("publication hold carries %q: %s", banned, all)
				}
			}
			row := apitypes.RunListItemDTO{RunDTO: r}
			if runBandOf(row) != bandNeedsYou || runStateWord(row) != publicationHeldWord || runStatusCell(row) == "" {
				t.Fatal("board classification wrong")
			}
		})
	}
	// An omitted retry key with a hold reason is still a hold; no reason at all is said honestly.
	r := pubRun(t, `,"publication_hold_reason":"publication_model_required"`)
	if !isForgeOwnerHold(r) {
		t.Fatal("hold reason without a retry key must still be a hold")
	}
	r = pubRun(t, `,"recovery_retry_not_before":null`)
	if got := forgeParkLine(r); !strings.Contains(got, "reason not reported") {
		t.Fatalf("missing reason: %q", got)
	}
}

func TestPublicationUnknownPhaseIsGenericNotPreClone(t *testing.T) {
	var r apitypes.RunDTO
	if err := json.Unmarshal([]byte(`{"id":"r1","status":"recovery_wait","recovery_wait_cause":"forge_unreachable","recovery_wait_phase":"future_phase","forge_park_count":2,"forge_park_max":5,"recovery_retry_not_before":null}`), &r); err != nil {
		t.Fatal(err)
	}
	got := forgeParkLine(r)
	if !strings.Contains(got, `recovery phase "future_phase" not recognised`) || strings.Contains(got, "2 of 5") || strings.Contains(got, "exhausted") {
		t.Fatalf("unknown phase rendered as pre-clone: %q", got)
	}
	all := strings.Join(forgeRunLines(r), "\n")
	if strings.Contains(all, "Counters, cooldown") || !strings.Contains(all, "uzi run resume r1") {
		t.Fatalf("unknown phase hold lines wrong: %s", all)
	}
}

func TestPublicationPreCloneParkUnchanged(t *testing.T) {
	// A null/empty phase is the pre-clone park, byte for byte.
	for _, phase := range []string{``, `,"recovery_wait_phase":null`, `,"recovery_wait_phase":""`} {
		r := forgeTimingRun(t, phase+`,"recovery_retry_not_before":"2099-01-02T03:04:05Z"`)
		want := "waiting for the forge, retry at " + r.RecoveryRetryNotBefore.Local().Format("15:04") + " (3 of 3)"
		if got := forgeParkLine(r); got != want {
			t.Fatalf("pre-clone changed: %q want %q", got, want)
		}
	}
	held := forgeTimingRun(t, `,"recovery_retry_not_before":null`)
	if got := forgeParkLine(held); got != "forge automatic retries exhausted; owner-held; no automatic resume (3 of 3); owner Resume or Cancel" {
		t.Fatalf("pre-clone hold changed: %q", got)
	}
	if n := len(forgeHoldLines(held)); n != 10 {
		t.Fatalf("pre-clone hold lines changed: %d", n)
	}
}

func TestPublicationParkSurfacesAndHostileBranch(t *testing.T) {
	const hostile = "\x1b[2J\x1b]0;bad\x07\u202e\nFORGED"
	r := pubRun(t, `,"recovery_retry_not_before":null,"publication_hold_reason":"publication_source_unavailable"`)
	r.PublicationIntent = &apitypes.PublicationIntentDTO{Branch: "agent/x" + hostile}
	row := apitypes.RunListItemDTO{RunDTO: r}
	m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
	m = applyDetail(m, r, nil)
	m = step(m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = step(m, tea.WindowSizeMsg{Width: 120, Height: 50})
	for name, out := range map[string]string{
		"get":    renderDetailString(t, r),
		"list":   runStatusCell(row),
		"detail": m.View().Content,
		"board":  m.boardSecondLine(row),
	} {
		assertNoRawControls(t, name, out)
	}
	requireWorkerCopy(t, stripANSI(m.View().Content), "publishing held", "starts a new publication-only attempt")
	requireWorkerCopy(t, renderDetailString(t, r), "publishing held", "branch agent/x")
	if strings.Contains(renderDetailString(t, r), "\nFORGED") {
		t.Fatal("injected row")
	}
	if runStateWord(row) != publicationHeldWord || m.pal.runStateToken(r, false).word != publicationHeldWord {
		t.Fatal("missing textual owner attention")
	}
}
