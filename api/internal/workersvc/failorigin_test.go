package workersvc

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestCoerceFailOrigin pins the three-way contract (PRD #69 M7a): nil stays nil,
// an in-set value passes through verbatim, and an unrecognised value coerces to nil
// (NOT a placeholder member — unlike CoerceRateLimitType's "unknown"), so the coercer
// never fabricates a bogus class the judge would trust.
func TestCoerceFailOrigin(t *testing.T) {
	if got := CoerceFailOrigin(nil); got != nil {
		t.Fatalf("CoerceFailOrigin(nil) = %v, want nil", got)
	}
	// The worker-reportable subset passes through verbatim.
	for in := range workerReportableFailOrigins {
		v := in
		got := CoerceFailOrigin(&v)
		if got == nil || *got != in {
			t.Fatalf("CoerceFailOrigin(%q) = %v, want passthrough", in, got)
		}
	}
	// Server-authoritative classes are NOT worker-reportable: a worker naming one is a
	// forgery and must coerce to nil (→ the failed arm defaults to agent_failure), so a
	// worker cannot inject worker_lost/run_timeout/plan_rejected/auto_stopped/
	// guardrail_blocked into the trusted classification, nor steer Gate 4b via a forged
	// guardrail_blocked. Each must still be a real stored member (else the split is stale).
	serverOnly := []string{"worker_lost", "run_timeout", "plan_rejected", "auto_stopped", "guardrail_blocked"}
	for _, s := range serverOnly {
		if !failOriginSet[s] {
			t.Fatalf("%q is in the server-only list but not in the stored vocabulary", s)
		}
		v := s
		if got := CoerceFailOrigin(&v); got != nil {
			t.Fatalf("CoerceFailOrigin(%q) = %v, want nil (server-authoritative class must not be worker-reportable)", s, *got)
		}
	}
	// The worker-reportable subset and the server-only set must PARTITION the vocabulary,
	// so a future member added to failOrigins must be classified into exactly one rather
	// than defaulting into worker-reportable-by-omission (the very hole this fix closed).
	if len(workerReportableFailOrigins)+len(serverOnly) != len(failOrigins) {
		t.Fatalf("worker-reportable(%d) + server-only(%d) != vocabulary(%d); a new fail_origin was not classified",
			len(workerReportableFailOrigins), len(serverOnly), len(failOrigins))
	}
	for _, bad := range []string{"", "nonsense", "AGENT_FAILURE", "rate limited", "unknown"} {
		v := bad
		if got := CoerceFailOrigin(&v); got != nil {
			t.Fatalf("CoerceFailOrigin(%q) = %v, want nil (unknown must not fabricate a class)", bad, *got)
		}
	}
}

// TestFailOriginVocabularyMatchesCheck is the instrument migration 00126's comment
// promises, copied from TestRateLimitTypeVocabularyMatchesCheck (00091's) for the same
// reason it exists there: a value Go writes and the CHECK rejects becomes a constraint
// violation (23514) at a failed run's write — turning a classification into a second
// failure — and a value in the CHECK Go never writes is a promise nothing keeps. It
// parses the migration rather than restating the list, because a second hand-typed copy
// is exactly the drift it prevents.
func TestFailOriginVocabularyMatchesCheck(t *testing.T) {
	const path = "../store/migrations/00139_run_finalize_base_align_conflict.sql"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// Strip comment lines first: the prose above the statement names several members
	// (plan_rejected/auto_stopped), and a whole-file regex would collect them and agree
	// with itself.
	var stripped strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		stripped.WriteString(line)
		stripped.WriteString("\n")
	}
	body := stripped.String()

	// Scope to THIS CHECK's statement (from the FIRST fail_origin IN list — the Up's
	// widened set — to its closing paren), so the Down section's narrower re-declared
	// CHECK, which appears later in the file, contributes nothing.
	start := strings.Index(body, "fail_origin IN (")
	if start < 0 {
		t.Fatalf("%s no longer declares a fail_origin IN (...) CHECK; the guard is reading "+
			"the wrong thing, or the CHECK was dropped without dropping this test", path)
	}
	end := strings.Index(body[start:], ")")
	if end < 0 {
		t.Fatalf("the fail_origin CHECK in %s has no closing paren", path)
	}
	stmt := body[start : start+end]

	var fromSQL []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(stmt, -1) {
		fromSQL = append(fromSQL, m[1])
	}
	if len(fromSQL) == 0 {
		t.Fatalf("parsed no quoted values out of the fail_origin CHECK in %s; the guard "+
			"is reading the wrong thing", path)
	}

	fromGo := AllFailOrigins()
	sort.Strings(fromSQL)
	sort.Strings(fromGo)
	if strings.Join(fromSQL, ",") != strings.Join(fromGo, ",") {
		t.Fatalf("the fail_origin vocabulary has drifted.\n  Go:  %v\n  SQL: %v\n"+
			"A value Go writes and 00126's CHECK rejects is a constraint violation at a "+
			"failed run's write; add or remove a member on either side and the other must "+
			"move in the same commit.", fromGo, fromSQL)
	}
}

