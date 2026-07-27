package store_test

// PRD #113 M4 — the parts of roll health that ONLY a real Postgres can answer, because
// they live in SQL rather than in Go:
//
//   * the upsert's confinement to EXISTING, HOSTED rows (an INSERT ... SELECT, so the
//     guard is the query, not a caller's check);
//   * the SET-IF-NULL on upgrading_since, including that a `settled` report does NOT
//     clear it;
//   * that RegisterWorker clears the anchor ONLY when the version actually MOVES — the
//     INV-5 invariant, expressed as a CTE so the move and the clear are one round trip;
//   * cross-tenancy: the per-user summary cannot see another user's workers, which is
//     structural here because the table has no user_id at all.
//
// The classifier's behaviour GIVEN this persistence is pinned without a database in
// workersvc/upgrade_ceiling_test.go. The split is deliberate: these tests judge the SQL,
// those judge the decision table, and neither can paper over the other.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// rollReport builds an upsert param set for one worker at one instant.
func rollReport(workerID uuid.UUID, phase string, observedAt time.Time) store.UpsertWorkerRollHealthParams {
	return store.UpsertWorkerRollHealthParams{
		WorkerID:             workerID,
		Phase:                phase,
		ObservedAt:           ts(observedAt),
		ControllerReportedAt: ts(observedAt),
		RestartCount:         0,
		WorkerImageTag:       pgtype.Text{String: "0.11.7", Valid: true},
	}
}

func TestWorkerRollHealthPersistenceLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	userID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("roll-%s@e2e", userID))

	// ck_workers_hosted_metadata (migration 00066) requires hosted_size AND
	// template_declared on a hosted row, and forbids hosted_size on an external one.
	seedWorker := func(kind, version string) uuid.UUID {
		id := uuid.New()
		var size any
		if kind == "hosted" {
			size = "m"
		}
		mustExec(ctx, t, pool,
			`INSERT INTO workers (id, user_id, name, token_hash, status, kind, version, hosted_size, template_declared)
			 VALUES ($1, $2, $3, $4, 'offline', $5, $6, $7, 'base')`,
			id, userID, "w-"+id.String()[:8], []byte(id.String()), kind, version, size)
		return id
	}

	hosted := seedWorker("hosted", "0.11.0")
	external := seedWorker("external", "0.11.0")

	t0 := time.Now().UTC().Truncate(time.Second)

	// ---- Confinement: hosted accepts, external and unknown do not. ----
	n, err := q.UpsertWorkerRollHealth(ctx, rollReport(hosted, "rolling", t0))
	if err != nil {
		t.Fatalf("upsert hosted: %v", err)
	}
	if n != 1 {
		t.Fatalf("upsert for a hosted worker affected %d rows, want 1", n)
	}

	n, err = q.UpsertWorkerRollHealth(ctx, rollReport(external, "stuck", t0))
	if err != nil {
		t.Fatalf("upsert external: %v", err)
	}
	if n != 0 {
		t.Errorf("upsert for an EXTERNAL worker affected %d rows, want 0. The controller has no "+
			"jurisdiction over a worker its owner runs by hand; without the kind='hosted' guard it "+
			"could assert upgrade_failed with attacker-authored text against one.", n)
	}

	n, err = q.UpsertWorkerRollHealth(ctx, rollReport(uuid.New(), "stuck", t0))
	if err != nil {
		t.Fatalf("upsert unknown: %v", err)
	}
	if n != 0 {
		t.Errorf("upsert for an UNKNOWN worker id affected %d rows, want 0 — a report must never be "+
			"able to create rows, or a hostile controller grows this table without bound", n)
	}

	// ---- Liveness must be untouched. ----
	var status string
	var lastHeartbeat *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, last_heartbeat_at FROM workers WHERE id = $1`, hosted).
		Scan(&status, &lastHeartbeat); err != nil {
		t.Fatalf("read worker: %v", err)
	}
	if status != "offline" || lastHeartbeat != nil {
		t.Errorf("after a roll report the worker reads status=%q last_heartbeat_at=%v; liveness is "+
			"heartbeat-owned and a report that can reach it lets a lying controller hold a dead worker "+
			"online, which is run-scheduling state", status, lastHeartbeat)
	}

	anchorAfter := func(what string) *time.Time {
		var got *time.Time
		if err := pool.QueryRow(ctx, `SELECT upgrading_since FROM worker_upgrade_reports WHERE worker_id = $1`, hosted).
			Scan(&got); err != nil {
			t.Fatalf("read upgrading_since (%s): %v", what, err)
		}
		return got
	}

	first := anchorAfter("first report")
	if first == nil {
		t.Fatalf("the first non-terminal report did not stamp upgrading_since; the ceiling has no anchor")
	}

	// ---- SET-IF-NULL: a later report keeps the ORIGINAL anchor. ----
	if _, err := q.UpsertWorkerRollHealth(ctx, rollReport(hosted, "rolling", t0.Add(10*time.Minute))); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	second := anchorAfter("second report")
	if second == nil || !second.Equal(*first) {
		t.Errorf("upgrading_since moved from %v to %v on the second report. Re-stamping it does not "+
			"weaken the ceiling, it DELETES it: a controller posting every 10s would satisfy any window "+
			"forever.", first, second)
	}

	// ---- A `settled` report must NOT clear the anchor. ----
	if _, err := q.UpsertWorkerRollHealth(ctx, rollReport(hosted, "settled", t0.Add(20*time.Minute))); err != nil {
		t.Fatalf("settled upsert: %v", err)
	}
	if got := anchorAfter("settled report"); got == nil {
		t.Errorf("a `settled` report cleared upgrading_since. That hands the reset back to the " +
			"controller — report settled once, then resume lying, and the clock restarts — which is " +
			"exactly the forgeability the anchor exists to prevent. Only a register may clear it.")
	}

	// ---- The clear: register with the SAME version must NOT clear. ----
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: hosted, Version: pgtype.Text{String: "0.11.0", Valid: true},
	}); err != nil {
		t.Fatalf("register same version: %v", err)
	}
	if got := anchorAfter("register, unchanged version"); got == nil {
		t.Errorf("registering with an UNCHANGED version cleared the anchor. A register is evidence the " +
			"pod came back; only a version MOVE is evidence the roll completed. Clearing on any register " +
			"opens an unbounded re-arm path, and it is most available exactly where a stuck worker lives: " +
			"a crash-looping agent re-registers on every start.")
	}

	// ---- The clear: register with a MOVED version clears. ----
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: hosted, Version: pgtype.Text{String: "0.11.7", Valid: true},
	}); err != nil {
		t.Fatalf("register moved version: %v", err)
	}
	if got := anchorAfter("register, moved version"); got != nil {
		t.Errorf("upgrading_since = %v after the worker re-registered on a NEW version; the roll "+
			"completed, so the anchor must clear or the next roll gets only the remainder of this "+
			"window", got)
	}

	// ---- Build metadata is NOT a version move. ----
	//
	// The anchor's "the version moved" test and the classifier's "same release" test
	// must agree, and on raw strings they do not: 0.11.7+g1a2b3c4 and 0.11.7+gdeadbeef
	// are different text and an identical release. Un-stripped, a re-registration
	// carrying only a new short-sha would satisfy "the roll completed" while nothing
	// about the release changed — re-arming the ceiling, so the window silently becomes
	// "45 minutes from the latest re-register". That points the UNSAFE way: eager
	// clearing LENGTHENS suppression.
	//
	// The trigger is the scenario the +g<sha> stamp was adopted to expose: a re-cut tag
	// producing two images at one release. Composed with a crash-looping agent, which
	// re-registers on every start, each restart would buy a fresh window.
	if _, err := q.UpsertWorkerRollHealth(ctx, rollReport(hosted, "rolling", t0.Add(25*time.Minute))); err != nil {
		t.Fatalf("re-arm before the metadata check: %v", err)
	}
	if anchorAfter("armed for the metadata check") == nil {
		t.Fatalf("precondition: the anchor must be set before testing that metadata does not clear it")
	}
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: hosted, Version: pgtype.Text{String: "0.11.7+g1a2b3c4", Valid: true},
	}); err != nil {
		t.Fatalf("register with build metadata: %v", err)
	}
	if anchorAfter("register 0.11.7 -> 0.11.7+g1a2b3c4") == nil {
		t.Errorf("registering 0.11.7+g1a2b3c4 over 0.11.7 cleared the anchor. Build metadata is excluded " +
			"from SemVer precedence, so the classifier reads these as the SAME release; the anchor must " +
			"not disagree with it, or the ceiling re-arms on a roll that never happened.")
	}
	// And a DIFFERENT short-sha at the same release must likewise not clear.
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: hosted, Version: pgtype.Text{String: "0.11.7+gdeadbeef", Valid: true},
	}); err != nil {
		t.Fatalf("register with a second build metadata: %v", err)
	}
	if anchorAfter("register +g1a2b3c4 -> +gdeadbeef") == nil {
		t.Errorf("re-registering at the same release with a different short-sha cleared the anchor; a " +
			"re-cut tag must not buy a fresh suppression window")
	}
	// CONTROL: a real release move, carrying metadata on BOTH sides, still clears.
	// Without this the assertions above would pass against a clause that never clears.
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: hosted, Version: pgtype.Text{String: "0.11.8+gdeadbeef", Valid: true},
	}); err != nil {
		t.Fatalf("register a real move: %v", err)
	}
	if got := anchorAfter("register +gdeadbeef -> 0.11.8+gdeadbeef"); got != nil {
		t.Errorf("upgrading_since = %v after a genuine release move (0.11.7 -> 0.11.8, both carrying "+
			"build metadata); stripping metadata must not have disabled the clear altogether", got)
	}

	// A fresh roll re-arms with a NEW anchor — the ceiling is not a one-shot latch.
	if _, err := q.UpsertWorkerRollHealth(ctx, rollReport(hosted, "rolling", t0.Add(30*time.Minute))); err != nil {
		t.Fatalf("re-arm upsert: %v", err)
	}
	rearmed := anchorAfter("re-arm")
	if rearmed == nil {
		t.Fatalf("after the clear, a new roll did not stamp a new anchor")
	}
	if rearmed.Equal(*first) {
		t.Errorf("the re-armed anchor equals the original (%v); a second roll must get a full fresh "+
			"window, not the remainder of the first", first)
	}
}

// Issue #145 — A REPORT MUST NOT BLANK FIELDS IT DID NOT MEASURE.
//
// deriveRollHealth returns `settled` the instant the pod's Ready condition is True,
// BEFORE the blocking-container lookup, so a settled report carries the zero value of
// every diagnostic: blocking_container NULL, blocking_reason NULL, restart_count 0,
// last_exit_code NULL. Written unconditionally into the ON CONFLICT arms, those zeros
// erased the real observation — a worker with 5 restarts and exit 1 persisted as
// pristine, at exactly the moment somebody was reading the row to debug it.
//
// The four columns are ONE measurement, which is why this test moves them as a block:
// deriveRollHealth fills all four from a single container status, or leaves all four
// zero. Keeping a stale blocking_container beside a fresh restart_count would describe
// a row that was never observed, so per-column COALESCE is not the fix and the control
// below (a report carrying only a reason) is what separates the two.
//
// THE PHASE IS THE DISCRIMINATOR, not "the report carries nothing", and the `rolling`
// case below is what separates THOSE two. A `rolling` report with no diagnostics is a
// measurement — the pod-less Recreate gap looked and had nothing to blame — so it must
// blank. Preserving there leaks the previous roll's reason into rollingDetail, which
// reads BlockingReason first, and out through upgrade_detail, which is ungated.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh.
func TestRollReportDoesNotBlankUnmeasuredDiagnosticsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	userID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("diag-%s@e2e", userID))
	workerID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, kind, version, hosted_size, template_declared)
		 VALUES ($1, $2, $3, $4, 'offline', 'hosted', '0.11.0', 'm', 'base')`,
		workerID, userID, "w-"+workerID.String()[:8], []byte(workerID.String()))

	// diag is the diagnostic block as it sits in the row: a nil pointer is SQL NULL.
	type diag struct {
		container *string
		reason    *string
		restarts  int32
		exitCode  *int32
	}
	readDiag := func(what string) diag {
		var d diag
		if err := pool.QueryRow(ctx,
			`SELECT blocking_container, blocking_reason, restart_count, last_exit_code
			   FROM worker_upgrade_reports WHERE worker_id = $1`, workerID).
			Scan(&d.container, &d.reason, &d.restarts, &d.exitCode); err != nil {
			t.Fatalf("read diagnostics (%s): %v", what, err)
		}
		return d
	}
	show := func(d diag) string {
		s := func(p *string) string {
			if p == nil {
				return "NULL"
			}
			return *p
		}
		e := "NULL"
		if d.exitCode != nil {
			e = fmt.Sprintf("%d", *d.exitCode)
		}
		return fmt.Sprintf("container=%s reason=%s restarts=%d exit=%s", s(d.container), s(d.reason), d.restarts, e)
	}
	// report builds an upsert carrying an explicit diagnostic block, the shape the
	// controller sends for a pod it could blame a container for. PhaseSince is stamped
	// from the same instant so the test can prove a report LANDED, not merely that the
	// diagnostics survived it.
	report := func(phase string, at time.Time, container, reason string, restarts int32, exit *int32) store.UpsertWorkerRollHealthParams {
		p := rollReport(workerID, phase, at)
		p.PhaseSince = ts(at)
		p.BlockingContainer = pgtype.Text{String: container, Valid: container != ""}
		p.BlockingReason = pgtype.Text{String: reason, Valid: reason != ""}
		p.RestartCount = restarts
		if exit != nil {
			p.LastExitCode = pgtype.Int4{Int32: *exit, Valid: true}
		}
		return p
	}
	// readLanded returns the fields a report ALWAYS writes, whatever its phase. If a
	// preservation assertion passes while these did not move, the settled report was
	// rejected outright and the test proved nothing about preservation.
	readLanded := func(what string) (phase string, observedAt, phaseSince time.Time) {
		if err := pool.QueryRow(ctx,
			`SELECT phase, observed_at, phase_since FROM worker_upgrade_reports WHERE worker_id = $1`, workerID).
			Scan(&phase, &observedAt, &phaseSince); err != nil {
			t.Fatalf("read landed fields (%s): %v", what, err)
		}
		return phase, observedAt, phaseSince
	}

	t0 := time.Now().UTC().Truncate(time.Second)
	exit1 := int32(1)

	// ---- The real observation lands. ----
	if _, err := q.UpsertWorkerRollHealth(ctx, report("stuck", t0, "agent", "CrashLoopBackOff", 5, &exit1)); err != nil {
		t.Fatalf("upsert the stuck report: %v", err)
	}
	if got := readDiag("the stuck report"); got.container == nil || *got.container != "agent" ||
		got.reason == nil || *got.reason != "CrashLoopBackOff" || got.restarts != 5 ||
		got.exitCode == nil || *got.exitCode != 1 {
		t.Fatalf("precondition: the stuck report did not persist its diagnostics; got %s", show(got))
	}

	// ---- THE BUG: a `settled` report carries zeros and must not write them. ----
	//
	// This is the exact param set the controller produces for a Ready pod — no
	// container, no reason, restart_count 0, no exit code — because the settled arm
	// returns before the blocking-container lookup runs.
	settledAt := t0.Add(time.Minute)
	if _, err := q.UpsertWorkerRollHealth(ctx, report("settled", settledAt, "", "", 0, nil)); err != nil {
		t.Fatalf("upsert the settled report: %v", err)
	}
	// The settled report LANDED. Without this the preservation assertion below is also
	// satisfied by an upsert that rejected the row entirely, which would be a different
	// bug wearing this one's green.
	if phase, observed, since := readLanded("after the settled report"); phase != "settled" ||
		!observed.Equal(settledAt) || !since.Equal(settledAt) {
		t.Errorf("the settled report did not land: phase=%q observed_at=%v phase_since=%v, want settled/%v/%v. "+
			"The diagnostics assertion below cannot distinguish 'preserved' from 'the whole report was rejected' "+
			"on its own.", phase, observed, since, settledAt, settledAt)
	}
	if got := readDiag("after the settled report"); got.container == nil || *got.container != "agent" ||
		got.reason == nil || *got.reason != "CrashLoopBackOff" || got.restarts != 5 ||
		got.exitCode == nil || *got.exitCode != 1 {
		t.Errorf("a `settled` report blanked the diagnostics: %s. It measured NONE of these — the "+
			"settled arm returns before the blocking-container lookup — so every value it carries is a "+
			"zero, not an observation. Writing them makes a worker with 5 restarts and exit 1 read as "+
			"pristine to whoever is debugging it, which is the state they are reading the row FOR.",
			show(got))
	}

	// ---- CONTROL: a report that DID measure still overwrites, and downward. ----
	//
	// Without this the assertion above passes against an arm that never writes these
	// columns at all. The restart count DROPS (5 -> 2), which is what a new pod for the
	// same worker genuinely looks like; a GREATEST-style "keep the highest" preservation
	// would survive every other check here and fail this one.
	exit137 := int32(137)
	if _, err := q.UpsertWorkerRollHealth(ctx, report("stuck", t0.Add(2*time.Minute), "seed-nix", "ImagePullBackOff", 2, &exit137)); err != nil {
		t.Fatalf("upsert the second stuck report: %v", err)
	}
	if got := readDiag("after a measuring report"); got.container == nil || *got.container != "seed-nix" ||
		got.reason == nil || *got.reason != "ImagePullBackOff" || got.restarts != 2 ||
		got.exitCode == nil || *got.exitCode != 137 {
		t.Errorf("a report that DID measure failed to overwrite the previous diagnostics: %s. "+
			"Preservation must be conditional on the report carrying nothing; a column that is never "+
			"written is frozen, not preserved.", show(got))
	}

	// ---- ATOMICITY: the block moves together, so a partial report clears its siblings. ----
	//
	// The pod-less ReplicaFailure branch reports a REASON with no container, no restarts
	// and no exit code (deriveRollHealth's `{Phase: stuck, BlockingReason: replicaFailure}`).
	// That is a measurement, and it says "there is no pod" — so the previous pod's
	// container name and exit code must go, or the row names a container that no longer
	// exists beside a reason about a pod that never started. Per-column COALESCE keeps
	// them and is why this is a CASE over the whole block.
	//
	// `FailedCreate` is an EXAMPLE, not a constant. rollhealth.go carries whatever reason
	// the ReplicaFailure condition holds ("Nothing here hardcodes FailedCreate"), so this
	// fixture must not be read as pinning that string.
	if _, err := q.UpsertWorkerRollHealth(ctx, report("stuck", t0.Add(3*time.Minute), "", "FailedCreate", 0, nil)); err != nil {
		t.Fatalf("upsert the pod-less report: %v", err)
	}
	if got := readDiag("after a pod-less report"); got.container != nil || got.reason == nil ||
		*got.reason != "FailedCreate" || got.restarts != 0 || got.exitCode != nil {
		t.Errorf("the pod-less ReplicaFailure report did not replace the whole diagnostic block: %s. "+
			"All four columns come from ONE container status, so they must move together; a stale "+
			"container name beside a fresh reason describes a row that was never observed.", show(got))
	}

	// ---- THE DISCRIMINATING CASE: a `rolling` report with NOTHING must BLANK. ----
	//
	// This is what separates "the phase decides" from the tempting "the report carries
	// nothing" rule, which fixes the headline symptom and passes every assertion above.
	//
	// deriveRollHealth has two branches that return `rolling` carrying no diagnostics
	// at all: the pod-less Recreate gap (measured ~1.4s, on EVERY release) and the
	// stale-pod-only branch. Both LOOKED and had nothing to blame, so both are
	// measurements. Preserve there and a healthy mid-roll worker inherits the previous
	// roll's reason — and rollingDetail reads BlockingReason FIRST, so R2's
	// upgrade_detail reads "CrashLoopBackOff" for a worker that is fine. That string is
	// not gated the way the three blocking_* DTO fields are: it is the badge's own
	// title attribute, so it renders.
	if _, err := q.UpsertWorkerRollHealth(ctx, report("stuck", t0.Add(4*time.Minute), "agent", "CrashLoopBackOff", 5, &exit1)); err != nil {
		t.Fatalf("re-arm before the rolling check: %v", err)
	}
	if got := readDiag("armed for the rolling check"); got.reason == nil {
		t.Fatalf("precondition: diagnostics must be present before testing that `rolling` blanks them")
	}
	if _, err := q.UpsertWorkerRollHealth(ctx, report("rolling", t0.Add(5*time.Minute), "", "", 0, nil)); err != nil {
		t.Fatalf("upsert the pod-less rolling report: %v", err)
	}
	if got := readDiag("after a diagnostic-free `rolling` report"); got.container != nil || got.reason != nil ||
		got.restarts != 0 || got.exitCode != nil {
		t.Errorf("a `rolling` report carrying no diagnostics did not blank them: %s. Unlike `settled`, a "+
			"rolling report DID run the blocking-container lookup and found nothing to blame — that is an "+
			"observation, not a gap. Preserving here carries the previous roll's reason into a healthy "+
			"Recreate gap, and rollingDetail reads BlockingReason first, so the badge's title would say "+
			"CrashLoopBackOff for a worker that is fine.", show(got))
	}

	// ---- THE CLEAR, half one: a register at an UNCHANGED version keeps them. ----
	//
	// Same rule the INV-5 anchor holds: a register is evidence the pod came back, only a
	// version MOVE is evidence the roll completed. A crash-looping agent re-registers on
	// every start, so clearing on any register would wipe the diagnostics of exactly the
	// worker whose diagnostics are worth reading, several times a minute.
	if _, err := q.UpsertWorkerRollHealth(ctx, report("stuck", t0.Add(6*time.Minute), "seed-nix", "CreateContainerConfigError", 4, &exit137)); err != nil {
		t.Fatalf("re-arm before the register checks: %v", err)
	}
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: workerID, Version: pgtype.Text{String: "0.11.0", Valid: true},
	}); err != nil {
		t.Fatalf("register at the same version: %v", err)
	}
	if got := readDiag("after a register at the same version"); got.container == nil ||
		*got.container != "seed-nix" || got.reason == nil || *got.reason != "CreateContainerConfigError" ||
		got.restarts != 4 {
		t.Errorf("registering at an UNCHANGED version cleared the diagnostics: %s. A crash-looping "+
			"agent re-registers on every start, so this would blank the row of the worst-off worker in "+
			"the fleet on a loop.", show(got))
	}

	// ---- THE CLEAR, half two: a version MOVE clears the whole block. ----
	//
	// The roll completed, so the diagnostics describe a pod that is gone. This is the
	// ONLY legitimate clear — the worker's own authenticated re-registration — which is
	// the same sentence the INV-5 anchor block states, now true of the diagnostics too.
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: workerID, Version: pgtype.Text{String: "0.11.7", Valid: true},
	}); err != nil {
		t.Fatalf("register at a moved version: %v", err)
	}
	if got := readDiag("after a version move"); got.container != nil || got.reason != nil ||
		got.restarts != 0 || got.exitCode != nil {
		t.Errorf("a version move left the diagnostics behind: %s. They describe a pod that no longer "+
			"exists; carrying them into the next release is how a fixed worker keeps looking broken.",
			show(got))
	}

	// ---- THE GUARD: diagnostics with a NULL anchor still clear on a version move. ----
	//
	// The `cleared` CTE used to be guarded by `upgrading_since IS NOT NULL` alone. That
	// was equivalent only by a coupling neither statement states: a report carrying
	// diagnostics is `rolling` or `stuck`, and a non-terminal report always stamps the
	// anchor, so a diagnostics-without-anchor row could not arise. It is therefore not
	// reachable through the public API today, and CONSTRUCTING it is the point — the
	// widened guard is pinned by a test rather than by that coupling, which commit 2
	// is in a position to break the moment a terminal report carries a diagnostic.
	if _, err := q.UpsertWorkerRollHealth(ctx, report("stuck", t0.Add(20*time.Minute), "agent", "CrashLoopBackOff", 9, &exit1)); err != nil {
		t.Fatalf("populate before forcing the anchor NULL: %v", err)
	}
	mustExec(ctx, t, pool, `UPDATE worker_upgrade_reports SET upgrading_since = NULL WHERE worker_id = $1`, workerID)
	var anchor *time.Time
	if err := pool.QueryRow(ctx, `SELECT upgrading_since FROM worker_upgrade_reports WHERE worker_id = $1`, workerID).
		Scan(&anchor); err != nil {
		t.Fatalf("read the forced anchor: %v", err)
	}
	if anchor != nil {
		t.Fatalf("precondition: the anchor must be NULL for this case, got %v", anchor)
	}
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: workerID, Version: pgtype.Text{String: "0.11.8", Valid: true},
	}); err != nil {
		t.Fatalf("register a move against a NULL anchor: %v", err)
	}
	if got := readDiag("version move with a NULL anchor"); got.container != nil || got.reason != nil ||
		got.restarts != 0 || got.exitCode != nil {
		t.Errorf("a version move left the diagnostics behind on a row whose anchor was already NULL: %s. "+
			"The clear's guard must ask whether there is anything to clear, not whether the INV-5 anchor "+
			"happens to be set — otherwise the diagnostics clear silently depends on the upsert's phase "+
			"behaviour rather than on the version move.", show(got))
	}

	// ---- And the clear is not a one-shot: the next incident records again. ----
	//
	// A guard that skipped rows with nothing to clear could, written carelessly, also
	// skip rows that need clearing later. Re-arming here proves the column is still live
	// after the clear.
	if _, err := q.UpsertWorkerRollHealth(ctx, report("stuck", t0.Add(10*time.Minute), "agent", "CrashLoopBackOff", 3, &exit1)); err != nil {
		t.Fatalf("re-arm after the clear: %v", err)
	}
	if got := readDiag("after re-arming"); got.container == nil || *got.container != "agent" || got.restarts != 3 {
		t.Errorf("after the clear a new incident did not record: %s", show(got))
	}
}

