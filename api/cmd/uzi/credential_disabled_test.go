package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1732 D12: the CLI's read-only parity for disabled credentials. Enabling and disabling
// stay web-only, so every surface here reads the server's state and points at Settings.

func disabledSecretFixture() []apitypes.SecretDTO {
	since := time.Date(2026, 9, 20, 23, 30, 0, 0, time.UTC)
	created := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	return []apitypes.SecretDTO{
		{ID: "s-on", Kind: kindAnthropicToken, Label: "work", IsDefault: true, Enabled: true, AutoEligible: true, CreatedAt: created},
		{ID: "s-off", Kind: kindAnthropicToken, Label: "old-key", Enabled: false, DisabledAt: &since, AutoEligible: true, CreatedAt: created},
		{ID: "c-off", Kind: kindCodexAuth, Label: "spare", Enabled: false, DisabledAt: &since, CodexStatus: "linked", CreatedAt: created},
	}
}

// tokenRow returns the table row of `uzi token list` output whose cells include label.
func tokenRow(t *testing.T, out, label string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for _, f := range fields {
			if f == label {
				return fields
			}
		}
	}
	t.Fatalf("no row for %q in:\n%s", label, out)
	return nil
}

// TestTokenListStateColumn: the table carries a trailing STATE column reading "enabled" or "disabled
// since <date>", and a disabled pooled token's ELIGIBLE reads "-" (it is never picked while
// disabled, and the meters read omits it) rather than the "?" of an unknown reading.
func TestTokenListStateColumn(t *testing.T) {
	fc := &uzicli.FakeClient{
		Secrets:    disabledSecretFixture(),
		SelfMeters: []apitypes.TokenRateLimitDTO{{SecretID: "s-on", Label: "work", AutoStatus: "eligible"}},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "token", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	header := strings.Fields(strings.SplitN(out, "\n", 2)[0])
	want := []string{"ID", "KIND", "LABEL", "DEFAULT", "POOL", "ELIGIBLE", "STATUS", "CREATED", "STATE"}
	if strings.Join(header, " ") != strings.Join(want, " ") {
		t.Fatalf("header = %v, want %v", header, want)
	}
	if row := tokenRow(t, out, "work"); len(row) != 9 || row[8] != "enabled" || row[5] != "eligible" {
		t.Errorf("enabled row = %v, want STATE enabled and ELIGIBLE eligible", row)
	}
	// "disabled since 2026-09-20" splits into three fields: STATE spans row[8:].
	off := tokenRow(t, out, "old-key")
	if len(off) != 11 {
		t.Fatalf("disabled row = %v, want 11 fields", off)
	}
	if strings.Join(off[8:], " ") != "disabled since 2026-09-20" {
		t.Errorf("disabled row STATE = %v, want \"disabled since 2026-09-20\"", off[8:])
	}
	if off[4] != "true" || off[5] != "-" {
		t.Errorf("disabled pooled row POOL/ELIGIBLE = %q/%q, want true/-", off[4], off[5])
	}
	if cx := tokenRow(t, out, "spare"); len(cx) != 11 || strings.Join(cx[8:], " ") != "disabled since 2026-09-20" {
		t.Errorf("disabled Codex row STATE = %v", cx[8:])
	}
}

// TestTokenListStateOlderServer: a server that predates PRD #1732 sends neither `enabled` nor
// `disabled_at`. The missing boolean decodes false, so keying STATE on it would read every
// credential as disabled; disabled_at absent must read as enabled.
func TestTokenListStateOlderServer(t *testing.T) {
	var secrets []apitypes.SecretDTO
	if err := json.Unmarshal([]byte(`[{"id":"s1","kind":"anthropic_token","label":"legacy","is_default":true,"auto_eligible":false,"created_at":"2026-08-01T00:00:00Z","updated_at":"2026-08-01T00:00:00Z"}]`), &secrets); err != nil {
		t.Fatal(err)
	}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{Secrets: secrets}), "token", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if row := tokenRow(t, out, "legacy"); len(row) != 9 || row[8] != "enabled" {
		t.Errorf("legacy row = %v, want a trailing STATE of enabled:\n%s", row, out)
	}
}

