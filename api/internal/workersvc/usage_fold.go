package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// AppendMessages persists a worker's batched messages (idempotent on
// (run_id, seq)) and advances the run's last_seq high-water mark. The worker
// must own the run.
//
// It is a thin RECORDER around appendMessages (PRD #108 M4): the persistence work
// is unchanged and lives below, and this layer exists only to feed the per-run
// failure streak the health detector and the auto-stop evaluator read. The split
// is what lets the counter see EVERY failure return of a function that has five of
// them, instead of the one a caller happens to remember to instrument.
//
// The recording rules and the reasons they are rules, not defaults, are on each
// arm of the switch; the invariant they all serve is persistfail.go's ownership
// tripwire.
func (s *Service) AppendMessages(ctx context.Context, wkr store.Worker, runID uuid.UUID, msgs []IncomingMessage) error {
	obs, err := s.appendMessages(ctx, wkr, runID, msgs)
	switch {
	case !obs.resolved:
		// Ownership never resolved (ErrRunNotOwned, or the lookup itself failed), so
		// this run is not this worker's to vouch for. Recording here is what would let
		// worker A build a kill streak against user B's run — see persistfail.go's
		// ownership tripwire. NOT a persistence failure, and not counted as one.
	case obs.status != "running":
		// NOT RUNNING — checked BEFORE the success arm on purpose, and stated as one
		// rule rather than as a terminal special case: A STREAK IS EVIDENCE ABOUT ONE
		// RUNNING ATTEMPT, so leaving `running` ends that attempt's claim on it.
		//
		// This single arm retires a whole class of defect instead of one path of it:
		//
		//   - TERMINAL. worker_id survives the terminal transition and neither
		//     GetRunOwnedByWorker nor this method filters on status, so a late or
		//     hostile POST would resurrect a streak on a dead run one tick after
		//     eviction — and on the SUCCESS path would keep a terminal run in M5's
		//     comparison set indefinitely for one deduplicated append every few minutes.
		//   - REQUEUED. The evaluator evicts too, but it only ever sees CANDIDATES
		//     (streak >= autoStopStreak and the window elapsed), so a SUB-THRESHOLD
		//     streak crossed a requeue untouched — and kept growing, since this method
		//     went on recording against a queued run. Measured: 12 carried across, then
		//     the fresh attempt killed after 8 new failures, with the entire window leg
		//     satisfied by the DEAD attempt's firstAt. A streak must pass through 12 to
		//     reach 20, so which side of the threshold an OOM lands on is close to a
		//     coin flip; half that population landed here.
		//   - Every OTHER path that resets status without a hook, including Register's
		//     RequeueWorkerRuns, which returns no ids and so can never have one.
		//
		// Sweep's two requeue-site evictions and the evaluator's are now belt and
		// braces rather than the mechanism, which is the safer arrangement: this arm
		// needs no candidacy test and no enumeration of the paths.
		//
		// 🔴 AND IT HAS A SECOND EFFECT, WHICH IS DELIBERATE. Sitting above the
		// success arm means recordSuccess fires ONLY for running runs, so this also
		// narrows M5's G4 COMPARISON SET to running runs. Measured, per status:
		//
		//	running            joins lastOK  -> counts as a peer
		//	awaiting_approval  does not      -> does not count
		//	claimed            does not      -> does not count
		//	queued             does not      -> does not count
		//	completed          does not      -> does not count
		//
		// So a run parked at the approval gate that IS successfully persisting
		// messages — alive and doing real work by any ordinary reading — no longer
		// vouches for the write path. That is intended on both counts: it is the
		// fail-safe direction (fewer peers ⇒ fewer kills), and it keeps the earlier
		// hardening's rule intact, that warming the comparison set should cost a live
		// run doing real work rather than a parked or finished one.
		//
		// Written down because the three bullets above are all about STREAKS, and a
		// reader restoring recordSuccess for a parked run would be undoing something
		// deliberate with nothing in the place they would look to say so. Pinned by
		// TestAppendMessagesComparisonSetIsRunningRunsOnly.
		s.persistFail.evict(runID)
	case err == nil:
		s.persistFail.recordSuccess(runID, s.now())
	default:
		s.persistFail.recordFailure(runID, classifyPersistFail(err), obs.lastSeq, s.now())
	}
	return err
}

