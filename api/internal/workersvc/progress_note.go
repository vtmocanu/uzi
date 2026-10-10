package workersvc

// The progress_note run-message kind (PRD #2603): a worker-written one-sentence "Now"
// summary of the active milestone, plus the usage of the small-model call that wrote it.
//
// Two api-side rules live here:
//
//  1. INGEST (normalizeProgressNotePayload): the stored payload is rebuilt from scratch with
//     only {text, milestone_id, model_usage}, each usage entry carrying the server-resolved
//     costStatus (and costUSD when metered). Any other key is dropped, in particular `event`
//     (a payload that says event:"result" would be read by the usage tail and the web fold as
//     the end of a leg) and `usage` (the web fold would count it a second time).
//  2. USAGE FOLD (foldProgressNoteUsage): the note's model_usage is folded into run_usage under
//     its OWN key so it can neither collapse into, nor be collapsed by, the run's own result
//     frames. adr/2603-progress-note-usage-key.md records why the key is
//     (model = "progress_note:<model>", lineage_epoch = the note's seq).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/anthropicprice"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runactivity"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	// KindProgressNote is the run_messages.kind of a Now summary note.
	KindProgressNote = "progress_note"

	// progressNoteModelPrefix keys a note's usage apart from the run's own result frames
	// that may name the same model in the same leg.
	progressNoteModelPrefix = "progress_note:"

	// MaxProgressNoteTextRunes is the display cap of the note text (PRD #2603 Output).
	MaxProgressNoteTextRunes = 120
	// maxProgressNoteModels bounds model_usage entries kept per note: one summary call names
	// one model, so a handful is generous and a hostile object cannot fan out run_usage rows. Past the cap the
	// first valid models in sorted raw-key order are kept, so the kept set is deterministic per payload.
	maxProgressNoteModels = 4
	// maxProgressNoteMarkerRunes caps the service_tier / speed / inference_geo strings.
	maxProgressNoteMarkerRunes = 32
)

// SanitizeProgressNoteText applies the ingest rule to a note's text: strip the
// terminal-unsafe runes (control incl. ESC, and the Cf format runes that carry bidi
// overrides) with the runactivity rune rule, trim, and cap at MaxProgressNoteTextRunes
// runes. Markup is left as inert plain text; every surface renders the note as text.
func SanitizeProgressNoteText(s string) string {
	return truncateRunes(strings.TrimSpace(runactivity.Sanitize(s)), MaxProgressNoteTextRunes)
}

// progressNoteModelUsage is one stored model_usage entry. The embedded resultModelUsage
// carries the same camelCase token/cost fields the result frames use; the three markers
// decide whether an unpriced Claude entry can be priced from the standard table.
type progressNoteModelUsage struct {
	resultModelUsage
	// CacheCreation5mInputTokens / CacheCreation1hInputTokens are the optional TTL split of
	// the cache writes; anthropicprice.Price refuses to price cache writes without it.
	CacheCreation5mInputTokens *int64 `json:"cacheCreation5mInputTokens,omitempty"`
	CacheCreation1hInputTokens *int64 `json:"cacheCreation1hInputTokens,omitempty"`
	ServiceTier                string `json:"service_tier,omitempty"`
	Speed                      string `json:"speed,omitempty"`
	InferenceGeo               string `json:"inference_geo,omitempty"`
}

// progressNotePayload is the whole stored shape of a progress_note payload.
type progressNotePayload struct {
	Text        string                            `json:"text"`
	MilestoneID string                            `json:"milestone_id"`
	ModelUsage  map[string]progressNoteModelUsage `json:"model_usage,omitempty"`
}