// Cross-tenancy. The roll-health table has no user_id, so the ONLY way to read a row is
// through `workers` — which is what makes per-user scoping unavoidable rather than
// remembered. Two users with distinct coordinates, because a single-user fixture passes
// against a query with no WHERE at all.
func TestWorkerUpgradeSummaryIsPerUserLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	mkUser := func(tag string) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			id, fmt.Sprintf("%s-%s@e2e", tag, id))
		return id
	}
	mkWorker := func(user uuid.UUID, version string) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO workers (id, user_id, name, token_hash, status, kind, version, hosted_size, template_declared)
			 VALUES ($1, $2, $3, $4, 'online', 'hosted', $5, 'm', 'base')`,
			id, user, "w-"+id.String()[:8], []byte(id.String()), version)
		return id
	}

	alice, bob := mkUser("alice"), mkUser("bob")
	aliceWorker := mkWorker(alice, "0.11.7")
	bobWorker := mkWorker(bob, "0.11.0")

	now := time.Now().UTC()
	// Bob's worker is the STUCK one. If the query leaked, Alice would see a failing
	// worker she does not own — a nav badge counting someone else's problem.
	if _, err := q.UpsertWorkerRollHealth(ctx, rollReport(bobWorker, "stuck", now)); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	if _, err := q.UpsertWorkerRollHealth(ctx, rollReport(aliceWorker, "settled", now)); err != nil {
		t.Fatalf("upsert alice: %v", err)
	}

	rows, err := q.GetWorkerUpgradeSummaryForUser(ctx, alice)
	if err != nil {
		t.Fatalf("summary for alice: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("alice sees %d workers, want exactly her own 1", len(rows))
	}
	if rows[0].WorkerID != aliceWorker {
		t.Errorf("alice's summary returned worker %s, want %s", rows[0].WorkerID, aliceWorker)
	}
	if rows[0].Phase.Valid && rows[0].Phase.String == "stuck" {
		t.Errorf("alice's summary carries the STUCK phase that belongs to bob's worker — the join " +
			"through workers is not scoping by owner")
	}

	// And the mute is per-user too: Alice muting her worker must not mute Bob's.
	if err := q.MuteWorkerUpgrade(ctx, store.MuteWorkerUpgradeParams{
		UserID: alice, WorkerID: aliceWorker, Release: "0.11.7",
	}); err != nil {
		t.Fatalf("mute alice: %v", err)
	}
	// Alice attempting to mute BOB's worker writes nothing: the (worker, user) match in
	// the INSERT ... SELECT is the authorization, exactly as notifications does it.
	if err := q.MuteWorkerUpgrade(ctx, store.MuteWorkerUpgradeParams{
		UserID: alice, WorkerID: bobWorker, Release: "0.11.7",
	}); err != nil {
		t.Fatalf("cross-user mute returned an error; it should simply write nothing: %v", err)
	}
	var mutes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM worker_upgrade_mutes WHERE worker_id = $1`, bobWorker).
		Scan(&mutes); err != nil {
		t.Fatalf("count bob mutes: %v", err)
	}
	if mutes != 0 {
		t.Errorf("alice muted bob's worker (%d rows); the (user_id, worker_id) match IS the "+
			"authorization and it did not hold", mutes)
	}
}

