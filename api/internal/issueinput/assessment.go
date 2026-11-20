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

// AuthorMapper is D11 of the external issue trackers PRD: an issue tracker
// driver that can say whether an author login is the connection's own account.
// It is the only author evidence a linked repo has; the forge's repository
// membership lookup says nothing about an issue tracker's people. The consumer
// declares the interface; the YouTrack driver and the forgesvc write guard
// around it satisfy it.
type AuthorMapper interface {
	MapsAuthor(ctx context.Context, login string) (bool, error)
}

// IssueTextMapper is D11 for the issue text: whether every recorded change to the issue's title and body (creation included) was made by the connection's own account.
type IssueTextMapper interface {
	IssueTextMapped(ctx context.Context, projectID, issueIID int64) (bool, error)
}

// LinkedIssueSide marks a linked repo's issue side (the forgesvc write guard).
// Its presence, not a caller flag, switches an Assessment to D11 mapping, so
// the choice comes from the same resolver output that served the issue.
type LinkedIssueSide interface{ LinkedIssueSource() }

// Option configures an Assessment.
type Option func(*Assessment)

// WithIssueSide hands the assessment the issue side that served the issue. When
// it is a linked repo's issue side, RepositoryAuthorEligibility is never called:
// an author is eligible only when the side's AuthorMapper maps the login, and
// everything else (not mapped, an error, an empty login, a side that is not an
// AuthorMapper) is permission_unknown. An unlinked side changes nothing.
func WithIssueSide(tr forge.IssueTracker) Option {
	return func(a *Assessment) {
		if _, ok := tr.(LinkedIssueSide); ok {
			a.linked = true
			a.mapper, _ = tr.(AuthorMapper)
			a.textMapper, _ = tr.(IssueTextMapper)
		}
	}
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
	ctx     context.Context
	cancel  context.CancelFunc
	lookup  AuthorLookup
	project int64
	linked  bool         // D11: never consult lookup
	mapper  AuthorMapper // linked only; nil maps nobody
	// textMapper decides the issue text of a linked repo; nil means unknown.
	textMapper IssueTextMapper
	mu         sync.Mutex
	decisions  map[identity]decision
}

// NewAssessment starts the child deadline immediately, before raw issue capture.
func NewAssessment(parent context.Context, lookup AuthorLookup, projectID int64, opts ...Option) *Assessment {
	return NewAssessmentWithTimeout(parent, lookup, projectID, AssessmentTimeout, opts...)
}

// NewAssessmentWithTimeout starts one total deadline, bounded by AssessmentTimeout.
// Nonpositive timeouts use the background default; parent deadlines still win.
func NewAssessmentWithTimeout(parent context.Context, lookup AuthorLookup, projectID int64, timeout time.Duration, opts ...Option) *Assessment {
	if timeout <= 0 || timeout > AssessmentTimeout {
		timeout = AssessmentTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	a := &Assessment{ctx: forge.BeginAuthorAssessment(ctx), cancel: cancel,
		lookup: lookup, project: projectID, decisions: make(map[identity]decision)}
	for _, o := range opts {
		o(a)
	}
	return a
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
	if a.linked {
		// D11: a linked author is its login; the forge account id is not evidence.
		key = identity{unresolved: issue.Author}
	} else if key.id <= 0 {
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
	if a.linked {
		v = a.mapLinked(issue.Author)
	} else if key.id > 0 {
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

// IssueText assesses who wrote an issue's text. An unlinked repo's issue text is
// its author's: exactly Author. On a linked repo the reporter must map (Author)
// and every recorded change to the title and body must have been made by the
// connection's own account (IssueTextMapper); a side that cannot say, an error,
// or an expired deadline is permission_unknown. The text verdict is per issue
// and is never cached under the author's login, which Author keys by, so an
// issue with edited text never decides a comment by the same login.
func (a *Assessment) IssueText(issue forge.Issue) (forge.AuthorEligibility, error) {
	d, err := a.Author(issue)
	if !a.linked || d != forge.AuthorEligible || err != nil {
		return d, err
	}
	if a.textMapper == nil {
		return forge.AuthorUnknown, forge.ErrAuthorUnknown
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if cerr := a.ctx.Err(); cerr != nil {
		return forge.AuthorUnknown, cerr
	}
	ok, merr := a.textMapper.IssueTextMapped(a.ctx, a.project, issue.IID)
	if cerr := a.ctx.Err(); cerr != nil {
		return forge.AuthorUnknown, cerr
	}
	if merr != nil {
		return forge.AuthorUnknown, merr
	}
	if !ok {
		return forge.AuthorUnknown, forge.ErrAuthorUnknown
	}
	return forge.AuthorEligible, nil
}

// mapLinked decides a linked repo's author by D11: eligible only on (true, nil).
func (a *Assessment) mapLinked(login string) decision {
	unknown := decision{forge.AuthorUnknown, forge.ErrAuthorUnknown}
	if a.mapper == nil || login == "" {
		return unknown
	}
	ok, err := a.mapper.MapsAuthor(a.ctx, login)
	if cerr := a.ctx.Err(); cerr != nil {
		return decision{forge.AuthorUnknown, cerr}
	}
	if err != nil {
		return decision{forge.AuthorUnknown, err}
	}
	if !ok {
		return unknown
	}
	return decision{disposition: forge.AuthorEligible}
}
