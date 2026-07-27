package workersvc

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/autoselect"
	"gitlab.example.com/vtmocanu/uzi/api/internal/autoselectrow"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Anthropic usage-limit park (PRD #35): the server's half of the rate_limit_type
// vocabulary.
//
// The worker reports `rate_limit_type` as UNTRUSTED free text — it is whatever the
// SDK frame carried, forwarded verbatim on purpose so the authoritative copy of the
// vocabulary lives on this side of the wire rather than in the agent. Everything
// that reaches the database, a DTO, the run feed or Slack passes through
// CoerceRateLimitType first.
//
// WHY THE ALLOWLIST IS THE WHOLE SANITIZER. The state path has no
// sanitizeSelfReported (that is the register path's), and this field deliberately
// does not get stripNULParam either. It does not need it: an allowlist that maps
// everything outside a seven-member set to the literal "unknown" is strictly
// stronger than any stripping filter, because no worker-controlled byte survives it
// at all. Length caps, control characters, NUL, and injection are closed in one
// move rather than four.
//
// WHY THIS LIVES IN workersvc RATHER THAN IN ITS OWN PACKAGE, unlike
// autoselect.Reason, whose comment argues the opposite for a superficially similar
// vocabulary. Reason has THREE enumerating guards (workersvc's, cmd/uzi's, and the
// web's), two of which must not import the server, so a shared home was the only
// way to stop it drifting. rate_limit_type has exactly one producer — this package,
// on the state path — and no consumer that must enumerate it: the DTO, the CLI and
// the Slack line all render whatever string they are handed, and are required to
// render an unrecognised one honestly rather than dropping it. One producer, one
// home.
//
// 🔴 THE TRIGGER FOR REVISITING THAT, WRITTEN DOWN SO IT IS A DECIDABLE RULE RATHER
// THAN A JUDGEMENT CALL: if a consumer ever needs to ENUMERATE this vocabulary, it
// moves to its own package rather than being mirrored. `slacksvc` deliberately holds
// no workersvc import (stated twice in slacksvc/gate.go), so the single consumer this
// PRD schedules — M5's Slack line — is precisely the package that could not import
// this home if it ever needed the set rather than the string. And the repo has
// already paid that cost once for exactly this shape: slacksvc/health.go hand-mirrors
// workersvc's reasonPersistFailing, pinned by a drift test whose failure message has
// to tell you to update both. Mirroring is the outcome to avoid; a speculative
// package for a single producer is worse than this comment; so the line is drawn at
// enumeration.

// RateLimitTypeUnknown is what every unrecognised report becomes. It is a real
// member of the stored vocabulary (migration 00090's CHECK admits it), not a NULL:
// "the run was rejected by a window we could not name" and "the run never parked"
// are different facts and a support query must be able to tell them apart.
const RateLimitTypeUnknown = "unknown"

// rateLimitTypes is the closed set migration 00090's CHECK enforces: the six
// members of SDKRateLimitInfo.rateLimitType at the pinned
// @anthropic-ai/claude-agent-sdk (sdk.d.ts), plus RateLimitTypeUnknown.
//
// An SDK pin bump adding a seventh member is the ONE thing that moves this list,
// and it must move 00090 in the same commit — TestRateLimitTypeVocabularyMatchesCheck
// parses the CHECK out of the migration file and compares, so forgetting either
// half reddens at `go test` instead of raising 23514 on a user's run.
var rateLimitTypes = []string{
	"five_hour",
	"seven_day",
	"seven_day_opus",
	"seven_day_sonnet",
	"seven_day_overage_included",
	"overage",
	RateLimitTypeUnknown,
}

// rateLimitTypeSet is the lookup form. Built once; rateLimitTypes stays the
// declaration so the vocabulary reads as a list in source order.
var rateLimitTypeSet = func() map[string]bool {
	m := make(map[string]bool, len(rateLimitTypes))
	for _, t := range rateLimitTypes {
		m[t] = true
	}
	return m
}()

// AllRateLimitTypes returns the whole vocabulary, in a form a guard can enumerate.
//
// Returns a fresh slice for the same reason autoselect.AllReasons does: a
// package-level var would let one caller's append corrupt every other reader's view
// of a CLOSED set.
func AllRateLimitTypes() []string {
	out := make([]string, len(rateLimitTypes))
	copy(out, rateLimitTypes)
	return out
}

// CoerceRateLimitType maps a worker-reported rate_limit_type onto the stored
// vocabulary.
//
// Absent (nil) stays absent — a report that carried no type must not invent
// "unknown", because a NULL column and an "unknown" column mean different things
// (see RateLimitTypeUnknown). Present-but-unrecognised becomes "unknown", never an
// error: per the design brief's §7.8 constraint, a terminal report is NEVER failed
// on a technicality, mirroring runningStateParams' stated principle rather than
// departing from it.
func CoerceRateLimitType(reported *string) *string {
	if reported == nil {
		return nil
	}
	if rateLimitTypeSet[*reported] {
		return reported
	}
	unknown := RateLimitTypeUnknown
	return &unknown
}

// -------------------------------------------------------------------------
// The park computation (PRD #35 M2, Decisions 4, 5, 6e and 8)
// -------------------------------------------------------------------------

// Jitter bounds for retry_not_before.
//
// 🔴 THIS IS LOAD-BEARING, NOT STAMPEDE-AVOIDANCE GARNISH (ADR-35 D4). It is the
// ONLY mechanism that spreads a promoted wave across a user's credential pool. Do
// not remove it as redundant with the sweeper tick — that reasoning is exactly
// backwards, because PromoteLimitWaitRuns is a SINGLE UPDATE that releases every
// eligible row in one tick. The tick does not stagger anything; it is what makes the
// staggering necessary.
//
// How it composes with the in-flight bias, which is the part that is easy to miss:
// the claim path is ClaimRun → assembleClaim → autoChoice → rank →
// recordRunCredential, so a run that claimed earlier has already RECORDED its pick
// before a later run ranks, and ListAutoSelectCandidates' load counter then sees it.
// Jitter separates the promotions; claims serialize; each pick lands before the next
// ranking reads it. The jitter and the counter are ONE mechanism, not two — which is
// also why the counter must not be widened to count parked runs (see
// deadCredentialReset's note below and ADR-35 D4).
//
// Accepted residual: two promotions closer together than one claim round-trip can
// still converge on the same credential. That race predates this PRD — PRD #111
// accepted it in the in-flight bias' own comment — and D4 makes it likelier by
// correlating promotions. If it fires and the token has real headroom, both runs
// simply run; only if the token is near-empty does the second re-park, costing one
// unit of RUN_LIMIT_MAX_WAITS. IF IT EVER BITES, THE KNOB IS THIS RANGE, NOT THE
// COUNTER. The signal is SweepResult.LimitPromoted > 1 on a single tick, which is
// already reported and needs no new instrumentation.
//
// The floor is non-zero on purpose: it is also the clamp applied when the computed
// stamp lands at or before `now`, so a park can never produce a stamp the very next
// promotion pass already satisfies.
const (
	limitParkJitterMin = 60 * time.Second
	limitParkJitterMax = 180 * time.Second
)

// limitParkFallback is the base used when NOTHING says when the window reopens:
// the worker reported no usable reset, the gauge has none either (or the credential
// is unknown), and no pooled alternative contributes. That case is real rather than
// defensive — the classifier's PRIMARY signal is `terminal_reason`, which names the
// cause outright and carries no reset of its own, so a limit death with no timestamp
// is an ordinary outcome, not a malformed report.
//
// The value is bounded from both sides and neither bound is arbitrary. Too short and
// a genuinely sustained limit burns all of RUN_LIMIT_MAX_WAITS in minutes and fails a
// run that would have succeeded an hour later; too long and a run waits out a window
// that reopened almost immediately. At 15 minutes the whole default budget spans
// about 75 minutes of parking before the run fails honestly, which is a defensible
// ceiling for the no-information case.
//
// Deliberately a CONSTANT and not an env knob: it is the answer to "we know nothing",
// and an operator with something to say about their limits has RUN_LIMIT_MAX_WAITS
// and RUN_LIMIT_MAX_PARK to say it with.
const limitParkFallback = 15 * time.Minute

// reportedResetBounds brackets a worker-reported limit_resets_at, which arrives as
// epoch MILLISECONDS (the worker normalizes the SDK's unit-less number before
// sending; see agent/src/limit.ts normalizeResetsAt).
//
// The range check is not hygiene, it is the thing that stops the park loop. An
// out-of-range value — zero, negative, a seconds value mistaken for milliseconds, a
// wrapped or overflowed epoch — converts to an instant in 1970 or in the year 50000.
// The first gives retry_not_before <= now, so the very next promotion pass requeues
// the run straight back into the exhausted window, it re-parks, and the cycle burns
// RUN_LIMIT_MAX_WAITS in seconds. The second is caught by the RUN_LIMIT_MAX_PARK
// ceiling, but only after it has been through time.UnixMilli, which is why both ends
// are checked HERE, before conversion, rather than relying on the clamp downstream.
//
// The window is deliberately wide (2001-09-09 to 2100) rather than "near now": this
// is a plausibility filter on the UNIT and the encoding, not a policy on how far out
// a reset may be. That policy is RUN_LIMIT_MAX_PARK's, and keeping the two separate
// is what lets the ceiling be an operator knob while this stays a constant.
const (
	reportedResetMinMs int64 = 1_000_000_000_000 // 2001-09-09; below this it is not milliseconds
	reportedResetMaxMs int64 = 4_102_444_800_000 // 2100-01-01; above this it is not a real reset
)

// validReportedReset converts a worker-reported epoch-millisecond reset, or reports
// that there is none to be had. Absent and implausible collapse to the same answer on
// purpose: both mean "the worker told us nothing usable", and the caller's fallback
// is identical for the two.
func validReportedReset(ms *int64) (time.Time, bool) {
	if ms == nil || *ms < reportedResetMinMs || *ms > reportedResetMaxMs {
		return time.Time{}, false
	}
	return time.UnixMilli(*ms).UTC(), true
}

// limitParkInput is everything the park decision reads. It is a struct rather than
// twelve arguments because the decision is PURE and its test fixtures are built by
// varying one field at a time.
type limitParkInput struct {
	// WaitOnLimit is the run's own opt-in, read from the row — never from the report.
	WaitOnLimit bool
	// LimitWaitCount is how many times this run has ALREADY parked.
	LimitWaitCount int32
	// DeadSecretID is the credential the run was spending, from runs.anthropic_secret_id.
	// uuid.Nil for a run that predates PRD #111 or whose credential recording failed;
	// NextAvailable refuses that case rather than letting the dead credential vote for
	// its own replacement.
	DeadSecretID uuid.UUID
	// ReportedResetMs and ReportedType are the worker's UNTRUSTED report.
	ReportedResetMs *int64
	ReportedType    *string
	// Candidates is this user's pool as ListAutoSelectCandidates returns it, including
	// the dead credential (which carries the gauge row the Decision 4 cross-check
	// reads). Empty is legal and means "no pool information".
	Candidates []autoselect.Candidate
	Policy     autoselect.Policy
	MaxWaits   int
	MaxPark    time.Duration
	// Jitter is supplied by the caller so this function has no clock and no
	// randomness of its own, which is what makes every case below assertable.
	Jitter time.Duration
	Now    time.Time
}

// limitParkDecision is the answer: park until RetryNotBefore, or fail with Reason.
//
// LimitResetsAt and RateLimitType are the SANITIZED report and are populated on BOTH
// outcomes — the park stores them on the row, and the failure path uses them to
// compose Reason. They are nil when the worker reported nothing usable.
type limitParkDecision struct {
	Park           bool
	RetryNotBefore time.Time
	LimitResetsAt  *time.Time
	RateLimitType  *string
	Reason         string
}

// decideLimitPark is the WHOLE park policy, and it is pure: no clock, no randomness,
// no database. The caller fetches the candidates, reads the clock, rolls the jitter
// and executes the resulting statement.
//
// # The three ways a park becomes a failure, and why none of them is a REFUSAL
//
// A refusal (0 rows) maps to 409, and the worker treats 409 as "the server already
// moved on" and stops — so a refused report VANISHES: no failure reason, no feed
// line, and the run rots at `running` until RUN_TIMEOUT. Every one of these three is
// therefore a server-side FAIL with a composed reason, delivered as a 200 whose body
// carries status "failed", which the worker can see and act on:
//
//   - the run opted out (WaitOnLimit false). The worker's own opt-out path is to
//     report `failed` directly and is the normal one; this is the guard behind it,
//     for a stale worker or a bug.
//   - the retry budget is spent (LimitWaitCount >= MaxWaits). MaxWaits == 0 is the
//     operator's "never park" switch and lands here on the first limit, which is
//     exactly today's behaviour with a better reason.
//   - the computed stamp is further out than MaxPark. Waiting is not free — a parked
//     run holds its issue's one-active lock and its worker's disk — so past the
//     ceiling, failing honestly beats holding both for weeks.
//
// # Where the base comes from, in order
//
//  1. the worker's reported reset, if it survives validReportedReset;
//  2. cross-checked against the DEAD credential's own gauge row (Decision 4): when
//     that candidate is Measured, take max(worker, gauge) for the window the
//     rate_limit_type names. Both describe the SAME window, so promoting before the
//     later of them guarantees a re-park. The window mapping is what stops `max` from
//     over-delaying by days — a 5-hour limit is never compared against a 7-day
//     rollover;
//  3. lowered to autoselect.NextAvailable's floor when the user has a pooled
//     alternative that is spendable sooner (Decision 6e);
//  4. limitParkFallback when steps 1 and 2 produced nothing and step 3 did not fire.
//
// Then jitter is added and the result is floored at Now+Jitter, so a stamp that would
// land in the past cannot spin the promotion pass.
func decideLimitPark(in limitParkInput) limitParkDecision {
	d := limitParkDecision{RateLimitType: CoerceRateLimitType(in.ReportedType)}
	if reset, ok := validReportedReset(in.ReportedResetMs); ok {
		d.LimitResetsAt = &reset
	}

	if !in.WaitOnLimit {
		d.Reason = limitFailureReason(d.RateLimitType, d.LimitResetsAt, "")
		return d
	}
	if int(in.LimitWaitCount) >= in.MaxWaits {
		d.Reason = limitFailureReason(d.RateLimitType, d.LimitResetsAt,
			fmt.Sprintf("usage-limit retry budget exhausted after %d attempt(s)", in.LimitWaitCount))
		return d
	}

	base, haveBase := in.Now, false
	if d.LimitResetsAt != nil {
		base, haveBase = *d.LimitResetsAt, true
	}
	if gauge, ok := deadCredentialReset(in.Candidates, in.DeadSecretID, in.Policy, d.RateLimitType, in.Now); ok {
		if !haveBase || gauge.After(base) {
			base, haveBase = gauge, true
		}
	}
	// Decision 6e. Only ever LOWERS the base — a pooled alternative cannot make the
	// wait longer, and NextAvailable's own contract is a floor on "something is
	// spendable", not a promise.
	if alt, ok := autoselect.NextAvailable(in.Candidates, in.DeadSecretID, in.Policy, in.Now); ok {
		if !haveBase || alt.Before(base) {
			base, haveBase = alt, true
		}
	}
	if !haveBase {
		base = in.Now.Add(limitParkFallback)
	}

	retry := base.Add(in.Jitter)
	// The floor. A reset already in the past is not an error to reject — it is a
	// window that reopened while the report was in flight, and the right answer is
	// "retry shortly", not "fail the run".
	if floor := in.Now.Add(in.Jitter); !retry.After(floor) {
		retry = floor
	}
	if retry.Sub(in.Now) > in.MaxPark {
		d.Reason = limitFailureReason(d.RateLimitType, d.LimitResetsAt,
			fmt.Sprintf("the window reopens further out than the %s maximum park", in.MaxPark))
		return d
	}

	d.Park, d.RetryNotBefore = true, retry
	return d
}

// deadCredentialReset is the Decision 4 cross-check: the DEAD credential's own gauge
// reading of the window that rejected the run.
//
// It reads the row ListAutoSelectCandidates already returned, so there is no second
// fetch. Only a MEASURED candidate contributes — Measured implies fresh and both
// windows present, which is precisely the condition under which the gauge is worth
// believing against the worker's report.
//
// The window mapping is the load-bearing half. seven_day_opus, seven_day_sonnet and
// seven_day_overage_included are all seven-day windows under different names, so they
// map to the same column. `overage` and `unknown` have NO gauge column at all
// (anthropic_rate_limits stores the five-hour and seven-day windows only), and they
// return false rather than falling back to some other window: cross-checking a
// 5-hour reading against an overage rejection would produce a confidently wrong
// answer, which is worse than no cross-check.
func deadCredentialReset(cands []autoselect.Candidate, dead uuid.UUID, p autoselect.Policy, rateLimitType *string, now time.Time) (time.Time, bool) {
	if dead == uuid.Nil || rateLimitType == nil {
		return time.Time{}, false
	}
	for _, c := range cands {
		if c.SecretID != dead {
			continue
		}
		if !autoselect.Classify(c, p, now).Measured {
			return time.Time{}, false
		}
		var col *time.Time
		switch *rateLimitType {
		case "five_hour":
			col = c.FiveResetsAt
		case "seven_day", "seven_day_opus", "seven_day_sonnet", "seven_day_overage_included":
			col = c.SevenResetsAt
		default: // overage, unknown — no gauge column names this window
			return time.Time{}, false
		}
		if col == nil {
			return time.Time{}, false
		}
		return *col, true
	}
	return time.Time{}, false
}

// limitFailureReason composes Decision 8's sentence SERVER-SIDE, from the allowlisted
// enum and the validated timestamp.
//
// 🔴 THE WORKER MUST NOT COMPOSE THIS. If it did, the enum would live worker-side and
// receive only sanitizeFailureReason (stripNUL plus a rune cap), and the Success
// Criterion "a compromised worker cannot smuggle a non-enum rate_limit_type past the
// server" would be false on exactly the path that renders it to a human. One
// allowlist, both the park path and the failed path.
//
// Each part is omitted rather than defaulted when it is unknown, so the sentence
// never claims a fact the server does not have: no type means no parenthetical, no
// reset means no "resets at" clause.
func limitFailureReason(rateLimitType *string, resetsAt *time.Time, detail string) string {
	s := "Anthropic usage limit"
	if rateLimitType != nil {
		s += " (" + *rateLimitType + ")"
	}
	s += " reached"
	if resetsAt != nil {
		s += "; resets at " + resetsAt.UTC().Format(time.RFC3339)
	}
	if detail != "" {
		s += "; " + detail
	}
	return s
}

// limitParkJitter rolls one jitter value. The ONLY nondeterminism in the park path,
// isolated to this call so decideLimitPark stays pure and every case in its test is
// an exact-value assertion rather than a range check.
//
// math/rand rather than crypto/rand deliberately: this is stampede avoidance, not a
// secret. An attacker who could predict it would learn when their own run resumes.
func limitParkJitter() time.Duration {
	return limitParkJitterMin + time.Duration(rand.Int63n(int64(limitParkJitterMax-limitParkJitterMin+1)))
}

// setLimitWait is SetState's `limit_wait` arm: the impure half of the park.
//
// It returns the row count SetState maps to `applied`, and it NEVER returns an
// applied=false for a policy decision. That distinction is the whole reason this
// function exists rather than the SQL being called inline:
//
//	0 rows  the SQL guard refused (not `running`, wrong worker, a judge run, or a
//	        re-delivery). SetState maps that to 409, which the worker treats as
//	        "the server already moved on" and stops. Correct for those causes: the
//	        run really did move, and the worker's cleanup carve-out keys off the
//	        RETURNED STATUS, so a 409 carrying anything other than "limit_wait"
//	        makes it clean up rather than leak.
//	1 row   either the park landed, or the run was FAILED here on purpose. Both are
//	        200s carrying the resulting status, which is what lets the worker tell
//	        "parked, keep the disk" from "failed, clean up" — the discrimination an
//	        `applied`-keyed branch would get wrong on the three most common causes.
//
// 🔴 THE OPT-OUT COERCION IS THE POINT, NOT A NICETY. If a worker reports
// limit_wait for a run with wait_on_limit=false (a stale worker, a bug), refusing it
// would produce 0 rows → 409 → the worker treats it as success and exits, and the
// park VANISHES: no failure reason, no feed line, and the run rots at `running`
// until RUN_TIMEOUT fails it with something misleading. So the report is coerced to
// a server-composed failure instead of refused. Same branch point as the budget cap.
func (s *Service) setLimitWait(ctx context.Context, run store.Run, wkr store.Worker, req StateRequest, sessionID pgtype.Text) (int64, error) {
	// The candidate pool is fetched ONLY when the run names the credential it was
	// spending. Without that id there is nothing to exclude, and NextAvailable would
	// have to be trusted not to let the dead credential vote for its own replacement
	// — a query saved is beside the point; this is the correctness half.
	var cands []autoselect.Candidate
	dead := uuid.Nil
	if run.AnthropicSecretID.Valid {
		dead = uuid.UUID(run.AnthropicSecretID.Bytes)
		rows, err := s.q.ListAutoSelectCandidates(ctx, run.UserID)
		if err != nil {
			return 0, fmt.Errorf("auto-select candidates: %w", err)
		}
		cands = make([]autoselect.Candidate, 0, len(rows))
		for _, row := range rows {
			cands = append(cands, autoselectrow.FromCandidateRow(row))
		}
	}

	d := decideLimitPark(limitParkInput{
		WaitOnLimit:     run.WaitOnLimit,
		LimitWaitCount:  run.LimitWaitCount,
		DeadSecretID:    dead,
		ReportedResetMs: req.LimitResetsAt,
		ReportedType:    req.RateLimitType,
		Candidates:      cands,
		Policy:          s.p.Autoselect,
		MaxWaits:        s.p.RunLimitMaxWaits,
		MaxPark:         s.p.RunLimitMaxPark,
		Jitter:          limitParkJitter(),
		Now:             s.now(),
	})

	if !d.Park {
		return s.q.SetRunFailed(ctx, store.SetRunFailedParams{
			FailureReason: pgText(d.Reason),
			SessionID:     sessionID,
			ID:            run.ID,
			WorkerID:      pgUUID(wkr.ID),
		})
	}
	return s.q.SetRunLimitWait(ctx, store.SetRunLimitWaitParams{
		ID:             run.ID,
		WorkerID:       pgUUID(wkr.ID),
		LimitResetsAt:  pgTimePtr(d.LimitResetsAt),
		RateLimitType:  pgTextPtr(d.RateLimitType),
		RetryNotBefore: pgTime(d.RetryNotBefore),
		SessionID:      sessionID,
	})
}

// limitAwareFailureReason is the `failed` arm's half of the same rule (§7.8): a
// worker that opts out of parking reports `failed` directly and sends the structured
// limit fields alongside, and the server composes the sentence from its own enum.
//
// Two constraints, both deliberate:
//   - a terminal report is NEVER failed on a technicality. An unrecognised type
//     coerces to "unknown"; an implausible timestamp is dropped. Neither rejects the
//     report.
//   - the replacement fires ONLY when the fields are present. Absent, this falls
//     straight through to sanitizeFailureReason and no other failure path in the
//     product changes behaviour.
func limitAwareFailureReason(req StateRequest) pgtype.Text {
	if req.RateLimitType == nil && req.LimitResetsAt == nil {
		return sanitizeFailureReason(req.FailureReason)
	}
	var resetsAt *time.Time
	if t, ok := validReportedReset(req.LimitResetsAt); ok {
		resetsAt = &t
	}
	return pgText(limitFailureReason(CoerceRateLimitType(req.RateLimitType), resetsAt, ""))
}

// pgTimePtr / pgTextPtr lift the park decision's optional fields into the pgtype
// forms the generated params take. Nil becomes SQL NULL, which for
// limit_resets_at and rate_limit_type is the honest answer ("the worker reported
// nothing usable") rather than a placeholder.
func pgTimePtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func pgTextPtr(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

// resolveWaitOnLimit answers "does THIS new run park on a usage limit" (PRD #35
// Decision 7): the caller's explicit choice when it made one, else the owner's
// users.wait_on_limit default.
//
// It lives in the SERVICE layer rather than as a SQL DEFAULT because the defaulting
// has to be visible at every creation site. A DEFAULT clause would make a creation
// path added later silently opt its users out with nothing going red — and note that
// Go does NOT save you here either: sqlc's params are a struct literal, so an
// unstamped call site compiles and yields false. Measured while wiring this: adding
// the column and regenerating left `go build ./...` green with all three call sites
// unstamped. The real guard is a per-path test, and there is one.
//
// A FAILED USER LOOKUP RESOLVES TO false, NOT AN ERROR, and the direction matters:
// false is today's behaviour exactly (the run fails on a limit, now with a better
// reason), whereas failing the creation would turn a preference lookup into a run
// that never exists. A user row is also always present here — every creation path
// has already proven ownership of a repo or an issue by this point — so this is the
// fallback for a torn read, not a routine branch.
func (s *Service) resolveWaitOnLimit(ctx context.Context, userID uuid.UUID, requested *bool) bool {
	if requested != nil {
		return *requested
	}
	u, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		return false
	}
	return u.WaitOnLimit
}
