// Package forge is uzi's forge-generic integration layer. Callers depend only
// on the Forge interface and the neutral domain types in this package; a driver
// (GitLab, GitHub or Forgejo) is selected by New and never imported directly.
// This is the seam that lets a second forge land without touching handlers,
// schema, or UI flows.
package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/vtmocanu/uzi/api/internal/redirectguard"
)

// ErrTokenIntrospectionUnsupported is returned by TokenInfo when the forge does
// not expose the token-introspection endpoint (GitLab < 15.5, which 404s
// GET /personal_access_tokens/self). It is a distinct sentinel — not a redacted
// generic error — so the privilege checker can downgrade "cannot verify scopes"
// to a warning instead of a hard block. It carries no secret material.
var ErrTokenIntrospectionUnsupported = errors.New("forge: token introspection not supported by this forge version")

// ErrNoPipeline is returned by LatestPipeline / LatestMRPipeline when the ref (or
// MR) has no pipeline at all: a project with no CI configured, or a ref/MR that
// has never triggered one. It is a distinct sentinel — not a redacted generic
// error — so the pipeline sync (PRD #6) can cache "no CI" for the ref instead of
// treating it as a failure. It carries no secret material.
var ErrNoPipeline = errors.New("forge: no pipeline for ref")

// ErrForgeVersionUnsupported is wrapped by the error VerifyToken returns when the
// forge server is older than the driver's minimum supported version (Forgejo <
// 16.0.0, D4 — the CI-fix loop's job-logs route first ships in 16.0.0). The
// wrapping error names the reported and required versions so it stays actionable
// when surfaced verbatim at connect. A re-run of VerifyToken on the periodic
// privilege sweep re-fires the gate automatically; today a downgrade surfaces
// there as the sweep's generic StatusError report (fail-safe — flagged, not
// silently OK). This sentinel exists so a privcheck consumer can errors.Is it and
// give a downgrade its own distinct finding — that consumer is not wired yet. It
// carries no secret material. The version gate is a FEATURE gate, never a security
// control (D4 L2): GET /version is public, unauthenticated and self-reported, so
// nothing security-relevant may hang on it. GitLab has no version floor and never
// returns this; it lives here because the sentinel is part of the driver contract,
// not one driver's implementation.
var ErrForgeVersionUnsupported = errors.New("forge: server version is older than this driver's minimum")

// ErrJobAttemptsUnsupported is returned by ListPipelineJobAttempts on a driver that
// cannot return a run's complete attempt history (Forgejo/Gitea: the jobs endpoint
// has no attempt filter and its rerun semantics are unverified). It means "complete
// history unavailable", never "no prior attempts": a caller may fall back to
// ListPipelineJobs and must then treat earlier attempts as unknown.
var ErrJobAttemptsUnsupported = errors.New("forge: complete job attempt history unavailable")

// ErrResolveUnsupported is returned by ResolveMergeRequestThread on a driver
// whose forge has no resolvable review-thread concept (Forgejo/Gitea — PRD #700
// Resolved facts). It is a distinct sentinel — not a redacted generic error — so
// the MR-review-rework worker can errors.Is it and swallow it (reply-only is the
// documented Forgejo contract), while a real resolve failure on GitLab/GitHub
// surfaces as an ordinary error. A never-resolved thread does not affect
// termination (that keys on the high-water mark); the only cost is UX. It carries
// no secret material.
var ErrResolveUnsupported = errors.New("forge: resolve is not supported on this forge")

// ErrMergeRequestNotFound is returned by GetMergeRequestSummary when the forge 404s
// the requested iid — the PR does not exist, or raced closed-and-purged between a
// ListMergeRequestRefs row and the per-iid read (PRD #1255 M2b). It is a distinct
// sentinel (not a redacted generic error) so a caller can errors.Is it: GetPull maps
// it to 404, and ListPulls SKIPS the raced-away row rather than failing the whole
// list. Each driver detects the forge 404 BEFORE wrapErr and returns this sentinel
// directly — the redactor severs the Unwrap chain (see redactor.error), so a wrapped
// sentinel would not survive errors.Is; the sentinel carries no token material, so
// returning it unredacted is safe.
var ErrMergeRequestNotFound = errors.New("forge: merge request not found")

// ErrRefNotFound is returned by BranchHead when the forge 404s the named branch
// (issue #1582 M1). A distinct sentinel, not a redacted generic error, so the
// ancestry-release caller can tell "the branch is gone" (retained/branch_missing) from a
// transport failure (retained/ancestry_unknown). Each driver detects the 404 BEFORE its
// redactor (which severs the Unwrap chain) and returns this directly; it carries no
// token material.
var ErrRefNotFound = errors.New("forge: ref not found")

// RateLimitError is the forge-neutral rate-limit error (PRD #1255 D4). The GitHub
// driver wraps GitHub's rate-limit shapes into it so a caller can errors.As it and
// map it to HTTP 429 + Retry-After. Only the GitHub driver produces it today: GitHub
// carries the 5 000/h primary budget the interactive reads share with the poller
// (D4), so it is the driver whose limit must reach the caller. GitLab and Forgejo
// return a plain redacted error instead — their limits are per-instance, the SDK/
// transport handles a 429 differently, and D4's per-connection interactive budget in
// the handler guards all three forges BEFORE the call regardless. Reset is the wall
// time the primary budget refills (GitHub's Rate.Reset); Retry is a wait duration
// where the forge supplies one directly (GitHub's AbuseRateLimitError.RetryAfter).
// Err is the already-REDACTED underlying error — RateLimitError carries no
// unredacted material, and Unwrap exposes only that redacted error, so errors.As
// through this type can never surface a token.
type RateLimitError struct {
	// Reset is when the primary rate-limit window refills; zero when unknown.
	Reset time.Time
	// Retry is an explicit wait hint from the forge; zero when none was supplied.
	Retry time.Duration
	// Err is the redacted underlying error (its message is authoritative).
	Err error
}

