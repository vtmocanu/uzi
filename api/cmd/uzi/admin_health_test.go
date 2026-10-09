package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1484 M3: `uzi admin health` prints the attention checks, a verdict and a tally, and
// exits 8 on a 200-carried danger verdict (a SUCCESS-path exit distinct from a transport
// failure). These drive the real command against a FakeClient returning a canned document.

const healthSinceTS = "2026-09-20T02:14:00Z"

// healthDoc builds a HealthDocDTO with the given overall status and checks, deriving the
// per-severity tally from the checks so the tally line the command prints is self-consistent
// with what it renders.
func healthDoc(status string, checks ...apitypes.HealthCheckDTO) apitypes.HealthDocDTO {
	var counts apitypes.HealthCountsDTO
	blocking := false
	for _, ck := range checks {
		if ck.Scope == "instance" && ck.Severity == "danger" {
			blocking = true
		}
		switch ck.Severity {
		case "ok":
			counts.OK++
		case "warn":
			counts.Warn++
		case "danger":
			counts.Danger++
		case "unknown":
			counts.Unknown++
		case "na":
			counts.NA++
		}
	}
	return apitypes.HealthDocDTO{
		Status:    status,
		Blocking:  blocking,
		CheckedAt: "2026-09-20T02:52:10Z",
		Counts:    counts,
		Checks:    checks,
	}
}

func rollDangerCheck() apitypes.HealthCheckDTO {
	since := healthSinceTS
	return apitypes.HealthCheckDTO{
		ID: "fleet.roll", Scope: "instance", Group: "workers", Title: "Worker image roll", Severity: "danger",
		Summary: "4 of 4 workers stuck rolling to 0.84.0-rc.2: ImagePullBackOff", Since: &since,
	}
}

func queueWarnCheck() apitypes.HealthCheckDTO {
	since := healthSinceTS
	return apitypes.HealthCheckDTO{
		ID: "queue.waiting", Scope: "owner", Group: "queue", Title: "Queued runs waiting", Severity: "warn",
		Summary: "oldest waiting_worker run has waited 12m", Since: &since,
	}
}

func dbOKCheck() apitypes.HealthCheckDTO {
	return apitypes.HealthCheckDTO{
		ID: "db", Scope: "instance", Group: "control", Title: "Database", Severity: "ok",
		Summary: "ping 4ms, pool 3/20 acquired",
	}
}

func slackNACheck() apitypes.HealthCheckDTO {
	return apitypes.HealthCheckDTO{
		ID: "slack.socket", Scope: "owner", Group: "integrations", Title: "Slack socket", Severity: "na",
		Summary: "Slack is not configured on this deployment",
	}
}

// Overall ok exits 0 and, with no attention items, says so rather than printing an empty
// table. The ok/na checks are hidden by default (only attention items list), so the summary
// of the ok check must not appear.
func TestAdminHealthExitZeroWhenOK(t *testing.T) {
	fc := &uzicli.FakeClient{AdminHealthDoc: healthDoc("ok", dbOKCheck(), slackNACheck())}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "health")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (ok verdict)", code)
	}
	if !strings.Contains(out, "status: OK") {
		t.Errorf("want the verdict line, got:\n%s", out)
	}
	if !strings.Contains(out, "all checks passing") {
		t.Errorf("an ok verdict with no attention items must say so, got:\n%s", out)
	}
	if strings.Contains(out, "ping 4ms") {
		t.Errorf("an ok check must not be listed in the default (attention-only) view:\n%s", out)
	}
}

