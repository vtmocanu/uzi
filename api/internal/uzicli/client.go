package uzicli

import (
	"bytes"
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
	// RunInputs returns a run's follow_up steer queue (newest-first) with delivery
	// status (PRD #95). Owner-only server-side (GetRunByIDForUser): a non-owner —
	// including an admin_ro token on another user's run — gets a 404 →
	// *ExitError{ExitNotFound}, never another user's steer text.
	RunInputs(ctx context.Context, id string) ([]apitypes.SteerInputDTO, error)
	// StreamRun subscribes to one run's live events over /api/ws (PRD #112 M2),
	// authenticating with the same Bearer CLI token the REST reads use (M1 opened
	// that route to it). The returned stream reconnects, replays what a drop
	// swallowed, and periodically re-reads authoritative run state — see RunStream
	// for why the last of those is required rather than defensive.
	//
	// It is on this INTERFACE, not only on *HTTPClient, so a consumer can take a
	// Client and still be testable: FakeClient implements it too.
	StreamRun(ctx context.Context, runID string) (*RunStream, error)
	ListWorkers(ctx context.Context) ([]apitypes.WorkerDTO, error)
	// ListSecrets returns the caller's Anthropic tokens as metadata (labels, ids,
	// default flags — never values): GET /api/me/secrets, RequireUser (PRD #104 D8,
	// a list is safe from a CLI token; creating/rotating/deleting is web-only). Each
	// entry's value appears nowhere — there is no reveal endpoint.
	ListSecrets(ctx context.Context) ([]apitypes.SecretDTO, error)
	// SetTokenAutoEligible opts one of the caller's Anthropic tokens into or out of
	// the auto-selection pool (PRD #111 M2, D2): PATCH
	// /api/me/secrets/anthropic_token/{id}/auto-eligible {auto_eligible}.
	//
	// It takes an ID. The label→id resolution happens in the COMMAND, client-side —
	// deliberately NOT the shape SetWorkerBindMode uses, which sends the label for the
	// server to resolve. An earlier version of this comment claimed they were the
	// same; see cmd/uzi/token.go for the two ways they differ (Go vs Postgres case
	// folding, and a missing kind filter) and why this side is accepted for now.
	//
	// This is the ONE secrets write a CLI token can reach, and its own narrow route
	// is why (D13). Every other secrets write is cookie-only, because minting or
	// replacing a credential from a stolen uzc_ is the exposure PRD #104 D8 closes;
	// this one mints nothing and reveals nothing, it only re-points which of the
	// caller's OWN tokens the pool may spend. An unknown label is a usage error
	// resolved client-side (exit 3); an unknown id is a 404 (exit 4).
	SetTokenAutoEligible(ctx context.Context, id string, eligible bool) (apitypes.SecretDTO, error)
	// SelfRateLimits returns the caller's OWN per-token rate-limit meters, each
	// carrying the server-computed auto-selection status: GET /api/me/rate-limits.
	//
	// RequireUser since PRD #111 D23. Before that this was cookie-only, which left
	// `uzi token pool <label> --on` a silent no-op from a script's point of view: it
	// could opt a token in and had no way to learn the token could never be picked
	// (no gauge reading, or one that aged out) — R7's hazard, reintroduced on the CLI
	// half. The status string is computed server-side by autoselect.Classify and is
	// RENDERED here, never re-derived (D21).
	SelfRateLimits(ctx context.Context) ([]apitypes.TokenRateLimitDTO, error)
	ListRepos(ctx context.Context) ([]apitypes.RepoDTO, error)
	AdminListUsers(ctx context.Context) ([]apitypes.UserDTO, error)
	AdminListRuns(ctx context.Context) ([]apitypes.RunListItemDTO, error)
	AdminListWorkers(ctx context.Context) ([]apitypes.AdminWorkerDTO, error)
	AdminListCLITokens(ctx context.Context) ([]apitypes.AdminCLITokenDTO, error)
	AdminUsage(ctx context.Context) (apitypes.AdminUsageDTO, error)
	AdminRateLimits(ctx context.Context) ([]apitypes.AdminRateLimitRowDTO, error)

	// StartCLIAuth begins a browser-brokered login: POST /api/auth/cli/start with the
	// PKCE S256 challenge and a client description. UNAUTH by design (the CLI has no
	// credential yet). Returns the request_id, the display user_code, the request
	// lifetime, and the poll cadence to honour.
	StartCLIAuth(ctx context.Context, challenge, clientDesc string) (CLIAuthStartResult, error)
	// PollCLIAuth polls POST /api/auth/cli/poll {request_id, verifier} once and
	// classifies the reply: 202 pending, 200 authorized (Token/User set, once), or 410
	// terminal (Reason set). POST not GET so the verifier never lands in an access log.
	PollCLIAuth(ctx context.Context, requestID, verifier string) (CLIAuthPollResult, error)
	// CreateRun queues an agent run on a repo's PRD issue: POST /api/repos/{id}/runs
	// {issue_iid, wait_on_limit?}. Returns the created run.
	//
	// waitOnLimit is the PRD #35 usage-limit opt-in and is TRI-STATE, which is why it
	// is a pointer rather than a bool: nil OMITS the key, and the server then stamps
	// the run from the owner's own Settings default. A non-nil false is a different
	// statement — "this run, explicitly, must not park" — and overrides that default.
	// Collapsing the two would make every CLI-created run silently opt OUT the moment
	// this parameter shipped, which is the exact defect the server's own field comment
	// cites for taking a *bool.
	CreateRun(ctx context.Context, repoID string, issueIID int64, waitOnLimit *bool) (apitypes.RunDTO, error)
	// SubmitRunInput submits a steering input: POST /api/runs/{id}/inputs
	// {kind, body, selection}. kind ∈ {approve_plan, reject_plan, cancel, follow_up}.
	// sel is legal only with approve_plan; the server validates it against the run's
	// real roster (the client never composes the worker-bound body itself).
	SubmitRunInput(ctx context.Context, runID, kind, body string, sel *apitypes.AgentSelection) (apitypes.RunInputResponse, error)
	// DeleteWorker removes one of the caller's workers: DELETE /api/workers/{id}
	// (204 No Content on success). A worker with active runs is a 409 (exit 5); an
	// unknown/foreign id is a 404 (exit 4). Minting a worker stays a webui action —
	// there is no create counterpart here.
	DeleteWorker(ctx context.Context, id string) error
	// SetWorkerBindMode sets HOW a worker chooses its Anthropic credential:
	// PATCH /api/workers/{id} {anthropic_bind_mode, anthropic_token}.
	//
	//   - "pinned" + a LABEL (the name a human knows, not a secret id) → that token;
	//   - "default" → the caller's default token;
	//   - "auto"    → the selector picks from the caller's opted-in pool per claim.
	//
	// Unlike minting a worker this yields no credential the caller lacks, so it is
	// RequireUser and reachable from a CLI token (PRD #104 D8). An unknown label or
	// an illegal mode is a 400 (exit 3); an unknown/foreign worker is a 404 (exit 4).
	// The change lands on the worker's next claim — no restart, no re-minted join
	// token.
	SetWorkerBindMode(ctx context.Context, id, mode, label string) (apitypes.WorkerDTO, error)
	// ListMemory returns the caller's agent memory across all repos (PRD #90):
	// GET /api/me/memory. Each entry carries its repo + provenance so the owner can
	// see what a future run would read back.
	ListMemory(ctx context.Context) ([]apitypes.AgentMemoryDTO, error)
	// DeleteMemory purges one of the caller's memory entries: DELETE
	// /api/me/memory/{id} (204 on success). An unknown/foreign id is a 404 (exit 4).
	DeleteMemory(ctx context.Context, id string) error
	// SetDisposition records a triage verdict on a judge recommendation (PRD #94
	// Decision 10): PUT /api/runs/{id}/review/recommendations/{recID}/disposition
	// {status, reason?}. status ∈ {done, dismissed}; reason (∈ {wont_do,
	// not_an_issue}) is sent only for a dismissal and omitted otherwise. Owner-only
	// on RequireUser (Decision 5): a uza_ read-only token mutating another user's
	// review gets a 404 (exit 4). 204 → nil.
	SetDisposition(ctx context.Context, runID, recID, status, reason string) error
	// DeleteDisposition undoes a recommendation's disposition (PRD #94 Decision 6):
	// DELETE the same path. A 404 means no disposition existed — returned as the
	// sentinel ErrNoDisposition (a plain error, NOT an *ExitError) so the command
	// can soften it to "already undone" and exit 0. Every other failure propagates
	// as an *ExitError with the documented exit code.
	DeleteDisposition(ctx context.Context, runID, recID string) error
	// JudgeStats returns the caller's all-time triage totals across every run (PRD
	// #94 Decision 8): GET /api/me/judge/stats. Owner-scoped; the reply is an
	// unenveloped TriageDTO.
	JudgeStats(ctx context.Context) (apitypes.TriageDTO, error)
	// JudgeBacklog returns the caller's all-time recommendation backlog deduped by
	// (category, target) (PRD #98 M1): GET /api/me/judge/recommendations. Owner-scoped
	// on RequireUser, read-only, no token spend; the reply is an unenveloped
	// JudgeBacklogDTO carrying the groups, the canonical triage tally and `truncated`.
	//
	// bucket is passed through VERBATIM and an empty bucket OMITS the parameter, so the
	// SERVER's default (todo) applies. The valid set is deliberately not restated on this
	// side: the CLI never compares against a bucket value, it only forwards one, so there
	// is no predicate to fail silently — an unknown bucket comes back as the server's own
	// 400 ("invalid bucket") and maps to the usage exit code. Restating the enum here
	// would add a second definition that could drift from the enforcing one; forwarding
	// keeps the validator the only place the set is written down.
	// runAnchor is the ?run= coordinate semi-join (PRD #98 Decision 1). It is the ONLY
	// predicate pushed into SQL before the row cap — bucket filtering happens in Go on the
	// already-cut rows — so it is the only parameter that can narrow a pull below
	// JudgeBacklogMaxRows and thereby answer a `truncated` warning. Empty omits it.
	JudgeBacklog(ctx context.Context, bucket, runAnchor string) (apitypes.JudgeBacklogDTO, error)
	// BulkSetDispositions applies one triage verdict to every member coordinate of the
	// given groups (PRD #98 M2, Decision 3): PUT
	// /api/me/judge/recommendations/disposition. status ∈ {done, dismissed}; reason
	// (∈ {wont_do, not_an_issue}) rides only a dismissal.
	//
	// The request's `scope` is left at its zero value, which the server reads as the
	// default open scope — settle what is open, never re-assert a settled member. The CLI
	// exposes no scope choice, so it names no scope: sending "" is what makes the server's
	// default the single definition of that behaviour.
	//
	// There is NO 404 on this route and none should be expected. It is owner-only by
	// construction (the service resolves members under `user_id = caller`), and
	// coordinates are not ids, so a coordinate that does not exist and one belonging to
	// another user are BOTH a 200 with Updated == 0 — #94 Decision 5's no-existence-oracle
	// rule. A caller learning "0 written" learns only that none of THEIR rows matched, and
	// must render that as "nothing was written" rather than as success.
	BulkSetDispositions(ctx context.Context, items []apitypes.JudgeDispositionCoordDTO, status, reason string) (apitypes.JudgeDispositionResultDTO, error)
}