// Error returns the redacted underlying message, or a generic fallback.
func (e *RateLimitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return "forge: rate limited by the forge"
}

// Unwrap exposes the redacted underlying error (never the raw forge error, whose
// message could carry the token).
func (e *RateLimitError) Unwrap() error { return e.Err }

// Class identifies a neutral rate limit, including a locally reserved budget.
func (e *RateLimitError) Class() ErrorClass { return ErrorClassRateLimited }

// Type identifies a forge driver. It maps 1:1 to the forge_connections.forge_type
// column, which is CHECK-constrained to the same set.
type Type string

const (
	// TypeGitLab is the GitLab REST driver.
	TypeGitLab Type = "gitlab"
	// TypeForgejo is the Forgejo REST driver.
	TypeForgejo Type = "forgejo"
	// TypeGitHub is the GitHub REST driver (github.com only, classic PAT).
	TypeGitHub Type = "github"
)

// SupportedTypes lists every forge driver, in the order the connect UI offers
// them. It is the one list the connect API, its config advert and the startup
// seed validate against.
func SupportedTypes() []Type {
	return []Type{TypeGitLab, TypeForgejo, TypeGitHub}
}

// ParseType returns the driver named by s, or false when s names none.
// Matching is exact: the stored forge_type column is lowercase.
func ParseType(s string) (Type, bool) {
	for _, t := range SupportedTypes() {
		if string(t) == s {
			return t, true
		}
	}
	return "", false
}

// SupportedTypesList renders SupportedTypes for an error message:
// "gitlab, forgejo, github".
func SupportedTypesList() string {
	names := make([]string, 0, 3)
	for _, t := range SupportedTypes() {
		names = append(names, string(t))
	}
	return strings.Join(names, ", ")
}

// BotIdentity is the connected bot account, returned by VerifyToken.
type BotIdentity struct {
	// ForgeUserID is the bot's stable numeric user id on the forge.
	ForgeUserID int64
	// Username is the bot's login (e.g. "uzi-bot-vmocanu").
	Username string
	// IsAdmin reports whether the bot user is an instance administrator. GitLab
	// includes is_admin on GET /user only when the caller is an admin, so an
	// absent field decodes to false — exactly the non-admin (compliant) case. An
	// instance-admin PAT is effectively god-mode, so the privilege checker treats
	// true as a violation.
	IsAdmin bool
}

// Project is a repo the bot has membership on.
type Project struct {
	// ForgeProjectID is the stable numeric project id (not the path, which can
	// be renamed).
	ForgeProjectID    int64
	PathWithNamespace string
	WebURL            string
	DefaultBranch     string
}

// Forge is the driver contract. Every method takes a context and surfaces
// redacted errors (the PAT never appears in an error string). Implementations
// paginate internally and return complete result sets.
//
// Forge is the union of IssueTracker (issuetracker.go) and CodeHost (codehost.go).
// Each driver (github.go, gitlab.go, forgejo.go) and forgetest.BaseFake asserts
// at compile time that it implements both halves.
// From PRD #4173 M2 on, callers take the narrower half they need instead of the
// whole Forge.
type Forge interface {
	IssueTracker
	CodeHost
}

// New constructs a driver for the given forge type. baseURL must already be
// allowlist-validated by the caller (the SSRF guard lives in config, not here).
// timeout bounds every HTTP call the driver makes.
func New(t Type, baseURL, token string, timeout time.Duration) (Forge, error) {
	return newWithGitLabBackoff(t, baseURL, token, timeout, nil)
}

// NewWithGitLabBackoff builds a driver with a per-client GitLab retry wait policy.
// Other forge types keep their normal construction path.
func NewWithGitLabBackoff(t Type, baseURL, token string, timeout time.Duration, backoff retryablehttp.Backoff) (Forge, error) {
	return newWithGitLabBackoff(t, baseURL, token, timeout, backoff)
}

func newWithGitLabBackoff(t Type, baseURL, token string, timeout time.Duration, backoff retryablehttp.Backoff) (Forge, error) {
	switch t {
	case TypeGitLab:
		if backoff != nil {
			return newGitLab(baseURL, token, timeout, backoff)
		}
		return newGitLab(baseURL, token, timeout)
	case TypeForgejo:
		return newForgejo(baseURL, token, timeout), nil
	case TypeGitHub:
		return newGitHub(baseURL, token, timeout)
	default:
		return nil, fmt.Errorf("forge: unsupported forge type %q", t)
	}
}

// createIssueContext marks the one request whose POST must never be replayed
// by net/http's redirect handling. The marker follows SDK-created requests.
type createIssueContextKey struct{}

func createIssueContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, createIssueContextKey{}, true)
}

func createIssueRedirectGuard(fallback func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && via[0].Context().Value(createIssueContextKey{}) == true {
			return http.ErrUseLastResponse
		}
		return fallback(req, via)
	}
}

// timeoutClient builds an *http.Client with a hard per-call timeout. Every
// driver routes its transport through one of these, so no call can hang on an
// untimeouted DefaultClient. Ordinary redirects are limited to the original
// request's origin; CreateIssue stops at every redirect to avoid replaying a POST.
func timeoutClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &http.Client{Transport: forgeBudgetTransport(), Timeout: timeout, CheckRedirect: createIssueRedirectGuard(redirectguard.SameOrigin)}
}
