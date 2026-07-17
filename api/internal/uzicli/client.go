package uzicli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
)

// Client is the read surface the CLI's command stubs depend on. Commands take a
// Client, never a concrete HTTP client, so tests inject FakeClient and the live
// HTTPClient below serves the real requests. Every method returns the stdlib-only
// apitypes DTOs directly (PRD #64 M1/M7): the CLI decodes the exact shapes the
// handlers serialize, without importing internal/handler (which would drag pgx +
// chi into the binary — the go list -deps layering assertion).
type Client interface {
	Whoami(ctx context.Context) (apitypes.UserDTO, error)
	ListRuns(ctx context.Context) ([]apitypes.RunListItemDTO, error)
	GetRun(ctx context.Context, id string) (apitypes.RunDTO, error)
	// RunLogs returns a run's persisted messages after seq (0 = from the start);
	// `uzi run logs` polls it with the highest seq seen (Decision: REST polling, not
	// a WebSocket — PRD #64 Out of scope).
	RunLogs(ctx context.Context, id string, after int32) ([]apitypes.MessageDTO, error)
	// RunReview returns the judge's review for a run, or nil when the run is visible
	// but unjudged (the endpoint answers 200 {"review":null}). A run that is absent
	// or not visible is a real 404 → *ExitError{ExitNotFound}. The two cases are
	// deliberately distinct: nil is exit 0 "not judged", 404 is exit 4.
	RunReview(ctx context.Context, id string) (*apitypes.ReviewDTO, error)
	ListWorkers(ctx context.Context) ([]apitypes.WorkerDTO, error)
	ListRepos(ctx context.Context) ([]apitypes.RepoDTO, error)
	AdminListUsers(ctx context.Context) ([]apitypes.UserDTO, error)
	AdminListRuns(ctx context.Context) ([]apitypes.RunListItemDTO, error)
	AdminListWorkers(ctx context.Context) ([]apitypes.AdminWorkerDTO, error)
	AdminUsage(ctx context.Context) (apitypes.AdminUsageDTO, error)
	AdminRateLimits(ctx context.Context) ([]apitypes.AdminRateLimitRowDTO, error)
}

// maxRespBytes caps how much of a response body the client reads, so a broken or
// hostile endpoint cannot make the CLI allocate without bound. 32 MiB is far above
// any real run-detail or message-history payload.
const maxRespBytes = 32 << 20

