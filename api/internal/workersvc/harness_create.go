package workersvc

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// harness_create.go is the PRD #1429 M2 consumer layer over M1's frozen createRunAtomic
// seam: the shared origin-routing helper, the exported error aliases + raw-override carrier
// the handler/scheduler need, the harness↔model compatibility check (D6) and the visible
// model-fallback status note. It resolves and freezes the harness for EVERY activated
// non-chat run origin so runs.harness equals the harness the freeze decision used.

// Exported aliases for the two typed resolver refusals so the handler (a different package)
// can classify them without reaching the unexported sentinels: an explicit harness with no
// usable credential is a stable no_credential_for_harness refusal (D2), and "neither harness
// usable" retains the existing errCredentialUnavailable-wrapped failure.
var (
	// ErrNoCredentialForHarness is the exported alias of the resolver's errNoCredentialForHarness:
	// an EXPLICIT harness (a request/pin, or an inherited source-run harness) whose credential is
	// unusable. The create fails and NEVER falls back to the other harness (D2/D4); the handler
	// maps it to 422 with a stable classification.
	ErrNoCredentialForHarness = errNoCredentialForHarness
	// ErrNoUsableCredential is the exported alias of the resolver's errNoUsableCredential: NEITHER
	// harness has a usable credential and no explicit selection forces one. It wraps
	// errCredentialUnavailable, so a consumer keying on that keeps the existing refusal behaviour.
	ErrNoUsableCredential = errNoUsableCredential
)

// RawCredentialOverride is an UNVALIDATED create-time credential-override request threaded from
// the manual issue-create / chat start_run handler into the run-creation transaction (PRD #1429
// M2, D5). It is validated against the D11-RESOLVED harness INSIDE the transaction — not against
// a pre-transaction harness guess — so an explicit-Claude request with a Codex default still
// accepts a valid Anthropic override, while an effective-Codex harness refuses it. nil ⇒ no
// override requested ⇒ inherit the worker binding, byte-identical to a pre-#1247 create.
type RawCredentialOverride struct {
	// Mode is the requested override mode ("pinned"/"auto"/"default"/"inherit"); the validator
	// maps an empty/invalid mode to its typed refusals.
	Mode string
	// SecretID is the pinned target credential, set only for a "pinned" request.
	SecretID *uuid.UUID
}

// codexModels is the CLOSED Codex model vocabulary (PRD #1429 D6; gpt-6-sol added with the Codex
// 0.156.1 pin, whose catalog requires client >= 0.155.0): the product-owned picker is EXACTLY these
// ids and catalog discovery never adds more. It mirrors the agent's
// agent/src/codex/render.ts CONTRACT_MODELS and codex-pricing.ts price table.
var codexModels = map[string]bool{
	"gpt-6-astra": true,
	"gpt-5.6-sol": true,
	"gpt-6-sol":   true,
}

// harnessModelCompatible reports whether the stored model may ride a claim for harness h, given
// the two closed harness vocabularies (PRD #1429 D6). An empty (inherit) model is always
// compatible — the worker uses its own harness default. The rule NEVER lets a Claude alias reach
// a Codex claim or a Codex-only model reach a Claude claim:
//
//   - Codex harness: ONLY a known Codex model is compatible. A Claude alias, or a custom/unknown
//     id whose Codex-compatibility cannot be known here, is refused so the worker falls back to
//     its own Codex default — the conservative direction the PRD requires (Codex has a closed
//     closed vocabulary, so anything else is unsafe to forward).
//   - Claude harness: a known Codex-only model is refused; every other value (a Claude alias OR a
//     custom full Claude id like claude-opus-4-8) passes through unchanged, preserving today's
//     honour-any-custom-id behaviour.
func harnessModelCompatible(h Harness, model string) bool {
	m := strings.TrimSpace(model)
	if m == "" {
		return true
	}
	switch h {
	case HarnessCodex:
		return codexModels[m]
	case HarnessClaude:
		return !codexModels[m]
	default:
		return true
	}
}

