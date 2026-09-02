package uzicli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// client_account.go holds the account verbs (whoami / login / token / memory /
// version / self settings) of the Client/HTTPClient split out of client.go (PRD #1017).

func (c *HTTPClient) Whoami(ctx context.Context) (apitypes.UserDTO, error) {
	var env struct {
		User apitypes.UserDTO `json:"user"`
	}
	if err := c.get(ctx, "/api/auth/me", &env); err != nil {
		return apitypes.UserDTO{}, err
	}
	return env.User, nil
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

func (c *HTTPClient) GetMySettings(ctx context.Context) (apitypes.UserSettingsDTO, error) {
	var env struct {
		Settings apitypes.UserSettingsDTO `json:"settings"`
	}
	if err := c.get(ctx, "/api/me/settings", &env); err != nil {
		return apitypes.UserSettingsDTO{}, err
	}
	return env.Settings, nil
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

// BuildInfo reads GET /api/version. The response is decoded into the SHARED
// apitypes.BuildInfoDTO rather than a CLI-local struct, which is what keeps
// "unknown" distinguishable from zero for the fields that carry that distinction:
// Commits and UptimeSeconds are pointers, and Commit and BuiltAt are omitempty, so
// an unstamped server's absent values stay absent through `uzi version --json`. A
// local struct with plain int fields would render a dev server as commits 0 and
// uptime 0 — a build claiming to know things it does not, which is precisely what
// the server side of this PRD spent a pointer to prevent.
//
// FOUNDED IS THE EXCEPTION, and the boundary is worth stating because the sentence
// above is otherwise one field too broad. Founded has no omitempty — deliberately,
// since the server always sends it and TestBuildInfoDTOTags pins version+founded as
// the always-present pair. Against a PRE-#175 server, which sends neither, the
// decode leaves it "" and re-marshalling emits `"founded": ""`: present-but-empty,
// the one place this response conflates unknown with empty. Adding omitempty would
// fix the rollout-window cosmetic by changing the server's contract, which is the
// wrong trade. The window closes when the server is upgraded.
//
// Confined to --json. The text path skips empty values (serverRows in
// cmd/uzi/version.go), so it already prints nothing for an unknown founded.
// Measured against a server returning `{"version":"0.11.11"}`: --json emits
// `"founded": ""` while the text output shows only the version line.
func (c *HTTPClient) BuildInfo(ctx context.Context) (apitypes.BuildInfoDTO, error) {
	var out apitypes.BuildInfoDTO
	if err := c.get(ctx, "/api/version", &out); err != nil {
		return apitypes.BuildInfoDTO{}, err
	}
	return out, nil
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
