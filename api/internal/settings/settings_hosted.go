package settings

// This file holds the hosted-worker accessors, their bounds const and their
// write-time validator (PRD #1021 M3, split verbatim from settings.go).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// maxHostedWorkerQuota bounds the per-user hosted-worker quota (PRD #58). Each
// unit is a real pod plus its volumes, so the number an admin types spends cluster
// capacity; the worker namespace's ResourceQuota is the actual backstop (Decision
// 8) and this only catches a typo — an admin meaning 2 and typing 20 gets a
// crowded namespace, one typing 200 gets a rejected write instead of a
// ResourceQuota incident.
const maxHostedWorkerQuota = 20

// EphemeralWorkersEnabled reports whether ephemeral worker auto-provisioning is
// enabled instance-wide (PRD #529 M2): the global kill-switch. Stored as the text
// "true"/"false"; any other value falls back to the compiled-in default (false) —
// the same strict junk-tolerance as JudgeEnabled, so a malformed value never
// silently starts spinning cluster capacity on demand. This is the INSTANCE gate;
// the per-user opt-in (users.ephemeral_workers_enabled) is checked separately, and
// both must be true before the provisioner acts.
func (c *Cache) EphemeralWorkersEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyEphemeralWorkersEnabled)
}

// HostedWorkerQuota returns the per-user hosted-worker quota (PRD #58 Decision 8);
// 0 means self-service provisioning is disabled.
//
// Its caller (the provision handler) reads it STRICTLY — a non-nil error is a 500,
// not a fallback — unlike the best-effort `v, _ :=` label reads. Those degrade
// toward the safe side (an unlabeled issue stays gated); this one would degrade
// toward provisioning against a number no admin chose, on a cold-cache blip. The
// junk-tolerance inside intSetting still applies to a hand-edited row, which
// Validate cannot reach retroactively.
func (c *Cache) HostedWorkerQuota(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHostedWorkerQuota)
}

// validateHostedWorkerQuota is the write-time gate for the per-user hosted-worker
// quota (PRD #58 Decision 8): a base-10 integer in {0} ∪ [1, maxHostedWorkerQuota],
// where 0 is the documented "self-service disabled" value rather than a rejection.
// Negatives and non-integers are refused.
//
// The explicit Validate case this backs is load-bearing, not decoration. Validate's
// default branch falls through to ValidateLabel, which accepts any non-empty
// ≤64-char string — so an integer key that is in Defaults but missing from the
// switch would accept "abc", and intSetting would then silently fall back to the
// compiled-in default on every read. An admin typing 0 to disable self-service
// would be told it saved and would still get 2. An int setting must fail the WRITE,
// which is the only moment a human is present to be told.
func validateHostedWorkerQuota(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of workers")
	}
	if n == 0 {
		return nil
	}
	if n < 0 || n > maxHostedWorkerQuota {
		return fmt.Errorf("must be 0 (self-service disabled) or between 1 and %d workers", maxHostedWorkerQuota)
	}
	return nil
}
