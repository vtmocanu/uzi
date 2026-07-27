package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestMrAbbrev pins the forge-aware merge/pull-request label (PRD #65 D2), the
// CLI twin of the web's mrAbbrev and slacksvc's forgeMrAbbrev: Forgejo "PR",
// everything else (GitLab, empty, unknown) "MR".
func TestMrAbbrev(t *testing.T) {
	for _, tc := range []struct {
		forge string
		want  string
	}{
		{"forgejo", "PR"},
		{"gitlab", "MR"},
		{"", "MR"},
		{"something_else", "MR"},
	} {
		if got := mrAbbrev(tc.forge); got != tc.want {
			t.Errorf("mrAbbrev(%q) = %q, want %q", tc.forge, got, tc.want)
		}
	}
}

// TestRenderRunDetailForgeAwareMRColumn is the end-to-end proof that `uzi run get`
// labels the request column per the run's forge — a Forgejo run reads "PR", a
// GitLab run "MR", off r.ForgeType (the field the landing merge threads onto
// apitypes.RunDTO). This is the forge-blind "MR" hardcode the CLI had until #65.
func TestRenderRunDetailForgeAwareMRColumn(t *testing.T) {
	iid := int64(42)
	render := func(forge string) string {
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
		r := apitypes.RunDTO{
			ID:         "run-1",
			Kind:       "issue",
			Status:     "completed",
			IssueTitle: "do the thing",
			ForgeType:  forge,
			MrIID:      &iid,
			Health:     "ok",
		}
		if err := renderRunDetail(p, r); err != nil {
			t.Fatalf("renderRunDetail(%q): %v", forge, err)
		}
		return buf.String()
	}

	fj := render("forgejo")
	if !strings.Contains(fj, "PR") {
		t.Errorf("a Forgejo run must render a PR column, got:\n%s", fj)
	}
	// The GitLab label must not leak onto a Forgejo run's detail.
	if strings.Contains(fj, "MR ") || strings.Contains(fj, "MR\t") {
		t.Errorf("a Forgejo run must NOT render an MR column label, got:\n%s", fj)
	}

	gl := render("gitlab")
	if !strings.Contains(gl, "MR") {
		t.Errorf("a GitLab run must render an MR column, got:\n%s", gl)
	}
}

// TestRenderRunDetailAnthropicToken pins `uzi run get`'s ANTHROPIC_TOKEN row
// (PRD #111 M1) and, more importantly, the sanitization it goes through.
//
// The label is the first genuinely USER-AUTHORED string this block renders, and the
// two facts that make it dangerous are both measured, not assumed:
// validateSecretLabel (handler/secrets.go) rejects control characters and U+FFFD but
// NOT unicode.Cf, so a bidi-override label is storable and passes the DB CHECK; and
// uzicli.Printer.Table does not sanitize what it is handed — it joins the cells and
// flushes. cellText is what closes that, and this asserts it rather than the row's
// mere presence, because a row rendered through the wrong helper looks identical
// until someone stores a hostile label.
func TestRenderRunDetailAnthropicToken(t *testing.T) {
	render := func(label *string) string {
		t.Helper()
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
		r := apitypes.RunDTO{
			ID: "run-1", Kind: "issue", Status: "completed",
			IssueTitle: "do the thing", ForgeType: "gitlab", Health: "ok",
			AnthropicSecretLabel: label,
		}
		if err := renderRunDetail(p, r); err != nil {
			t.Fatalf("renderRunDetail: %v", err)
		}
		return buf.String()
	}

	label := "console-key"
	if out := render(&label); !strings.Contains(out, "ANTHROPIC_TOKEN") || !strings.Contains(out, "console-key") {
		t.Errorf("a run with a recorded credential must name it, got:\n%s", out)
	}

	// Absent and empty both render NO row: a run that cannot say which account it
	// billed must not appear to have said something. Empty is unreachable through the
	// API (user_secrets.label is NOT NULL with a 1..64 CHECK) and is pinned anyway,
	// because the guard has to be emptiness-based rather than a nil check.
	if out := render(nil); strings.Contains(out, "ANTHROPIC_TOKEN") {
		t.Errorf("a run with no recorded credential must render no row, got:\n%s", out)
	}
	empty := ""
	if out := render(&empty); strings.Contains(out, "ANTHROPIC_TOKEN") {
		t.Errorf("an empty label must render no row, got:\n%s", out)
	}

	// A bidi override (U+202E RIGHT-TO-LEFT OVERRIDE) plus a CSI escape and a newline:
	// the escape would repaint the terminal, the override would visually reverse the
	// label so it READS as a different account, and the newline would break the table
	// rail. All three must be gone, and the printable text must survive.
	//
	// 🔴 THIS FIXTURE IS DELIBERATELY UN-STORABLE THROUGH THE API, AND THE TEST STILL
	// EARNS ITS KEEP. Since PRD #111 M2, validateSecretLabel (handler/secrets.go)
	// rejects both unicode.IsControl and unicode.Cf, so no label containing these bytes
	// can be written through any handler today. Do NOT "tidy" this into a reachable
	// fixture, and do NOT drop the cellText routing it pins:
	//
	//   - The two live on opposite sides of a trust boundary. The validator is what the
	//     SERVER accepts; this is what the RENDERER does with whatever it is handed.
	//     Depending on the far side of a boundary for local safety is exactly the
	//     coupling that turns one regression into two.
	//   - Three routes reach the renderer without passing that validator: a label
	//     stored before M2 landed (existing rows are not re-validated), a future write
	//     path that forgets the check, and a row written straight to the database.
	//
	// 🔴 THE NEWLINE PROBE IS THE ONLY ONE THAT DISCRIMINATES, AND IT WAS WRONG.
	// It read `"\n\nnext-line"` — a DOUBLE newline neither implementation ever emits —
	// so it was satisfied by construction. The bidi and ESC probes do not discriminate
	// either: sanitizeTTY and cellText both strip unicode.Cf and unicode.IsControl, so
	// two of the three agree under either helper. Measured: swapping cellText for
	// sanitizeTTY left this test GREEN.
	//
	// Newline FOLDING is the whole difference between them (cellText → compactText
	// replaces "\n" with a space; sanitizeTTY deliberately spares "\n"), so a single
	// "\nnext-line" is what tells them apart, and it is what is asserted now.
	//
	// The class this belongs to is worth naming: the broken implementation and the
	// correct one AGREE on the case that was picked, so the fixture passed against
	// both and read as proof of something it never tested.
	hostile := "safe‮dnetsop\x1b[31m\nnext-line"
	out := render(&hostile)
	// The first two pin the shared floor (either helper satisfies them); the third is
	// the discriminating one.
	for _, bad := range []string{"‮", "\x1b", "\nnext-line"} {
		if strings.Contains(out, bad) {
			t.Errorf("hostile label reached the terminal carrying %q, got:\n%q", bad, out)
		}
	}
	if !strings.Contains(out, "safe") || !strings.Contains(out, "next-line") {
		t.Errorf("sanitizing dropped the printable text too, got:\n%q", out)
	}
}

// ---- PRD #35: the usage-limit park -----------------------------------------

// parkedRun is the fixture every test below starts from: a run parked on a five-hour
// window with a promotion stamped an hour out. Built by a helper rather than shared as
// a package var so a test that mutates one field cannot silently change another's
// input.
func parkedRun(now time.Time) apitypes.RunDTO {
	rlt := "five_hour"
	retry := now.Add(time.Hour)
	resets := now.Add(90 * time.Minute)
	return apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: statusLimitWait,
		IssueTitle: "do the thing", ForgeType: "gitlab", Health: "ok",
		WaitOnLimit: true, RateLimitType: &rlt,
		RetryNotBefore: &retry, LimitResetsAt: &resets, LimitWaitCount: 2,
	}
}