// appendObservation is what the recorder needs from one append attempt and cannot
// get from the error alone: whether ownership resolved at all, whether the run was
// already terminal, and the run's message high-water mark AS THIS ATTEMPT SAW IT.
//
// lastSeq is max(runs.last_seq, maxStored) — the PRD's "max(seq) has not advanced"
// evidence. It reads runs.last_seq rather than a SELECT max(seq) FROM run_messages
// because UpdateRunLastSeq is `last_seq = GREATEST(last_seq, @seq)` over maxStored,
// which counts deduplicated rows too, so the column is a faithful high-water mark
// that is already on the row this path holds and costs no extra query.
type appendObservation struct {
	// resolved is true once runOwnedByWorker has returned a run — i.e. once the
	// caller is known to own this run. Nothing may be recorded while it is false.
	resolved bool
	// status is the run's status as this attempt read it. Carried whole rather than
	// as a derived `terminal bool` on purpose: the recorder's rule is "is this run
	// RUNNING", and a boolean named for one of the several non-running cases invites
	// the next reader to treat that case as the rule. One field cannot disagree
	// with itself.
	status  string
	lastSeq int32
}

// NoteOversizeBatch records a 413 against the run's persistence-failure streak.
//
// It exists because a 413 is answered in handler.WorkerRunMessages BEFORE
// AppendMessages is ever called, so an oversize batch is otherwise invisible to
// the recorder. That is not academic: a pre-0.10.1 worker's retry batch GROWS (PRD
// #108 M0 defect 4), so the incident's own long tail rotates 500 → 413 and then
// stays 413 forever. Without this hook, both M4's flag and M5's kill go blind in
// exactly that steady state.
//
// It re-checks ownership ITSELF — the one recording hook not already below
// runOwnedByWorker — because an unowned record is a cross-tenant kill primitive.
// Best-effort: a lookup failure records nothing.
//
// COST, stated for the case that is not benign. This arm previously did zero
// database work; it now does one indexed lookup. Rare for the incident's own
// shape (a 413 means a worker already past the 1 MiB cap), but a worker holding a
// valid join token can POST oversized bodies as fast as it likes, so this is one
// GetRunOwnedByWorker per such request — on a path that exists because the
// database is already under stress. The alternative was leaving both the flag and
// the kill blind in the incident's own steady state, which is worse; naming the
// cost is not the same as calling it free.
func (s *Service) NoteOversizeBatch(ctx context.Context, wkr store.Worker, runID uuid.UUID) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return
	}
	// THE SAME RULE AS THE RECORDER'S, and it must stay the same rule. This hook was
	// left on the old terminal-only check when AppendMessages moved to
	// `status != "running"`, so the two recording sites disagreed with each other —
	// one recording on any non-running run, one unless terminal. That is a worse
	// divergence than the `terminal bool` the observation type was changed to avoid,
	// because the boolean at least meant the same thing in both places.
	//
	// It was reachable, and through the case that forced the status narrowing to
	// begin with: /state is a different route and does not wedge, so a run reports
	// its plan and parks at `awaiting_approval` while a pre-0.10.1 batcher keeps
	// re-POSTing its grown batch and takes a 413 each time. Measured: streak 20 built
	// entirely at the gate, `window_seconds=95`, then the human approves and the
	// first sweep after the run returns to `running` kills it — and `oversize` IS a
	// killable class, so nothing downstream stopped it.
	if run.Status != "running" {
		s.persistFail.evict(runID)
		return
	}
	s.persistFail.recordFailure(runID, persistFailOversize, run.LastSeq, s.now())
}