// Overall warn WITHOUT --strict exits 0: warn is not a probe failure by default. The warn
// check is listed (it needs attention); the ok check is not.
func TestAdminHealthExitZeroWhenWarnWithoutStrict(t *testing.T) {
	fc := &uzicli.FakeClient{AdminHealthDoc: healthDoc("warn", queueWarnCheck(), dbOKCheck())}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "health")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (warn without --strict)", code)
	}
	for _, want := range []string{"WARN", "queue.waiting", "oldest waiting_worker run has waited 12m", "status: WARN"} {
		if !strings.Contains(out, want) {
			t.Errorf("warn view missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ping 4ms") {
		t.Errorf("an ok check must not appear in the attention-only view:\n%s", out)
	}
}

// Overall danger exits 8 — and the FULL report is printed to stdout BEFORE the nonzero exit
// (exit 8 is a success-path exit, not a short-circuit). The sentinel's terse reason lands on
// stderr; the exit code, not the text, is the contract.
func TestAdminHealthExitEightWhenDangerAfterPrinting(t *testing.T) {
	fc := &uzicli.FakeClient{AdminHealthDoc: healthDoc("danger", rollDangerCheck(), queueWarnCheck(), dbOKCheck(), slackNACheck())}
	out, errb, code := runCLI(t, fakeEnv(fc), "admin", "health")
	if code != uzicli.ExitHealthDanger {
		t.Fatalf("exit = %d, want %d (danger)", code, uzicli.ExitHealthDanger)
	}
	// The full report reached stdout despite the nonzero exit: the danger check and its
	// summary, the warn check, and the verdict line.
	for _, want := range []string{"fleet.roll", "4 of 4 workers stuck rolling", "queue.waiting", "status: DANGER", "blocking: true (instance-wide)"} {
		if !strings.Contains(out, want) {
			t.Errorf("danger report did not print %q before the exit-8 return:\n%s", want, out)
		}
	}
	if !strings.Contains(errb, "health check reports danger") {
		t.Errorf("want the sentinel reason on stderr, got:\n%s", errb)
	}
}

// --strict turns a warn (and an unknown) into exit 8; the report still prints first.
func TestAdminHealthStrictExitsEightOnWarnAndUnknown(t *testing.T) {
	for _, status := range []string{"warn", "unknown"} {
		t.Run(status, func(t *testing.T) {
			check := queueWarnCheck()
			if status == "unknown" {
				check.Severity = "unknown"
				check.Summary = "run-health detector is off; capacity signal unavailable"
			}
			fc := &uzicli.FakeClient{AdminHealthDoc: healthDoc(status, check, dbOKCheck())}
			out, _, code := runCLI(t, fakeEnv(fc), "admin", "health", "--strict")
			if code != uzicli.ExitHealthDanger {
				t.Fatalf("--strict %s exit = %d, want %d", status, code, uzicli.ExitHealthDanger)
			}
			if !strings.Contains(out, "status: "+strings.ToUpper(status)) {
				t.Errorf("--strict %s did not print the verdict before exiting:\n%s", status, out)
			}
		})
	}
}

// Without --strict, warn and unknown are both exit 0: only danger fails a default probe.
func TestAdminHealthUnknownWithoutStrictIsZero(t *testing.T) {
	check := queueWarnCheck()
	check.Severity = "unknown"
	fc := &uzicli.FakeClient{AdminHealthDoc: healthDoc("unknown", check)}
	_, _, code := runCLI(t, fakeEnv(fc), "admin", "health")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (unknown without --strict)", code)
	}
}

// A document whose overall Status is empty or outside the closed enum (ok | warn | danger |
// unknown — "na" is never an overall status) is MALFORMED, not healthy. The CLI must exit
// nonzero-but-not-8 so a cron probe reads "could not trust the answer" rather than a false
// green. The concrete regression this pins: a `{}` body leaves Status empty, which reached the
// old `return nil` and exited 0 — a broken endpoint reporting success.
func TestAdminHealthMalformedStatusIsRejected(t *testing.T) {
	for _, status := range []string{"", "bogus", "na"} {
		t.Run("status="+status, func(t *testing.T) {
			fc := &uzicli.FakeClient{AdminHealthDoc: healthDoc(status, dbOKCheck())}
			_, _, code := runCLI(t, fakeEnv(fc), "admin", "health")
			if code == uzicli.ExitOK {
				t.Fatalf("malformed status %q exited 0: a broken document must not read as healthy to a probe", status)
			}
			if code == uzicli.ExitHealthDanger {
				t.Fatalf("malformed status %q exited 8 (danger): it is 'could not trust the answer', not danger", status)
			}
			if code != uzicli.ExitGeneric {
				t.Fatalf("malformed status %q exit = %d, want %d (generic)", status, code, uzicli.ExitGeneric)
			}
		})
	}
}

// A transport/auth failure keeps its EXISTING code and never becomes 8: only a 200 response
// can yield 8, so a probe can tell "unhealthy" (8) from "could not ask" (3/6). A 401 → exit 3,
// a 5xx/unreachable → exit 6, under --strict too.
func TestAdminHealthTransportFailuresKeepTheirCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"401-auth", uzicli.Exitf(uzicli.ExitAuth, "admin scope required"), uzicli.ExitAuth},
		{"5xx-unreachable", uzicli.Exitf(uzicli.ExitUnreachable, "cannot reach uzi"), uzicli.ExitUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{Err: tc.err}
			for _, args := range [][]string{{"admin", "health"}, {"admin", "health", "--strict"}} {
				_, _, code := runCLI(t, fakeEnv(fc), args...)
				if code != tc.want {
					t.Fatalf("%v exit = %d, want %d (a transport failure must never yield 8)", args, code, tc.want)
				}
			}
		})
	}
}

