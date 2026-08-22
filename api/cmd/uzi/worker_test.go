package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestWorkerRm(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "rm", "w1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDeletedWorkerID != "w1" {
		t.Fatalf("rm called DeleteWorker(%q), want w1", fc.LastDeletedWorkerID)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("rm output = %q, want a 'removed' confirmation", out)
	}
}

func TestWorkerRmJSON(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "rm", "w1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"deleted": true`) || !strings.Contains(out, `"id": "w1"`) {
		t.Errorf("rm --json = %q, want deleted/id", out)
	}
}

// A worker with active runs is a 409 (exit 5); the CLI must surface it.
func TestWorkerRmConflict(t *testing.T) {
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitConflict, "worker has active runs")}
	_, _, code := runCLI(t, fakeEnv(fc), "worker", "rm", "w1")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (conflict)", code, uzicli.ExitConflict)
	}
}

func TestWorkerRmRequiresArg(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "worker", "rm")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastDeletedWorkerID != "" {
		t.Error("rm with no id must not call DeleteWorker")
	}
}

// -------------------------------------------------------------------------
// worker set-token (PRD #104 M3)
// -------------------------------------------------------------------------

func TestWorkerSetToken(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "set-token", "w1", "console-key")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastSetTokenWorkerID != "w1" || fc.LastSetTokenLabel != "console-key" {
		t.Fatalf("set-token called SetWorkerBindMode(%q,_,%q), want (w1,_,console-key)",
			fc.LastSetTokenWorkerID, fc.LastSetTokenLabel)
	}
	// The MODE rides with the label since PRD #111 M3. Asserted because the label
	// alone no longer determines what the server does: a label sent with mode
	// "default" or "auto" is a 400, not a pin.
	if fc.LastSetTokenMode != "pinned" {
		t.Errorf("a label sent mode %q, want pinned", fc.LastSetTokenMode)
	}
	if !strings.Contains(out, "console-key") {
		t.Errorf("set-token output = %q, want it to name the token", out)
	}
}

// --default clears the binding, which the client expresses as an empty label plus
// the explicit mode.
func TestWorkerSetTokenDefault(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "set-token", "w1", "--default")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastSetTokenWorkerID != "w1" || fc.LastSetTokenLabel != "" || fc.LastSetTokenMode != "default" {
		t.Fatalf("--default called SetWorkerBindMode(%q,%q,%q), want (w1,default,\"\")",
			fc.LastSetTokenWorkerID, fc.LastSetTokenMode, fc.LastSetTokenLabel)
	}
	if !strings.Contains(out, "default") {
		t.Errorf("--default output = %q, want it to say the worker uses the default", out)
	}
}

// --auto is PRD #111 M3's third mode. It sends NO label: the server refuses a
// label alongside a non-pinned mode rather than quietly dropping one of them, so a
// client that sent both would be rejected, not silently reconciled.
func TestWorkerSetTokenAuto(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "set-token", "w1", "--auto")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastSetTokenWorkerID != "w1" || fc.LastSetTokenMode != "auto" || fc.LastSetTokenLabel != "" {
		t.Fatalf("--auto called SetWorkerBindMode(%q,%q,%q), want (w1,auto,\"\")",
			fc.LastSetTokenWorkerID, fc.LastSetTokenMode, fc.LastSetTokenLabel)
	}
	// The confirmation must say POOL, not "default": a user who cannot tell the two
	// apart from the output has no way to know whether --auto took effect.
	if !strings.Contains(out, "auto-selects") || !strings.Contains(out, "pool") {
		t.Errorf("--auto output = %q, want it to say the worker auto-selects from the pool", out)
	}
}

