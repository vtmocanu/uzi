package uzicli

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// client_schedules.go holds the schedule verbs (uzi schedule) of the
// Client/HTTPClient split out of client.go (PRD #1017).

// The schedule endpoints (PRD #241 M6) return their DTOs BARE — the handler writes
// the value directly (httpx.JSON), not under a `{"schedule": …}` key — so these
// decode into the DTO itself rather than an envelope struct.

func (c *HTTPClient) ListSchedules(ctx context.Context) ([]apitypes.ScheduleDTO, error) {
	var out []apitypes.ScheduleDTO
	if err := c.get(ctx, "/api/me/schedules", &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *HTTPClient) CreateSchedule(ctx context.Context, repoID string, req apitypes.ScheduleRequest) (apitypes.ScheduleDTO, error) {
	var out apitypes.ScheduleDTO
	if err := c.postJSON(ctx, "/api/repos/"+url.PathEscape(repoID)+"/schedules", req, &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) GetSchedule(ctx context.Context, id string) (apitypes.ScheduleDTO, error) {
	var out apitypes.ScheduleDTO
	if err := c.get(ctx, "/api/schedules/"+url.PathEscape(id), &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) SetScheduleEnabled(ctx context.Context, id string, enabled bool) (apitypes.ScheduleDTO, error) {
	// A struct carrying ONLY `enabled`, so the handler's onlyEnabled() pause/resume path
	// fires: the config is left untouched and just the flag flips. A full ScheduleRequest
	// here would re-submit empty target/timing fields and be rejected on re-validation.
	reqBody := struct {
		Enabled bool `json:"enabled"`
	}{Enabled: enabled}
	var out apitypes.ScheduleDTO
	if err := c.patch(ctx, "/api/schedules/"+url.PathEscape(id), reqBody, &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) PatchSchedule(ctx context.Context, id string, req apitypes.ScheduleRequest) (apitypes.ScheduleDTO, error) {
	var out apitypes.ScheduleDTO
	if err := c.patch(ctx, "/api/schedules/"+url.PathEscape(id), req, &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) DeleteSchedule(ctx context.Context, id string) error {
	return c.del(ctx, "/api/schedules/"+url.PathEscape(id))
}

func (c *HTTPClient) RunScheduleNow(ctx context.Context, id string) (apitypes.RunNowResponse, error) {
	var out apitypes.RunNowResponse
	if err := c.postJSON(ctx, "/api/schedules/"+url.PathEscape(id)+"/run-now", nil, &out); err != nil {
		return apitypes.RunNowResponse{}, err
	}
	return out, nil
}

func (c *HTTPClient) ListScheduleCatalog(ctx context.Context) (apitypes.ScheduleCatalogResponse, error) {
	var out apitypes.ScheduleCatalogResponse
	if err := c.get(ctx, "/api/schedule-catalog", &out); err != nil {
		return apitypes.ScheduleCatalogResponse{}, err
	}
	return out, nil
}

func (c *HTTPClient) EnableCatalogSchedule(ctx context.Context, repoID, slug string) (apitypes.ScheduleDTO, bool, error) {
	// Call doJSONRead directly (not postJSON) so the 201-vs-200 status survives: the server
	// answers 201 for a fresh enable and 200 for an idempotent repeat, and the CLI renders
	// created vs already-enabled off that distinction.
	path := "/api/repos/" + url.PathEscape(repoID) + "/schedule-catalog/" + url.PathEscape(slug)
	resp, body, err := c.doJSONRead(ctx, http.MethodPost, path, nil)
	if err != nil {
		return apitypes.ScheduleDTO{}, false, err
	}
	var out apitypes.ScheduleDTO
	if err := decode2xx(resp, body, path, &out); err != nil {
		return apitypes.ScheduleDTO{}, false, err
	}
	return out, resp.StatusCode == http.StatusCreated, nil
}

func (c *HTTPClient) ResetSchedule(ctx context.Context, id string) (apitypes.ScheduleDTO, error) {
	var out apitypes.ScheduleDTO
	if err := c.postJSON(ctx, "/api/schedules/"+url.PathEscape(id)+"/reset", nil, &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) CloneSchedule(ctx context.Context, id, repoID string) (apitypes.ScheduleDTO, error) {
	// An empty repoID clones into the source's own repo, so the body is nil (no repo_id
	// key); a non-empty one sends {"repo_id": …} to clone into that owned repo.
	var body any
	if strings.TrimSpace(repoID) != "" {
		trimmed := strings.TrimSpace(repoID)
		body = apitypes.ScheduleCloneRequest{RepoID: &trimmed}
	}
	var out apitypes.ScheduleDTO
	if err := c.postJSON(ctx, "/api/schedules/"+url.PathEscape(id)+"/clone", body, &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) AddScheduleRepo(ctx context.Context, id, repoID string) (apitypes.ScheduleDTO, error) {
	// postJSON maps a non-2xx through statusError, so a foreign source/repo 404→ExitNotFound
	// and a duplicate-sibling 409→ExitConflict reach the caller as documented exit codes; the
	// add-repo command special-cases the 409 into a clean no-op.
	var out apitypes.ScheduleDTO
	body := apitypes.AddScheduleRepoRequest{RepoID: strings.TrimSpace(repoID)}
	if err := c.postJSON(ctx, "/api/schedules/"+url.PathEscape(id)+"/add-repo", body, &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) GetSchedulePause(ctx context.Context) (apitypes.SchedulePauseDTO, error) {
	var out apitypes.SchedulePauseDTO
	if err := c.get(ctx, "/api/schedules/pause", &out); err != nil {
		return apitypes.SchedulePauseDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) SetSchedulePause(ctx context.Context, until *time.Time) (apitypes.SchedulePauseDTO, error) {
	// The wire body is {"until": <RFC3339>|null}: an anonymous struct with a nil-able
	// pointer, so a nil `until` (the indefinite "until I resume" pause) marshals as
	// `null` rather than being omitted — the server distinguishes the two.
	reqBody := struct {
		Until *time.Time `json:"until"`
	}{until}
	var out apitypes.SchedulePauseDTO
	if err := c.put(ctx, "/api/schedules/pause", reqBody, &out); err != nil {
		return apitypes.SchedulePauseDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) ClearSchedulePause(ctx context.Context) (apitypes.SchedulePauseDTO, error) {
	// delRead (not del): this DELETE returns the resulting state body, which del discards.
	var out apitypes.SchedulePauseDTO
	if err := c.delRead(ctx, "/api/schedules/pause", &out); err != nil {
		return apitypes.SchedulePauseDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) CheckRepoLabels(ctx context.Context, repoID string, labels []string) ([]string, error) {
	var out apitypes.LabelCheckResponse
	path := "/api/repos/" + url.PathEscape(repoID) + "/labels/check"
	if err := c.postJSON(ctx, path, apitypes.LabelCheckRequest{Labels: labels}, &out); err != nil {
		return nil, err
	}
	return out.Missing, nil
}

func (c *HTTPClient) EnsureRepoLabels(ctx context.Context, repoID string, labels []string) error {
	var out apitypes.LabelEnsureResponse
	path := "/api/repos/" + url.PathEscape(repoID) + "/labels/ensure"
	return c.postJSON(ctx, path, apitypes.LabelEnsureRequest{Labels: labels}, &out)
}
