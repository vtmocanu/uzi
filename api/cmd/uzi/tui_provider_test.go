package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestProviderTUIBoardSummary(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
	hold, retry := providerHoldFixture(), providerRetryFixture()
	hold.Health, retry.Health = "slow", "slow"
	queued := hold
	queued.Status, queued.Health = "queued", ""
	credential := hold
	reason := "credential_disabled"
	credential.HoldReason, credential.Health = &reason, ""
	m.board.runs = []apitypes.RunListItemDTO{
		{RunDTO: hold}, {RunDTO: retry}, {RunDTO: queued}, {RunDTO: credential},
	}
	out := stripANSI(m.boardSummary())
	if !strings.Contains(out, "⊘ 2") || strings.Contains(out, "▲") {
		t.Fatalf("summary must count provider/credential holds without stale stalls: %q", out)
	}
}

func TestProviderTUIBoardAttention(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "r1")
	for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii} {
		m.profile = profile
		for _, held := range []bool{false, true} {
			r := providerRetryFixture()
			if held {
				r = providerHoldFixture()
			}
			r.Health = "slow"
			r.CurrentActivity = &apitypes.RunActivity{Agent: "stale", Detail: "obsolete activity", At: time.Now()}
			row := apitypes.RunListItemDTO{RunDTO: r}
			word, band := "provider wait", bandFloor
			if held {
				word, band = "provider hold", bandNeedsYou
			}
			tok := m.pal.runStateToken(r, false)
			if runBandOf(row) != band || runStateWord(row) != word || tok.word != word {
				t.Fatalf("held %v: band/word/token = %d/%s/%s", held, runBandOf(row), runStateWord(row), tok.word)
			}
			if held && tok.color != m.pal.amber {
				t.Fatal("provider owner hold must use amber attention ink")
			}
			if !m.boardShowSecondLine(row) {
				t.Fatal("typed provider row needs a second line")
			}
			for _, width := range []int{12, 40, 100} {
				m.width = width
				out := stripANSI(m.boardSecondLine(row))
				if strings.Contains(out, "obsolete") || strings.ContainsAny(out, "\r\n") || visualWidth(out) > width {
					t.Fatalf("width %d unsafe/stale row: %q", width, out)
				}
			}
			m.width = 100
			m.blinkOn = false
			first := m.boardSecondLine(row)
			m.blinkOn = true
			if first != m.boardSecondLine(row) {
				t.Fatal("provider attention line pulses")
			}
		}
	}
}

func TestProviderTUIDetailCopy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*apitypes.RunDTO)
		want   string
		scope  bool
	}{
		{"eligible", func(r *apitypes.RunDTO) {}, "Stop: uzi run scope r1 --through 4", true},
		{"server refused", func(r *apitypes.RunDTO) { r.CanStopAtProviderHold = false }, "server does not permit Stop", false},
		{"no completed", func(r *apitypes.RunDTO) { r.CanStopAtProviderHold = false; r.MilestonesCompleted = nil }, "no completed milestone", false},
		{"non milestone", func(r *apitypes.RunDTO) { r.CanStopAtProviderHold = false; r.Milestones = nil }, "requires a milestone issue run", false},
		{"non issue", func(r *apitypes.RunDTO) { r.Kind = "task"; r.CanStopAtProviderHold = false }, "requires a milestone issue run", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := providerHoldFixture()
			tc.change(&r)
			m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
			m = applyDetail(m, r, nil)
			m.width = 160
			out := stripANSI(m.renderDetail())
			requireWorkerCopy(t, out, "provider unavailable: waiting for owner", "no automatic retry deadline",
				"claim and worker affinity retained", "clone and session files stay on worker",
				"recovery depends on the worker or a verified recovery copy", "owner Resume: uzi run resume r1",
				"Cancel: uzi run cancel r1", tc.want)
			if tc.scope {
				requireWorkerCopy(t, out, "then, only if scope succeeds, Resume: uzi run resume r1")
			}
			if strings.Contains(out, "uzi run scope") != tc.scope || strings.Contains(out, "paused by you") {
				t.Fatalf("wrong hold copy: %s", out)
			}
			if r.Kind != "issue" && strings.Contains(out, "issue lock") {
				t.Fatalf("non issue claims issue lock: %s", out)
			}
		})
	}
}