// A label AND --default is ambiguous; so is neither. Both are usage errors rather
// than a silent choice of one meaning over the other.
func TestWorkerSetTokenAmbiguousArgs(t *testing.T) {
	for _, args := range [][]string{
		{"worker", "set-token", "w1", "console-key", "--default"},
		{"worker", "set-token", "w1"},
		// PRD #111 M3 makes three choices, so every pair is a usage error and so is
		// all three. Enumerated rather than sampled: the pair a pairwise check
		// forgets is always the one added last.
		{"worker", "set-token", "w1", "console-key", "--auto"},
		{"worker", "set-token", "w1", "--default", "--auto"},
		{"worker", "set-token", "w1", "console-key", "--default", "--auto"},
	} {
		fc := &uzicli.FakeClient{}
		_, _, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitUsage {
			t.Fatalf("%v: exit = %d, want %d (usage)", args, code, uzicli.ExitUsage)
		}
		if fc.LastSetTokenWorkerID != "" {
			t.Errorf("%v: must not reach the API", args)
		}
	}
}

// An unknown label is a 400 → exit 3; an unknown worker is a 404 → exit 4. Both
// must reach the caller as their documented codes rather than a generic failure.
func TestWorkerSetTokenErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"unknown label", uzicli.Exitf(uzicli.ExitAuth, "no Anthropic token with that label"), uzicli.ExitAuth},
		{"unknown worker", uzicli.Exitf(uzicli.ExitNotFound, "worker not found"), uzicli.ExitNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{Err: tc.err}
			_, _, code := runCLI(t, fakeEnv(fc), "worker", "set-token", "w1", "some-label")
			if code != tc.want {
				t.Fatalf("exit = %d, want %d", code, tc.want)
			}
		})
	}
}

// TestWorkerListShowsBindMode is PRD #111 M5, and it closes a CLI-parity hole M3
// opened: `uzi worker set-token --auto` is a WRITE the CLI can perform, and until
// this column there was no human-readable READ to confirm it — only `--json`. A
// three-way user choice you can set and cannot see is worse than one you cannot set.
//
// The fixture stages all three modes at once because the failure that matters is a
// renderer that collapses two of them. One mode per test would pass against a
// function that returned the same word for `default` and `auto`.
//
// MUTATION THIS CATCHES: returning w.AnthropicBindMode verbatim for `pinned` — the
// pinned row then reads "pinned" instead of naming the token, which is the fact a
// user needs and the argument `uzi worker set-token` takes back.
func TestWorkerListShowsBindMode(t *testing.T) {
	label := "console-key"
	fc := &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{
		{ID: "w1", Name: "alpha", Status: "online", AnthropicBindMode: "default"},
		{ID: "w2", Name: "bravo", Status: "online", AnthropicBindMode: "auto"},
		{
			ID: "w3", Name: "charlie", Status: "online", AnthropicBindMode: "pinned",
			AnthropicSecretID: sptr("sec-1"), AnthropicSecretLabel: &label,
		},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "TOKEN") {
		t.Fatalf("worker list is missing the TOKEN column: %q", out)
	}
	cellOf := func(name string) string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, name) {
				f := strings.Fields(line)
				return f[len(f)-1]
			}
		}
		t.Fatalf("no row for %s in %q", name, out)
		return ""
	}
	if got := cellOf("alpha"); got != "default" {
		t.Errorf("default worker's TOKEN = %q, want default", got)
	}
	if got := cellOf("bravo"); got != "auto" {
		t.Errorf("auto worker's TOKEN = %q, want auto — an auto worker has no fixed token, and "+
			"naming one would present a snapshot as a setting", got)
	}
	// The LABEL, not the word "pinned": it is what the user set and what
	// `uzi worker set-token` takes back.
	if got := cellOf("charlie"); got != "console-key" {
		t.Errorf("pinned worker's TOKEN = %q, want the label console-key", got)
	}
}

