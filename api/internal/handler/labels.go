package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/httpx"
)

// CheckRepoLabels reports which of the requested labels are absent from the repo's forge
// (POST /api/repos/{id}/labels/check, PRD #589 M4). It is the sweep-label WARN primitive:
// the caller supplies the selector labels (a sweep default's catalog labels, or a custom
// sweep's form labels) and gets back the subset that does not exist yet, so the UI/CLI can
// warn and offer to create them. Owner-scoped via repoForRequest (404 for a foreign repo),
// forge-limited at the mount. It performs a read (ListLabels) only; it never writes.
func (h *Handler) CheckRepoLabels(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	var req apitypes.LabelCheckRequest
	if err := httpx.DecodeJSONLimited(w, r, &req); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("check repo labels: build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	existing, err := f.ListLabels(r.Context(), repo.ForgeProjectID)
	if err != nil {
		// err is already PAT-redacted by the driver; an upstream forge read failure is a 502.
		httpx.Error(w, http.StatusBadGateway, "could not list the repo's labels on the forge: "+err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.LabelCheckResponse{Missing: missingLabels(existing, req.Labels)})
}

// EnsureRepoLabels creates any of the requested labels that do not already exist on the
// repo's forge (POST /api/repos/{id}/labels/ensure, PRD #589 M4). It is the sweep-label
// CONFIRM primitive: after the WARN, the caller confirms and this ensures the missing
// selector labels exist so the sweep will match. EnsureLabels is idempotent by name, so
// re-ensuring an existing label is a no-op. Owner-scoped via repoForRequest (404 for a
// foreign repo), forge-limited at the mount.
func (h *Handler) EnsureRepoLabels(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	var req apitypes.LabelEnsureRequest
	if err := httpx.DecodeJSONLimited(w, r, &req); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("ensure repo labels: build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	ensured, err := ensureRepoLabels(r.Context(), f, repo.ForgeProjectID, req.Labels)
	if err != nil {
		// err is already PAT-redacted by the driver; an upstream forge write failure is a 502.
		httpx.Error(w, http.StatusBadGateway, "could not ensure the labels on the forge: "+err.Error())
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.LabelEnsureResponse{Ensured: ensured})
}

// missingLabels returns the requested labels that are absent from existing, deduped and
// order-preserving, blanks dropped. Names are compared case-sensitively — that is exactly
// how the forge drivers decide label existence in EnsureLabels (an exact-name map lookup in
// each of gitlab/github/forgejo), so a case-only difference IS a missing label the forge
// would create, and reporting it as missing keeps check and ensure consistent.
func missingLabels(existing []forge.Label, requested []string) []string {
	have := make(map[string]struct{}, len(existing))
	for _, l := range existing {
		have[l.Name] = struct{}{}
	}
	missing := make([]string, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, name := range requested {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := have[name]; ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		missing = append(missing, name)
	}
	return missing
}

// ensureRepoLabels ensures the requested labels exist on the forge project and returns the
// deduped, non-blank set it ensured. It hands EnsureLabels the requested labels directly
// (EnsureLabels skips any that already exist), with an empty color so each driver applies
// its own default — mirroring findings_file.go's `forge.Label{{Name: marker}}`. An empty
// request is a no-op (no forge call). Split out as a package function taking a forge.Forge
// so the WARN/CONFIRM logic is unit-testable against a fake forge without a DB.
func ensureRepoLabels(ctx context.Context, f forge.Forge, projectID int64, requested []string) ([]string, error) {
	names := dedupNonBlank(requested)
	if len(names) == 0 {
		return []string{}, nil
	}
	labels := make([]forge.Label, 0, len(names))
	for _, n := range names {
		labels = append(labels, forge.Label{Name: n})
	}
	if err := f.EnsureLabels(ctx, projectID, labels); err != nil {
		return nil, err
	}
	return names, nil
}

// dedupNonBlank trims each entry, drops the blanks, and removes later duplicates while
// preserving first-seen order.
func dedupNonBlank(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
