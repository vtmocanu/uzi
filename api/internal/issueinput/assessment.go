// Package issueinput provides server-side repository author assessment.
package issueinput

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

const AssessmentTimeout = 30 * time.Second
const InteractiveAssessmentTimeout = 5 * time.Second
const MaxDistinctAuthors = 200

var ErrAuthorBudget = errors.New("issueinput: distinct author lookup budget exhausted")

// AuthorLookup is the M2 caller seam; Forge and forgetest.BaseFake implement it.
type AuthorLookup interface {
	RepositoryAuthorEligibility(context.Context, int64, int64) (forge.AuthorEligibility, error)
}

type decision struct {
	disposition forge.AuthorEligibility
	err         error
}
type identity struct {
	id         int64
	unresolved string
}

// Assessment owns one operation, repository, and driver's evidence cache.
// Checks are serialized, with at most 200 distinct identities triggering lookups.
// A failure is cached for that identity and does not block independent authors.
type Assessment struct {
	ctx       context.Context
	cancel    context.CancelFunc
	lookup    AuthorLookup
	project   int64
	mu        sync.Mutex
	decisions map[identity]decision
}

// NewAssessment starts the child deadline immediately, before raw issue capture.
func NewAssessment(parent context.Context, lookup AuthorLookup, projectID int64) *Assessment {
	return NewAssessmentWithTimeout(parent, lookup, projectID, AssessmentTimeout)
}

// NewAssessmentWithTimeout starts one total deadline, bounded by AssessmentTimeout.
// Nonpositive timeouts use the background default; parent deadlines still win.
func NewAssessmentWithTimeout(parent context.Context, lookup AuthorLookup, projectID int64, timeout time.Duration) *Assessment {
	if timeout <= 0 || timeout > AssessmentTimeout {
		timeout = AssessmentTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	return &Assessment{ctx: forge.BeginAuthorAssessment(ctx), cancel: cancel,
		lookup: lookup, project: projectID, decisions: make(map[identity]decision)}
}

// Context must also be used for raw issue capture so it shares the deadline.
func (a *Assessment) Context() context.Context { return a.ctx }
func (a *Assessment) Close()                   { a.cancel() }

// Author assesses an issue's stable identity. Unresolved identities consume the
// budget too, keyed by captured username; they never fall back to username lookup.
func (a *Assessment) Author(issue forge.Issue) (forge.AuthorEligibility, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := identity{id: issue.AuthorForgeUserID}
	if key.id <= 0 {
		key.id = 0
		key.unresolved = issue.Author
	}
	if v, ok := a.decisions[key]; ok {
		return v.disposition, v.err
	}
	if err := a.ctx.Err(); err != nil {
		return forge.AuthorUnknown, err
	}
	if len(a.decisions) >= MaxDistinctAuthors {
		return forge.AuthorUnknown, ErrAuthorBudget
	}
	v := decision{disposition: forge.AuthorUnknown, err: forge.ErrAuthorUnknown}
	if key.id > 0 {
		v.disposition, v.err = a.lookup.RepositoryAuthorEligibility(a.ctx, a.project, key.id)
		if err := a.ctx.Err(); err != nil {
			v = decision{forge.AuthorUnknown, err}
		}
		if v.err != nil || (v.disposition != forge.AuthorEligible && v.disposition != forge.AuthorNotEligible) {
			v.disposition = forge.AuthorUnknown
			if v.err == nil {
				v.err = forge.ErrAuthorUnknown
			}
		}
	}
	a.decisions[key] = v
	return v.disposition, v.err
}