// TestTokenListJSONCarriesEnablement: `--json` carries `enabled` and `disabled_at` on every
// element (null disabled_at for an enabled credential), beside the existing keys.
func TestTokenListJSONCarriesEnablement(t *testing.T) {
	fc := &uzicli.FakeClient{Secrets: disabledSecretFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "token", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	byLabel := map[string]map[string]any{}
	for _, it := range items {
		for _, k := range []string{"enabled", "disabled_at"} {
			if _, ok := it[k]; !ok {
				t.Errorf("item %v lacks %q", it["label"], k)
			}
		}
		byLabel[it["label"].(string)] = it
	}
	if on := byLabel["work"]; on["enabled"] != true || on["disabled_at"] != nil {
		t.Errorf("enabled item = enabled:%v disabled_at:%v, want true/null", on["enabled"], on["disabled_at"])
	}
	if off := byLabel["old-key"]; off["enabled"] != false || off["disabled_at"] != "2026-09-20T23:30:00Z" {
		t.Errorf("disabled item = enabled:%v disabled_at:%v, want false/2026-09-20T23:30:00Z", off["enabled"], off["disabled_at"])
	}
}

// TestTokenPoolOnRefusesDisabled: opting a disabled token INTO the pool is refused before any
// write, exit 5 (conflict), naming Settings; opting it OUT still reaches the server.
func TestTokenPoolOnRefusesDisabled(t *testing.T) {
	fc := &uzicli.FakeClient{Secrets: disabledSecretFixture()}
	_, stderr, code := runCLI(t, fakeEnv(fc), "token", "pool", "old-key", "--on")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitConflict)
	}
	if !strings.Contains(stderr, "disabled") || !strings.Contains(stderr, "Settings") {
		t.Errorf("refusal must say the token is disabled and name Settings, stderr = %q", stderr)
	}
	if fc.LastPoolSecretID != "" {
		t.Errorf("a refused opt-in must not reach the server, got a write for %q", fc.LastPoolSecretID)
	}

	fc = &uzicli.FakeClient{Secrets: disabledSecretFixture(), PoolSecret: apitypes.SecretDTO{Label: "old-key"}}
	if _, _, code := runCLI(t, fakeEnv(fc), "token", "pool", "old-key", "--off"); code != uzicli.ExitOK {
		t.Fatalf("--off exit = %d, want 0", code)
	}
	if fc.LastPoolSecretID != "s-off" || fc.LastPoolValue {
		t.Errorf("--off must reach the server for s-off=false, got %q=%v", fc.LastPoolSecretID, fc.LastPoolValue)
	}
}

// TestHoldRowCredentialDisabled pins the HOLD row for a credential_disabled hold: the label, the
// park clock, the milestones, then the next step. Settings is always named (enabling is web-only,
// D12); the set-token switch rides only lanes that accept a per-run override (D16).
func TestHoldRowCredentialDisabled(t *testing.T) {
	now := time.Date(2026, 9, 12, 15, 12, 0, 0, time.UTC)
	parkedAt := now.Add(-70 * time.Minute)

	issue := heldRun("r1", holdCredentialDisabled, parkedAt)
	want := "credential disabled · parked 14:02 (1h10m) · 4/7 milestones · enable it in Settings, or switch token: uzi run set-token r1 <label>"
	if row := holdRow(issue, now); row == nil || row[1] != want {
		t.Errorf("holdRow(issue) = %v, want [HOLD %q]", row, want)
	}

	codex := heldRun("r1", holdCredentialDisabled, parkedAt)
	codex.Harness = "codex"
	chat := heldRun("r1", holdCredentialDisabled, parkedAt)
	chat.Kind = "chat"
	judge := heldRun("r1", holdCredentialDisabled, parkedAt)
	judge.Kind = "judge"
	selfImprove := heldRun("r1", holdCredentialDisabled, parkedAt)
	selfImprove.Kind = "self_improve"
	review := heldRun("r1", holdCredentialDisabled, parkedAt)
	review.TriggerSource = "task_review"
	for name, r := range map[string]apitypes.RunDTO{"codex": codex, "chat": chat, "judge": judge, "self_improve": selfImprove, "task_review": review} {
		row := holdRow(r, now)
		if row == nil || !strings.HasSuffix(row[1], "· enable it in Settings") || strings.Contains(row[1], "set-token") {
			t.Errorf("holdRow(%s) = %v, want Settings only (no per-run switch on this lane)", name, row)
		}
	}
}