// TestLimitWaitLine pins the ONE sentence every CLI surface renders for a park.
func TestLimitWaitLine(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	got := limitWaitLine(parkedRun(now), now)
	for _, want := range []string{"paused", "five_hour", "resumes in 1h00m", "attempt 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("limitWaitLine = %q, want it to contain %q", got, want)
		}
	}

	// 🔴 THE COUNTDOWN IS OFF RetryNotBefore, NOT LimitResetsAt, and this fixture is
	// built so the two disagree (60m vs 90m) precisely so the wrong field cannot pass.
	// They differ in normal operation, not in a corner case: RetryNotBefore carries
	// jitter, is clamped, and is pool-aware — a user with a second credential that
	// still has headroom is promoted long before the window they hit reopens. Reading
	// LimitResetsAt would tell that user to wait half an hour longer than they must.
	if strings.Contains(got, "1h30m") {
		t.Errorf("limitWaitLine = %q — the countdown is being computed from limit_resets_at; it must come from retry_not_before, which is when the server actually promotes the run", got)
	}

	// Not parked ⇒ empty, which is what makes the call safe to make unconditionally at
	// every surface that renders it.
	for _, status := range []string{"running", "queued", "completed", "failed", "cancelled", "awaiting_approval"} {
		r := parkedRun(now)
		r.Status = status
		if line := limitWaitLine(r, now); line != "" {
			t.Errorf("limitWaitLine(status=%q) = %q, want \"\" — only a parked run has a park to describe", status, line)
		}
	}

	// A missing stamp is a real case (an older server, or a park whose stamp failed to
	// record), and it must neither promise a time nor go silent.
	noStamp := parkedRun(now)
	noStamp.RetryNotBefore = nil
	if line := limitWaitLine(noStamp, now); !strings.Contains(line, "no resume time recorded") {
		t.Errorf("limitWaitLine(no retry_not_before) = %q, want it to say the resume time is unknown rather than imply one", line)
	}

	// A stamp already past is the NORMAL steady state for a few seconds — the sweeper
	// runs on a ticker — so it must read as imminent, never as a negative countdown.
	past := parkedRun(now)
	elapsed := now.Add(-30 * time.Second)
	past.RetryNotBefore = &elapsed
	if line := limitWaitLine(past, now); !strings.Contains(line, "resuming shortly") {
		t.Errorf("limitWaitLine(retry_not_before in the past) = %q, want \"resuming shortly\"", line)
	}

	// No count ⇒ no "attempt" clause at all, rather than "attempt 0".
	if line := limitWaitLine(func() apitypes.RunDTO { r := parkedRun(now); r.LimitWaitCount = 0; return r }(), now); strings.Contains(line, "attempt") {
		t.Errorf("limitWaitLine(count=0) = %q, want no attempt clause", line)
	}

	// An UNRECOGNISED type prints as itself. The server allowlists this field, but the
	// CLI ships on its own cadence, so a newer server can send a member this binary has
	// never heard of — and dropping it, or inventing a rendering, would both be worse
	// than passing it through. Same stance as credentialCell's unknown reason.
	novel := parkedRun(now)
	future := "seven_day_opus_max"
	novel.RateLimitType = &future
	if line := limitWaitLine(novel, now); !strings.Contains(line, future) {
		t.Errorf("limitWaitLine(unknown rate_limit_type) = %q, want it to render %q verbatim", line, future)
	}
}