// A hostile ~1 MB server string in a cell is bounded by the package-local cellText, not the
// unbounded CellText the Printer's table boundary applies. Without cellText the whole megabyte
// would print; the cap (compactText, 200 runes) is what keeps an admin's terminal usable.
func TestAdminHealthBoundsHostileCell(t *testing.T) {
	hostile := strings.Repeat("A", 1<<20) // 1 MiB
	check := apitypes.HealthCheckDTO{
		ID: "fleet.roll", Scope: "instance", Group: "workers", Title: "Worker image roll", Severity: "danger",
		Summary: hostile,
	}
	fc := &uzicli.FakeClient{AdminHealthDoc: healthDoc("danger", check)}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "health")
	if code != uzicli.ExitHealthDanger {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitHealthDanger)
	}
	if strings.Contains(out, strings.Repeat("A", 1000)) {
		t.Errorf("a 1 MB summary reached the terminal unbounded — cellText did not cap the cell")
	}
	if len(out) > 4096 {
		t.Errorf("rendered output is %d bytes; the hostile cell was not bounded", len(out))
	}
	// Positive control: the truncation ellipsis and the check id are still there.
	if !strings.Contains(out, "…") || !strings.Contains(out, "fleet.roll") {
		t.Errorf("bounding ate the whole cell instead of truncating it:\n%s", out[:min(len(out), 400)])
	}
}

// --json emits the endpoint's document unchanged (decodes back to a HealthDocDTO), and the
// verdict still drives the exit code so a probe piping --json still distinguishes danger.
func TestAdminHealthJSONEmitsTheDocAndExits(t *testing.T) {
	fc := &uzicli.FakeClient{AdminHealthDoc: healthDoc("danger", rollDangerCheck(), dbOKCheck())}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "health", "--json")
	if code != uzicli.ExitHealthDanger {
		t.Fatalf("--json exit = %d, want %d (the verdict drives the code in json mode too)", code, uzicli.ExitHealthDanger)
	}
	var got apitypes.HealthDocDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not a HealthDocDTO: %v\n%s", err, out)
	}
	if len(got.Checks) != 2 {
		t.Fatalf("--json lost checks: %+v", got)
	}
	if !got.Blocking || got.Checks[0].Scope != "instance" {
		t.Errorf("--json lost scope/blocking: %+v", got)
	}
	if got.Status != "danger" || len(got.Checks) != 2 {
		t.Errorf("--json lost the document: status=%q checks=%d", got.Status, len(got.Checks))
	}
	if got.Checks[0].ID != "fleet.roll" || got.Counts.Danger != 1 || got.Counts.OK != 1 {
		t.Errorf("--json altered the document: %+v", got)
	}
}

// --all lists EVERY check, including the ok and na ones the default view hides.
func TestAdminHealthAllListsEveryCheck(t *testing.T) {
	fc := &uzicli.FakeClient{AdminHealthDoc: healthDoc("warn", queueWarnCheck(), dbOKCheck(), slackNACheck())}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "health", "--all")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"queue.waiting", "db", "slack.socket", "OK", "NA"} {
		if !strings.Contains(out, want) {
			t.Errorf("--all view missing %q:\n%s", want, out)
		}
	}
}

// `uzi admin health` is registered under `admin` with the --all and --strict flags.
func TestAdminHealthIsRegistered(t *testing.T) {
	admin := findCmd(newRootCmd(fakeEnv(&uzicli.FakeClient{})), "admin")
	if admin == nil {
		t.Fatal("missing `admin` group")
	}
	health := findCmd(admin, "health")
	if health == nil {
		t.Fatal("missing `admin health` subcommand")
	}
	for _, f := range []string{"all", "strict"} {
		if health.Flags().Lookup(f) == nil {
			t.Errorf("`admin health` missing --%s flag", f)
		}
	}
}

func TestAdminHealthOwnerDangerRemainsExitEightWithoutInstanceBlocking(t *testing.T) {
	check := queueWarnCheck()
	check.Severity = "danger"
	doc := healthDoc("danger", check)
	fc := &uzicli.FakeClient{AdminHealthDoc: doc}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "health")
	if code != uzicli.ExitHealthDanger {
		t.Fatalf("owner danger exit = %d, want 8", code)
	}
	for _, want := range []string{"blocking: false (instance-wide)", "status: DANGER", "1 danger", "queue.waiting"} {
		if !strings.Contains(out, want) {
			t.Errorf("owner report missing %q: %s", want, out)
		}
	}
	out, _, code = runCLI(t, fakeEnv(fc), "admin", "health", "--json")
	var got apitypes.HealthDocDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if code != uzicli.ExitHealthDanger || got.Blocking || len(got.Checks) != 1 || got.Checks[0].Scope != "owner" {
		t.Fatalf("owner JSON scope/blocking/exit changed: exit=%d doc=%+v", code, got)
	}
}

func codexPricingWarnCheck() apitypes.HealthCheckDTO {
	return apitypes.HealthCheckDTO{
		ID: "pricing.codex", Scope: "instance", Group: "housekeeping", Title: "Codex price coverage",
		Severity: "warn", Summary: "2 Codex models lack usable pricing", Doc: func() *string { s := "admin-health"; return &s }(),
		Evidence: []apitypes.HealthEvidenceDTO{
			{Label: "gpt-5.5", Value: "3 runs, no price"},
			{Label: "gpt-5.6-sol", Value: "2 runs, promotional price expired"},
		},
	}
}

