package workersvc

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
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
	// guardrail_blocked/forge_unreachable into the trusted classification, nor steer Gate 4b
	// via a forged guardrail_blocked/forge_unreachable. Each must still be a real stored member
	// (else the split is stale). forge_unreachable (PRD #1392 M1) is server-derived: SetState's
	// forge-park transaction stamps it directly, never the worker.
	serverOnly := []string{"worker_lost", "run_timeout", "plan_rejected", "auto_stopped", "guardrail_blocked", "forge_unreachable"}
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
	// The CURRENT fail_origin CHECK is declared by the LATEST migration that widened it, so
	// this DISCOVERS that migration at runtime instead of pinning a number that goes stale at
	// every landing-time renumber. Discovery returns the exact Up-section CHECK expression it
	// matched, so unrelated DML containing fail_origin IN (...) cannot become the parsed source.
	path, stmt := latestFailOriginCheckMigration(t, "../store/migrations")

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

// latestFailOriginCheckMigration discovers the migration that declares the CURRENT
// fail_origin CHECK: the highest-numbered migration under dir whose Up section contains an
// actual CHECK expression over fail_origin. It returns that same expression for parsing, so
// discovery and comparison cannot disagree about which SQL construct they selected.
func latestFailOriginCheckMigration(t *testing.T, dir string) (string, string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	best, bestStmt, bestNum := "", "", -1
	for _, file := range files {
		num, ok := migrationNumber(filepath.Base(file))
		if !ok {
			continue
		}
		raw, err := os.ReadFile(file) //nolint:gosec // G304: test reads migration files from the fixed repo-relative ../store/migrations dir, never user input
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		stmt, ok := upSectionFailOriginCheck(string(raw))
		if num > bestNum && ok {
			best, bestStmt, bestNum = file, stmt, num
		}
	}
	if best == "" {
		t.Fatalf("no migration under %s declares a fail_origin IN (...) CHECK in its Up "+
			"section; the scan is reading the wrong directory or the CHECK vanished. A broken "+
			"scan that finds nothing must fail here, not pass vacuously", dir)
	}
	return best, bestStmt
}