// HTTPClient is the live API client. It talks only to leaf packages (apitypes +
// net/http + stdlib), keeping cmd/uzi off the server stack.
type HTTPClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewHTTPClient builds the live client from resolved settings.
func NewHTTPClient(s Settings) *HTTPClient {
	return &HTTPClient{
		BaseURL: s.URL,
		Token:   s.Token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

var _ Client = (*HTTPClient)(nil)

// newRequest builds a GET request to base+path, rejecting a non-https base URL
// before any credential is attached. This is a credential-leak guard, not a
// nicety: --url / $UZI_URL / config flow verbatim into BaseURL, and every method
// attaches `Authorization: Bearer <uzc_/uza_>` — so a plaintext or
// attacker-controlled URL would leak the token in the clear. https everywhere,
// with an http exception ONLY for the loopback compose stack
// (127.0.0.1/localhost). Mirrors the server's https-only FORGE_ALLOWED_BASE_URLS.
func (c *HTTPClient) newRequest(ctx context.Context, method, path string) (*http.Request, error) {
	base := strings.TrimSpace(c.BaseURL)
	if base == "" {
		return nil, Exitf(ExitUsage, "no uzi API URL configured: pass --url or set $UZI_URL")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.Scheme == "" {
		return nil, Exitf(ExitUsage, "invalid uzi API URL %q", base)
	}
	if u.Scheme != "https" && !isLoopbackURL(u) {
		return nil, Exitf(ExitUsage,
			"refusing to send credentials to %q: only https (or http on 127.0.0.1/localhost) is allowed — a plaintext URL leaks your token",
			base)
	}
	full := strings.TrimRight(u.String(), "/") + path
	req, err := http.NewRequestWithContext(ctx, method, full, nil)
	if err != nil {
		return nil, Exitf(ExitGeneric, "build request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

// isLoopbackURL reports whether the URL's host is loopback (127.0.0.0/8, ::1) or
// the literal "localhost" — the only hosts the http scheme is permitted for.
func isLoopbackURL(u *url.URL) bool {
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// get executes a GET and, on a 2xx, decodes the body into out (out may be nil to
// discard). Every failure — transport, HTTP status, or decode — is returned as an
// *ExitError carrying the right process exit code (never a bare error): a raw
// error leaking to main would be misclassified as a usage error (see ExitCodeFor).
func (c *HTTPClient) get(ctx context.Context, path string, out any) error {
	req, err := c.newRequest(ctx, http.MethodGet, path)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Any error from Do is a transport failure (dial refused, DNS, TLS,
		// timeout, context deadline): the server is effectively unreachable.
		return Exitf(ExitUnreachable, "cannot reach uzi at %s: %v", c.BaseURL, transportMsg(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return Exitf(ExitUnreachable, "reading response from uzi: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		return statusError(resp.StatusCode, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return Exitf(ExitGeneric, "malformed response from uzi (%s): %v", path, err)
		}
	}
	return nil
}

// transportMsg unwraps a *url.Error so the message reads as the underlying cause
// (dial/DNS/TLS/timeout) rather than repeating the request URL.
func transportMsg(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// statusError maps a non-2xx status to an *ExitError with the documented exit
// code, folding in the server's {"error": "..."} message when present.
func statusError(status int, body []byte) *ExitError {
	msg := serverErrMsg(body)
	switch {
	case status == http.StatusBadRequest:
		if msg == "" {
			msg = "bad request"
		}
		return Exitf(ExitUsage, "%s", msg)
	case status == http.StatusUnauthorized:
		if msg == "" {
			msg = "authentication required or token invalid — run `uzi auth token` or set $UZI_TOKEN"
		}
		return Exitf(ExitAuth, "%s", msg)
	case status == http.StatusForbidden:
		// The only 403 a CLI read verb hits is the admin_ro scope gate
		// (RequireAdminRO); owner-scoped reads 404 a foreign resource, never 403.
		if msg == "" {
			msg = "forbidden"
		}
		return Exitf(ExitAuth, "%s: your token lacks the required scope (admin views need an admin-scoped token)", msg)
	case status == http.StatusNotFound:
		if msg == "" {
			msg = "not found"
		}
		return Exitf(ExitNotFound, "%s", msg)
	case status == http.StatusConflict:
		if msg == "" {
			msg = "conflict"
		}
		return Exitf(ExitConflict, "%s", msg)
	case status >= 500:
		if msg == "" {
			msg = fmt.Sprintf("server error (%d)", status)
		}
		return Exitf(ExitUnreachable, "%s", msg)
	default:
		if msg == "" {
			msg = fmt.Sprintf("request failed (%d)", status)
		}
		return Exitf(ExitGeneric, "%s", msg)
	}
}

// serverErrMsg extracts the message from the API's {"error": "..."} body, or ""
// when the body is not that shape.
func serverErrMsg(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil {
		return strings.TrimSpace(e.Error)
	}
	return ""
}

func (c *HTTPClient) Whoami(ctx context.Context) (apitypes.UserDTO, error) {
	var env struct {
		User apitypes.UserDTO `json:"user"`
	}
	if err := c.get(ctx, "/api/auth/me", &env); err != nil {
		return apitypes.UserDTO{}, err
	}
	return env.User, nil
}

func (c *HTTPClient) ListRuns(ctx context.Context) ([]apitypes.RunListItemDTO, error) {
	var env struct {
		Runs []apitypes.RunListItemDTO `json:"runs"`
	}
	if err := c.get(ctx, "/api/runs", &env); err != nil {
		return nil, err
	}
	return env.Runs, nil
}

func (c *HTTPClient) GetRun(ctx context.Context, id string) (apitypes.RunDTO, error) {
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(id), &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

func (c *HTTPClient) RunLogs(ctx context.Context, id string, after int32) ([]apitypes.MessageDTO, error) {
	var env struct {
		Messages []apitypes.MessageDTO `json:"messages"`
	}
	path := fmt.Sprintf("/api/runs/%s/messages?after=%d", url.PathEscape(id), after)
	if err := c.get(ctx, path, &env); err != nil {
		return nil, err
	}
	return env.Messages, nil
}

func (c *HTTPClient) RunReview(ctx context.Context, id string) (*apitypes.ReviewDTO, error) {
	// The envelope is {"review": <dto>|null}. A 200 with review:null is a
	// visible-but-unjudged run — return (nil, nil) so the command exits 0 "not
	// judged". A real 404 arrives here as *ExitError{ExitNotFound} from get.
	var env struct {
		Review *apitypes.ReviewDTO `json:"review"`
	}
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(id)+"/review", &env); err != nil {
		return nil, err
	}
	return env.Review, nil
}

func (c *HTTPClient) ListWorkers(ctx context.Context) ([]apitypes.WorkerDTO, error) {
	var env struct {
		Workers []apitypes.WorkerDTO `json:"workers"`
	}
	if err := c.get(ctx, "/api/workers", &env); err != nil {
		return nil, err
	}
	return env.Workers, nil
}

func (c *HTTPClient) ListRepos(ctx context.Context) ([]apitypes.RepoDTO, error) {
	var env struct {
		Repos []apitypes.RepoDTO `json:"repos"`
	}
	if err := c.get(ctx, "/api/repos", &env); err != nil {
		return nil, err
	}
	return env.Repos, nil
}

func (c *HTTPClient) AdminListUsers(ctx context.Context) ([]apitypes.UserDTO, error) {
	var env struct {
		Users []apitypes.UserDTO `json:"users"`
	}
	if err := c.get(ctx, "/api/admin/users", &env); err != nil {
		return nil, err
	}
	return env.Users, nil
}

func (c *HTTPClient) AdminListRuns(ctx context.Context) ([]apitypes.RunListItemDTO, error) {
	var env struct {
		Runs []apitypes.RunListItemDTO `json:"runs"`
	}
	if err := c.get(ctx, "/api/admin/runs", &env); err != nil {
		return nil, err
	}
	return env.Runs, nil
}

func (c *HTTPClient) AdminListWorkers(ctx context.Context) ([]apitypes.AdminWorkerDTO, error) {
	var env struct {
		Workers []apitypes.AdminWorkerDTO `json:"workers"`
	}
	if err := c.get(ctx, "/api/admin/workers", &env); err != nil {
		return nil, err
	}
	return env.Workers, nil
}

func (c *HTTPClient) AdminUsage(ctx context.Context) (apitypes.AdminUsageDTO, error) {
	var out apitypes.AdminUsageDTO
	if err := c.get(ctx, "/api/admin/usage", &out); err != nil {
		return apitypes.AdminUsageDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) AdminRateLimits(ctx context.Context) ([]apitypes.AdminRateLimitRowDTO, error) {
	var env struct {
		Users []apitypes.AdminRateLimitRowDTO `json:"users"`
	}
	if err := c.get(ctx, "/api/admin/rate-limits", &env); err != nil {
		return nil, err
	}
	return env.Users, nil
}