func TestAdminHealthCodexEvidence(t *testing.T) {
	check := codexPricingWarnCheck()
	for _, flags := range [][]string{nil, {"--no-color"}, {"--all"}, {"--no-color", "--all"}} {
		t.Run(fmt.Sprint(flags), func(t *testing.T) {
			doc := healthDoc("warn", check, dbOKCheck())
			out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{AdminHealthDoc: doc}), append([]string{"admin", "health"}, flags...)...)
			if code != uzicli.ExitOK {
				t.Fatalf("exit = %d", code)
			}
			for _, want := range []string{"EVIDENCE", "gpt-5.5: 3 runs, no price", "gpt-5.6-sol: 2 runs, promotional price expired", "blocking: false"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestAdminHealthCodexFullCapAndHostileEvidence(t *testing.T) {
	check := codexPricingWarnCheck()
	for i := 2; i < 10; i++ {
		check.Evidence = append(check.Evidence, apitypes.HealthEvidenceDTO{Label: fmt.Sprintf("model-%02d", i), Value: "1 run, no price"})
	}
	check.Evidence = append(check.Evidence, apitypes.HealthEvidenceDTO{Label: "and more", Value: "4 additional models"})
	for _, flags := range [][]string{{"--no-color"}, {"--no-color", "--all"}} {
		out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{AdminHealthDoc: healthDoc("warn", check)}), append([]string{"admin", "health"}, flags...)...)
		if code != uzicli.ExitOK {
			t.Fatalf("exit = %d", code)
		}
		for _, want := range []string{"gpt-5.5: 3 runs, no price", "gpt-5.6-sol: 2 runs, promotional price expired", "and more: 4 additional models"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q:\n%s", want, out)
			}
		}
		for i := 2; i < 10; i++ {
			if !strings.Contains(out, fmt.Sprintf("model-%02d: 1 run, no price", i)) {
				t.Errorf("model %d lost:\n%s", i, out)
			}
		}
	}
	check.Evidence = []apitypes.HealthEvidenceDTO{
		{Label: "hostile\x1b\r\t\n" + strings.Repeat("界", 300), Value: "tail"},
		{Label: "value", Value: "hostile\x1b\r\t\n" + strings.Repeat("界", 300)},
	}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{AdminHealthDoc: healthDoc("warn", check)}), "admin", "health", "--no-color")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var cells int
	evidenceStart := strings.Index(strings.Split(out, "\n")[0], "EVIDENCE")
	if evidenceStart < 0 {
		t.Fatalf("missing EVIDENCE header:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		for _, r := range line {
			if unicode.IsControl(r) && r != '\t' {
				t.Errorf("control %U survived", r)
			}
		}
		if len(line) > evidenceStart && strings.Contains(line[evidenceStart:], "hostile") {
			cells++
			if utf8.RuneCountInString(strings.TrimSpace(line[evidenceStart:])) > 201 {
				t.Errorf("unbounded evidence: %d runes", utf8.RuneCountInString(strings.TrimSpace(line[evidenceStart:])))
			}
		}
	}
	if cells != 2 {
		t.Errorf("want two bounded hostile evidence cells, got %d:\n%s", cells, out)
	}
}

func TestAdminHealthCodexOKAllAndJSONUnchanged(t *testing.T) {
	check := codexPricingWarnCheck()
	doc := healthDoc("warn", check)
	fc := &uzicli.FakeClient{AdminHealthDoc: doc}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "health", "--json")
	var got apitypes.HealthDocDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if code != uzicli.ExitOK || !reflect.DeepEqual(got, doc) {
		t.Fatalf("JSON changed: exit=%d got=%+v", code, got)
	}
	check.Severity = "ok"
	check.Summary = "no recent Codex usage on unpriced models"
	check.Evidence = []apitypes.HealthEvidenceDTO{}
	fc.AdminHealthDoc = healthDoc("ok", check)
	out, _, code = runCLI(t, fakeEnv(fc), "admin", "health", "--all", "--no-color")
	if code != uzicli.ExitOK || !strings.Contains(out, "pricing.codex") || !strings.Contains(out, check.Summary) {
		t.Fatalf("OK pricing missing: exit=%d\n%s", code, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "pricing.codex") {
			evidenceStart := strings.Index(strings.Split(out, "\n")[0], "EVIDENCE")
			if evidenceStart < 0 || (len(line) > evidenceStart && strings.TrimSpace(line[evidenceStart:]) != "") {
				t.Fatalf("no-evidence cell must be blank: %q", line)
			}
		}
	}
}