// TestAllFailOriginsIsNotAliasable pins the fresh-slice contract, mirroring
// TestAllRateLimitTypesIsNotAliasable: one caller's append must not rewrite a CLOSED
// set for every other reader.
func TestAllFailOriginsIsNotAliasable(t *testing.T) {
	got := AllFailOrigins()
	if len(got) == 0 {
		t.Fatal("the vocabulary is empty")
	}
	got[0] = "clobbered"
	if AllFailOrigins()[0] == "clobbered" {
		t.Fatal("AllFailOrigins returns an aliasable view of the package-level slice")
	}
}

// TestSetStateFailedStampsReportedOrigin: a worker-reported `failed` with an explicit,
// in-set fail_origin stamps THAT class onto runs.fail_origin (PRD #69 M7a (5)).
func TestSetStateFailedStampsReportedOrigin(t *testing.T) {
	run := runningRun(false)
	fs, svc, wkr := limitParkFixture(t, run)

	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State:      "failed",
		FailOrigin: strPtr("provisioning_failed"),
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.setFailed == nil {
		t.Fatal("SetRunFailed was never called")
	}
	if got := fs.setFailed.FailOrigin; !got.Valid || got.String != "provisioning_failed" {
		t.Fatalf("fail_origin = %+v, want provisioning_failed", got)
	}
}

// TestSetStateFailedDefaultsAgentFailure: a worker-reported `failed` with NO explicit
// origin is the judgeable agent-failure case, so the server defaults it to
// 'agent_failure' rather than storing NULL (PRD #69 M7a (5)).
func TestSetStateFailedDefaultsAgentFailure(t *testing.T) {
	run := runningRun(false)
	fs, svc, wkr := limitParkFixture(t, run)

	worker := "clone failed: exit 128"
	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "failed", FailureReason: &worker,
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.setFailed == nil {
		t.Fatal("SetRunFailed was never called")
	}
	if got := fs.setFailed.FailOrigin; !got.Valid || got.String != "agent_failure" {
		t.Fatalf("fail_origin = %+v, want the agent_failure default", got)
	}
}

// TestSetStateFailedCoercesUnknownOriginToAgentFailure: an UNTRUSTED worker origin
// outside the allowlist must not smuggle a bogus class past the server — it coerces to
// nil, which the failed arm then defaults to 'agent_failure'.
func TestSetStateFailedCoercesUnknownOriginToAgentFailure(t *testing.T) {
	run := runningRun(false)
	fs, svc, wkr := limitParkFixture(t, run)

	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "failed", FailOrigin: strPtr("i-made-this-up"),
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if got := fs.setFailed.FailOrigin; !got.Valid || got.String != "agent_failure" {
		t.Fatalf("fail_origin = %+v, want agent_failure (unknown coerced away)", got)
	}
}

// TestRecoverClaimAssemblyStampsInfraOrigin: the three infra sentinels used to collapse
// into one indistinguishable failed write; PRD #69 M7a (4) splits them so each stamps
// its own TRUSTED class through MarkRunFailedByID.
func TestRecoverClaimAssemblyStampsInfraOrigin(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"credential", errCredentialUnavailable, "credential_unavailable"},
		{"toolpackages", errToolPackagesRejected, "provisioning_failed"},
		{"guardrail", errGuardrailBlockedClaim, "guardrail_blocked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := runningRun(false)
			fs, svc, _ := limitParkFixture(t, run)

			if err := svc.recoverClaimAssembly(context.Background(), run, tc.err); err != nil {
				t.Fatalf("recoverClaimAssembly: %v", err)
			}
			if fs.markedFailed == nil {
				t.Fatal("MarkRunFailedByID was never called")
			}
			if got := fs.markedFailed.FailOrigin; !got.Valid || got.String != tc.want {
				t.Fatalf("fail_origin = %+v, want %s", got, tc.want)
			}
		})
	}
}
