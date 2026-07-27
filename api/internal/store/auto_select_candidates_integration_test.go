package store_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/autoselect"
	"gitlab.example.com/vtmocanu/uzi/api/internal/autoselectrow"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestAutoSelectCandidatesLiveDB pins PRD #111 M4's store half against a REAL
// Postgres: the ranking query and migration 00089's CHECK.
//
// 🔴 THE FIRST ASSERTION IS THAT THE QUERY RUNS AT ALL, and it is not padding.
// `sqlc generate` passing proves nothing about whether Postgres will accept the
// statement — measured on this repo, a query generated clean Go, built, vetted, and
// then failed the first time it was PREPARED with SQLSTATE 42P08. So "sqlc
// regenerated cleanly" is not a measurement of this query, and nothing short of
// executing it against a server is.
//
// What the fake store cannot see, in order:
//
//  1. The statement prepares and executes.
//  2. `COALESCE(f.n, 0)::bigint` really is a non-NULL int64 — the cast is the repo's
//     measured expression-inference trap, and without it the field can generate as
//     interface{} and be unusable.
//  3. Both LEFT JOINs. A token with NO gauge row must APPEAR (classified no_reading,
//     which is R7's silent no-op made visible) and a token with NO runs must yield 0
//     rather than vanish — which is precisely when it should be winning.
//  4. auto_eligible is projected, NOT filtered. The gate lives in one place
//     (autoselect.Classify, D21); an un-pooled token must come back and classify
//     not_pooled, so the ranker can tell "you pooled nothing" from "you pooled
//     tokens that are all stale".
//  5. The in-flight rollup counts EVERY lane and EVERY reason, counts
//     awaiting_approval, and excludes terminal and queued runs (D18).
//  6. Owner scoping. The outer WHERE is what keeps another tenant's tokens out of
//     this user's ranking. The rollup subquery's own @user_id is measured too, but
//     honestly: see the note beside that assertion — it is defense in depth, and the
//     composite FK is the thing that actually makes a cross-tenant count impossible.
//  7. Migration 00089's CHECK accepts all eight reasons and NULL, and rejects
//     anything else.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestAutoSelectCandidatesLiveDB(t *testing.T) {
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

	owner, stranger := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{owner, stranger} {
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("autosel-%s@e2e", u))
	}
	mkSecret := func(user uuid.UUID, label string, isDefault, pooled bool) uuid.UUID {
		row, serr := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
			UserID: user, Kind: store.KindAnthropicToken, Label: label, WantDefault: isDefault,
			Ciphertext: []byte("ct-" + label), SealedWith: store.SealedWithMaster,
		})
		if serr != nil {
			t.Fatalf("insert secret %q: %v", label, serr)
		}
		if pooled {
			if _, aerr := q.SetUserSecretAutoEligible(ctx, store.SetUserSecretAutoEligibleParams{
				ID: row.ID, UserID: user, Kind: store.KindAnthropicToken, AutoEligible: true,
			}); aerr != nil {
				t.Fatalf("pool %q: %v", label, aerr)
			}
		}
		return row.ID
	}

	// The design note's SEVEN discriminating shapes, one per behaviour the two queries
	// could disagree on. Authored deliberately, not snapshotted: a set drawn from
	// whatever demo data happens to hold does not discriminate, which is the whole
	// point of the table.
	//
	//   busy    (1,7) pooled, fresh reading, runs on it across three lanes
	//   idle    (1)   pooled, fresh reading, no runs at all — the rollup's 0 case
	//   unread  (2)   pooled, NO gauge row at all — the gauge LEFT JOIN's miss
	//   halfnul (3)   pooled, a row whose five_hour_pct is NULL — D12
	//   aged    (4)   pooled, a complete reading with an ancient synced_at
	//   noreset (6)   pooled, complete and fresh, both resets_at NULL — +inf
	//   unpool  (5)   NOT pooled, a perfect reading — must still be RETURNED
	//   foreign       another tenant's, for the scoping assertions
	busy := mkSecret(owner, "busy-key", true, true)
	idle := mkSecret(owner, "idle-key", false, true)
	unread := mkSecret(owner, "unread-key", false, true)
	halfnul := mkSecret(owner, "half-measured-key", false, true)
	aged := mkSecret(owner, "aged-key", false, true)
	noreset := mkSecret(owner, "no-reset-key", false, true)
	unpool := mkSecret(owner, "reserved-key", false, false)
	foreign := mkSecret(stranger, "stranger-key", true, true)

	synced := time.Now().UTC().Add(-time.Minute)
	mkGauge := func(secret uuid.UUID, user uuid.UUID, five, seven int16) {
		if uerr := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
			UserSecretID:     secret,
			UserID:           user,
			FiveHourPct:      pgtype.Int2{Int16: five, Valid: true},
			SevenDayPct:      pgtype.Int2{Int16: seven, Valid: true},
			FiveHourResetsAt: pgtype.Timestamptz{Time: synced.Add(time.Hour), Valid: true},
			SevenDayResetsAt: pgtype.Timestamptz{Time: synced.Add(72 * time.Hour), Valid: true},
			Source:           pgtype.Text{String: "usage_endpoint", Valid: true},
			SyncedAt:         pgtype.Timestamptz{Time: synced, Valid: true},
		}); uerr != nil {
			t.Fatalf("upsert gauge: %v", uerr)
		}
	}
	mkGauge(busy, owner, 40, 10)
	mkGauge(idle, owner, 5, 5)
	mkGauge(unpool, owner, 0, 0)
	mkGauge(foreign, stranger, 0, 0)

	// Case 3 (D12): a row exists and measured only ONE window. This also catches a
	// query that COALESCEs a NULL pct to 0, which would silently turn "we know
	// nothing" into "this token is completely free" — the single worst direction for
	// this bug, since an unmeasured token would then win every ranking.
	if uerr := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
		UserSecretID: halfnul, UserID: owner,
		FiveHourPct: pgtype.Int2{}, // NULL
		SevenDayPct: pgtype.Int2{Int16: 30, Valid: true},
		Source:      pgtype.Text{String: "usage_endpoint", Valid: true},
		SyncedAt:    pgtype.Timestamptz{Time: synced, Valid: true},
	}); uerr != nil {
		t.Fatalf("upsert half-measured gauge: %v", uerr)
	}

	// Case 4: a complete reading that aged out. Catches a query that omits synced_at
	// entirely — every other case would still pass, because Classify's other gates
	// would answer first.
	if uerr := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
		UserSecretID: aged, UserID: owner,
		FiveHourPct: pgtype.Int2{Int16: 10, Valid: true},
		SevenDayPct: pgtype.Int2{Int16: 10, Valid: true},
		Source:      pgtype.Text{String: "usage_endpoint", Valid: true},
		SyncedAt:    pgtype.Timestamptz{Time: synced.Add(-90 * 24 * time.Hour), Valid: true},
	}); uerr != nil {
		t.Fatalf("upsert aged gauge: %v", uerr)
	}

	// Case 6: fresh and fully measured, but the poller reported no reset for either
	// window (00080 makes both nullable). +inf must survive the round trip as NULL
	// rather than arriving as a zero time, which would sort BEFORE every real reset
	// and make this token win every tie — the exact inversion of the rule.
	if uerr := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
		UserSecretID: noreset, UserID: owner,
		FiveHourPct: pgtype.Int2{Int16: 20, Valid: true},
		SevenDayPct: pgtype.Int2{Int16: 20, Valid: true},
		// both resets_at deliberately left NULL
		Source:   pgtype.Text{String: "usage_endpoint", Valid: true},
		SyncedAt: pgtype.Timestamptz{Time: synced, Valid: true},
	}); uerr != nil {
		t.Fatalf("upsert no-reset gauge: %v", uerr)
	}

	connID, repoID := uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	iid := int64(0)
	mkIssueRun := func(user uuid.UUID, secret uuid.UUID, status, reason string) {
		iid++
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status,
			                   anthropic_secret_id, anthropic_secret_label, anthropic_select_reason)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6, 'lbl', $7)`,
			uuid.New(), user, repoID, iid, status, secret, reason)
	}
	mkChatRun := func(user uuid.UUID, secret uuid.UUID, status string) {
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status,
			                   anthropic_secret_id, anthropic_secret_label, anthropic_select_reason)
			 VALUES ($1, $2, 'chat', 't', 'd', $3, $4, 'lbl', 'default')`,
			uuid.New(), user, status, secret)
	}

	// D18, all three sub-decisions in one fixture. Counted: claimed, running, and
	// awaiting_approval; a CHAT run (a lane that only became countable when M1 made
	// every lane record its credential); and a run whose credential was chosen by a
	// FALLBACK, because filtering on the reason would blind the bias to exactly the
	// pile-up a fallback creates.
	mkIssueRun(owner, busy, "claimed", "auto")
	mkIssueRun(owner, busy, "running", "auto")
	mkIssueRun(owner, busy, "awaiting_approval", "pool_stale")
	mkChatRun(owner, busy, "running")
	// NOT counted: not concurrent spend.
	mkIssueRun(owner, busy, "queued", "auto")
	mkIssueRun(owner, busy, "completed", "auto")
	mkIssueRun(owner, busy, "failed", "auto")
	mkIssueRun(owner, busy, "cancelled", "auto")
	// NOT counted: another tenant's run, even against a token id in THIS query's
	// outer scope would be wrong — here it is the stranger's own token, and the
	// stranger's runs must not appear in the owner's rollup at all.
	mkIssueRun(stranger, foreign, "running", "auto")

	// --- 1. The statement prepares and executes ------------------------------
	rows, err := q.ListAutoSelectCandidates(ctx, owner)
	if err != nil {
		t.Fatalf("ListAutoSelectCandidates: %v\n"+
			"a green `sqlc generate` does not mean the server accepts the statement; this is where that "+
			"is measured", err)
	}

	byID := map[uuid.UUID]store.ListAutoSelectCandidatesRow{}
	for _, r := range rows {
		byID[r.UserSecretID] = r
	}
	// --- 4 + 6. Every one of the owner's tokens, pooled or not; none of the
	// stranger's.
	if len(rows) != 7 {
		t.Fatalf("got %d candidate rows, want 7 (the design note's seven shapes): %+v", len(rows), rows)
	}
	if _, leaked := byID[foreign]; leaked {
		t.Fatal("the stranger's token appeared in the owner's candidate list; the outer WHERE is not owner-scoped")
	}
	if r, ok := byID[unpool]; !ok || r.AutoEligible {
		t.Fatalf("the un-pooled token is %+v (present=%v); it must be RETURNED with auto_eligible=false, "+
			"not filtered out in SQL — the gate lives in autoselect.Classify (D21)", r, ok)
	}

	// --- 3. The LEFT JOIN to the gauge: a never-polled token is present, with a
	// NULL reading rather than an absent row.
	un, ok := byID[unread]
	if !ok {
		t.Fatal("a pooled token with no gauge row vanished; an INNER JOIN would do exactly this, and it " +
			"would drop precisely the token whose silent ineligibility R7 is about")
	}
	if un.SyncedAt.Valid || un.FiveHourPct.Valid || un.SevenDayPct.Valid {
		t.Fatalf("never-polled token came back with a reading: %+v", un)
	}

	// --- 2 + 3. The rollup: COALESCEd to a real 0, never NULL, never a missing row.
	id := byID[idle]
	if id.InFlightRuns != 0 {
		t.Fatalf("idle token in_flight_runs = %d, want 0", id.InFlightRuns)
	}
	if un.InFlightRuns != 0 {
		t.Fatalf("never-polled token in_flight_runs = %d, want 0", un.InFlightRuns)
	}

	// --- 5. Four counted, five not.
	if got := byID[busy].InFlightRuns; got != 4 {
		t.Fatalf("busy token in_flight_runs = %d, want 4 (claimed + running + awaiting_approval + chat).\n"+
			"queued/completed/failed/cancelled must not count (they are not concurrent spend), and "+
			"awaiting_approval MUST (the worker holds the session and resumes on the same token, D18c)", got)
	}

	// --- 6, second scope. The stranger's own list is theirs alone, and their
	// running run counts for them — which is what proves the owner's 4 was not an
	// accident of the stranger having no runs.
	srows, serr := q.ListAutoSelectCandidates(ctx, stranger)
	if serr != nil {
		t.Fatalf("ListAutoSelectCandidates(stranger): %v", serr)
	}
	if len(srows) != 1 || srows[0].UserSecretID != foreign {
		t.Fatalf("stranger's candidates = %+v, want exactly their own token", srows)
	}
	if srows[0].InFlightRuns != 1 {
		t.Fatalf("stranger's in_flight_runs = %d, want 1 — if this is 0 the rollup counts nothing at "+
			"all and the owner's 4 above proved less than it looked", srows[0].InFlightRuns)
	}

	// 🔴 A CORRECTION, kept rather than quietly dropped, because the wrong version of
	// it is the kind of assertion that reads as proof forever.
	//
	// This test first claimed that removing the rollup subquery's own `@user_id`
	// would "leak another tenant's in-flight count onto this user's ranking". A
	// mutation run says otherwise: deleting that predicate changes NOTHING, and the
	// guard SURVIVED. The reason is 00086's composite FK — (user_id,
	// anthropic_secret_id) → user_secrets (user_id, id) — which makes a run owned by
	// B referencing A's token unrepresentable. So inside a subquery already joined on
	// `f.sid = s.id` where `s.user_id = @user_id`, the run's own user_id is implied.
	//
	// The predicate stays (it is free, it narrows the scan, and it states the intent
	// where a reader will look), but it is DEFENSE IN DEPTH and the FK is the real
	// mechanism. That is what is asserted below — the property that is actually load
	// bearing, rather than the one that merely sounded like it.
	_, ferr := pool.Exec(ctx,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status,
		                   anthropic_secret_id, anthropic_secret_label)
		 VALUES ($1, $2, $3, 9001, 't', 'd', 'running', $4, 'lbl')`,
		uuid.New(), stranger, repoID, busy)
	if ferr == nil {
		t.Fatal("a run owned by the stranger recorded the OWNER's credential. 00086's composite FK is " +
			"what makes cross-tenant in-flight counting impossible; without it the rollup's own " +
			"@user_id would be the only thing standing between tenants, and it is not written as a " +
			"security boundary")
	}
	if !strings.Contains(ferr.Error(), "runs_anthropic_secret_fk") {
		t.Fatalf("the cross-tenant insert failed for the wrong reason: %v", ferr)
	}

	// --- The two queries agree, which is what D21 rests on --------------------
	// M6 owes the full seven-case fixture table from the design note; this is the
	// two-case floor that comes free with M4's query and is here so the seam is
	// measured the day it lands rather than three milestones later. EXTEND this,
	// do not duplicate it.
	//
	// Deliberately NOT whole-struct equality: InFlight is the one field the two
	// queries are ALLOWED to differ on (only the ranking query counts runs), so
	// asserting equality outright would be unsatisfiable and asserting it after
	// zeroing InFlight is the honest form.
	meters, merr := q.ListRateLimitsForUser(ctx, owner)
	if merr != nil {
		t.Fatalf("ListRateLimitsForUser: %v", merr)
	}
	if len(meters) != len(rows) {
		t.Fatalf("the settings query returned %d tokens and the ranking query %d; they enumerate the "+
			"same set by contract", len(meters), len(rows))
	}
	for _, m := range meters {
		fromMeter := autoselectrow.FromRateLimitRow(m)
		fromRank := autoselectrow.FromCandidateRow(byID[m.UserSecretID])
		if fromRank.InFlight == 0 && m.UserSecretID == busy {
			t.Fatal("the ranking query lost the in-flight count; the differential below would then pass " +
				"for the wrong reason")
		}
		fromRank.InFlight = 0 // the ONE permitted difference
		if why, same := sameCandidate(fromMeter, fromRank); !same {
			t.Fatalf("the two queries disagree about %v: %s\n  settings: %+v\n  ranking:  %+v",
				m.UserSecretID, why, fromMeter, fromRank)
		}
	}

	// --- Every fixture case is actually EXERCISED ----------------------------
	// A meta-assertion, and not ceremony: the seven shapes above are only worth
	// authoring if each one reaches Classify as the state it was built to represent.
	// A fixture that silently degenerates — a NULL that arrived as 0, an ancient
	// synced_at that the query dropped — still produces a row, still passes the
	// differential (both queries would degenerate identically), and quietly stops
	// discriminating. Asserting the STATUS is what pins the shape to its purpose.
	//
	// MaxStaleness is 15m here so `aged` classifies stale on its own merits rather
	// than because a zero policy makes everything stale, which would let three of
	// these cases pass for the same wrong reason.
	classifyPolicy := autoselect.Policy{MinHeadroom: 15, HeadroomTiePct: 5, MaxStaleness: 15 * time.Minute}
	for _, tc := range []struct {
		name   string
		id     uuid.UUID
		want   autoselect.Status
		shapes string
	}{
		{"busy", busy, autoselect.StatusEligible, "case 1+7: fresh, measurable, with in-flight runs"},
		{"idle", idle, autoselect.StatusEligible, "case 1: fresh and measurable"},
		{"unread", unread, autoselect.StatusNoReading, "case 2: no gauge row at all"},
		{"half-measured", halfnul, autoselect.StatusUnmeasured, "case 3: one window NULL (D12)"},
		{"aged", aged, autoselect.StatusStale, "case 4: the reading aged out"},
		{"no-reset", noreset, autoselect.StatusEligible, "case 6: fresh, measurable, NULL resets"},
		{"reserved", unpool, autoselect.StatusNotPooled, "case 5: not opted in"},
	} {
		got := autoselect.Classify(autoselectrow.FromCandidateRow(byID[tc.id]), classifyPolicy, time.Now())
		if got.Status != tc.want {
			t.Errorf("%s classified %q, want %q — %s. The fixture has degenerated and is no "+
				"longer discriminating anything", tc.name, got.Status, tc.want, tc.shapes)
		}
	}
	// Case 6's payload specifically: NULL must arrive as nil, not as a zero time.
	if c := autoselectrow.FromCandidateRow(byID[noreset]); c.FiveResetsAt != nil || c.SevenResetsAt != nil {
		t.Errorf("a NULL reset round-tripped as %v/%v, want nil on both — a zero time is a real, "+
			"orderable instant that sorts before every genuine reset", c.FiveResetsAt, c.SevenResetsAt)
	}

	// --- The A/B/C = 80/75/70 intransitivity case, end to end ----------------
	// R1 at the STORE layer. The unit test proves Select's arithmetic; this proves the
	// same answer survives the real query, the real pgtype unwrapping and the real
	// row order — which is where a future refactor would put a sort.Slice over an
	// intransitive `less` and where the unit fixtures could not see it.
	//
	// C carries by far the soonest reset, so any implementation that lets it into the
	// tie cluster picks it. With T=5 the cluster is {80,75} and B wins on its reset.
	abcOwner := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		abcOwner, fmt.Sprintf("abc-%s@e2e", abcOwner))
	abc := map[string]uuid.UUID{}
	for _, tc := range []struct {
		label   string
		pct     int16
		resetIn time.Duration
	}{
		{"a-80", 20, 2 * time.Hour},
		{"b-75", 25, time.Hour},
		{"c-70", 30, time.Minute}, // the bait
	} {
		id := mkSecret(abcOwner, tc.label, false, true)
		abc[tc.label] = id
		if uerr := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
			UserSecretID: id, UserID: abcOwner,
			FiveHourPct:      pgtype.Int2{Int16: tc.pct, Valid: true},
			SevenDayPct:      pgtype.Int2{Int16: 0, Valid: true},
			FiveHourResetsAt: pgtype.Timestamptz{Time: time.Now().UTC().Add(tc.resetIn), Valid: true},
			Source:           pgtype.Text{String: "usage_endpoint", Valid: true},
			SyncedAt:         pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		}); uerr != nil {
			t.Fatalf("upsert %s gauge: %v", tc.label, uerr)
		}
	}
	abcRows, aerr := q.ListAutoSelectCandidates(ctx, abcOwner)
	if aerr != nil {
		t.Fatalf("ListAutoSelectCandidates(abc): %v", aerr)
	}
	if len(abcRows) != 3 {
		t.Fatalf("got %d A/B/C rows, want 3", len(abcRows))
	}
	abcCands := make([]autoselect.Candidate, 0, 3)
	for _, r := range abcRows {
		abcCands = append(abcCands, autoselectrow.FromCandidateRow(r))
	}

	// 🔴 EVERY PERMUTATION, and the reason is a measured near-miss rather than rigour
	// for its own sake. The first version of this ran Select once, over the query's
	// own `ORDER BY s.id` — and the ids are gen_random_uuid(), so the order was
	// random per run. Under the cluster-guard mutation that fixture picks C for SOME
	// orderings and B for others, which makes it a FLAKY guard: it would have passed
	// here, passed in CI, and failed for somebody once a month. Measured: with the
	// guard removed, order (A,B,C) yields C while (C,A,B) yields B.
	//
	// That order-dependence is not an artefact of the test — it IS the intransitivity
	// symptom. So asserting over all 3! orderings tests the property directly and
	// deterministically, which is strictly better than pinning the row order would
	// have been.
	perms := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, perm := range perms {
		shuffled := []autoselect.Candidate{abcCands[perm[0]], abcCands[perm[1]], abcCands[perm[2]]}
		out := autoselect.Select(shuffled, classifyPolicy, time.Now())
		if !out.Picked {
			t.Fatalf("A/B/C order %v picked nothing (reason %q)", perm, out.Reason)
		}
		if out.SecretID == abc["c-70"] {
			t.Fatalf("order %v picked the 70-headroom token through the real query. It is 10 points "+
				"outside a 5-point tie window, and only an INTRANSITIVE pairwise comparator lets it "+
				"in (A ties B, B ties C, A does not tie C) — R1, and the reason the ranking is "+
				"anchored to H* rather than compared pairwise", perm)
		}
		if out.SecretID != abc["b-75"] {
			t.Fatalf("order %v picked %v, want b-75 — the cluster is {80,75} and 75 resets sooner",
				perm, out.SecretID)
		}
	}

	// --- 7. Migration 00089's CHECK -----------------------------------------
	// The column is display-only — nothing in the state machine, the claim path or
	// any sweep reads it — so the database is the ONLY reader positioned to notice a
	// value that is not in the vocabulary.
	var legal []string
	for _, r := range []string{"default", "pinned", "judge"} {
		legal = append(legal, r)
	}
	for _, r := range autoselect.AllReasons() {
		legal = append(legal, string(r))
	}
	for _, reason := range legal {
		iid++
		if _, xerr := pool.Exec(ctx,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status,
			                   anthropic_select_reason)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', $5)`,
			uuid.New(), owner, repoID, iid, reason); xerr != nil {
			t.Fatalf("00089's CHECK rejected %q, which Go writes: %v", reason, xerr)
		}
	}
	iid++
	_, xerr := pool.Exec(ctx,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status,
		                   anthropic_select_reason)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', 'not_a_reason')`,
		uuid.New(), owner, repoID, iid)
	if xerr == nil {
		t.Fatal("00089's CHECK accepted 'not_a_reason'; the constraint is not doing anything")
	}
	if !strings.Contains(xerr.Error(), "runs_anthropic_select_reason_check") {
		t.Fatalf("a bogus reason failed for the wrong reason: %v", xerr)
	}
	// NULL stays legal: every run that predates 00086 has one, and so does any run
	// that never reached a claim assemble point.
	iid++
	if _, nerr := pool.Exec(ctx,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued')`,
		uuid.New(), owner, repoID, iid); nerr != nil {
		t.Fatalf("00089's CHECK rejected a NULL reason: %v", nerr)
	}
}