// appendMessages is AppendMessages' persistence half. It returns the observation
// the recorder needs alongside the error; every early return therefore carries the
// state observed so far.
//
// ONLY the InsertRunMessage error is eligible for the ErrUnstorableMessage (→
// 400) classification, and that narrowness is the point rather than an oversight.
// This function returns store errors from three places — the insert,
// UpdateRunLastSeq and foldRunUsage — and 400 on this route tells the worker
// "this batch is permanently poisoned, stop retrying it". Only the insert's error
// is evidence for that claim. Reporting a failure from elsewhere as the batch's
// fault makes the worker drop messages that were never the problem: data loss
// from a misattributed error. The narrow shape has in-repo precedent in
// handler/secrets.go's constraint-name check, for the same reason.
//
// 🔴 THE ASSUMPTION THIS RESTS ON, AND WHEN IT BREAKS:
//
//	EVERY worker-controlled value that reaches the store on this path does so
//	through the sanitized InsertRunMessage.
//
// If you add a store call here that writes a worker-controlled value, or add such
// a value to one of the two existing calls, THAT VALUE IS NO LONGER COVERED —
// silently. It gets a 500, the worker retries it forever, and this PRD's exact
// wedge reappears in a new location with no signal. Revisit this placement in the
// same change, and extend the audit below rather than assuming it still holds.
//
// The audit as it stands, per non-insert call:
//
//   - UpdateRunLastSeq — takes the run id and an int32 seq. No worker text.
//   - CountRunInitFramesBefore — foldRunUsage's per-frame leg-index READ (PRD
//     #1079). It takes only the run id (a uuid) and a server-side int32 seq (the
//     frame's own seq, already validated > 0 in the loop above), and it READS a
//     COUNT rather than writing anything — no worker-controlled text or value
//     reaches the store through it, and a read cannot poison a row. So its error is
//     correctly returned RAW (500): a failure is a genuine server error the worker
//     should retry, never a "batch poisoned" 400. A CLEARED suspect, written out for
//     the same reason as foldRunUsage below — it is the fold's other store call and
//     the check is what keeps this placement defensible.
//   - foldRunUsage → UpsertRunUsage — every column it writes, checked one by one
//     because "I cannot think of a case" is not the same as "there is no case".
//     Read this as a CLEARED suspect, not a live hazard: it is written out because
//     it is the call that looks most dangerous and the check is what makes the
//     placement defensible, not because it can currently produce these codes.
//     `model` IS worker-controlled (a JSON object key out of the payload), and
//     `session_id` is too (the worker reports it; the runs row only relays it), so
//     BOTH are handled on TWO axes, not one. Byte VALIDITY: sanitation writes
//     through the index in a pass that completes before foldRunUsage iterates msgs,
//     so the model name the fold inserts is already NUL-free — pinned by
//     TestWorkerMessagesUsageFoldSeesSanitizedModelNamesLiveDB, which reddens if
//     that ordering inverts. LENGTH: both are members of run_usage's composite PK,
//     whose btree index entry caps at 2704 bytes, so foldRunUsage caps each with
//     truncateRunes before the upsert (maxUsageSessionRunes) — without which an over-long
//     value raises SQLSTATE 54000, which is NOT in unstorableSQLSTATEs and would
//     wedge the run one sink over (pinned by the UsageFoldCapsOversized*LiveDB
//     tests). `session_id`'s earlier acceptance into the runs row is not evidence
//     here: that column is unindexed `text`, and acceptance there says nothing
//     about indexability inside this composite PK — which is why relaying it is not
//     on its own sufficient. `run_id` is a uuid. The token columns are bigint and
//     take an int64, which always fits. `cost_usd` is numeric(12,6) and numericUSD
//     clamps to that domain (its own comment names 22003 as the poison-loop trigger
//     it exists to prevent).
//
// A broader wrap was considered and rejected: with the above holding it catches
// nothing extra, while reintroducing exactly the misattribution this narrowness
// exists to prevent.
func (s *Service) appendMessages(ctx context.Context, wkr store.Worker, runID uuid.UUID, msgs []IncomingMessage) (appendObservation, error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return appendObservation{}, err
	}
	obs := appendObservation{resolved: true, status: run.Status, lastSeq: run.LastSeq}
	// Validate the whole batch before persisting any of it: a single invalid
	// message rejects the batch with nothing written, so a [valid, valid, invalid]
	// batch never leaves the first two half-persisted.
	//
	// That all-or-nothing property is TRUE OF VALIDATION AND FALSE OF THE STORE.
	// The insert loop below is not transactional, so a batch whose third message
	// the database refuses leaves the first two committed. This comment used to
	// claim the batch was all-or-nothing outright; that was unreachable in practice
	// before PRD #108 and is routine after it, and it is about to be load-bearing
	// for the worker's bisection, so it is corrected here rather than left to
	// mislead. Idempotency on (run_id, seq) is what keeps the partial apply benign:
	// any regrouping or re-post converges. Making it genuinely transactional needs
	// a Store interface change (the generated queries take no tx) and is deferred
	// to Phase 2, which has its own reason to care — its "max(seq) has not
	// advanced" guard reads the column this path leaves behind.
	for i := range msgs {
		m := &msgs[i]
		if m.Seq <= 0 || m.Kind == "" || len(m.Payload) == 0 || !json.Valid(m.Payload) {
			return obs, ErrInvalidMessage
		}
		// A kind of nothing but NUL escapes passes the emptiness check above — it is
		// non-empty on the wire — and would then strip to "" in the sanitation pass,
		// which `text NOT NULL` accepts happily. Check the POST-STRIP value HERE, in
		// the validation pass, rather than after stripping: an empty kind is exactly
		// what that check exists to reject, and rejecting it here keeps both batch
		// invariants intact — nothing is written, and nothing is logged as laundered.
		// The double stripNUL costs one strings.Count on the fast path.
		if stripped, _ := stripNUL(m.Kind); stripped == "" {
			return obs, ErrInvalidMessage
		}
	}
	// SECOND pass for sanitation, separate from validation on purpose. The
	// count-and-log requirement (PRD #108 Risk 3) exists so a future NUL-emitting
	// tool stays visible, and folding it into the loop above would report only up
	// to the first invalid message — the batches most worth understanding are
	// exactly the ones that would go unreported. Split this way, a batch rejected
	// by validation logs nothing (nothing was laundered, because nothing is
	// stored) and a batch that proceeds logs every message it altered.
	//
	// It writes through the index (not a range copy), so the capped and sanitized
	// values are what the insert, the WS broadcast and the usage fold all see —
	// otherwise the stored row and the live frame would disagree, and the fold
	// would still be reading the unstorable bytes.
	for i := range msgs {
		m := &msgs[i]
		var c stripCounts
		// Strictly AFTER the json.Valid check above: the scanner presumes
		// well-formed JSON, and today's invalid-JSON→400 arm must keep answering
		// first.
		m.Payload, c = sanitizePayloadJSON(m.Payload)
		// FOUR text sinks, not three. `kind` is worker-supplied and lands in a bare
		// `text NOT NULL` column whose vocabulary lives in a COMMENT with no CHECK
		// constraint (00020_workers_runs.sql), and it decodes into a Go string exactly
		// like the other three — so a u0000 escape becomes a real 0x00 and Postgres
		// answers 22021. Ordinary tool output cannot produce it (kind comes from a
		// fixed SDK-frame vocabulary), so this needs a hostile or buggy worker — which
		// is precisely the threat model that produced sanitizeSelfReported on this
		// same route. Stripped BEFORE the log below, so the log line reports a clean
		// kind rather than smuggling the NUL into the operator's terminal.
		var nKind, nAgent, nInstance, nLabel int
		m.Kind, nKind = stripNUL(m.Kind)
		m.Agent, nAgent = stripNUL(m.Agent)
		m.AgentInstance, nInstance = stripNUL(m.AgentInstance)
		m.AgentLabel, nLabel = stripNUL(m.AgentLabel)
		c.textNUL = nKind + nAgent + nInstance + nLabel
		// Cap `kind` HERE, before the warn log below echoes it (and the second log on
		// a permanently-unstorable insert): an unbounded kind is an unbounded log
		// write, not only an unbounded column. Capped after the strip so the cap
		// counts runes the row will actually hold; `agent` is capped further down.
		m.Kind = truncateRunes(m.Kind, maxKindRunes)
		if c.any() {
			slog.Warn("workersvc: sanitized unstorable bytes out of a worker message",
				"run_id", runID.String(), "seq", m.Seq, "kind", m.Kind,
				"payload_nul_dropped", c.payloadNUL,
				"payload_unpaired_surrogates_replaced", c.payloadSurrogate,
				"payload_invalid_utf8_replaced", c.payloadBadUTF8,
				"text_column_nul_dropped", c.textNUL)
		}
		// Strip BEFORE truncating, so each rune cap is counted over the NUL-free
		// string the row will actually hold.
		m.Agent = truncateRunes(m.Agent, maxAgentRunes)
		m.AgentInstance = truncateRunes(m.AgentInstance, maxAgentInstanceRunes)
		m.AgentLabel = truncateRunes(m.AgentLabel, maxAgentLabelRunes)
	}
	// maxStored is the high-water mark of what ACTUALLY reached the table,
	// including rows the insert deduplicated (rows == 0 means this (run_id, seq)
	// was already persisted by an earlier delivery — stored is stored).
	var maxStored int32
	var insertErr error
	inserted := make([]IncomingMessage, 0, len(msgs))
	for _, m := range msgs {
		rows, err := s.q.InsertRunMessage(ctx, store.InsertRunMessageParams{
			RunID:         runID,
			Seq:           m.Seq,
			Kind:          m.Kind,
			Agent:         pgconv.TextOrNull(m.Agent),
			AgentInstance: pgconv.TextOrNull(m.AgentInstance),
			AgentLabel:    pgconv.TextOrNull(m.AgentLabel),
			Payload:       []byte(m.Payload),
		})
		if err != nil {
			// The ONLY classified error on this path. See the tripwire on
			// AppendMessages before widening this to another store call.
			insertErr = classifyStoreError(err)
			// The one diagnostic line for this event, emitted HERE because this is
			// the only place that holds the seq and kind alongside the SQLSTATE. The
			// code only, never pgErr.Message: for 22P02 and 22021 that text quotes a
			// fragment of the offending value (measured: `invalid byte sequence for
			// encoding "UTF8": 0xff`), which is worker-controlled bytes in a log line.
			var pgErr *pgconn.PgError
			code := ""
			if errors.As(err, &pgErr) {
				code = pgErr.Code
			}
			if errors.Is(insertErr, ErrUnstorableMessage) {
				slog.Warn("workersvc: message permanently unstorable",
					"run_id", runID.String(), "seq", m.Seq, "kind", m.Kind, "sqlstate", code)
			}
			break
		}
		if m.Seq > maxStored {
			maxStored = m.Seq
		}
		// rows == 0 means a duplicate (run_id, seq) — a worker re-delivery. Only
		// broadcast genuinely new messages so a retry never double-emits over WS.
		if rows > 0 {
			inserted = append(inserted, m)
		}
	}
	// The high-water mark AS OBSERVED, whether or not the insert loop broke and
	// whether or not the UpdateRunLastSeq below runs. This is what the streak's
	// no-progress reset compares (PRD #108 M4).
	//
	// The partial apply the comment above the validation loop flags, worked through:
	// on a batch whose Nth message the database refuses, rows 1..N-1 commit and this
	// advances the mark ONCE. It is frozen from the next attempt onward, because the
	// loop breaks at the same message every time so maxStored can never exceed the
	// value it already set. Whether that costs a streak reset depends on whether an
	// entry already existed — this line runs BEFORE the recorder sees the failure, so
	// a run whose first-ever failure is the partial apply is recorded at the advanced
	// mark and never resets at all. Either way it can only DELAY a kill by one
	// failure; it can never cause a false one.
	//
	// Where it genuinely helps: a 0.10.1+ worker bisecting the poison out re-groups
	// the batch, so messages after it DO land, this advances repeatedly, and the
	// streak keeps resetting — the server correctly declines to kill a run whose
	// client is already handling it.
	if maxStored > obs.lastSeq {
		obs.lastSeq = maxStored
	}
	// Advance the high-water mark to what landed, BEFORE propagating any insert
	// error. Leaving it stale is not cosmetic: on resume the worker restarts from
	// runs.last_seq and re-emits those seq numbers carrying DIFFERENT content, the
	// idempotent insert answers rows == 0, the server reads that as a re-delivery,
	// and the new content is silently dropped and never broadcast. That was
	// unreachable before PRD #108 (a failing insert was the anomaly) and is routine
	// after it, which is why it is fixed here rather than left to the transaction
	// Phase 2 will consider.
	if maxStored > run.LastSeq {
		if _, err := s.q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{ID: runID, Seq: maxStored}); err != nil {
			if insertErr != nil {
				return obs, insertErr // the insert failure is the more informative of the two
			}
			return obs, err
		}
	}
	if insertErr != nil {
		return obs, insertErr
	}
	// Fold every DELIVERED result frame's usage into run_usage (PRD #40 Decision 2)
	// — over `msgs`, NOT `inserted`: a seq-deduped re-delivery (crash retry) must
	// still re-run the fold, which is exactly what makes at-least-once delivery plus
	// the idempotent GREATEST merge converge to correct totals with no crash window.
	// Malformed/absent usage is skipped inside; a DB error propagates so the worker
	// re-delivers and the fold retries. No terminal-status guard — a result frame
	// that lands after a mid-flight cancel still folds (pre-cancel spend is real
	// spend, Decision 4).
	if err := s.foldRunUsage(ctx, run, msgs); err != nil {
		return obs, err
	}
	// Fan out after the log + high-water mark are durably advanced, so a browser
	// that reacts by replaying from last_seq sees a consistent state.
	if s.bcast != nil {
		now := s.now()
		for _, m := range inserted {
			s.bcast.PublishMessage(runID, m.Seq, m.Kind, m.Agent, m.AgentInstance, m.AgentLabel, []byte(m.Payload), now)
		}
	}
	return obs, nil
}

