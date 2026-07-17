package uzicli

import (
	"context"
	"net/http"
	"time"
)

// The result types below are placeholders. M1 introduces api/internal/apitypes
// (a stdlib-only leaf holding the real DTOs), and M7 re-points Client to return
// those types directly. Until M1 lands, these thin locals keep uzicli
// compilable as a leaf; the fields are only what the M3 command stubs render.
// Do NOT grow these into the real contract — that is apitypes' job.

// User mirrors the shape of GET /api/auth/me (whoami).
type User struct {
	ID      string `json:"id"`
	Email   string `json:"email"`
	IsAdmin bool   `json:"is_admin"`
}

// Run mirrors a run list/detail row.
type Run struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Title  string `json:"title"`
}

// Review mirrors the judge review envelope for `uzi run review`.
type Review struct {
	RunID   string `json:"run_id"`
	Verdict string `json:"verdict"`
	Status  string `json:"status"`
}

// Worker mirrors a worker list row.
type Worker struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Repo mirrors a repo list row.
type Repo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Usage mirrors the admin usage summary.
type Usage struct {
	Tokens  int64   `json:"tokens"`
	CostUSD float64 `json:"cost_usd"`
}

// RateLimit mirrors an admin rate-limit row.
type RateLimit struct {
	Name  string `json:"name"`
	Limit int    `json:"limit"`
}

// Client is the read surface the CLI's command stubs depend on. Commands take a
// Client, never a concrete HTTP client, so tests inject FakeClient and M7 can
// land the real implementation without touching cmd/uzi.
type Client interface {
	Whoami(ctx context.Context) (User, error)
	ListRuns(ctx context.Context) ([]Run, error)
	GetRun(ctx context.Context, id string) (Run, error)
	RunReview(ctx context.Context, id string) (Review, error)
	ListWorkers(ctx context.Context) ([]Worker, error)
	ListRepos(ctx context.Context) ([]Repo, error)
	AdminListUsers(ctx context.Context) ([]User, error)
	AdminListRuns(ctx context.Context) ([]Run, error)
	AdminListWorkers(ctx context.Context) ([]Worker, error)
	AdminUsage(ctx context.Context) (Usage, error)
	AdminRateLimits(ctx context.Context) ([]RateLimit, error)
}

// HTTPClient is the real-client stub. M3 wires the plumbing (base URL, token,
// timeout) but makes no live calls: every method returns notImplemented. M7
// implements these against apitypes + the M2 endpoints.
type HTTPClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewHTTPClient builds the real-client stub from resolved settings.
func NewHTTPClient(s Settings) *HTTPClient {
	return &HTTPClient{
		BaseURL: s.URL,
		Token:   s.Token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

var _ Client = (*HTTPClient)(nil)

func notImplemented(op string) error {
	return Exitf(ExitGeneric, "%s: not implemented in this build (the real client lands in M7)", op)
}

func (c *HTTPClient) Whoami(context.Context) (User, error) { return User{}, notImplemented("whoami") }
func (c *HTTPClient) ListRuns(context.Context) ([]Run, error) {
	return nil, notImplemented("run list")
}
func (c *HTTPClient) GetRun(context.Context, string) (Run, error) {
	return Run{}, notImplemented("run get")
}
func (c *HTTPClient) RunReview(context.Context, string) (Review, error) {
	return Review{}, notImplemented("run review")
}
func (c *HTTPClient) ListWorkers(context.Context) ([]Worker, error) {
	return nil, notImplemented("worker list")
}
func (c *HTTPClient) ListRepos(context.Context) ([]Repo, error) {
	return nil, notImplemented("repo list")
}
func (c *HTTPClient) AdminListUsers(context.Context) ([]User, error) {
	return nil, notImplemented("admin users")
}
func (c *HTTPClient) AdminListRuns(context.Context) ([]Run, error) {
	return nil, notImplemented("admin runs")
}
func (c *HTTPClient) AdminListWorkers(context.Context) ([]Worker, error) {
	return nil, notImplemented("admin workers")
}
func (c *HTTPClient) AdminUsage(context.Context) (Usage, error) {
	return Usage{}, notImplemented("admin usage")
}
func (c *HTTPClient) AdminRateLimits(context.Context) ([]RateLimit, error) {
	return nil, notImplemented("admin rate-limits")
}