// sameCandidate compares two Candidates BY VALUE, and it exists because `==` does
// not.
//
// 🔴 autoselect.Candidate is `==`-comparable and comparing it that way is a bug that
// compiles, passes vet, and produces a diff whose two halves look identical. Four of
// its fields are POINTERS (*int16, *time.Time), so `==` compares ADDRESSES — and two
// Candidates built from two different query rows never share one. Measured while
// writing this file: the differential below failed with `settings:` and `ranking:`
// lines that were character-identical except for two `0xc000…` values that fmt prints
// for *int16 while it silently DEREFERENCES *time.Time through its String method, so
// the timestamps looked equal and the pcts looked like noise.
//
// The failure direction is the merciful one — `==` can only be too strict here, never
// too lax — but M6 owes this file five more fixture cases and will reach for `==`
// again, so the helper is here rather than the trap.
func sameCandidate(a, b autoselect.Candidate) (string, bool) {
	// 🔴 THE FIELD COUNT IS THE OTHER HALF OF THIS HELPER, and without it the whole
	// differential is one struct change away from being decorative. The comparison
	// below enumerates ten fields BY HAND; add an eleventh to autoselect.Candidate and
	// it is silently uncompared, so D21's test — the entire reason this seam exists —
	// would go on agreeing about a field it never looked at. That failure is invisible
	// in every existing fixture, which is precisely the class this repo keeps getting
	// caught by.
	//
	// It matters more than usual here because this file's own comment tells M6 to
	// EXTEND it with five more fixture cases, and M5/M6 are the milestones most likely
	// to widen Candidate. Reflection is the Go-side equivalent of the exhaustive
	// Record<AutoStatus, …> the web already gets from its typechecker.
	if n := reflect.TypeOf(autoselect.Candidate{}).NumField(); n != 10 {
		return fmt.Sprintf("autoselect.Candidate has %d fields and this comparison covers 10 by "+
			"hand; add the new one below or the differential silently stops checking it", n), false
	}
	i16 := func(p *int16) string {
		if p == nil {
			return "NULL"
		}
		return fmt.Sprintf("%d", *p)
	}
	ts := func(p *time.Time) string {
		if p == nil {
			return "NULL"
		}
		return p.UTC().Format(time.RFC3339Nano)
	}
	for _, f := range []struct{ name, x, y string }{
		{"SecretID", a.SecretID.String(), b.SecretID.String()},
		{"Label", a.Label, b.Label},
		{"AutoEligible", fmt.Sprint(a.AutoEligible), fmt.Sprint(b.AutoEligible)},
		{"HasReading", fmt.Sprint(a.HasReading), fmt.Sprint(b.HasReading)},
		{"FiveHourPct", i16(a.FiveHourPct), i16(b.FiveHourPct)},
		{"SevenDayPct", i16(a.SevenDayPct), i16(b.SevenDayPct)},
		{"FiveResetsAt", ts(a.FiveResetsAt), ts(b.FiveResetsAt)},
		{"SevenResetsAt", ts(a.SevenResetsAt), ts(b.SevenResetsAt)},
		{"SyncedAt", ts(a.SyncedAt), ts(b.SyncedAt)},
		{"InFlight", fmt.Sprint(a.InFlight), fmt.Sprint(b.InFlight)},
	} {
		if f.x != f.y {
			return fmt.Sprintf("%s differs (%s vs %s)", f.name, f.x, f.y), false
		}
	}
	return "", true
}
