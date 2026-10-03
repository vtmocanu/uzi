package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// usage_tail.go is the write half of the estimated usage tail (issue #2014, ADR-2014): the
// /usage route's RecordRunUsage, and the incremental fold's leg-identity/coverage stamps
// (foldUsageTailStamps). The read half is usage_tail_read.go. None of it touches run_usage or
// run_usage_totals.
//
// LOCK ORDER (ADR-2014 D11). Both writers open their own transaction and take the per-run
// advisory lock (store.LockRunUsage) as its FIRST statement, before any usage-row write, and
// neither holds any other lock a peer could wait on in the opposite order. So the two paths
// serialize per run and cannot deadlock, and the per-run caps, counted under the lock, cannot be
// passed by two concurrent writers.

// Per-run and per-request bounds (ADR-2014 D9, D11). The per-run caps are variables only so a test
// can lower them; production never reassigns them.
const (
	defaultMaxUsageMessagesPerRun = 20000
	defaultMaxUsageLegsPerRun     = 500
	// maxUsagePostRecords and maxUsagePostLegs bound ONE /usage request; more is a 400.
	maxUsagePostRecords = 500
	maxUsagePostLegs    = 16
	// maxUsageIDRunes caps message_id, frame_session_id and sdk_session_id; maxUsageMarkerRunes
	// caps service_tier / speed / inference_geo. The columns have matching CHECKs.
	maxUsageIDRunes     = 200
	maxUsageMarkerRunes = 64
)

var (
	maxUsageMessagesPerRun = defaultMaxUsageMessagesPerRun
	maxUsageLegsPerRun     = defaultMaxUsageLegsPerRun
)

var (
	// ErrUsageInvalid is a /usage request the server refuses as malformed (a nil leg id, an
	// ordinal outside 1..MaxInt32, a negative token count, an empty message id or model) → 400.
	ErrUsageInvalid = errors.New("invalid run usage report")
	// ErrUsageTooLarge is a /usage request over the per-request record or leg-marker bound → 400.
	ErrUsageTooLarge = errors.New("run usage report exceeds the per-request bound")
	// ErrUsageRunUnsupported is a /usage report for a run that carries no tail: a chat run or a
	// Codex-harness run (ADR-2014 D9) → 400.
	ErrUsageRunUnsupported = errors.New("run usage tail is not recorded for this run")
)

// UsageLegMarker is one leg's marker on the /usage wire: its identity, and optionally its close
// (the highest ordinal the leg produced) and the records the worker dropped for memory.
type UsageLegMarker struct {
	LegID          uuid.UUID `json:"leg_id"`
	ClosedThrough  *int64    `json:"closed_through,omitempty"`
	DroppedRecords *int64    `json:"dropped_records,omitempty"`
}

// UsageRecord is one Anthropic message's usage on the /usage wire. Compat: the handler decodes
// with DisallowUnknownFields, so a field a newer worker sends must be declared here first.
type UsageRecord struct {
	MessageID                  string    `json:"message_id"`
	LegID                      uuid.UUID `json:"leg_id"`
	Ordinal                    int64     `json:"ordinal"`
	FrameSessionID             string    `json:"frame_session_id,omitempty"`
	Model                      string    `json:"model"`
	Subagent                   bool      `json:"subagent"`
	InputTokens                int64     `json:"input_tokens"`
	CacheReadInputTokens       int64     `json:"cache_read_input_tokens"`
	CacheCreationInputTokens   int64     `json:"cache_creation_input_tokens"`
	CacheCreation5mInputTokens *int64    `json:"cache_creation_5m_input_tokens,omitempty"`
	CacheCreation1hInputTokens *int64    `json:"cache_creation_1h_input_tokens,omitempty"`
	OutputTokens               int64     `json:"output_tokens"`
	OutputFinal                bool      `json:"output_final"`
	ServiceTier                string    `json:"service_tier,omitempty"`
	Speed                      string    `json:"speed,omitempty"`
	InferenceGeo               string    `json:"inference_geo,omitempty"`
}