// TestRunGetAndListCredentialDisabled: `uzi run get` prints the HOLD row, and `uzi run list`
// says why the paused run is paused, so it does not read as an owner pause.
func TestRunGetAndListCredentialDisabled(t *testing.T) {
	run := heldRun("r1", holdCredentialDisabled, time.Now().Add(-5*time.Minute))
	fc := &uzicli.FakeClient{
		RunByID: map[string]apitypes.RunDTO{"r1": run},
		Runs:    []apitypes.RunListItemDTO{{RunDTO: run}},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("run get exit = %d, want 0", code)
	}
	if !strings.Contains(out, "HOLD") || !strings.Contains(out, "credential disabled") || !strings.Contains(out, "enable it in Settings") {
		t.Errorf("run get lacks the credential_disabled HOLD row:\n%s", out)
	}
	list, _, code := runCLI(t, fakeEnv(fc), "run", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("run list exit = %d, want 0", code)
	}
	if !strings.Contains(list, "paused (credential disabled)") {
		t.Errorf("run list STATUS lacks the credential_disabled qualifier:\n%s", list)
	}
}

// TestTUICredentialDisabledHold drives the real model: the detail draws the hold line instead
// of "paused by you", and the board bands the run into NEEDS YOU with the ⊘ token, the selected
// row's next-step line and a ⊘ count in the summary cluster.
func TestTUICredentialDisabledHold(t *testing.T) {
	run := heldRun("99999999-cred", holdCredentialDisabled, time.Now().Add(-5*time.Minute))
	run.Health = "ok"

	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m = applyDetail(m, run, nil)
	raw := m.View().Content
	assertNoRawControls(t, "credential_disabled detail", raw)
	out := stripANSI(raw)
	if !strings.Contains(out, "waiting: credential disabled · enable it in Settings") {
		t.Errorf("detail lacks the credential_disabled line:\n%s", out)
	}
	if strings.Contains(out, "paused by you") {
		t.Errorf("a credential_disabled hold must not read as an owner pause:\n%s", out)
	}
	if !strings.Contains(out, credDisabledGlyph+" "+credDisabledWord) {
		t.Errorf("detail header lacks the %s %s token:\n%s", credDisabledGlyph, credDisabledWord, out)
	}

	item := apitypes.RunListItemDTO{RunDTO: run}
	if b := runBandOf(item); b != bandNeedsYou {
		t.Errorf("runBandOf(credential_disabled) = %d, want bandNeedsYou (%d)", b, bandNeedsYou)
	}
	if w := runStateWord(item); w != credDisabledWord {
		t.Errorf("runStateWord = %q, want %q", w, credDisabledWord)
	}
	board := tuiTestModel(t, &uzicli.FakeClient{}, "")
	board = step(board, boardRunsMsg{reqID: board.board.waitID, runs: []apitypes.RunListItemDTO{item}})
	bout := stripANSI(board.View().Content)
	for _, want := range []string{"NEEDS YOU", credDisabledWord, "▸ waiting: credential disabled", credDisabledGlyph + " 1"} {
		if !strings.Contains(bout, want) {
			t.Errorf("board lacks %q:\n%s", want, bout)
		}
	}

	// An owner pause (no hold reason) keeps its own vocabulary.
	paused := heldRun("r2", "", time.Now())
	if isCredentialDisabledHold(paused) || credentialDisabledLine(paused) != "" {
		t.Errorf("an owner pause must not read as a credential_disabled hold")
	}
}