// TestFormatUptimeDuration covers each bucket of the pure formatter (PRD #251). It
// is split from uptimeCell precisely so the bucket boundaries test without the wall
// clock; the buckets must match the web's formatUptimeSince / rateLimits.formatCountdown
// so uptime reads in the same vocabulary as the reset countdowns (Decision 4).
func TestFormatUptimeDuration(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    time.Duration
		want string
	}{
		{"days rounds to Nd Nh", 2*24*time.Hour + 4*time.Hour + 30*time.Minute, "2d 4h"},
		{"hours to Nh Nm", 1*time.Hour + 23*time.Minute, "1h 23m"},
		{"minutes to Nm", 44 * time.Minute, "44m"},
		{"sub-minute floors", 42 * time.Second, "<1m"},
		{"zero floors", 0, "<1m"},
		{"negative (clock skew) floors", -5 * time.Minute, "<1m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatUptimeDuration(tc.d); got != tc.want {
				t.Errorf("formatUptimeDuration(%v) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}

// TestUptimeCell covers the "-" gates (which need no clock) and that an online
// worker with a recent anchor renders a real duration rather than "-" (PRD #251).
// Offline or anchorless → "-", mirroring upgradeCell's empty→"-" convention: an
// offline worker is not "up", and a nil OnlineSince has no session to count from.
func TestUptimeCell(t *testing.T) {
	recent := time.Now().Add(-90 * time.Minute)
	for _, tc := range []struct {
		name string
		w    apitypes.WorkerDTO
		want string
	}{
		{"offline worker", apitypes.WorkerDTO{Status: "offline", OnlineSince: &recent}, "-"},
		{"online but nil anchor", apitypes.WorkerDTO{Status: "online"}, "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := uptimeCell(tc.w); got != tc.want {
				t.Errorf("uptimeCell(%+v) = %q, want %q", tc.w, got, tc.want)
			}
		})
	}
	// An online worker with a recent anchor renders a duration, not "-".
	if got := uptimeCell(apitypes.WorkerDTO{Status: "online", OnlineSince: &recent}); got == "-" || got == "" {
		t.Errorf("online worker with a recent anchor = %q, want a duration", got)
	}
}

// TestWorkerListShowsUptime asserts the UPTIME column is present and renders a
// worker's continuous-online duration, with "-" for an offline worker (PRD #251).
//
// The fixture stages an online worker with a fixed-hours anchor and an offline one,
// so the failure that matters — a renderer that prints the same thing for both, or
// drops the column — is caught. The online anchor is set well inside the hours
// bucket so the assertion does not race the wall clock across a bucket boundary.
func TestWorkerListShowsUptime(t *testing.T) {
	up := time.Now().Add(-3*time.Hour - 12*time.Minute)
	down := time.Now().Add(-2 * time.Hour)
	fc := &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{
		{ID: "w1", Name: "alpha", Status: "online", AnthropicBindMode: "default", OnlineSince: &up},
		{ID: "w2", Name: "bravo", Status: "offline", AnthropicBindMode: "default", OnlineSince: &down},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "UPTIME") {
		t.Fatalf("worker list is missing the UPTIME column: %q", out)
	}
	uptimeOf := func(name string) string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, name) {
				// UPTIME is the 4th column: ID NAME STATUS UPTIME VERSION UPGRADE TOKEN.
				f := strings.Fields(line)
				if len(f) < 4 {
					t.Fatalf("row for %s has too few fields: %q", name, line)
				}
				return f[3]
			}
		}
		t.Fatalf("no row for %s in %q", name, out)
		return ""
	}
	if got := uptimeOf("alpha"); got != "3h" {
		t.Errorf("online worker's UPTIME first field = %q, want the hours bucket (3h ...)", got)
	}
	if got := uptimeOf("bravo"); got != "-" {
		t.Errorf("offline worker's UPTIME = %q, want - (an offline worker is not up)", got)
	}
}