// ErrNoDisposition is returned by DeleteDisposition when the recommendation had
// no disposition to undo (the endpoint answers 404). It is a plain error, NOT an
// *ExitError, so `uzi review undo` can treat it as "already undone" (a friendly
// message, exit 0) instead of a hard not-found failure.
var ErrNoDisposition = errors.New("no disposition to undo")

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

	// StreamRun tuning, zero = the package defaults. Unexported so they are not part
	// of the CLI's configuration surface: they exist so the stream tests can drive
	// the reconnect/reconcile timing deterministically instead of sleeping for real
	// seconds, which is the difference between pinning the recovery contract and
	// hoping for it.
	streamReconcile   time.Duration
	streamBackoffBase time.Duration
	streamBackoffMax  time.Duration
}

// NewHTTPClient builds the live client from resolved settings.
func NewHTTPClient(s Settings) *HTTPClient {
	return &HTTPClient{
		BaseURL: s.URL,
		Token:   s.Token,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			// Refuse every redirect. The https guard in newRequest only vets the
			// INITIAL URL; without this, Go's default policy would follow a 3xx
			// and replay `Authorization: Bearer <uzc_/uza_>` across a same-host
			// scheme downgrade (https→http), handing the token to a cleartext
			// endpoint. A legitimate uzi /api/* endpoint never redirects, so a
			// redirect is refused (surfaces as a clean transport failure, exit 6),
			// never followed — mirroring the worker's redirect:"error" posture.
			CheckRedirect: refuseRedirect,
		},
	}
}