// UsageRequest is the body of POST /api/worker/runs/{id}/usage, less the claim generation the
// handler decodes beside it.
type UsageRequest struct {
	Legs     []UsageLegMarker `json:"legs"`
	Messages []UsageRecord    `json:"messages"`
}

// UsagePostBody is the whole wire body of POST /api/worker/runs/{id}/usage: the request plus the
// claim generation, in the body exactly as on /messages. The handler decodes into it with
// DisallowUnknownFields, so a field a newer worker sends must be declared on this type (or on
// UsageRequest, UsageLegMarker, UsageRecord) in the release BEFORE that worker ships.
type UsagePostBody struct {
	ClaimGeneration *int64 `json:"claim_generation,omitempty"`
	UsageRequest
}

// cleanUsageText strips NUL bytes (Postgres 22021) and caps the value at n runes, returning ""
// for an absent value so the caller stores NULL.
func cleanUsageText(s string, n int) string {
	s, _ = stripNUL(s)
	return truncateRunes(s, n)
}

func usageInt32(v int64) (int32, bool) {
	if v < 0 || v > math.MaxInt32 {
		return 0, false
	}
	return int32(v), true
}

// sanitizeUsageRequest validates and sanitizes the whole request before any write: nothing
// worker-supplied reaches a row unvalidated, and an invalid request writes nothing. The returned
// request is a sanitized copy.
func sanitizeUsageRequest(req UsageRequest) (UsageRequest, error) {
	if len(req.Legs) > maxUsagePostLegs || len(req.Messages) > maxUsagePostRecords {
		return UsageRequest{}, ErrUsageTooLarge
	}
	out := UsageRequest{
		Legs:     make([]UsageLegMarker, 0, len(req.Legs)),
		Messages: make([]UsageRecord, 0, len(req.Messages)),
	}
	for _, l := range req.Legs {
		if l.LegID == uuid.Nil {
			return UsageRequest{}, fmt.Errorf("%w: leg_id", ErrUsageInvalid)
		}
		if l.ClosedThrough != nil {
			if _, ok := usageInt32(*l.ClosedThrough); !ok {
				return UsageRequest{}, fmt.Errorf("%w: closed_through", ErrUsageInvalid)
			}
		}
		if l.DroppedRecords != nil && *l.DroppedRecords < 0 {
			return UsageRequest{}, fmt.Errorf("%w: dropped_records", ErrUsageInvalid)
		}
		out.Legs = append(out.Legs, l)
	}
	for _, m := range req.Messages {
		m.MessageID = cleanUsageText(m.MessageID, maxUsageIDRunes)
		m.Model = cleanUsageText(m.Model, maxUsageModelRunes)
		if m.MessageID == "" || m.Model == "" {
			return UsageRequest{}, fmt.Errorf("%w: message_id and model are required", ErrUsageInvalid)
		}
		if m.LegID == uuid.Nil {
			return UsageRequest{}, fmt.Errorf("%w: leg_id", ErrUsageInvalid)
		}
		if o, ok := usageInt32(m.Ordinal); !ok || o < 1 {
			return UsageRequest{}, fmt.Errorf("%w: ordinal", ErrUsageInvalid)
		}
		if m.InputTokens < 0 || m.CacheReadInputTokens < 0 || m.CacheCreationInputTokens < 0 || m.OutputTokens < 0 ||
			(m.CacheCreation5mInputTokens != nil && *m.CacheCreation5mInputTokens < 0) ||
			(m.CacheCreation1hInputTokens != nil && *m.CacheCreation1hInputTokens < 0) {
			return UsageRequest{}, fmt.Errorf("%w: negative token count", ErrUsageInvalid)
		}
		m.FrameSessionID = cleanUsageText(m.FrameSessionID, maxUsageIDRunes)
		m.ServiceTier = cleanUsageText(m.ServiceTier, maxUsageMarkerRunes)
		m.Speed = cleanUsageText(m.Speed, maxUsageMarkerRunes)
		m.InferenceGeo = cleanUsageText(m.InferenceGeo, maxUsageMarkerRunes)
		out.Messages = append(out.Messages, m)
	}
	return out, nil
}

