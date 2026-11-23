package workersvc

import (
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// next_claim_view.go: read-only accessors over a NextClaimSnapshot for the weeklylimit package (PRD
// #4123), which names a run's credential, its worker's bind mode and the owner's effective limit
// without reaching into the snapshot's unexported fields.

// ExcludedFromNextClaim reports a run the next-claim preview and the weekly-limit block never speak
// for: a provider-bound run or a Codex API-key run. It is the one copy of that predicate.
func ExcludedFromNextClaim(run store.Run) bool { return nextClaimExcluded(run) }

// EffectiveLimit is the percent at which the owner's weekly reserve holds, or 100 with no threshold.
func (n *NextClaimSnapshot) EffectiveLimit() float64 { return n.effectiveLimit() }

// AnthropicCredentialKey is the credential key of an Anthropic token secret. It is the one spelling
// shared by the run weekly-limit block and the weekly-warning ledger.
func AnthropicCredentialKey(secretID uuid.UUID) string { return "anthropic:" + secretID.String() }

// CodexCredentialKey is the credential key of a Codex provider account (never of an alias).
func CodexCredentialKey(accountID uuid.UUID) string { return "codex:" + accountID.String() }

// CurrentCredential names the credential the run is spending: its credential key
// ("anthropic:<secret id>" or "codex:<provider account id>") and display label. ok is false when the
// run has no resolvable credential in the snapshot (unbound, disabled or unlinked).
func (n *NextClaimSnapshot) CurrentCredential(run store.Run) (key, label string, ok bool) {
	if run.Harness == harnessCodex {
		if !run.CodexSecretID.Valid {
			return "", "", false
		}
		alias := uuid.UUID(run.CodexSecretID.Bytes)
		acct, found := n.codexAccountOfAlias(alias)
		if !found {
			return "", "", false
		}
		lbl, found := n.codexAliasLabels[alias]
		if !found {
			return "", "", false
		}
		return CodexCredentialKey(acct.id), lbl, true
	}
	id := currentAnthropic(run)
	if id == uuid.Nil && run.LimitDeadSecretID.Valid {
		id = uuid.UUID(run.LimitDeadSecretID.Bytes)
	}
	if id == uuid.Nil {
		return "", "", false
	}
	c, found := n.candidate(id)
	if !found {
		return "", "", false
	}
	return AnthropicCredentialKey(id), c.Label, true
}

// WorkerBinding is the recorded name and Anthropic bind mode of one of the owner's workers; ok is
// false for an unknown worker.
func (n *NextClaimSnapshot) WorkerBinding(id uuid.UUID) (name, mode string, ok bool) {
	w, found := n.workers[id]
	if !found {
		return "", "", false
	}
	return w.Name, w.AnthropicBindMode, true
}

// UsableAlternative is the Anthropic token other than the run's current one that the owner could
// switch to: enabled, not rejected, with a fresh weekly reading below the effective limit (the lowest
// reading wins, ties by id). It is what NextClaimReason leaves unnamed for the pinned and not_auto
// lanes, whose next claim does not run the selector. Nil when there is none.
func (n *NextClaimSnapshot) UsableAlternative(run store.Run) *CredentialRef {
	if run.Harness == harnessCodex || n.reserve == nil {
		return nil
	}
	cur := currentAnthropic(run)
	limit := n.effectiveLimit()
	var best *CredentialRef
	bestPct := 0.0
	for _, c := range n.cands {
		if c.SecretID == cur || c.Rejected {
			continue
		}
		w, ok := n.reserve.weeklyClaude[c.SecretID]
		if !ok || !w.OK || w.Pct >= limit {
			continue
		}
		if best == nil || w.Pct < bestPct || (w.Pct == bestPct && c.SecretID.String() < best.ID.String()) {
			best, bestPct = n.anthropicRef(c), w.Pct
		}
	}
	return best
}

// ReserveThreshold is the owner's weekly reserve threshold, or nil with none (the effective limit is
// then 100). It is the threshold input of usagereserve.EvaluateNear.
func (n *NextClaimSnapshot) ReserveThreshold() *int16 {
	if n.reserve == nil || n.reserve.threshold == nil {
		return nil
	}
	t := *n.reserve.threshold
	return &t
}
