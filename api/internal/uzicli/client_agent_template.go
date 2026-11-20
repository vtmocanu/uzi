package uzicli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// client_agent_template.go holds the owner-scoped agent-template pin verbs (PRD #3264 M3):
// GET /api/me/agent-templates and PUT /api/me/agent-templates/{id}/model-pins.

// AgentTemplatePins is one template in the restricted CLI view: identity, the two stored pins
// (nil when the template inherits) and two flags. It carries no prompt body, description or tools.
type AgentTemplatePins struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Scope      string  `json:"scope"`
	Model      *string `json:"model"`
	CodexModel *string `json:"codex_model"`
	Customized bool    `json:"customized"`
	Editable   bool    `json:"editable"`
}

// AgentTemplateListResult is the 200 reply of GET /api/me/agent-templates: the decoded templates
// and the server's envelope exactly as sent, so `--json` prints what the server said.
type AgentTemplateListResult struct {
	Templates []AgentTemplatePins
	Raw       json.RawMessage
}

// AgentTemplatePinsResult is the 200 reply of PUT /api/me/agent-templates/{id}/model-pins.
type AgentTemplatePinsResult struct {
	Template AgentTemplatePins
	Raw      json.RawMessage
}

// errAgentTemplatesUnsupported is the refusal for a server that predates the agent-template pin routes.
const errAgentTemplatesUnsupported = "this server does not support CLI model-setting writes; upgrade the server"

func (c *HTTPClient) ListMyAgentTemplates(ctx context.Context) (AgentTemplateListResult, error) {
	const path = "/api/me/agent-templates"
	resp, body, err := c.doJSONRead(ctx, http.MethodGet, path, nil)
	if err != nil {
		return AgentTemplateListResult{}, err
	}
	if resp.StatusCode/100 != 2 {
		// The list route never answers a JSON 404 of its own, so an error-less 404 or 405 is an
		// older server.
		if e := unsupportedRouteError(resp.StatusCode, body, errAgentTemplatesUnsupported); e != nil {
			return AgentTemplateListResult{}, e
		}
		return AgentTemplateListResult{}, statusError(resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	var env struct {
		Templates *[]AgentTemplatePins `json:"templates"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Templates == nil {
		return AgentTemplateListResult{}, Exitf(ExitGeneric, "malformed response from uzi (%s)", path)
	}
	return AgentTemplateListResult{Templates: *env.Templates, Raw: json.RawMessage(body)}, nil
}

// PutAgentTemplateModelPins sets (string) or clears (nil, JSON null) the pins named in patch, whose
// keys are the API fields model and codex_model. Any 404 is a missing template (ExitNotFound); an
// error-less 405 is a server without the route, mapped to the "upgrade the server" error like the
// list.
func (c *HTTPClient) PutAgentTemplateModelPins(ctx context.Context, id string, patch map[string]*string) (AgentTemplatePinsResult, error) {
	path := "/api/me/agent-templates/" + url.PathEscape(id) + "/model-pins"
	resp, body, err := c.doJSONRead(ctx, http.MethodPut, path, patch)
	if err != nil {
		return AgentTemplatePinsResult{}, err
	}
	if resp.StatusCode/100 != 2 {
		// Any 404 is not-found (exit 4), JSON or not; only an error-less 405 is an older server.
		if resp.StatusCode != http.StatusNotFound {
			if e := unsupportedRouteError(resp.StatusCode, body, errAgentTemplatesUnsupported); e != nil {
				return AgentTemplatePinsResult{}, e
			}
		}
		return AgentTemplatePinsResult{}, statusError(resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	var env struct {
		Template *AgentTemplatePins `json:"template"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Template == nil {
		return AgentTemplatePinsResult{}, Exitf(ExitGeneric, "malformed response from uzi (%s)", path)
	}
	return AgentTemplatePinsResult{Template: *env.Template, Raw: json.RawMessage(body)}, nil
}