func TestProviderTUIRetryMetadataAndNegatives(t *testing.T) {
	r := providerRetryFixture()
	m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
	m = applyDetail(m, r, nil)
	m.width = 100
	out := stripANSI(m.renderDetail())
	requireWorkerCopy(t, out, "provider outage", "counted parks: 3",
		"streak started: 2026-10-10T12:00:00Z", "next retry: 2026-10-10T12:01:00Z")
	r.ProviderParkStreakStartedAt, r.RecoveryRetryNotBefore = nil, nil
	m = applyDetail(m, r, nil)
	requireWorkerCopy(t, stripANSI(m.renderDetail()), "streak started: unknown", "next retry: unknown")
	for _, cause := range []string{"", "provider_outage_future", "empty_turn"} {
		r.RecoveryWaitCause = &cause
		m = applyDetail(m, r, nil)
		if len(m.providerDetailLines()) != 0 || runStateWord(apitypes.RunListItemDTO{RunDTO: r}) != "recovery wait" {
			t.Fatalf("unknown cause got provider copy: %q", cause)
		}
	}
	for _, status := range []string{"queued", "claimed", "running", "completed"} {
		r = providerHoldFixture()
		r.Status = status
		m = applyDetail(m, r, nil)
		if len(m.providerDetailLines()) != 0 || strings.Contains(runStateWord(apitypes.RunListItemDTO{RunDTO: r}), "provider") {
			t.Fatalf("retained marker treated as hold: %s", status)
		}
	}
}

func TestProviderTUIDetailSafetyAndFrame(t *testing.T) {
	for _, id := range []struct {
		name, value string
	}{
		{"hostile", "r1\x1b[31m\r\n'$(inert)"},
		{"UUID", "aaaaaaaa-1111-2222-3333-444444444444"},
	} {
		for _, held := range []bool{false, true} {
			for _, variant := range []string{"plain", "stale deadline", "pending pause"} {
				for _, width := range []int{40, 100, 160} {
					for _, height := range []int{24, 70} {
						t.Run(fmt.Sprintf("%s/hold=%v/%s/width=%d/height=%d", id.name, held, variant, width, height), func(t *testing.T) {
							r := providerRetryFixture()
							if held {
								r = providerHoldFixture()
							}
							switch variant {
							case "stale deadline":
								deadline := time.Now().Add(time.Hour)
								r.Health, r.DeadlineAt = "slow", &deadline
							case "pending pause":
								requested := time.Now().Add(-time.Minute)
								r.PauseRequestedAt = &requested
							}
							r.ID = id.value
							r.IssueTitle = "title\x1b[31m\r\nfixture"
							m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
							m = applyDetail(m, r, []apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "one line", time.Now())})
							m = step(m, tea.WindowSizeMsg{Width: width, Height: height})
							for _, row := range m.providerDetailLines() {
								plain := stripANSI(row)
								if strings.ContainsAny(plain, "\r\n\x1b") || visualWidth(plain) > width {
									t.Fatalf("unsafe row: %q", row)
								}
							}
							out := stripANSI(m.View().Content)
							if rows := len(strings.Split(out, "\n")); rows != height {
								t.Fatalf("frame has %d rows, want %d", rows, height)
							}
							if held {
								// Ignore physical wrapping, including breaks inside a UUID.
								copy := strings.Join(strings.Fields(out), "")
								commandID := providerCommandID(r.ID)
								previous := -1
								for _, want := range []string{"provider unavailable: waiting for owner",
									"no automatic retry deadline", "issue lock, claim and worker affinity retained",
									"clone and session files stay on worker",
									"recovery depends on the worker or a verified recovery copy",
									"owner Resume: uzi run resume " + commandID,
									"Cancel: uzi run cancel " + commandID,
									"Stop: uzi run scope " + commandID + " --through 4",
									"then, only if scope succeeds, Resume: uzi run resume " + commandID} {
									at := strings.Index(copy, strings.Join(strings.Fields(want), ""))
									if at < 0 || at <= previous {
										t.Fatalf("missing or out-of-order policy clause %q in:\n%s", want, out)
									}
									previous = at
								}
							}
							if strings.Contains(out, "near timeout") || strings.Contains(out, "stops at") {
								t.Fatal("provider frame drew stale deadline")
							}
							if held && strings.Contains(out, "paused by you") {
								t.Fatal("provider hold claimed owner pause")
							}
							if pending := strings.Contains(out, "pause requested"); pending != (variant == "pending pause" && !held) {
								t.Fatalf("pending pause presence = %v", pending)
							}
						})
					}
				}
			}
		}
	}
}

func TestProviderTUIStaleDeadline(t *testing.T) {
	r := providerHoldFixture()
	r.Health = "slow"
	deadline := time.Now().Add(time.Hour)
	r.DeadlineAt = &deadline
	m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
	m = applyDetail(m, r, nil)
	if out := stripANSI(m.renderDetail()); strings.Contains(out, "near timeout") || strings.Contains(out, "stops at") {
		t.Fatalf("held run promises scheduled stopping: %s", out)
	}
}