// TestStatusCell covers statusCell's three branches (PRD #496 M4): a non-cordoned
// worker passes its raw status through untouched, and a cordoned one is annotated
// (draining) vs (cordoned) keyed on ActiveRuns, NOT on Status. The offline+draining
// row is the reason the helper never hardcodes "online": a cordoned worker whose pod
// dies is swept offline WITHOUT clearing draining_since, so `offline (draining)` is a
// real, reachable state (Decision 5).
func TestStatusCell(t *testing.T) {
	ts := time.Now()
	for _, tc := range []struct {
		name string
		w    apitypes.WorkerDTO
		want string
	}{
		{"online not cordoned", apitypes.WorkerDTO{Status: "online"}, "online"},
		{"offline not cordoned", apitypes.WorkerDTO{Status: "offline"}, "offline"},
		{"online draining", apitypes.WorkerDTO{Status: "online", DrainingSince: &ts, ActiveRuns: 1}, "online (draining)"},
		{"online idle cordoned", apitypes.WorkerDTO{Status: "online", DrainingSince: &ts, ActiveRuns: 0}, "online (cordoned)"},
		// The hardcode-"online" guard: an offline worker whose pod died mid-run was swept
		// offline without clearing draining_since, so this must read from the raw status.
		{"offline draining", apitypes.WorkerDTO{Status: "offline", DrainingSince: &ts, ActiveRuns: 1}, "offline (draining)"},
		// Chat-only cordoned: a `busy` worker with zero in-flight runs reads (cordoned).
		// statusCell keys on ActiveRuns, not on Busy — there is no way to set Busy on the
		// DTO to change this output, which is the point: ActiveRuns:0 IS the cordoned case.
		{"chat-only cordoned (ActiveRuns 0)", apitypes.WorkerDTO{Status: "online", DrainingSince: &ts, ActiveRuns: 0}, "online (cordoned)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusCell(tc.w); got != tc.want {
				t.Errorf("statusCell(%+v) = %q, want %q", tc.w, got, tc.want)
			}
		})
	}
}

// TestWorkerListShowsCordon asserts the STATUS column carries the cordon annotation
// through the full render (PRD #496 M4), and that the offline+draining fixture renders
// `offline (draining)` and never `online ...` — the raw-status guard end to end.
//
// The STATUS cell now contains a space for annotated rows, so strings.Fields row parsing
// would split `online (draining)` into two fields; this asserts with strings.Contains on
// each worker's row line instead. Each fixture gets a distinct NAME to locate its row.
func TestWorkerListShowsCordon(t *testing.T) {
	ts := time.Now()
	fc := &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{
		{ID: "w1", Name: "drainer", Status: "online", AnthropicBindMode: "default", DrainingSince: &ts, ActiveRuns: 1},
		{ID: "w2", Name: "cordoned", Status: "online", AnthropicBindMode: "default", DrainingSince: &ts, ActiveRuns: 0},
		{ID: "w3", Name: "deadpod", Status: "offline", AnthropicBindMode: "default", DrainingSince: &ts, ActiveRuns: 1},
		{ID: "w4", Name: "plain", Status: "online", AnthropicBindMode: "default"},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	rowOf := func(name string) string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, name) {
				return line
			}
		}
		t.Fatalf("no row for %s in %q", name, out)
		return ""
	}
	if got := rowOf("drainer"); !strings.Contains(got, "online (draining)") {
		t.Errorf("drainer row = %q, want it to contain %q", got, "online (draining)")
	}
	if got := rowOf("cordoned"); !strings.Contains(got, "online (cordoned)") {
		t.Errorf("cordoned row = %q, want it to contain %q", got, "online (cordoned)")
	}
	deadpod := rowOf("deadpod")
	if !strings.Contains(deadpod, "offline (draining)") {
		t.Errorf("deadpod row = %q, want it to contain %q", deadpod, "offline (draining)")
	}
	if strings.Contains(deadpod, "online") {
		t.Errorf("deadpod row = %q must not read online — its raw status is offline", deadpod)
	}
	// The plain worker carries no cordon annotation: bare status, no (draining)/(cordoned).
	plain := rowOf("plain")
	if strings.Contains(plain, "(draining)") || strings.Contains(plain, "(cordoned)") {
		t.Errorf("plain row = %q must show its bare status with no cordon annotation", plain)
	}
	// --json carries the raw draining_since field for scripting, untouched by the cell.
	jout, _, jcode := runCLI(t, fakeEnv(fc), "worker", "list", "--json")
	if jcode != uzicli.ExitOK {
		t.Fatalf("worker list --json exit = %d, want 0", jcode)
	}
	if !strings.Contains(jout, "draining_since") {
		t.Errorf("worker list --json = %q, want it to carry the draining_since field", jout)
	}
}