// TestLimitWaitLineSanitizesTheRateLimitType is the Risk 13 half.
//
// rate_limit_type is server-allowlisted today, which is exactly why this test states
// what it is defending: the renderer's safety must not depend on the far side of a
// trust boundary. The same three routes that reach credentialCell without passing a
// validator reach here — a row written before the allowlist existed, a future write
// path that skips it, and a direct database write.
//
// The NEWLINE is the discriminating probe, for the reason the ANTHROPIC_TOKEN test
// records at length: sanitizeTTY and cellText both strip Cf and control characters, so
// the bidi and ESC probes are satisfied by either helper and prove nothing about which
// one is wired. Only cellText folds "\n", and only cellText keeps the table rail intact.
func TestLimitWaitLineSanitizesTheRateLimitType(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	r := parkedRun(now)
	hostile := "five‮ruoh_\x1b[31m\nnext-line"
	r.RateLimitType = &hostile

	got := limitWaitLine(r, now)
	for _, bad := range []string{"‮", "\x1b", "\nnext-line"} {
		if strings.Contains(got, bad) {
			t.Errorf("a hostile rate_limit_type reached the terminal carrying %q, got:\n%q", bad, got)
		}
	}
	if !strings.Contains(got, "five") || !strings.Contains(got, "next-line") {
		t.Errorf("sanitizing dropped the printable text too, got:\n%q", got)
	}
}