// RecordRunUsage persists one /usage post (ADR-2014 D1, D8, D9, D11). Ownership, the
// missing-claim-generation refusal and the claim fence match AppendMessagesForClaim (a stale
// claim is ErrStaleClaim and writes nothing); a chat or Codex-harness run is refused with
// ErrUsageRunUnsupported. The whole write is ONE transaction whose first statement is the per-run
// advisory lock, then the fence, then the cap accounting: new ids are admitted only up to the
// per-run headroom (updates to existing rows are always allowed), and anything dropped is counted
// in run_usage_tail_state so coverage can say so.
func (s *Service) RecordRunUsage(ctx context.Context, wkr store.Worker, runID uuid.UUID, req UsageRequest, claimGen *int64) error {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return err
	}
	// Same fail-closed rule as appendMessages: a CAPABILITY worker must stamp its claim generation
	// so the fence can engage; a legacy worker keeps posting unfenced-by-generation (the claim must
	// still be unreleased).
	if claimGen == nil && run.Kind != runkind.Chat && slices.Contains(wkr.ProtocolCapabilities, capability.CredentialSwitchV1) {
		return ErrMissingClaimGeneration
	}
	if run.Kind == runkind.Chat || run.Harness == harnessCodex {
		return ErrUsageRunUnsupported
	}
	req, err = sanitizeUsageRequest(req)
	if err != nil {
		return err
	}
	if s.txBeginner == nil {
		return fmt.Errorf("run usage report unavailable: no tx beginner wired for run %s", runID)
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	if err := setUsageWriteIsolation(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	// A no-op after a successful Commit; on every early return it releases the advisory lock.
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)

	// FIRST statement: the per-run usage lock, before any row write (ADR-2014 D11).
	if err := q.LockRunUsage(ctx, runID); err != nil {
		return err
	}
	live, err := q.RunUsageFenceLive(ctx, store.RunUsageFenceLiveParams{RunID: runID, ClaimGeneration: pgconv.Int8Ptr(claimGen)})
	if err != nil {
		return err
	}
	if !live {
		return ErrStaleClaim
	}
	if s.usageAfterFenceHook != nil {
		s.usageAfterFenceHook()
	}
	if err := writeUsagePost(ctx, q, runID, req, claimGen); err != nil {
		return err
	}
	// Locked recheck as the LAST statement before Commit. The early RunUsageFenceLive read is not
	// enough: under READ COMMITTED a release or reclaim does not take the usage advisory lock, so
	// it can commit after that read and before ours, and a per-statement fence cannot cover the
	// whole write. RunUsageFenceLiveLocked row-locks the runs row (FOR SHARE), so a release that
	// committed before it is seen here (ErrStaleClaim, the deferred Rollback discards every
	// write) and one that starts after it waits for our commit. It is last so runs UPDATEs are not
	// blocked across up to 500 upserts; a post that inserted a leg row may already hold FOR KEY SHARE
	// on the row (the FK), which this upgrades.
	live, err = q.RunUsageFenceLiveLocked(ctx, store.RunUsageFenceLiveLockedParams{RunID: runID, WorkerID: pgconv.UUID(wkr.ID), ClaimGeneration: pgconv.Int8Ptr(claimGen)})
	if err != nil {
		return err
	}
	if !live {
		return ErrStaleClaim
	}
	if s.usageBeforeCommitHook != nil {
		s.usageBeforeCommitHook()
	}
	return tx.Commit(ctx)
}

// setUsageWriteIsolation pins a usage writer's transaction to READ COMMITTED. It must be the
// transaction's first statement. The cap argument depends on it: the caps are counted AFTER
// the per-run advisory lock is held, and under READ COMMITTED each statement sees everything the
// previous lock holder committed, whereas a REPEATABLE READ snapshot (a pool whose default
// isolation is stricter) would be fixed at the first statement and could count stale rows.
func setUsageWriteIsolation(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return fmt.Errorf("run usage: set isolation: %w", err)
	}
	return nil
}