// refuseRedirect is the CheckRedirect policy for the live client: it rejects any
// redirect hop rather than let net/http forward the bearer token to the redirect
// target. Returning a non-nil error makes Client.Do fail (and, per net/http, the
// intermediate response body is already closed) instead of following the hop.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("refusing to follow redirect to %s: uzi API endpoints never redirect", req.URL.Redacted())
}

var _ Client = (*HTTPClient)(nil)

// newRequest builds a request to base+path with an optional JSON body, rejecting a
// non-https base URL before any credential is attached. This is a credential-leak
// guard, not a nicety: --url / $UZI_URL / config flow verbatim into BaseURL, and
// every authenticated method attaches `Authorization: Bearer <uzc_/uza_>` — so a
// plaintext or attacker-controlled URL would leak the token in the clear (and the
// login flow's PKCE verifier rides these requests too). https everywhere, with an
// http exception ONLY for the loopback compose stack (127.0.0.1/localhost). Mirrors
// the server's https-only FORGE_ALLOWED_BASE_URLS.
func (c *HTTPClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	u, err := c.credentialSafeBase()
	if err != nil {
		return nil, err
	}
	full := strings.TrimRight(u.String(), "/") + path
	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return nil, Exitf(ExitGeneric, "build request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

// credentialSafeBase parses and vets BaseURL, returning it only when it is safe to
// attach a credential to. It is the SINGLE gate for that decision: newRequest uses
// it for every REST call and StreamRun uses it for the WebSocket dial (PRD #112 M2),
// because the socket carries the same `Authorization: Bearer <uzc_/uza_>` header and
// a second copy of this check is a second thing that can drift. A transport added
// later must call this before it sends the token, not re-derive the rule.
func (c *HTTPClient) credentialSafeBase() (*url.URL, error) {
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
	return u, nil
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

// doJSONRead performs one request with an optional JSON body and returns the
// response plus its (capped) body, already drained and closed. Every transport
// failure is returned as an *ExitError (ExitUnreachable): a raw error leaking to
// main would be misclassified as a usage error (see ExitCodeFor). The caller owns
// HTTP-status classification, since some callers (the poll loop) branch on
// non-error statuses like 202/410 that others treat as failures.
func (c *HTTPClient) doJSONRead(ctx context.Context, method, path string, reqBody any) (*http.Response, []byte, error) {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, nil, Exitf(ExitGeneric, "encode request: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Any error from Do is a transport failure (dial refused, DNS, TLS,
		// timeout, context deadline): the server is effectively unreachable.
		return nil, nil, Exitf(ExitUnreachable, "cannot reach uzi at %s: %v", c.BaseURL, transportMsg(err))
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, nil, Exitf(ExitUnreachable, "reading response from uzi: %v", err)
	}
	return resp, respBody, nil
}

// get executes a GET and, on a 2xx, decodes the body into out (out may be nil to
// discard). Every failure — transport, HTTP status, or decode — is returned as an
// *ExitError carrying the right process exit code.
func (c *HTTPClient) get(ctx context.Context, path string, out any) error {
	resp, body, err := c.doJSONRead(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return decode2xx(resp, body, path, out)
}

// postJSON executes a POST with a JSON body and, on a 2xx, decodes the reply into
// out (out may be nil). Non-2xx maps to the documented exit code via statusError.
func (c *HTTPClient) postJSON(ctx context.Context, path string, reqBody, out any) error {
	resp, body, err := c.doJSONRead(ctx, http.MethodPost, path, reqBody)
	if err != nil {
		return err
	}
	return decode2xx(resp, body, path, out)
}

// put executes a PUT with a JSON body and, on a 2xx, decodes the reply into out
// (out may be nil — the disposition endpoint answers 204). Non-2xx maps to the
// documented exit code via statusError. Mirrors postJSON with http.MethodPut.
func (c *HTTPClient) put(ctx context.Context, path string, reqBody, out any) error {
	resp, body, err := c.doJSONRead(ctx, http.MethodPut, path, reqBody)
	if err != nil {
		return err
	}
	return decode2xx(resp, body, path, out)
}

// patch executes a PATCH with a JSON body and, on a 2xx, decodes the reply into
// out. Mirrors put with http.MethodPatch — PATCH is the verb for a partial update
// whose absent fields mean "leave alone" (PRD #104 M3's worker rebind).
func (c *HTTPClient) patch(ctx context.Context, path string, reqBody, out any) error {
	resp, body, err := c.doJSONRead(ctx, http.MethodPatch, path, reqBody)
	if err != nil {
		return err
	}
	return decode2xx(resp, body, path, out)
}

// del executes a DELETE and treats any 2xx (the endpoint answers 204) as success,
// discarding the (empty) body. Non-2xx maps to the documented exit code.
func (c *HTTPClient) del(ctx context.Context, path string) error {
	resp, body, err := c.doJSONRead(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	return decode2xx(resp, body, path, nil)
}

// decode2xx maps a non-2xx status to an *ExitError and otherwise decodes the body
// into out (nil to discard). Shared by get and postJSON so success/error handling
// is identical across verbs.
func decode2xx(resp *http.Response, body []byte, path string, out any) error {
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

func (c *HTTPClient) RunInputs(ctx context.Context, id string) ([]apitypes.SteerInputDTO, error) {
	var env struct {
		Inputs []apitypes.SteerInputDTO `json:"inputs"`
	}
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(id)+"/inputs", &env); err != nil {
		return nil, err
	}
	return env.Inputs, nil
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

func (c *HTTPClient) ListSecrets(ctx context.Context) ([]apitypes.SecretDTO, error) {
	var env struct {
		Secrets []apitypes.SecretDTO `json:"secrets"`
	}
	if err := c.get(ctx, "/api/me/secrets", &env); err != nil {
		return nil, err
	}
	return env.Secrets, nil
}

func (c *HTTPClient) SetTokenAutoEligible(ctx context.Context, id string, eligible bool) (apitypes.SecretDTO, error) {
	body := struct {
		AutoEligible bool `json:"auto_eligible"`
	}{AutoEligible: eligible}
	var env struct {
		Secret apitypes.SecretDTO `json:"secret"`
	}
	if err := c.patch(ctx, "/api/me/secrets/anthropic_token/"+url.PathEscape(id)+"/auto-eligible", body, &env); err != nil {
		return apitypes.SecretDTO{}, err
	}
	return env.Secret, nil
}

func (c *HTTPClient) SelfRateLimits(ctx context.Context) ([]apitypes.TokenRateLimitDTO, error) {
	var env struct {
		Tokens []apitypes.TokenRateLimitDTO `json:"tokens"`
	}
	if err := c.get(ctx, "/api/me/rate-limits", &env); err != nil {
		return nil, err
	}
	return env.Tokens, nil
}

func (c *HTTPClient) DeleteWorker(ctx context.Context, id string) error {
	return c.del(ctx, "/api/workers/"+url.PathEscape(id))
}

func (c *HTTPClient) SetWorkerBindMode(ctx context.Context, id, mode, label string) (apitypes.WorkerDTO, error) {
	// An empty label sends JSON null, which is what a non-pinned mode requires —
	// distinct from omitting the field, which would mean "leave it alone". *string is
	// what makes the two expressible on the wire, and the server REFUSES a label
	// alongside default/auto rather than quietly dropping one of them.
	body := struct {
		AnthropicBindMode string  `json:"anthropic_bind_mode"`
		AnthropicToken    *string `json:"anthropic_token"`
	}{AnthropicBindMode: mode}
	if label != "" {
		body.AnthropicToken = &label
	}
	var env struct {
		Worker apitypes.WorkerDTO `json:"worker"`
	}
	if err := c.patch(ctx, "/api/workers/"+url.PathEscape(id), body, &env); err != nil {
		return apitypes.WorkerDTO{}, err
	}
	return env.Worker, nil
}

func (c *HTTPClient) ListMemory(ctx context.Context) ([]apitypes.AgentMemoryDTO, error) {
	var env struct {
		Memories []apitypes.AgentMemoryDTO `json:"memories"`
	}
	if err := c.get(ctx, "/api/me/memory", &env); err != nil {
		return nil, err
	}
	return env.Memories, nil
}

func (c *HTTPClient) DeleteMemory(ctx context.Context, id string) error {
	return c.del(ctx, "/api/me/memory/"+url.PathEscape(id))
}

// dispositionPath builds the disposition endpoint path for a (run, rec) pair,
// escaping both id segments.
func dispositionPath(runID, recID string) string {
	return "/api/runs/" + url.PathEscape(runID) + "/review/recommendations/" + url.PathEscape(recID) + "/disposition"
}

func (c *HTTPClient) SetDisposition(ctx context.Context, runID, recID, status, reason string) error {
	reqBody := map[string]string{"status": status}
	// reason is required iff dismissed; omit it otherwise so a "done" PUT never
	// carries a stray (and server-rejected) reason field.
	if reason != "" {
		reqBody["reason"] = reason
	}
	return c.put(ctx, dispositionPath(runID, recID), reqBody, nil)
}

func (c *HTTPClient) DeleteDisposition(ctx context.Context, runID, recID string) error {
	// The command resolves recID against the current review before calling, so the
	// run and recommendation exist; a 404 here therefore means "no disposition on
	// this coordinate" — softened to ErrNoDisposition (a plain error) so undo can
	// report "already undone" and exit 0, per Decision 6. Any other non-2xx keeps
	// its real exit code.
	resp, body, err := c.doJSONRead(ctx, http.MethodDelete, dispositionPath(runID, recID), nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoDisposition
	}
	return decode2xx(resp, body, dispositionPath(runID, recID), nil)
}

func (c *HTTPClient) JudgeStats(ctx context.Context) (apitypes.TriageDTO, error) {
	var out apitypes.TriageDTO
	if err := c.get(ctx, "/api/me/judge/stats", &out); err != nil {
		return apitypes.TriageDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) JudgeBacklog(ctx context.Context, bucket, runAnchor string) (apitypes.JudgeBacklogDTO, error) {
	path := "/api/me/judge/recommendations"
	// Both omitted rather than sent empty when unset: the handler's `== ""` branches are what
	// apply its defaults, and an explicit empty value would take the same branch only by
	// coincidence. Escaped because both are user input off a flag.
	q := url.Values{}
	if bucket != "" {
		q.Set("bucket", bucket)
	}
	if runAnchor != "" {
		q.Set("run", runAnchor)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out apitypes.JudgeBacklogDTO
	if err := c.get(ctx, path, &out); err != nil {
		return apitypes.JudgeBacklogDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) BulkSetDispositions(ctx context.Context, items []apitypes.JudgeDispositionCoordDTO, status, reason string) (apitypes.JudgeDispositionResultDTO, error) {
	// Scope is left zero on purpose — see the interface doc. Reason is likewise sent as
	// "" for a `done`, which is what the shared validator requires (done carries no
	// reason); this is a struct, not the omit-when-empty map SetDisposition builds, and
	// the two agree because "" IS the legal value there.
	reqBody := apitypes.JudgeBulkDispositionRequest{Items: items, Status: status, Reason: reason}
	var out apitypes.JudgeDispositionResultDTO
	if err := c.put(ctx, "/api/me/judge/recommendations/disposition", reqBody, &out); err != nil {
		return apitypes.JudgeDispositionResultDTO{}, err
	}
	return out, nil
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

// AdminListCLITokens reads the factory-wide standing-credential inventory. The
// response carries no token value and no hash — see apitypes.AdminCLITokenDTO.
func (c *HTTPClient) AdminListCLITokens(ctx context.Context) ([]apitypes.AdminCLITokenDTO, error) {
	var env struct {
		Tokens []apitypes.AdminCLITokenDTO `json:"tokens"`
	}
	if err := c.get(ctx, "/api/admin/cli-tokens", &env); err != nil {
		return nil, err
	}
	return env.Tokens, nil
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

func (c *HTTPClient) StartCLIAuth(ctx context.Context, challenge, clientDesc string) (CLIAuthStartResult, error) {
	reqBody := map[string]string{"code_challenge": challenge, "client_desc": clientDesc}
	var out struct {
		RequestID string `json:"request_id"`
		UserCode  string `json:"user_code"`
		ExpiresIn int    `json:"expires_in"`
		Interval  int    `json:"interval"`
	}
	if err := c.postJSON(ctx, "/api/auth/cli/start", reqBody, &out); err != nil {
		return CLIAuthStartResult{}, err
	}
	return CLIAuthStartResult{
		RequestID: out.RequestID,
		UserCode:  out.UserCode,
		ExpiresIn: out.ExpiresIn,
		Interval:  out.Interval,
	}, nil
}

func (c *HTTPClient) PollCLIAuth(ctx context.Context, requestID, verifier string) (CLIAuthPollResult, error) {
	reqBody := map[string]string{"request_id": requestID, "verifier": verifier}
	resp, body, err := c.doJSONRead(ctx, http.MethodPost, "/api/auth/cli/poll", reqBody)
	if err != nil {
		return CLIAuthPollResult{}, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		// 200 {token, user} — approved and minted, returned once. Store, never print.
		var out struct {
			Token string           `json:"token"`
			User  apitypes.UserDTO `json:"user"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return CLIAuthPollResult{}, Exitf(ExitGeneric, "malformed login response from uzi: %v", err)
		}
		return CLIAuthPollResult{Status: CLIAuthAuthorized, Token: out.Token, User: out.User}, nil
	case http.StatusAccepted:
		// 202 {status:"pending"} — keep polling.
		return CLIAuthPollResult{Status: CLIAuthPending}, nil
	case http.StatusGone:
		// 410 {status:"expired"|"denied"|"consumed"} — terminal, stop.
		return CLIAuthPollResult{Status: CLIAuthTerminal, Reason: pollStatusField(body)}, nil
	default:
		// 400/401/429/5xx etc. — map to the documented exit code (auth/usage/...).
		return CLIAuthPollResult{}, statusError(resp.StatusCode, body)
	}
}

// pollStatusField pulls {"status": "..."} from a poll reply, defaulting to
// "expired" when absent — the request_id is not a secret, so a missing/opaque
// terminal body reads as expired rather than leaking existence.
func pollStatusField(body []byte) string {
	var s struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &s) == nil && strings.TrimSpace(s.Status) != "" {
		return s.Status
	}
	return "expired"
}

func (c *HTTPClient) CreateRun(ctx context.Context, repoID string, issueIID int64, waitOnLimit *bool) (apitypes.RunDTO, error) {
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	// A struct rather than the map this used to build, because the map could not
	// express the tri-state: `omitempty` on a POINTER omits only nil, so a non-nil
	// false still marshals as `"wait_on_limit": false`. That is the whole contract —
	// omitted means "inherit my default", present-and-false means "explicitly not this
	// run". A `map[string]any` with an `if != nil` guard would work too and is worse:
	// it puts the rule in a conditional instead of in the type.
	reqBody := struct {
		IssueIID    int64 `json:"issue_iid"`
		WaitOnLimit *bool `json:"wait_on_limit,omitempty"`
	}{IssueIID: issueIID, WaitOnLimit: waitOnLimit}
	if err := c.postJSON(ctx, "/api/repos/"+url.PathEscape(repoID)+"/runs", reqBody, &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

func (c *HTTPClient) SubmitRunInput(ctx context.Context, runID, kind, body string, sel *apitypes.AgentSelection) (apitypes.RunInputResponse, error) {
	reqBody := apitypes.RunInputRequest{Kind: kind, Body: body, Selection: sel}
	var out apitypes.RunInputResponse
	if err := c.postJSON(ctx, "/api/runs/"+url.PathEscape(runID)+"/inputs", reqBody, &out); err != nil {
		return apitypes.RunInputResponse{}, err
	}
	return out, nil
}