// TestFmtUntil pins the TWO-unit rendering, which is where this parts company with
// relAge's single unit next door.
func TestFmtUntil(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m"},
		{45 * time.Minute, "45m"},
		{time.Hour, "1h00m"},
		// The pair that justifies two units at all: at one unit both of these read "3h",
		// an hour of error on the only number the user came for.
		{3*time.Hour + 1*time.Minute, "3h01m"},
		{3*time.Hour + 59*time.Minute, "3h59m"},
		{25 * time.Hour, "1d1h"},
		{7 * 24 * time.Hour, "7d0h"},
	} {
		if got := fmtUntil(tc.d); got != tc.want {
			t.Errorf("fmtUntil(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestRenderRunDetailLimitWait pins `uzi run get`'s park block — and specifically the
// distinction the block exists to make, since the server leaves the same three columns
// populated after a promotion as history.
func TestRenderRunDetailLimitWait(t *testing.T) {
	now := time.Now()
	render := func(r apitypes.RunDTO) string {
		t.Helper()
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
		if err := renderRunDetail(p, r); err != nil {
			t.Fatalf("renderRunDetail: %v", err)
		}
		return buf.String()
	}

	parked := render(parkedRun(now))
	for _, want := range []string{"LIMIT_WAIT", "five_hour", "resumes in", "attempt 2", "LIMIT_RESETS_AT"} {
		if !strings.Contains(parked, want) {
			t.Errorf("a parked run's detail is missing %q, got:\n%s", want, parked)
		}
	}

	// 🔴 THE HISTORY CASE, and the one an implementation that keys the block on the
	// COLUMNS rather than the STATUS gets wrong. limit_resets_at / retry_not_before /
	// rate_limit_type are deliberately left in place across a promotion, so a completed
	// run still carries all three — and rendering "resumes in 4h12m" on a run that
	// finished yesterday is nonsense. Same columns, different question.
	resumed := parkedRun(now)
	resumed.Status = "completed"
	out := render(resumed)
	for _, bad := range []string{"resumes in", "resuming shortly", "LIMIT_RESETS_AT"} {
		if strings.Contains(out, bad) {
			t.Errorf("a run that has already resumed rendered %q — the park columns are history once the status moves on, not a live countdown, got:\n%s", bad, out)
		}
	}
	// The history it SHOULD carry: this run spent part of its wall clock waiting.
	if !strings.Contains(out, "LIMIT_WAITS") || !strings.Contains(out, "resumed") {
		t.Errorf("a run that parked and resumed must still say so, got:\n%s", out)
	}

	// A run that never parked says nothing about parks beyond the opt-in itself.
	never := apitypes.RunDTO{ID: "run-2", Kind: "issue", Status: "completed", Health: "ok"}
	if out := render(never); strings.Contains(out, "LIMIT_WAIT ") || strings.Contains(out, "LIMIT_WAITS") || strings.Contains(out, "LIMIT_RESETS_AT") {
		t.Errorf("a run that never hit a usage limit must render no park rows, got:\n%s", out)
	}
}

// TestRenderRunDetailWaitOnLimitIsAlwaysPresent — unlike every other conditional row in
// this block, the opt-in rides EVERY run.
//
// It is meaningful before any park has happened: it answers "will this run survive a
// usage limit, or just die on one?", which is a question about a queued run as much as
// a parked one. Rendering it only when true would make "off" indistinguishable from an
// older CLI that does not know the field — and "off" is the default, so that is the
// answer users most need to be able to read.
func TestRenderRunDetailWaitOnLimitIsAlwaysPresent(t *testing.T) {
	render := func(on bool) string {
		t.Helper()
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false)
		r := apitypes.RunDTO{ID: "run-1", Kind: "issue", Status: "running", Health: "ok", WaitOnLimit: on}
		if err := renderRunDetail(p, r); err != nil {
			t.Fatalf("renderRunDetail: %v", err)
		}
		return buf.String()
	}
	if out := render(true); !strings.Contains(out, "WAIT_ON_LIMIT") || !strings.Contains(out, "true") {
		t.Errorf("opted-in run: want WAIT_ON_LIMIT true, got:\n%s", out)
	}
	if out := render(false); !strings.Contains(out, "WAIT_ON_LIMIT") || !strings.Contains(out, "false") {
		t.Errorf("opted-out run: want an explicit WAIT_ON_LIMIT false — a missing row is indistinguishable from an older CLI, got:\n%s", out)
	}
}

// TestSteerStateOnAParkedRun. A park suspends the run, not the queue: nothing is
// consuming follow-ups while parked, and everything queued drains when it resumes.
func TestSteerStateOnAParkedRun(t *testing.T) {
	consumed := time.Now()

	// The queue state is UNCHANGED by the park — the suffix explains why nothing is
	// moving, and must not overwrite the queued/delivered answer itself.
	if got := steerState(nil, statusLimitWait); !strings.HasPrefix(got, "queued") {
		t.Errorf("steerState(unconsumed, limit_wait) = %q, want it to still read as queued", got)
	}
	if got := steerState(&consumed, statusLimitWait); !strings.HasPrefix(got, "delivered") {
		t.Errorf("steerState(consumed, limit_wait) = %q, want it to still read as delivered", got)
	}

	// Both say WHY nothing is happening, which is the whole point: a follow-up sitting
	// untouched for four hours is otherwise indistinguishable from a wedged run.
	for _, got := range []string{steerState(nil, statusLimitWait), steerState(&consumed, statusLimitWait)} {
		if !strings.Contains(got, "usage limit") {
			t.Errorf("steerState on a parked run = %q, want it to name the usage-limit park", got)
		}
	}

	// 🔴 NOT "not delivered (run finished)". A parked run is not finished, and that
	// label would tell a user their follow-up had been dropped when it is about to be
	// delivered. This is the shape the terminal-status map would produce if limit_wait
	// were ever added to it.
	if strings.Contains(steerState(nil, statusLimitWait), "run finished") {
		t.Error(`steerState(unconsumed, limit_wait) claims the run finished — a parked run resumes and its queue drains, so the follow-up has NOT been dropped`)
	}

	// Every other status is untouched.
	if got := steerState(nil, "running"); got != "queued" {
		t.Errorf("steerState(unconsumed, running) = %q, want %q", got, "queued")
	}
	if got := steerState(&consumed, "awaiting_approval"); got != "delivered (applies after approval)" {
		t.Errorf("steerState(consumed, awaiting_approval) = %q — the gate label regressed", got)
	}
	if got := steerState(nil, "completed"); got != "not delivered (run finished)" {
		t.Errorf("steerState(unconsumed, completed) = %q — the terminal label regressed", got)
	}
}
