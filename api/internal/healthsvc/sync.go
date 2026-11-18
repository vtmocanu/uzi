package healthsvc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
)

// SyncRegistry holds process-local issue-sync history. Entry pointers are identity
// tokens: invalidation deletes them, so an old completion cannot restore history.
type SyncRegistry struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[uuid.UUID]*syncEntry
}

type syncEntry struct {
	lastSuccess     time.Time
	failingSince    time.Time
	lastClass       forge.ErrorClass
	observationTime time.Time
}

type syncSnapshot struct {
	identity *syncEntry
	value    syncEntry
}

// NewSyncRegistry creates an empty registry; seed syncs do not feed it.
func NewSyncRegistry(now func() time.Time) *SyncRegistry {
	if now == nil {
		now = time.Now
	}
	return &SyncRegistry{now: now, entries: make(map[uuid.UUID]*syncEntry)}
}

// Begin binds a completion to the current entry. Each completion is accepted once.
func (r *SyncRegistry) Begin(id uuid.UUID) func(bool, forge.ErrorClass) {
	r.mu.Lock()
	e := r.entries[id]
	if e == nil {
		e = &syncEntry{}
		r.entries[id] = e
	}
	r.mu.Unlock()
	done := false
	return func(ok bool, class forge.ErrorClass) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if done || r.entries[id] != e {
			return
		}
		done = true
		now := r.now()
		e.observationTime = now
		if ok {
			e.lastSuccess = now
			e.failingSince = time.Time{}
			e.lastClass = ""
		} else {
			if e.failingSince.IsZero() {
				e.failingSince = now
			}
			switch class {
			case forge.ErrorClassTimeout, forge.ErrorClassAuth, forge.ErrorClassServerError, forge.ErrorClassRateLimited:
				e.lastClass = class
			default:
				e.lastClass = forge.ErrorClassOther
			}
		}
	}
}

// Invalidate resets history after every successful enabled write, even idempotent writes.
func (r *SyncRegistry) Invalidate(id uuid.UUID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.entries, id)
	r.mu.Unlock()
}

// snapshot copies values and identities before enumeration. Enumeration runs without
// the mutex; only captured entries with unchanged identity are eligible for pruning.
func (r *SyncRegistry) snapshot(ctx context.Context, list func(context.Context) ([]uuid.UUID, error)) ([]syncEntry, error) {
	captured := make(map[uuid.UUID]syncSnapshot)
	if r != nil {
		r.mu.Lock()
		for id, e := range r.entries {
			captured[id] = syncSnapshot{e, *e}
		}
		r.mu.Unlock()
	}
	ids, err := list(ctx)
	if err != nil {
		return nil, err
	}
	enabled := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		enabled[id] = true
	}
	if r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
	}
	out := make([]syncEntry, 0, len(ids))
	for _, id := range ids {
		snap, ok := captured[id]
		if !ok || r == nil || r.entries[id] != snap.identity {
			out = append(out, syncEntry{})
		} else {
			out = append(out, snap.value)
		}
	}
	if r != nil {
		for id, snap := range captured {
			if !enabled[id] && r.entries[id] == snap.identity {
				delete(r.entries, id)
			}
		}
	}
	return out, nil
}

func (s *Service) checkForgeSync(ctx context.Context, now time.Time) apitypes.HealthCheckDTO {
	return s.checkSync(ctx, now, syncCheckSpec{
		id:       "forge.sync",
		registry: s.cfg.ForgeSyncRegistry,
		list:     s.cfg.Store.ListEnabledRepoIDs,
		naText:   "No enabled repositories.",
		actions: syncActions{
			other:   "read the api logs for `poller:` errors.",
			timeout: "check outbound reachability from the api pod. If other pods reach the forge and only the api process times out, restarting the api pod is a workaround, not a fix.",
			auth:    "the connection's token is invalid or lacks scope; re-check the forge connection.",
			server:  "forge-side; check the forge's status and rate limits, and wait.",
		},
	})
}