// normalizeProgressNotePayload rebuilds a progress_note payload from only the allowed
// keys. It never fails: a payload that is not an object, or whose fields are the wrong
// type, yields an empty note (empty text), which every reader ignores. It runs AFTER
// sanitizePayloadJSON, so the input is valid JSON free of NUL and unpaired surrogates.
//
// Each stored model_usage entry carries the SERVER-RESOLVED cost (resolveProgressNoteCost,
// for the run's harness): costStatus is always one of metered|subscription|unreported, and
// costUSD is written only for metered, quantized exactly as numericUSD stores it. That lets
// the web reader show the same dollars as run_usage without a client price table. costUSD is
// never written for a non-metered entry: for Claude a present costUSD refolds as metered, so a
// stored 0 on an unreported entry would turn it into a metered $0. An unreported Claude entry
// is therefore re-priced on every refold, so it is not strictly idempotent if a later price
// table learns the model.
func normalizeProgressNotePayload(raw json.RawMessage, harness string) json.RawMessage {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		in = nil
	}
	out := progressNotePayload{}
	var s string
	if json.Unmarshal(in["text"], &s) == nil {
		out.Text = SanitizeProgressNoteText(s)
	}
	if json.Unmarshal(in["milestone_id"], &s) == nil {
		out.MilestoneID = truncateRunes(strings.TrimSpace(runactivity.Sanitize(s)), maxMilestoneIDRunes)
	}
	var usage map[string]json.RawMessage
	if json.Unmarshal(in["model_usage"], &usage) == nil {
		// Walk the RAW keys in sorted order (map iteration order is random and a re-delivered
		// batch must normalise to the same rows). Two raw keys can sanitise to the same name
		// (" m" and "m"): the first valid entry in raw-key order wins, a later duplicate is
		// ignored. The cap then keeps the first maxProgressNoteModels valid names in that walk;
		// an invalid entry never consumes a slot. Raw-key order and sanitised-name order can
		// differ (leading whitespace sorts first), so the kept set is "first valid by raw key".
		rawKeys := make([]string, 0, len(usage))
		for k := range usage {
			rawKeys = append(rawKeys, k)
		}
		sort.Strings(rawKeys)
		dropped := 0
		for _, rawKey := range rawKeys {
			model := truncateRunes(strings.TrimSpace(runactivity.Sanitize(rawKey)), maxUsageModelRunes-len(progressNoteModelPrefix))
			if model == "" {
				continue
			}
			if _, dup := out.ModelUsage[model]; dup {
				continue
			}
			mu, ok := normalizeProgressNoteModelUsage(usage[rawKey], harness, model)
			if !ok {
				continue
			}
			if len(out.ModelUsage) >= maxProgressNoteModels {
				dropped++
				continue
			}
			if out.ModelUsage == nil {
				out.ModelUsage = map[string]progressNoteModelUsage{}
			}
			out.ModelUsage[model] = mu
		}
		if dropped > 0 {
			slog.Warn("progress_note model_usage entries dropped past the cap",
				"kept", len(out.ModelUsage), "dropped", dropped, "cap", maxProgressNoteModels)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		// Unreachable: every field is a string, an int64 or a sanitised raw number.
		return json.RawMessage(`{"text":"","milestone_id":""}`)
	}
	return b
}

// normalizeProgressNoteModelUsage keeps the known fields of one model_usage entry, each
// decoded tolerantly so one malformed field never discards its siblings. The returned
// entry's costStatus/costUSD are the resolved ones (see normalizeProgressNotePayload).
func normalizeProgressNoteModelUsage(raw json.RawMessage, harness, model string) (progressNoteModelUsage, bool) {
	var f map[string]json.RawMessage
	if json.Unmarshal(raw, &f) != nil {
		return progressNoteModelUsage{}, false
	}
	var mu progressNoteModelUsage
	mu.InputTokens = tolerantTokens(f["inputTokens"])
	mu.OutputTokens = tolerantTokens(f["outputTokens"])
	mu.CacheReadInputTokens = tolerantTokens(f["cacheReadInputTokens"])
	mu.CacheCreationInputTokens = tolerantTokens(f["cacheCreationInputTokens"])
	mu.CacheCreation5mInputTokens = tolerantOptionalTokens(f["cacheCreation5mInputTokens"])
	mu.CacheCreation1hInputTokens = tolerantOptionalTokens(f["cacheCreation1hInputTokens"])
	if usd, ok := resolveCostUSD(f["costUSD"]); ok && usd >= 0 && usd <= maxCostUSD {
		mu.CostUSD, _ = json.Marshal(usd)
	}
	if marker := resolveCostStatusMarker(f["costStatus"]); marker != "" && marker != costMarkerInvalid {
		mu.CostStatus, _ = json.Marshal(truncateRunes(marker, maxProgressNoteMarkerRunes))
	}
	mu.ServiceTier = tolerantMarker(f["service_tier"])
	mu.Speed = tolerantMarker(f["speed"])
	mu.InferenceGeo = tolerantMarker(f["inference_geo"])
	// Judge emptiness on the worker-supplied (validated) costUSD, before the resolved cost
	// below overwrites it: a table-priced $0 on a zero-token known model is not a cost claim.
	workerCost := len(mu.CostUSD) > 0
	// Replace the worker's cost claim with the server-resolved one.
	status, usd := resolveProgressNoteCost(harness, model, mu)
	mu.CostStatus, _ = json.Marshal(status)
	mu.CostUSD = nil
	if status == costStatusMetered {
		mu.CostUSD, _ = json.Marshal(numericToFloat64(usd))
	}
	// An entry with nothing to fold would only add a zero-token row.
	if mu.InputTokens == 0 && mu.OutputTokens == 0 && mu.CacheReadInputTokens == 0 &&
		mu.CacheCreationInputTokens == 0 && !workerCost {
		return progressNoteModelUsage{}, false
	}
	return mu, true
}