// legMarker is the merged marker for one leg across a request (GREATEST on repeats).
type legMarker struct {
	closedThrough  *int64
	droppedRecords int64
}

// writeUsagePost applies a sanitized request inside the already-locked, already-fenced
// transaction behind q.
func writeUsagePost(ctx context.Context, q *store.Queries, runID uuid.UUID, req UsageRequest, claimGen *int64) error {
	// Merge markers per leg, remembering first appearance (markers first, then message legs) so
	// leg admission under the cap is deterministic.
	markers := map[uuid.UUID]*legMarker{}
	var legOrder []uuid.UUID
	touch := func(id uuid.UUID) *legMarker {
		mk, ok := markers[id]
		if !ok {
			mk = &legMarker{}
			markers[id] = mk
			legOrder = append(legOrder, id)
		}
		return mk
	}
	for _, l := range req.Legs {
		mk := touch(l.LegID)
		if l.ClosedThrough != nil && (mk.closedThrough == nil || *l.ClosedThrough > *mk.closedThrough) {
			v := *l.ClosedThrough
			mk.closedThrough = &v
		}
		if l.DroppedRecords != nil && *l.DroppedRecords > mk.droppedRecords {
			mk.droppedRecords = *l.DroppedRecords
		}
	}
	for _, m := range req.Messages {
		touch(m.LegID)
	}

	// Legs: which already exist, and how many more the cap admits.
	var cappedLegs, cappedRecords int64
	admittedLeg := map[uuid.UUID]bool{}
	if len(legOrder) > 0 {
		existing, err := q.ListExistingRunUsageLegIDs(ctx, store.ListExistingRunUsageLegIDsParams{RunID: runID, LegIds: legOrder})
		if err != nil {
			return err
		}
		for _, id := range existing {
			admittedLeg[id] = true
		}
		legCount, err := q.CountRunUsageLegs(ctx, runID)
		if err != nil {
			return err
		}
		headroom := int64(maxUsageLegsPerRun) - legCount
		for _, id := range legOrder {
			if admittedLeg[id] {
				continue
			}
			if headroom > 0 {
				admittedLeg[id] = true
				headroom--
				continue
			}
			cappedLegs++
		}
	}

	// Messages: ids that already exist are always updated; a new id needs an admitted leg and
	// per-run headroom (lowest ordinals first).
	existingMsg := map[string]bool{}
	if len(req.Messages) > 0 {
		ids := make([]string, 0, len(req.Messages))
		for _, m := range req.Messages {
			ids = append(ids, m.MessageID)
		}
		have, err := q.ListExistingRunUsageMessageIDs(ctx, store.ListExistingRunUsageMessageIDsParams{RunID: runID, MessageIds: ids})
		if err != nil {
			return err
		}
		for _, id := range have {
			existingMsg[id] = true
		}
	}
	type newID struct {
		id      string
		ordinal int64
	}
	newIDs := map[string]int64{} // distinct new id -> lowest ordinal, for admission order
	for _, m := range req.Messages {
		if existingMsg[m.MessageID] || !admittedLeg[m.LegID] {
			continue
		}
		if o, ok := newIDs[m.MessageID]; !ok || m.Ordinal < o {
			newIDs[m.MessageID] = m.Ordinal
		}
	}
	admittedMsg := map[string]bool{}
	if len(newIDs) > 0 {
		msgCount, err := q.CountRunUsageMessages(ctx, runID)
		if err != nil {
			return err
		}
		headroom := int64(maxUsageMessagesPerRun) - msgCount
		order := make([]newID, 0, len(newIDs))
		for id, o := range newIDs {
			order = append(order, newID{id, o})
		}
		sort.Slice(order, func(i, j int) bool {
			if order[i].ordinal != order[j].ordinal {
				return order[i].ordinal < order[j].ordinal
			}
			return order[i].id < order[j].id
		})
		for _, n := range order {
			if headroom > 0 {
				admittedMsg[n.id] = true
				headroom--
			}
		}
	}
	// keep reports whether a record is written: an existing id is always updated (its leg is
	// never inserted, so the foreign key is not exercised), a NEW id needs the message cap AND an
	// admitted leg for THIS record. Checking the leg per record matters: the same new id posted
	// under an admitted leg and under a leg the legs cap refused is admitted by id, and writing
	// the refused-leg copy would violate the (run_id, leg_id) foreign key on every retry.
	keep := func(m UsageRecord) bool {
		return existingMsg[m.MessageID] || (admittedMsg[m.MessageID] && admittedLeg[m.LegID])
	}
	// Count each distinct dropped id once: records whose leg the cap blocked, and ids the message
	// cap blocked.
	dropped := map[string]bool{}
	for _, m := range req.Messages {
		if !keep(m) {
			dropped[m.MessageID] = true
		}
	}
	cappedRecords = int64(len(dropped))

	// Writes, in (leg_id, message_id) order. Legs first so a message's foreign key resolves.
	legIDs := make([]uuid.UUID, 0, len(admittedLeg))
	for id := range markers {
		if admittedLeg[id] {
			legIDs = append(legIDs, id)
		}
	}
	sort.Slice(legIDs, func(i, j int) bool { return legIDs[i].String() < legIDs[j].String() })
	for _, id := range legIDs {
		mk := markers[id]
		arg := store.UpsertRunUsageLegMarkerParams{
			RunID: runID, LegID: id, DroppedRecords: mk.droppedRecords, ClaimGeneration: pgconv.Int8Ptr(claimGen),
		}
		if mk.closedThrough != nil {
			arg.ClosedThrough.Int32, arg.ClosedThrough.Valid = int32(*mk.closedThrough), true //nolint:gosec // bounded to MaxInt32 by sanitizeUsageRequest
		}
		if err := q.UpsertRunUsageLegMarker(ctx, arg); err != nil {
			return err
		}
	}
	msgs := make([]UsageRecord, 0, len(req.Messages))
	for _, m := range req.Messages {
		if keep(m) {
			msgs = append(msgs, m)
		}
	}
	sort.SliceStable(msgs, func(i, j int) bool {
		if msgs[i].LegID != msgs[j].LegID {
			return msgs[i].LegID.String() < msgs[j].LegID.String()
		}
		return msgs[i].MessageID < msgs[j].MessageID
	})
	touched := map[uuid.UUID]bool{}
	for _, m := range msgs {
		if err := q.UpsertRunUsageMessage(ctx, store.UpsertRunUsageMessageParams{
			RunID:                      runID,
			MessageID:                  m.MessageID,
			LegID:                      m.LegID,
			Ordinal:                    int32(m.Ordinal), //nolint:gosec // bounded to 1..MaxInt32 by sanitizeUsageRequest
			FrameSessionID:             pgconv.TextOrNull(m.FrameSessionID),
			Model:                      m.Model,
			Subagent:                   m.Subagent,
			InputTokens:                m.InputTokens,
			CacheReadInputTokens:       m.CacheReadInputTokens,
			CacheCreationInputTokens:   m.CacheCreationInputTokens,
			CacheCreation5mInputTokens: pgconv.Int8Ptr(m.CacheCreation5mInputTokens),
			CacheCreation1hInputTokens: pgconv.Int8Ptr(m.CacheCreation1hInputTokens),
			OutputTokens:               m.OutputTokens,
			OutputFinal:                m.OutputFinal,
			ServiceTier:                pgconv.TextOrNull(m.ServiceTier),
			Speed:                      pgconv.TextOrNull(m.Speed),
			InferenceGeo:               pgconv.TextOrNull(m.InferenceGeo),
			ClaimGeneration:            pgconv.Int8Ptr(claimGen),
		}); err != nil {
			return err
		}
		touched[m.LegID] = true
	}
	if len(touched) > 0 {
		ids := make([]uuid.UUID, 0, len(touched))
		for id := range touched {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
		if err := q.FlagRunUsageOrdinalConflicts(ctx, store.FlagRunUsageOrdinalConflictsParams{RunID: runID, LegIds: ids}); err != nil {
			return err
		}
	}
	if cappedLegs > 0 || cappedRecords > 0 {
		if err := q.UpsertRunUsageTailState(ctx, store.UpsertRunUsageTailStateParams{RunID: runID, CappedRecords: cappedRecords, CappedLegs: cappedLegs}); err != nil {
			return err
		}
	}
	return nil
}

// usageStampKind is which stamp a frame carried.
type usageStampKind int

const (
	usageStampInit usageStampKind = iota
	usageStampResult
)

// usageStamp is one validated leg stamp lifted from an init or result frame.
type usageStamp struct {
	kind       usageStampKind
	legID      uuid.UUID
	seq        int32  // init: the frame's seq
	sessionID  string // init
	through    int32  // result
	cumulative bool   // result
	claimGen   *int64
}

// usageStampPayload reads only the stamp keys, each field decoded on its own so one malformed
// stamp never discards a sibling. The worker stamps them into the persisted init and result
// frames (agent projectInit / projectResult).
type usageStampPayload struct {
	Event        string          `json:"event"`
	UsageBasis   json.RawMessage `json:"usage_basis"`
	LegID        json.RawMessage `json:"leg_id"`
	SDKSessionID json.RawMessage `json:"sdk_session_id"`
	UsageThrough json.RawMessage `json:"usage_through"`
}

// resultFrameMetered reports whether the metered fold (foldUsageFrames) would meter this result
// frame: it decodes into resultUsagePayload, is a result event, and carries at least one
// non-empty model key.
func resultFrameMetered(payload json.RawMessage) bool {
	var p resultUsagePayload
	if json.Unmarshal(payload, &p) != nil || p.Event != "result" {
		return false
	}
	for model := range p.ModelUsage {
		if model != "" {
			return true
		}
	}
	return false
}

// collectUsageStamps lifts the valid stamps out of a delivered batch. A bad stamp is SKIPPED, never
// an error: the append and the metered fold already landed and must stay untouched by a hostile
// or garbled stamp.
func collectUsageStamps(msgs []IncomingMessage) []usageStamp {
	var out []usageStamp
	for _, m := range msgs {
		if m.Kind != "status" && m.Kind != "error" {
			continue
		}
		var p usageStampPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			continue
		}
		var legStr string
		if json.Unmarshal(p.LegID, &legStr) != nil {
			continue
		}
		legID, err := uuid.Parse(legStr)
		if err != nil || legID == uuid.Nil {
			continue
		}
		switch {
		case p.Event == "init" && m.Kind == "status":
			var sess string
			if json.Unmarshal(p.SDKSessionID, &sess) != nil {
				continue
			}
			sess, _ = stripNUL(sess)
			sess = truncateRunes(sess, maxUsageIDRunes)
			if sess == "" || m.Seq < 0 {
				continue
			}
			out = append(out, usageStamp{kind: usageStampInit, legID: legID, seq: m.Seq, sessionID: sess, claimGen: m.ClaimGeneration})
		case p.Event == "result":
			// The SAME acceptance as the metered fold (foldUsageFrames): the frame must decode
			// into resultUsagePayload, and at least one model key must be non-empty. A frame the
			// fold did not meter covers nothing, so it must never set covered_through.
			if !resultFrameMetered(m.Payload) {
				continue
			}
			var through int64
			if json.Unmarshal(p.UsageThrough, &through) != nil {
				continue
			}
			t, ok := usageInt32(through)
			if !ok {
				continue
			}
			var basis string
			_ = json.Unmarshal(p.UsageBasis, &basis)
			out = append(out, usageStamp{kind: usageStampResult, legID: legID, through: t, cumulative: basis == usageBasisSessionCumulative, claimGen: m.ClaimGeneration})
		}
	}
	return out
}