// migrationNumber parses the leading run of digits of a goose migration basename (its
// version). A basename that does not start with a digit is not a migration.
func migrationNumber(base string) (int, bool) {
	end := 0
	for end < len(base) && base[end] >= '0' && base[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(base[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

var failOriginCheckRE = regexp.MustCompile(
	`(?s)\bCHECK[[:space:]]*\([[:space:]]*fail_origin[[:space:]]+IN[[:space:]]*\(([^)]*)\)[[:space:]]*\)`,
)

// upSectionFailOriginCheck returns the value-list body of the actual fail_origin CHECK in
// the migration's Up section. Comment lines and the Down section are excluded first. Matching
// CHECK (...) rather than a bare fail_origin IN (...) prevents unrelated DML from becoming
// the vocabulary source.
func upSectionFailOriginCheck(raw string) (string, bool) {
	up := strings.Index(raw, "-- +goose Up")
	if up < 0 {
		return "", false
	}
	section := raw[up:]
	if down := strings.Index(section, "-- +goose Down"); down >= 0 {
		section = section[:down]
	}
	var stripped strings.Builder
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		stripped.WriteString(line)
		stripped.WriteString("\n")
	}
	match := failOriginCheckRE.FindStringSubmatch(stripped.String())
	if len(match) != 2 {
		return "", false
	}
	return match[1], true
}

func TestUpSectionFailOriginCheckIgnoresDMLAndDown(t *testing.T) {
	raw := `-- +goose Up
UPDATE runs SET failure_reason = 'x' WHERE fail_origin IN ('dml_only');
ALTER TABLE runs ADD CONSTRAINT runs_fail_origin_check
    CHECK (fail_origin IN ('agent_failure', 'worker_lost')) NOT VALID;
-- +goose Down
ALTER TABLE runs ADD CONSTRAINT runs_fail_origin_check
    CHECK (fail_origin IN ('down_only'));
`
	stmt, ok := upSectionFailOriginCheck(raw)
	if !ok {
		t.Fatal("Up-section fail_origin CHECK was not found")
	}
	if strings.Contains(stmt, "dml_only") || strings.Contains(stmt, "down_only") {
		t.Fatalf("matched unrelated fail_origin list: %q", stmt)
	}
	if !strings.Contains(stmt, "'agent_failure', 'worker_lost'") {
		t.Fatalf("matched CHECK body = %q, want the Up-section constraint values", stmt)
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

// TestHumanLandableFailOriginsExact pins humanLandableFailOrigins to its EXACT membership
// (issue #1418): the finalize-time publish failures whose committed work a human can land.
// An accidental add or drop (which would silently widen or narrow the needs_landing surface)
// reddens here. assertFailOriginSetExact additionally proves every member is a real stored
// fail_origin, so the set is a STRICT SUBSET of the vocabulary.
func TestHumanLandableFailOriginsExact(t *testing.T) {
	assertFailOriginSetExact(t, "humanLandableFailOrigins", humanLandableFailOrigins,
		"finalize_base_align_conflict", "workflow_scope_missing", "push_secret_blocked", "history_rewritten")
}

// TestAllHumanLandableFailOrigins pins the SQL-bind-array accessor (issue #1418): it returns
// exactly the human-landable set, in failOrigins source order, and — mirroring
// TestAllFailOriginsIsNotAliasable — as a FRESH slice one caller's append cannot clobber.
func TestAllHumanLandableFailOrigins(t *testing.T) {
	got := AllHumanLandableFailOrigins()
	// Membership matches the map exactly.
	if len(got) != len(humanLandableFailOrigins) {
		t.Fatalf("AllHumanLandableFailOrigins len = %d, want %d", len(got), len(humanLandableFailOrigins))
	}
	for _, o := range got {
		if !humanLandableFailOrigins[o] {
			t.Errorf("AllHumanLandableFailOrigins returned %q, not in the human-landable set", o)
		}
	}
	// Source order: the returned order is the order those members appear in failOrigins.
	var wantOrder []string
	for _, o := range AllFailOrigins() {
		if humanLandableFailOrigins[o] {
			wantOrder = append(wantOrder, o)
		}
	}
	if strings.Join(got, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("AllHumanLandableFailOrigins order = %v, want failOrigins source order %v", got, wantOrder)
	}
	// Fresh slice: mutating the result must not affect a later call.
	if len(got) > 0 {
		got[0] = "clobbered"
		if AllHumanLandableFailOrigins()[0] == "clobbered" {
			t.Fatal("AllHumanLandableFailOrigins returns an aliasable view")
		}
	}
}

// TestDeriveLandingState is the exhaustive truth table for the ONE landing_state derivation
// (issue #1418): EVERY origin in the vocabulary × hasPreservedPatch × hasAvailableCapture,
// plus the nil-failOrigin case. Expected: none if the origin is not human-landable (or nil);
// else needs_landing if (patch || capture) else unrecoverable.
func TestDeriveLandingState(t *testing.T) {
	// nil fail_origin is always none, regardless of the availability facts.
	for _, patch := range []bool{false, true} {
		for _, capture := range []bool{false, true} {
			if got := DeriveLandingState(nil, patch, capture); got != LandingStateNone {
				t.Errorf("DeriveLandingState(nil, patch=%t, capture=%t) = %q, want %q",
					patch, capture, got, LandingStateNone)
			}
		}
	}
	for _, origin := range AllFailOrigins() {
		landable := IsHumanLandableFailOrigin(origin)
		for _, patch := range []bool{false, true} {
			for _, capture := range []bool{false, true} {
				want := LandingStateNone
				if landable {
					if patch || capture {
						want = LandingStateNeedsLanding
					} else {
						want = LandingStateUnrecoverable
					}
				}
				o := origin
				if got := DeriveLandingState(&o, patch, capture); got != want {
					t.Errorf("DeriveLandingState(%q, patch=%t, capture=%t) = %q, want %q",
						origin, patch, capture, got, want)
				}
			}
		}
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

// TestSetStateFailedCancelledRoutesToCancel (PRD #503 M1, REC A): a LIVE worker cannot
// report a `cancelled` status, so a consumed cancel verdict arrives as `failed`. Because
// the loaded run carries stop_kind='cancelled' (stamped by CreateStopVerdictInput before
// the report), the failed arm must route to CancelRunByWorker (status 'cancelled',
// fail_origin NULL) instead of SetRunFailed — so an operator cancel is not mis-classified
// as agent_failure (and is not judged).
func TestSetStateFailedCancelledRoutesToCancel(t *testing.T) {
	run := runningRun(false)
	run.StopKind = pgconv.TextOrNull("cancelled")
	fs, svc, wkr := limitParkFixture(t, run)

	worker := "run cancelled"
	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "failed", FailureReason: &worker,
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.cancelledByWorker == nil {
		t.Fatal("CancelRunByWorker was never called for a stop_kind='cancelled' failed report")
	}
	if fs.cancelledByWorker.ID != run.ID {
		t.Fatalf("CancelRunByWorker id = %v, want %v", fs.cancelledByWorker.ID, run.ID)
	}
	if fs.setFailed != nil {
		t.Fatalf("SetRunFailed was called for a cancelled stop_kind (should route to cancel): %+v", fs.setFailed)
	}
}

// TestSetStateFailedPlanRejectedStampsPlanRejected (PRD #503 M1, REC A): a live plan-reject
// arrives as `failed` with stop_kind='plan_rejected'; the failed arm must stamp
// fail_origin='plan_rejected' via SetRunFailed (matching the server-side RejectRunServerSide
// path) rather than defaulting to agent_failure, and must NOT route to CancelRunByWorker.
func TestSetStateFailedPlanRejectedStampsPlanRejected(t *testing.T) {
	run := runningRun(false)
	run.StopKind = pgconv.TextOrNull("plan_rejected")
	fs, svc, wkr := limitParkFixture(t, run)

	worker := "plan rejected: not aligned with the issue"
	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "failed", FailureReason: &worker,
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.setFailed == nil {
		t.Fatal("SetRunFailed was never called for a stop_kind='plan_rejected' failed report")
	}
	if got := fs.setFailed.FailOrigin; !got.Valid || got.String != "plan_rejected" {
		t.Fatalf("fail_origin = %+v, want plan_rejected", got)
	}
	if fs.cancelledByWorker != nil {
		t.Fatalf("CancelRunByWorker was called for a plan_rejected stop_kind (should fail with plan_rejected): %+v", fs.cancelledByWorker)
	}
}

// TestSetStateFailedStoppedRoutesToCancel (PRD #517 M4 review): a graceful stop stamped
// stop_kind='stopped' before the report. Its happy path reports `completed`, but on the
// edge where the worker's finalize (push/MR) throws — or a cancel-then-stop lets the cancel
// win — the worker reports `failed`. The failed arm must route THAT to CancelRunByWorker
// (status 'cancelled', fail_origin NULL) exactly like the cancelled arm, NOT to SetRunFailed:
// so a deliberate wind-down is never labeled fail_origin='agent_failure' and, landing
// 'cancelled', is excluded from judging by maybeEnqueueJudge's Gate 0.
//
// MUTATION PROOF: remove the `case "stopped"` arm and this report falls to the default,
// calling SetRunFailed with fail_origin='agent_failure' (fs.setFailed set, fs.cancelledByWorker nil).
func TestSetStateFailedStoppedRoutesToCancel(t *testing.T) {
	run := runningRun(false)
	run.StopKind = pgconv.TextOrNull("stopped")
	fs, svc, wkr := limitParkFixture(t, run)

	worker := "finalize failed: push rejected"
	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "failed", FailureReason: &worker,
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.cancelledByWorker == nil {
		t.Fatal("CancelRunByWorker was never called for a stop_kind='stopped' failed report")
	}
	if fs.cancelledByWorker.ID != run.ID {
		t.Fatalf("CancelRunByWorker id = %v, want %v", fs.cancelledByWorker.ID, run.ID)
	}
	if fs.setFailed != nil {
		t.Fatalf("SetRunFailed was called for a stopped stop_kind (should route to cancel, never agent_failure): %+v", fs.setFailed)
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