// checkIssueTrackerSync is the linked repos' counterpart: the same severity and evidence
// rules over the linked issue sync results, with issue tracker action texts.
func (s *Service) checkIssueTrackerSync(ctx context.Context, now time.Time) apitypes.HealthCheckDTO {
	return s.checkSync(ctx, now, syncCheckSpec{
		id:       "issue_tracker.sync",
		registry: s.cfg.IssueTrackerSyncRegistry,
		list:     s.cfg.Store.ListEnabledLinkedRepoIDs,
		naText:   "No enabled repositories linked to an issue tracker.",
		actions: syncActions{
			other:   "read the api logs for `poller: linked issue sync` and `poller: build linked issue tracker` errors.",
			timeout: "check reachability of the issue tracker from the api pod.",
			auth:    "the issue tracker connection's token is invalid or lacks access; re-check the issue tracker connection.",
			server:  "issue-tracker side; check the issue tracker's status and rate limits, and wait.",
		},
	})
}

// syncCheckSpec parameterises checkSync: which check, which registry and enumeration,
// and the texts that differ between the forge and issue tracker checks.
type syncCheckSpec struct {
	id       string
	registry *SyncRegistry
	list     func(context.Context) ([]uuid.UUID, error)
	naText   string
	actions  syncActions
}

type syncActions struct{ other, timeout, auth, server string }

func (s *Service) checkSync(ctx context.Context, now time.Time, spec syncCheckSpec) apitypes.HealthCheckDTO {
	c := s.base(spec.id)
	entries, err := spec.registry.snapshot(ctx, spec.list)
	if err != nil {
		return degradeUnknown(c, spec.id, err)
	}
	if len(entries) == 0 {
		c.Severity = sevNA
		c.Summary = spec.naText
		return c
	}
	interval := s.cfg.ForgeSyncInterval
	if interval <= 0 {
		interval = time.Minute
	}
	failing, pending, aged := 0, 0, 0
	var longest time.Duration
	var baseline, latest time.Time
	class := forge.ErrorClassOther
	for _, e := range entries {
		if e.observationTime.IsZero() {
			pending++
			continue
		}
		if e.failingSince.IsZero() {
			continue
		}
		failing++
		age := now.Sub(e.failingSince)
		if baseline.IsZero() || e.failingSince.Before(baseline) {
			baseline = e.failingSince
			longest = age
		}
		if age >= 3*interval {
			aged++
		}
		// A lexical closed-enum tie-break makes evidence independent of database order.
		if latest.IsZero() || e.observationTime.After(latest) || (e.observationTime.Equal(latest) && e.lastClass < class) {
			latest, class = e.observationTime, e.lastClass
		}
	}
	c.Summary = fmt.Sprintf("%d of %d failing; %d pending.", failing, len(entries), pending)
	c.Evidence = []apitypes.HealthEvidenceDTO{
		{Label: "Failing repositories", Value: fmt.Sprintf("%d of %d failing", failing, len(entries))},
		{Label: "Pending repositories", Value: fmt.Sprintf("%d", pending)},
	}
	if failing > 0 {
		c.Evidence = append(c.Evidence,
			apitypes.HealthEvidenceDTO{Label: "Longest failure age", Value: humanDur(longest)},
			apitypes.HealthEvidenceDTO{Label: "Latest error class", Value: string(class)})
		c.Since = sincePtr(baseline)
	}
	switch {
	case failing > 0 && (longest >= 10*interval || aged == len(entries)):
		c.Severity = sevDanger
	case aged > 0:
		c.Severity = sevWarn
	case pending > 0:
		c.Severity = sevUnknown
	default:
		c.Severity = sevOK
	}
	if c.Severity == sevWarn || c.Severity == sevDanger {
		action := spec.actions.other
		switch class {
		case forge.ErrorClassTimeout:
			action = spec.actions.timeout
		case forge.ErrorClassAuth:
			action = spec.actions.auth
		case forge.ErrorClassServerError, forge.ErrorClassRateLimited:
			action = spec.actions.server
		}
		c.Action = strPtr(action)
	}
	return c
}