// usageFoldQuerier is the store surface foldUsageFrames writes through. Both the
// workersvc Store interface (via s.q, the incremental path) and *store.Queries (via
// s.q.WithTx(tx), M3's history refold) satisfy it, so the same fold body serves both
// callers and they cannot disagree about the epoch (both READ it from run_messages
// through CountRunInitFramesBefore).
type usageFoldQuerier interface {
	UpsertRunUsage(ctx context.Context, arg store.UpsertRunUsageParams) error
	CountRunInitFramesBefore(ctx context.Context, arg store.CountRunInitFramesBeforeParams) (int64, error)
}

// foldRunUsage upserts run_usage for every delivered result frame in the batch
// (PRD #40 Decision 2). It is called with ALL delivered messages, not just the
// newly-inserted ones, so a seq-deduped re-delivery re-runs the fold — the GREATEST
// merge in UpsertRunUsage makes that idempotent. It is a thin wrapper over
// foldUsageFrames writing through s.q (the incremental store); M3's history refold
// calls foldUsageFrames directly through a tx-bound querier over a run's full history.
// Malformed/absent usage is skipped (never fails the append); a DB error propagates so
// the append fails and the worker re-delivers.
func (s *Service) foldRunUsage(ctx context.Context, run store.Run, msgs []IncomingMessage) error {
	return foldUsageFrames(ctx, s.q, run, msgs)
}