// TestBindModeCellUnknownPassesThrough: the CLI ships separately from the API, so a
// newer server can send a fourth mode. Printing it as itself is honest; mapping it to
// "default" would state something false about where a worker's money goes.
func TestBindModeCellUnknownPassesThrough(t *testing.T) {
	got := bindModeCell(apitypes.WorkerDTO{AnthropicBindMode: "some_future_mode"})
	if got != "some_future_mode" {
		t.Fatalf("cell = %q, want the unrecognised mode verbatim", got)
	}
}

// TestBindModeCellPinnedWithNoLabel is belt-and-braces against a DTO that
// contradicts itself. The server reports the EFFECTIVE mode, so `pinned` always
// arrives with a label — D9's pinned-with-a-deleted-token case is mapped to
// `default` upstream. If one ever arrives anyway, "default" is what such a worker
// would actually spend, so it is the honest cell.
func TestBindModeCellPinnedWithNoLabel(t *testing.T) {
	if got := bindModeCell(apitypes.WorkerDTO{AnthropicBindMode: "pinned"}); got != "default" {
		t.Fatalf("cell = %q, want default — a pin with no credential resolves as the default (D9)", got)
	}
}

// TestWorkerNamesAreSanitizedForTheTerminal is the render-site defense for a field
// that has LESS protection than a token label, not more.
//
// 🔴 WORKER NAMES ARE VALIDATED FOR LENGTH ONLY. handler/workers.go is TrimSpace plus
// a 200-byte cap — no control-character check, no Cf check — and `workers.name` is a
// bare `text NOT NULL` with no CHECK (00020). So unlike a token label, an ESC is
// STORABLE in a worker name: the ANSI-injection class validateSecretLabel has always
// refused is live here, and an embedded newline breaks the tabwriter rail for every
// following row, i.e. a name can forge a table row.
//
// This asserts what the RENDERER does with what it is handed, which is a different
// question from what the server accepts on write and sits on the other side of a trust
// boundary. Hardening the validator is a separate change; the render site must hold
// without it.
//
// MUTATION THIS CATCHES: reverting either cell to the raw `w.Name`. Measured on both.
func TestWorkerNamesAreSanitizedForTheTerminal(t *testing.T) {
	hostile := "safe\u202ednetsop\x1b[31m\nforged\trow"
	for _, tc := range []struct {
		name string
		args []string
		fc   *uzicli.FakeClient
	}{
		{
			name: "uzi worker list",
			args: []string{"worker", "list"},
			fc: &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{
				{ID: "w1", Name: hostile, Status: "online", AnthropicBindMode: "default"},
			}},
		},
		{
			// 🔴 THE CROSS-TENANT ONE. This prints ANOTHER user's worker name into an
			// admin's terminal, beside their email — so a crafted name is terminal
			// control injection into someone else's session, and a forged row lands in
			// a table an admin reads to make decisions.
			name: "uzi admin workers",
			args: []string{"admin", "workers"},
			fc: &uzicli.FakeClient{AdminWorkers: []apitypes.AdminWorkerDTO{
				{WorkerDTO: apitypes.WorkerDTO{ID: "w1", Name: hostile, Status: "online"}, OwnerEmail: "victim@uzi.test"},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, code := runCLI(t, fakeEnv(tc.fc), tc.args...)
			if code != uzicli.ExitOK {
				t.Fatalf("exit = %d, want 0", code)
			}
			// The first two are the shared floor; the last two are what tell cellText
			// and sanitizeTTY apart — only cellText folds a newline and a tab, and a
			// newline in a table cell is what forges a row.
			for _, bad := range []string{"\u202e", "\x1b", "\nforged", "\trow"} {
				if strings.Contains(out, bad) {
					t.Errorf("a hostile worker name reached the terminal carrying %q: %q", bad, out)
				}
			}
			if !strings.Contains(out, "safe") {
				t.Errorf("sanitizing dropped the printable text too: %q", out)
			}
		})
	}
}