// resolveProgressNoteCost resolves one note entry's (cost_status, cost_usd) for the run's
// harness: deriveUsageCost as for a result frame, except a non-Codex entry with no provider
// costUSD is priced from the standard table (priceProgressNoteEntry). Both the fold and the
// ingest normalisation use it, so the stored entry and the run_usage row agree.
func resolveProgressNoteCost(harness, model string, mu progressNoteModelUsage) (string, pgtype.Numeric) {
	marker := resolveCostStatusMarker(mu.CostStatus)
	emitted, present := resolveCostUSD(mu.CostUSD)
	costStatus, costUSD := deriveUsageCost(harness, marker, emitted, present)
	if harness != harnessCodex && !present {
		costStatus, costUSD = priceProgressNoteEntry(model, mu)
	}
	return costStatus, costUSD
}

// tolerantOptionalTokens is tolerantTokens for a field whose absence matters: nil when the
// key is missing or malformed, else a pointer to the clamped count.
func tolerantOptionalTokens(raw json.RawMessage) *int64 {
	var v float64
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return nil
	}
	n := tolerantTokens(raw)
	return &n
}

// tolerantTokens decodes a token count that must be a finite non-negative JSON number;
// anything else is 0.
func tolerantTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var v float64
	if json.Unmarshal(raw, &v) != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// tolerantMarker decodes a short marker string, stripped and capped; non-strings are "".
func tolerantMarker(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return truncateRunes(strings.TrimSpace(runactivity.Sanitize(s)), maxProgressNoteMarkerRunes)
}

// foldProgressNoteUsage folds one progress_note frame's model_usage into run_usage.
//
// Key: model = "progress_note:<model>" and lineage_epoch = the note's own seq, lineage_index 0,
// usage_basis per_leg. Within one run the totals view takes a MAX per (model, lineage_epoch)
// group, so a shared key would let a note collapse into (or under) the run's own haiku result
// frame, and two notes in one leg would collapse into each other. The seq epoch is a pure
// function of the frame, so a re-delivered batch and RefoldRunUsage land on the same row, and
// GREATEST in UpsertRunUsage makes the repeat a no-op.
//
// Cost: resolveProgressNoteCost (deriveUsageCost as for a result frame, except a CLAUDE entry
// with no provider costUSD is priced from the standard table); an entry the table cannot price
// stores cost_status 'unreported' with cost 0 and keeps its tokens. A missing cost is never a
// metered $0. The stored entries already carry the resolved cost (see
// normalizeProgressNotePayload), so folding them resolves to the same row.
func foldProgressNoteUsage(ctx context.Context, q usageFoldQuerier, run store.Run, sessionID string, m IncomingMessage) error {
	var p progressNotePayload
	if err := json.Unmarshal(m.Payload, &p); err != nil || len(p.ModelUsage) == 0 {
		return nil // malformed or usage-free note: nothing to fold, never fail the append
	}
	for model, mu := range p.ModelUsage {
		if model == "" {
			continue
		}
		costStatus, costUSD := resolveProgressNoteCost(run.Harness, model, mu)
		if err := q.UpsertRunUsage(ctx, store.UpsertRunUsageParams{
			RunID:               run.ID,
			SessionID:           sessionID,
			Model:               truncateRunes(progressNoteModelPrefix+model, maxUsageModelRunes),
			LineageEpoch:        m.Seq,
			InputTokens:         nonNegTokens(mu.InputTokens),
			CacheReadTokens:     nonNegTokens(mu.CacheReadInputTokens),
			CacheCreationTokens: nonNegTokens(mu.CacheCreationInputTokens),
			OutputTokens:        nonNegTokens(mu.OutputTokens),
			CostUsd:             costUSD,
			Harness:             run.Harness,
			CostStatus:          costStatus,
			UsageBasis:          usageBasisPerLeg,
			LineageIndex:        0,
			ClaimGeneration:     pgconv.Int8Ptr(m.ClaimGeneration),
		}); err != nil {
			return fmt.Errorf("fold progress note usage (run %s, model %s): %w", run.ID, model, err)
		}
	}
	return nil
}

// priceProgressNoteEntry prices a Claude note entry that carries no provider costUSD from
// the standard Anthropic table, returning ('metered', price) or ('unreported', 0) when the
// table cannot price it (unknown model, non-standard tier/speed/geo, cache writes without a
// 5m/1h split).
func priceProgressNoteEntry(model string, mu progressNoteModelUsage) (string, pgtype.Numeric) {
	u := anthropicprice.Usage{
		Model:                    model,
		InputTokens:              nonNegTokens(mu.InputTokens),
		CacheReadInputTokens:     nonNegTokens(mu.CacheReadInputTokens),
		CacheCreationInputTokens: nonNegTokens(mu.CacheCreationInputTokens),
		OutputTokens:             nonNegTokens(mu.OutputTokens),
		ServiceTier:              mu.ServiceTier,
		Speed:                    mu.Speed,
		InferenceGeo:             mu.InferenceGeo,
		CacheCreation5mTokens:    mu.CacheCreation5mInputTokens,
		CacheCreation1hTokens:    mu.CacheCreation1hInputTokens,
	}
	c, ok := anthropicprice.Price(u)
	if !ok {
		return costStatusUnreported, numericUSD(0)
	}
	return costStatusMetered, numericUSD(c.USD())
}