// TestScheduleGetCredentialDisabledSkip: `uzi schedule get` renders the credential_disabled
// skip with its human label and the Settings hint.
func TestScheduleGetCredentialDisabledSkip(t *testing.T) {
	fired := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"sch_cd": {ID: "sch_cd", Target: "issue", IssueIID: ptrInt64(7), Timing: "once", Status: "active", Enabled: true,
			LastFire: &apitypes.LastFire{FiredAt: fired, Matched: 1,
				Skips: []apitypes.LastFireSkip{{IssueIID: ptrInt64(7), Reason: "credential_disabled"}}}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_cd")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"#7  pinned credential is disabled", "# enable the credential in Settings, or change the schedule's token"} {
		if !strings.Contains(out, want) {
			t.Errorf("schedule get lacks %q:\n%s", want, out)
		}
	}
}

// TestSelfRateLimitsEmptyMeansNoEnabled: the owner endpoints omit disabled credentials, so an
// empty answer says "no enabled" rather than printing a bare header that reads as "no tokens".
func TestSelfRateLimitsEmptyMeansNoEnabled(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "rate-limits")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "no enabled Anthropic token") || !strings.Contains(out, "Settings") {
		t.Errorf("empty claude meters = %q, want the no-enabled-token note", out)
	}
	out, _, code = runCLI(t, fakeEnv(&uzicli.FakeClient{}), "rate-limits", "--provider", "codex")
	if code != uzicli.ExitOK {
		t.Fatalf("codex exit = %d, want 0", code)
	}
	if !strings.Contains(out, "enabled login") || !strings.Contains(out, "Settings") {
		t.Errorf("empty codex meters = %q, want the enabled-login note", out)
	}
	// --json stays a bare array for scripts.
	out, _, _ = runCLI(t, fakeEnv(&uzicli.FakeClient{}), "rate-limits", "--json")
	if strings.TrimSpace(out) != "[]" && strings.TrimSpace(out) != "null" {
		t.Errorf("--json on no meters = %q, want an empty JSON value", out)
	}
}

// TestRunWaitCredentialDisabledLine: `run wait` names a credential_disabled hold once, points
// at Settings, and keeps waiting through it (the run resumes by itself on Enable).
func TestRunWaitCredentialDisabledLine(t *testing.T) {
	reason := holdCredentialDisabled
	held := apitypes.RunDTO{ID: "r1", Kind: "issue", Status: statusPaused, HoldReason: &reason}
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("running"), waitStep{run: held}, okStep("completed"))}

	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "held, a credential it needs is disabled; enable it in Settings") {
		t.Errorf("expected the credential_disabled line, stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "paused → completed") {
		t.Errorf("wait must keep going through the hold, stderr = %q", stderr)
	}
}