// foldUsageFrames folds every result frame in `frames` into run_usage, keyed per SDK
// query() LEG (PRD #1079). A leg is one SDK process, marked by its persisted `init`
// frame; a result frame's lineage_epoch is the position-absolute count of `init`
// frames of the run with a lower seq (CountRunInitFramesBefore), read from the table
// AFTER the batch's inserts have committed. Because that count is a pure function of
// (run_id, seq), a re-delivered frame recomputes the same epoch and lands in the same
// row whatever else has landed since, so idempotency holds by construction and the
// incremental path (this batch's frames) and M3's refold (the whole history) are the
// same computation. session_id is sourced from the run row (the frame payload carries
// none); it stays in the key as the cross-worker row-splitter (a fresh process also
// emits a fresh init, so the two agree). GREATEST in UpsertRunUsage then only de-dups
// re-delivery of the SAME leg.
func foldUsageFrames(ctx context.Context, q usageFoldQuerier, run store.Run, frames []IncomingMessage) error {
	// Fold work runs — issue AND ci_fix both spend the user's tokens working a card
	// or a pipeline end to end — and exclude ONLY chat. Chat-run spend is explicitly
	// OUT of scope for PRD #40 ("Counting tokens spent outside runs, e.g. the PRD #39
	// chat agent"), yet mapResult is shared with the chat executor so a chat run's
	// result frames now carry usage too; skip the whole fold for kind='chat' rather
	// than let chat consumption leak into run_usage. This is an exclude-list (skip
	// chat), NOT an allowlist of {issue, ci_fix}, so a future WORK-run kind folds by
	// default — matching the success criterion "every run started after this shows
	// tokens" (a new non-work kind would need adding here, the same as chat).
	if run.Kind == runkind.Chat {
		return nil
	}
	var sessionID string
	if run.SessionID.Valid {
		sessionID = run.SessionID.String
	}
	// Cap the composite-PK text columns before any upsert (see maxUsageSessionRunes
	// for the 54000/2704-byte reasoning). session_id is capped once here; model is
	// capped per-frame inside the loop, since each frame carries its own.
	sessionID = truncateRunes(sessionID, maxUsageSessionRunes)
	for _, m := range frames {
		// Result frames are only ever kind status (success) or error; skip the
		// rest without paying an unmarshal for every text/tool_use message.
		if m.Kind != "status" && m.Kind != "error" {
			continue
		}
		var p resultUsagePayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			continue // malformed payload → skip, never fail the append
		}
		if p.Event != "result" || len(p.ModelUsage) == 0 {
			continue // not a result frame, or no per-model usage to fold
		}
		// The frame's leg index: the count of persisted `init` frames of this run
		// before it in seq order. One indexed read per result frame — result frames
		// are one per iteration, so a handful per run, never per message. Read from
		// the table (not derived from `frames`) so the incremental path and M3's
		// refold read the SAME value the migration's idx_run_messages_init serves.
		legCount, err := q.CountRunInitFramesBefore(ctx, store.CountRunInitFramesBeforeParams{
			RunID: run.ID,
			Seq:   m.Seq,
		})
		if err != nil {
			return fmt.Errorf("count init frames before seq %d (run %s): %w", m.Seq, run.ID, err)
		}
		// The count is bounded by the run's legs (a handful), so it always fits int32;
		// the explicit bound co-locates the proof with the cast for gosec/CodeQL.
		var frameEpoch int32
		if legCount >= 0 && legCount <= math.MaxInt32 {
			frameEpoch = int32(legCount)
		} else {
			frameEpoch = math.MaxInt32
		}
		for model, mu := range p.ModelUsage {
			if model == "" {
				continue
			}
			model = truncateRunes(model, maxUsageModelRunes)
			if err := q.UpsertRunUsage(ctx, store.UpsertRunUsageParams{
				RunID:               run.ID,
				SessionID:           sessionID,
				Model:               model,
				LineageEpoch:        frameEpoch,
				InputTokens:         nonNegTokens(mu.InputTokens),
				CacheReadTokens:     nonNegTokens(mu.CacheReadInputTokens),
				CacheCreationTokens: nonNegTokens(mu.CacheCreationInputTokens),
				OutputTokens:        nonNegTokens(mu.OutputTokens),
				CostUsd:             numericUSD(mu.CostUSD),
			}); err != nil {
				return fmt.Errorf("fold run usage (run %s, model %s): %w", run.ID, model, err)
			}
		}
	}
	return nil
}

// numericUSD builds a numeric(12,6) cost from the SDK's float dollar amount by
// quantizing to microdollars (Int = round(usd*1e6), Exp = -6) — deterministic and
// free of the float-string-parse ambiguity of Scan. Out-of-range costs (never
// expected) are clamped into the column's domain rather than poisoning the fold:
// NaN/negative/-Inf → 0, and anything above the ceiling (incl. +Inf) → the ceiling.
func numericUSD(usd float64) pgtype.Numeric {
	switch {
	case math.IsNaN(usd) || usd < 0:
		usd = 0
	case usd > maxCostUSD: // also catches +Inf
		usd = maxCostUSD
	}
	return pgtype.Numeric{Int: big.NewInt(int64(math.Round(usd * 1e6))), Exp: -6, Valid: true}
}

// nonNegTokens clamps a token count to >= 0 at fold time. GREATEST only protects an
// existing row; a fresh (run_id, session_id, model) key inserts whatever arrives, so
// a negative count (buggy/hostile worker) would otherwise land verbatim.
func nonNegTokens(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}