// foldUsageTailStamps records the leg identity and coverage stamps a delivered batch carried
// (ADR-2014 D4, D11). It runs AFTER the unchanged metered fold, for Claude non-chat runs only, in
// its own short transaction that takes the per-run usage lock FIRST and applies the legs cap. A cap
// hit sets record_cap_reached, skips the write and still commits, so it never fails the append. A
// database error is returned (it carries no worker text) and fails the append, exactly like the
// metered fold, so the worker re-delivers; every write is idempotent or monotone on re-delivery:
// run_messages is ON CONFLICT (run_id, seq) DO NOTHING, UpsertRunUsage merges with GREATEST,
// UpsertRunUsageLegInit COALESCEs, UpsertRunUsageLegCoverage merges with GREATEST/OR, and a failed
// stamp transaction rolls back whole.
func (s *Service) foldUsageTailStamps(ctx context.Context, run store.Run, msgs []IncomingMessage) error {
	// An isolated-lane run (profile-bound) is refused by the /usage route and must not grow legs
	// through the stamp path either.
	if run.Kind == runkind.Chat || run.Harness == harnessCodex || run.EgressProfileID.Valid || s.txBeginner == nil {
		return nil
	}
	stamps := collectUsageStamps(msgs)
	if len(stamps) == 0 {
		return nil
	}
	if err := s.applyUsageStamps(ctx, run.ID, stamps); err != nil {
		return fmt.Errorf("usage tail stamp write for run %s: %w", run.ID, err)
	}
	return nil
}