// followLogs runs `uzi run logs r1 --follow` over a scripted GetRun sequence and returns its
// stderr, failing the test if it does not exit 0 on the sequence's terminal run.
func followLogs(t *testing.T, seq ...apitypes.RunDTO) string {
	t.Helper()
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")
	old := logsPollInterval
	logsPollInterval = time.Millisecond
	t.Cleanup(func() { logsPollInterval = old })
	pf := &codexParkFake{
		FakeClient: &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
			"r1": {{Seq: 1, Kind: "assistant", Payload: []byte(`{"text":"hi"}`)}},
		}},
		seq: seq,
	}
	var out, errBuf bytes.Buffer
	env := fakeEnv(pf)
	env.Stdout, env.Stderr = &out, &errBuf
	done := make(chan int, 1)
	go func() { done <- Main(env, []string{"run", "logs", "r1", "--follow"}) }()
	select {
	case code := <-done:
		if code != uzicli.ExitOK {
			t.Fatalf("--follow exit = %d, want 0 (stderr: %s)", code, errBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run logs --follow hung")
	}
	if strings.Contains(out.String(), "held") || strings.Contains(out.String(), "resumed") {
		t.Errorf("a hold notice reached STDOUT:\n%s", out.String())
	}
	return errBuf.String()
}

func pausedWithHold(hold string) apitypes.RunDTO {
	r := apitypes.RunDTO{ID: "r1", Kind: "issue", Status: statusPaused}
	if hold != "" {
		r.HoldReason = &hold
	}
	return r
}

// TestRunLogsFollowCredentialDisabledNotice: `run logs --follow` prints the credential_disabled
// notice once while the hold lasts, then "resumed" when the run is promoted to queued.
func TestRunLogsFollowCredentialDisabledNotice(t *testing.T) {
	held := pausedWithHold(holdCredentialDisabled)
	stderr := followLogs(t,
		apitypes.RunDTO{ID: "r1", Status: "running"},
		held, held, held,
		apitypes.RunDTO{ID: "r1", Status: "queued"},
		apitypes.RunDTO{ID: "r1", Status: "completed"},
	)
	want := "run r1 held — a credential it needs is disabled; enable it in Settings, or switch token: uzi run set-token r1 <label>; still following"
	if n := strings.Count(stderr, want); n != 1 {
		t.Errorf("credential_disabled notice appeared %d times, want exactly 1:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "run r1 resumed (queued)") {
		t.Errorf("missing the resume line:\n%s", stderr)
	}
}

// TestRunLogsFollowCredentialDisabledIntoPause: a run the promoter settles from the
// credential_disabled hold straight into an owner pause or budget_exhausted did not resume;
// --follow names the new pause instead, and prints "resumed" only once the run leaves paused.
// Reddening mutation: drop the paused branch in run_get.go (it prints "resumed (paused)").
func TestRunLogsFollowCredentialDisabledIntoPause(t *testing.T) {
	for _, tc := range []struct {
		hold, want string
	}{
		{"", "run r1 paused by its owner; resume with uzi run resume r1; still following"},
		{holdBudgetExhausted, "run r1 parked at its time limit; extend: uzi run extend r1 --by 2h; still following"},
	} {
		t.Run("hold="+tc.hold, func(t *testing.T) {
			held, next := pausedWithHold(holdCredentialDisabled), pausedWithHold(tc.hold)
			stderr := followLogs(t,
				apitypes.RunDTO{ID: "r1", Status: "running"},
				held, held, next, next,
				apitypes.RunDTO{ID: "r1", Status: "queued"},
				apitypes.RunDTO{ID: "r1", Status: "completed"},
			)
			if strings.Contains(stderr, "resumed (paused)") {
				t.Errorf("a move into another pause was reported as a resume:\n%s", stderr)
			}
			if n := strings.Count(stderr, tc.want); n != 1 {
				t.Errorf("new-pause notice %q appeared %d times, want 1:\n%s", tc.want, n, stderr)
			}
			if n := strings.Count(stderr, "run r1 resumed (queued)"); n != 1 {
				t.Errorf("resume line appeared %d times, want 1:\n%s", n, stderr)
			}
		})
	}
}

// TestRunWaitCredentialDisabledIntoPause: `run wait` names a hold-to-hold move that keeps the
// status at paused (credential_disabled settled into an owner pause), which the status
// transition line alone cannot show. Reddening mutation: drop the hold-change branch in
// run_wait.go.
func TestRunWaitCredentialDisabledIntoPause(t *testing.T) {
	held, owner := pausedWithHold(holdCredentialDisabled), pausedWithHold("")
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("running"), waitStep{run: held}, waitStep{run: owner}, okStep("completed"))}

	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if n := strings.Count(stderr, "run r1: now paused by its owner; resume with uzi run resume r1"); n != 1 {
		t.Errorf("hold-change line appeared %d times, want 1:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "paused → completed") {
		t.Errorf("wait must keep going through the pause, stderr = %q", stderr)
	}
}