// B-2: the mute must work for EXTERNAL workers, who are the population it exists for.
//
// They never auto-upgrade, so they sit `outdated` indefinitely — the PRD's own
// alert-fatigue risk. The original join key was COALESCE(r.worker_image_tag, ”), and the
// roll-health upsert is confined to kind='hosted', so an external worker had no report,
// its key was always ” and a mute stored against any real release silently never
// matched. A HOSTED fixture passes while the feature is dead for its actual population,
// which is why this test uses an external worker and moves its version.
func TestExternalWorkerMuteIsKeyedOnItsOwnVersionLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	userID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("mute-%s@e2e", userID))
	workerID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, kind, version)
		 VALUES ($1, $2, $3, $4, 'online', 'external', '0.11.0')`,
		workerID, userID, "ext-"+workerID.String()[:8], []byte(workerID.String()))

	muted := func(what string) bool {
		rows, err := q.GetWorkerUpgradeSummaryForUser(ctx, userID)
		if err != nil {
			t.Fatalf("summary (%s): %v", what, err)
		}
		if len(rows) != 1 {
			t.Fatalf("summary (%s) returned %d rows, want 1", what, len(rows))
		}
		return rows[0].Muted
	}

	if muted("before any mute") {
		t.Fatalf("precondition: the worker must start unmuted")
	}

	// The user mutes it at the version it is actually running. For a worker with no
	// controller-reported tag, THAT is what "release" means: the fact being muted is
	// "this worker runs 0.11.0 and I accept that".
	if err := q.MuteWorkerUpgrade(ctx, store.MuteWorkerUpgradeParams{
		UserID: userID, WorkerID: workerID, Release: "0.11.0",
	}); err != nil {
		t.Fatalf("mute: %v", err)
	}
	if !muted("after muting 0.11.0") {
		t.Errorf("an EXTERNAL worker's mute did not take effect. The join key must fall back to the " +
			"worker's own version: an external worker has no roll-health row, so a key derived only from " +
			"worker_image_tag is always '' and no real release value can ever match it — the mute is dead " +
			"for exactly the population that needs it.")
	}

	// ACROSS A VERSION CHANGE: the worker is upgraded by its owner. The muted fact no
	// longer holds, so the mute must stop matching rather than silencing the row forever.
	// Keying on '' would have made it permanent, contradicting the whole point of
	// scoping a mute to a release.
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: workerID, Version: pgtype.Text{String: "0.11.7", Valid: true},
	}); err != nil {
		t.Fatalf("register the upgraded version: %v", err)
	}
	if muted("after the worker moved to 0.11.7") {
		t.Errorf("the mute survived the worker moving from 0.11.0 to 0.11.7. A mute that outlives the " +
			"thing it muted silences a row permanently, which is how an alert stops being trusted.")
	}
}

// M7, closing the gap the M5/M6 warranty did not cover: the mute's EXTERNAL arm expiring.
//
// The composition that makes this the load-bearing case, and no single milestone stated it:
// Decision 5 makes the mute the mitigation for exactly the external-worker problem — they
// never auto-upgrade, so `outdated` fires forever and the badge becomes wallpaper. The nav
// badge is the surface that would become wallpaper. So the badge depends on the one arm of
// COALESCE(worker_image_tag, version, ”) that had no test.
//
// The earlier mute tests exercised per-user ISOLATION and jurisdiction, not key EXPIRY, and
// the external worker they seeded was never muted. A hosted fixture passes every step here,
// which is the same blindness B-2 itself was: "the mute cannot match for external workers"
// became "the fix's external arm is untested, and external is the case it exists for".
func TestExternalMuteExpiresOnVersionMoveAndTheCountReturnsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	userID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("mute-expiry-%s@e2e", userID))
	workerID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, kind, version)
		 VALUES ($1, $2, $3, $4, 'online', 'external', '0.11.0')`,
		workerID, userID, "ext-"+workerID.String()[:8], []byte(workerID.String()))

	// The count the nav badge would show: rows that are muted are subtracted, so this
	// stands in for "does the badge go quiet, and does it come back".
	mutedRows := func(what string) bool {
		rows, err := q.GetWorkerUpgradeSummaryForUser(ctx, userID)
		if err != nil {
			t.Fatalf("summary (%s): %v", what, err)
		}
		if len(rows) != 1 {
			t.Fatalf("summary (%s): %d rows, want 1", what, len(rows))
		}
		return rows[0].Muted
	}

	if err := q.MuteWorkerUpgrade(ctx, store.MuteWorkerUpgradeParams{
		UserID: userID, WorkerID: workerID, Release: "0.11.0",
	}); err != nil {
		t.Fatalf("mute at 0.11.0: %v", err)
	}
	if !mutedRows("muted at its own version") {
		t.Fatalf("the external worker's mute never applied, so the badge could not go quiet — the " +
			"COALESCE fallback to w.version is not reaching the join")
	}

	// The owner upgrades it by hand. The muted FACT — "this worker runs 0.11.0" — is no
	// longer true, so the mute must stop applying and the badge must come back.
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: workerID, Version: pgtype.Text{String: "0.11.7", Valid: true},
	}); err != nil {
		t.Fatalf("register the upgraded version: %v", err)
	}
	if mutedRows("after the worker moved to 0.11.7") {
		t.Errorf("the mute survived the worker moving 0.11.0 -> 0.11.7. A mute keyed on a release " +
			"must expire when that release changes, or it silences the row permanently and the badge " +
			"stops meaning anything — which is the alert-fatigue the mute exists to PREVENT, inverted.")
	}

	// CONTROL, so the assertion above cannot pass against a join that simply never matches:
	// muting the NEW version silences it again.
	if err := q.MuteWorkerUpgrade(ctx, store.MuteWorkerUpgradeParams{
		UserID: userID, WorkerID: workerID, Release: "0.11.7",
	}); err != nil {
		t.Fatalf("mute at 0.11.7: %v", err)
	}
	if !mutedRows("re-muted at the new version") {
		t.Errorf("muting the worker's NEW version did not apply; the previous assertion may have been " +
			"passing because the join never matches at all rather than because the key expired")
	}
}