// applyUsageStamps deliberately takes NO claim fence. Its stamps derive from frames the caller
// already stored under InsertRunMessage's generation_live fence; fencing again here would turn a
// valid stamp into a 409 (a release between the insert and this transaction) and lose it
// permanently, because the fenced re-delivery is rejected too.
func (s *Service) applyUsageStamps(ctx context.Context, runID uuid.UUID, stamps []usageStamp) error {
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setUsageWriteIsolation(ctx, tx); err != nil {
		return err
	}
	q := store.New(tx)
	// FIRST statement after the isolation pin: the per-run usage lock, before any row write
	// (ADR-2014 D11).
	if err := q.LockRunUsage(ctx, runID); err != nil {
		return err
	}
	seen := map[uuid.UUID]bool{}
	var ids []uuid.UUID
	for _, st := range stamps {
		if !seen[st.legID] {
			seen[st.legID] = true
			ids = append(ids, st.legID)
		}
	}
	existing, err := q.ListExistingRunUsageLegIDs(ctx, store.ListExistingRunUsageLegIDsParams{RunID: runID, LegIds: ids})
	if err != nil {
		return err
	}
	have := map[uuid.UUID]bool{}
	for _, id := range existing {
		have[id] = true
	}
	legCount, err := q.CountRunUsageLegs(ctx, runID)
	if err != nil {
		return err
	}
	var cappedLegs int64
	blocked := map[uuid.UUID]bool{}
	for _, id := range ids {
		if have[id] {
			continue
		}
		if legCount >= int64(maxUsageLegsPerRun) {
			blocked[id] = true
			cappedLegs++
			continue
		}
		legCount++
		have[id] = true
	}
	for _, st := range stamps {
		if blocked[st.legID] {
			continue
		}
		switch st.kind {
		case usageStampInit:
			err = q.UpsertRunUsageLegInit(ctx, store.UpsertRunUsageLegInitParams{
				RunID: runID, LegID: st.legID, InitSeq: int64(st.seq), SdkSessionID: st.sessionID, ClaimGeneration: pgconv.Int8Ptr(st.claimGen),
			})
		case usageStampResult:
			err = q.UpsertRunUsageLegCoverage(ctx, store.UpsertRunUsageLegCoverageParams{
				RunID: runID, LegID: st.legID, CoveredThrough: st.through, CoveredCumulative: st.cumulative, ClaimGeneration: pgconv.Int8Ptr(st.claimGen),
			})
		}
		if err != nil {
			return err
		}
	}
	if cappedLegs > 0 {
		if err := q.UpsertRunUsageTailState(ctx, store.UpsertRunUsageTailStateParams{RunID: runID, CappedLegs: cappedLegs}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
