package uzicli

import (
	"context"
	"net/url"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// client_providers.go holds the model-provider verbs (PRD #4320) of the Client/HTTPClient split.
// Only the RequireUser routes are reachable here; the key and endpoint routes are cookie-only
// by design and have no client method.

func (c *HTTPClient) ListProviders(ctx context.Context) (apitypes.ProviderListDTO, error) {
	var out apitypes.ProviderListDTO
	if err := c.get(ctx, "/api/me/providers", &out); err != nil {
		return apitypes.ProviderListDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) CreateProvider(ctx context.Context, req apitypes.ProviderCreateRequest) (apitypes.ProviderDTO, error) {
	var env struct {
		Provider apitypes.ProviderDTO `json:"provider"`
	}
	if err := c.postJSON(ctx, "/api/me/providers", req, &env); err != nil {
		return apitypes.ProviderDTO{}, err
	}
	return env.Provider, nil
}

func (c *HTTPClient) EditProvider(ctx context.Context, id string, req apitypes.ProviderPatchRequest) (apitypes.ProviderDTO, error) {
	var env struct {
		Provider apitypes.ProviderDTO `json:"provider"`
	}
	if err := c.patch(ctx, "/api/me/providers/"+url.PathEscape(id), req, &env); err != nil {
		return apitypes.ProviderDTO{}, err
	}
	return env.Provider, nil
}

func (c *HTTPClient) RetireProvider(ctx context.Context, id string) (apitypes.ProviderDTO, error) {
	var env struct {
		Provider apitypes.ProviderDTO `json:"provider"`
	}
	if err := c.postJSON(ctx, "/api/me/providers/"+url.PathEscape(id)+"/retire", nil, &env); err != nil {
		return apitypes.ProviderDTO{}, err
	}
	return env.Provider, nil
}

// TestProvider runs the connectivity probe and returns the stored result (never nil on success
// from a current server; a nil probe from a server that sent none is returned as nil).
func (c *HTTPClient) TestProvider(ctx context.Context, id string) (*apitypes.ProviderProbeDTO, error) {
	var env struct {
		Probe *apitypes.ProviderProbeDTO `json:"probe"`
	}
	if err := c.postJSON(ctx, "/api/me/providers/"+url.PathEscape(id)+"/test", nil, &env); err != nil {
		return nil, err
	}
	return env.Probe, nil
}

// SetClaudeProviderSource sends {"provider_id": <id>|null} and returns the selection the server
// reports back (claude_provider_id), nil when it is cleared.
func (c *HTTPClient) SetClaudeProviderSource(ctx context.Context, id *string) (*string, error) {
	var out struct {
		ClaudeProviderID *string `json:"claude_provider_id"`
	}
	body := struct {
		ProviderID *string `json:"provider_id"`
	}{id}
	if err := c.put(ctx, "/api/me/providers/claude-source", body, &out); err != nil {
		return nil, err
	}
	return out.ClaudeProviderID, nil
}

// SetCodexProviderSource sends {"provider_id": <id>|null} and returns the selection the server
// reports back (codex_provider_id), nil when it is cleared.
func (c *HTTPClient) SetCodexProviderSource(ctx context.Context, id *string) (*string, error) {
	var out struct {
		CodexProviderID *string `json:"codex_provider_id"`
	}
	body := struct {
		ProviderID *string `json:"provider_id"`
	}{id}
	if err := c.put(ctx, "/api/me/providers/codex-source", body, &out); err != nil {
		return nil, err
	}
	return out.CodexProviderID, nil
}