// createRunResolved runs `insert` — which MUST stamp runs.harness from the resolved harness — so
// that D11 harness resolution, the run INSERT and any Codex binding freeze commit in ONE database
// transaction (PRD #1429 M2, D1). It is the single funnel every activated non-chat origin (manual
// issue, chat start_run, scheduled issue/autopilot, prompt, self-improve, CI-fix, MR-rework, task)
// routes its INSERT through, so no origin can silently stay Claude or split the freeze from the
// INSERT.
//
// Production and the live-DB test suite hold a live *store.Queries (with a wired txBeginner), so
// they take the atomic seam. The in-memory unit fakeStore is NOT a *store.Queries and cannot open
// a real transaction; there the same insert runs directly against s.q after a Claude-default
// resolution (the harness those fakes assert), which keeps every existing unit test byte-identical.
// That non-atomic fallback is UNREACHABLE in production — the server always wires a live
// *store.Queries + txBeginner (cmd/server/main.go) — and it still fails an explicit non-Claude
// request closed, so it can never emit a non-atomic Codex insert.
func (s *Service) createRunResolved(ctx context.Context, userID uuid.UUID, explicit *Harness, insert func(q Store, resolved resolvedHarness) (store.Run, error)) (store.Run, error) {
	if _, ok := s.q.(*store.Queries); ok {
		// A live query surface: resolve D11, INSERT and freeze atomically. *store.Queries
		// satisfies Store, so the caller's insert closure is reused verbatim.
		run, _, err := s.createRunAtomic(ctx, userID, explicit, func(q *store.Queries, resolved resolvedHarness) (store.Run, error) {
			return insert(q, resolved)
		})
		return run, err
	}
	// In-memory unit fakeStore: no live *store.Queries and no real transaction. Resolve to Claude
	// (what these fakes assert) and run the insert directly. An explicit non-Claude request is
	// still refused rather than silently downgraded, so a fake can never assert a wrong harness.
	if explicit != nil && *explicit != HarnessClaude {
		return store.Run{}, errNoCredentialForHarness
	}
	return insert(s.q, resolvedHarness{Harness: HarnessClaude})
}

// ResolveHarnessForUser resolves the harness a user's next run WOULD get under D11 (PRD #1429 M2),
// reads-only and outside any transaction. It is the exported door the scheduler's fire-time
// codex+override conflict gate and the schedule create/edit override validator use to learn the
// effective harness; the authoritative atomic resolution still happens inside createRunAtomic at
// fire/create. explicit is a pinned selection (nil for implicit). It returns the resolver's typed
// refusals (ErrNoCredentialForHarness / ErrNoUsableCredential) unchanged.
func (s *Service) ResolveHarnessForUser(ctx context.Context, userID uuid.UUID, explicit *Harness) (Harness, error) {
	res, err := s.resolveRunHarness(ctx, userID, explicit)
	if err != nil {
		return "", err
	}
	return res.Harness, nil
}

// harnessModelFallbackKind is the run_messages.kind of the server-authored, nonsecret status note
// recorded when a run's stored model is incompatible with its resolved harness (PRD #1429 D6). The
// kind column is bare text so this needs no migration and no new query; it is NOT 'status'/'error',
// so usage folding (usage_fold.go) ignores it by construction.
const harnessModelFallbackKind = "harness_model_fallback"

// harnessModelFallbackPayload is the note body: enough for a reader (web M4 / CLI M5) to say the
// stored model was dropped for the harness's default. All fields are server-authored nonsecret
// labels (a harness name and a model id), never credential material.
type harnessModelFallbackPayload struct {
	Harness      string `json:"harness"`
	DroppedModel string `json:"dropped_model"`
	Note         string `json:"note"`
}

// recordHarnessModelFallbackNote appends the D6 nonsecret status note to the run's feed and
// advances runs.last_seq, returning the last_seq the claim payload must adopt so the resuming
// worker never re-uses the note's seq. It runs at claim-assembly time — synchronously right after
// ClaimRun and before the worker emits any frame — so seq = startLastSeq+1 is collision-free
// without a run-row lock. It is BEST-EFFORT: any error (including a fenced-out stale claim) logs
// and returns startLastSeq unchanged rather than failing the claim over an advisory note. The
// insert is fenced on the run's claim_generation, so a stale re-assembly persists nothing.
func (s *Service) recordHarnessModelFallbackNote(ctx context.Context, run store.Run, startLastSeq int32, harness Harness, droppedModel string) int32 {
	payload := harnessModelFallbackPayload{
		Harness:      string(harness),
		DroppedModel: droppedModel,
		Note:         "stored model is not valid for the " + string(harness) + " harness; using the harness default",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("workersvc: marshal harness model fallback note", "run_id", run.ID.String(), "error", err)
		return startLastSeq
	}
	seq := startLastSeq + 1
	gen := run.ClaimGeneration
	res, err := s.q.InsertRunMessage(ctx, store.InsertRunMessageParams{
		RunID:           run.ID,
		Seq:             seq,
		Kind:            harnessModelFallbackKind,
		Payload:         raw,
		ClaimGeneration: pgconv.Int8Ptr(&gen),
	})
	if err != nil {
		slog.Warn("workersvc: insert harness model fallback note", "run_id", run.ID.String(), "error", err)
		return startLastSeq
	}
	if !res.Inserted {
		// A seq collision (extraordinarily unlikely at claim-assembly time) or a fenced-out stale
		// claim: keep the caller's high-water, do not advance onto an occupied/absent seq.
		return startLastSeq
	}
	if _, err := s.q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{ID: run.ID, Seq: seq, ClaimGeneration: pgconv.Int8Ptr(&gen)}); err != nil {
		slog.Warn("workersvc: advance last_seq for harness model fallback note", "run_id", run.ID.String(), "error", err)
		return startLastSeq
	}
	if s.bcast != nil {
		s.bcast.PublishMessage(run.ID, seq, harnessModelFallbackKind, "", "", "", raw, s.now())
	}
	return seq
}
