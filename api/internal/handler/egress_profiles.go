package handler

// egress_profiles.go is the admin CRUD for egress profiles (PRD #1906 M1, Decision 4):
// named site lists a later milestone binds a no-internet research run to. Reads sit in
// the admin READ group (session or uza_ admin_ro token); create, update and delete sit
// in the cookie-only admin WRITE group, so no Bearer token ever writes a list (the PRD
// #64 split; see routes_admin.go).
//
// Every rule about an entry lives in api/internal/egressprofile, the same code the
// fetcher will match with. A write is all-or-nothing: every problem in the body is
// reported at once as a 422 with a structured list, and nothing is stored.

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/egressprofile"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// egressProfileCreateRequest is the POST body. Every PUT is a full replacement of the
// mutable fields, so an override must be re-sent with the entry it covers.
type egressProfileCreateRequest struct {
	Name                   string   `json:"name"`
	Description            string   `json:"description"`
	Hosts                  []string `json:"hosts"`
	MultiPublisherOverride []string `json:"multi_publisher_override"`
}

// egressProfileUpdateRequest is the PUT body. The name is the path parameter and is
// immutable (a run will reference a list by name), so the body has no name field and a
// body that carries one is refused as an unknown field.
type egressProfileUpdateRequest struct {
	Description            string   `json:"description"`
	Hosts                  []string `json:"hosts"`
	MultiPublisherOverride []string `json:"multi_publisher_override"`
}

// egressProfileInvalidBody is the 422 body: the usual {"error"} envelope, a stable
// reason, and one problem per refused field or entry.
type egressProfileInvalidBody struct {
	Error    string                  `json:"error"`
	Reason   string                  `json:"reason"`
	Problems []egressprofile.Problem `json:"problems"`
}

const egressProfileInvalidReason = "invalid_egress_profile"

// egressProfileRunsFK is the Postgres default name of runs.egress_profile_id's foreign key
// (migration 00270).
const egressProfileRunsFK = "runs_egress_profile_id_fkey"

func egressProfileToDTO(p store.EgressProfile, warnings []egressprofile.Warning) apitypes.EgressProfileDTO {
	dto := apitypes.EgressProfileDTO{
		ID:                     p.ID.String(),
		Name:                   p.Name,
		Description:            p.Description,
		Hosts:                  nonNilStrings(p.Hosts),
		MultiPublisherOverride: nonNilStrings(p.MultiPublisherOverride),
		Warnings:               make([]apitypes.EgressProfileWarningDTO, 0, len(warnings)),
		CreatedBy:              uuidPtrValue(p.CreatedBy),
		UpdatedBy:              uuidPtrValue(p.UpdatedBy),
		CreatedAt:              p.CreatedAt.Time,
		UpdatedAt:              p.UpdatedAt.Time,
	}
	for _, w := range warnings {
		dto.Warnings = append(dto.Warnings, apitypes.EgressProfileWarningDTO{Entry: w.Entry, Code: w.Code, Message: w.Message})
	}
	return dto
}

// egressProfileAdmin returns the admin actor, writing 401/403 otherwise. The route groups
// already enforce this; the check is defense in depth, like the tool allowlist writes.
func egressProfileAdmin(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, false
	}
	if !actor.IsAdmin {
		httpx.Error(w, http.StatusForbidden, "admin only")
		return store.User{}, false
	}
	return actor, true
}

// egressProfileName reads the {name} path parameter. A name that cannot exist (it fails
// the name rule) is a 404 like an unknown one: there is nothing to address.
func egressProfileName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := chi.URLParam(r, "name")
	if egressprofile.ValidateName(name) != nil {
		httpx.Error(w, http.StatusNotFound, "egress profile not found")
		return "", false
	}
	return name, true
}

// AdminListEgressProfiles lists every egress profile, by name.
func (h *Handler) AdminListEgressProfiles(w http.ResponseWriter, r *http.Request) {
	if _, ok := egressProfileAdmin(w, r); !ok {
		return
	}
	rows, err := h.q.ListEgressProfiles(r.Context())
	if err != nil {
		slog.Error("list egress profiles", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.EgressProfileDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, egressProfileToDTO(p, egressprofile.WarningsFor(p.Hosts, p.MultiPublisherOverride)))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"egress_profiles": out})
}

// AdminGetEgressProfile returns one egress profile by name.
func (h *Handler) AdminGetEgressProfile(w http.ResponseWriter, r *http.Request) {
	if _, ok := egressProfileAdmin(w, r); !ok {
		return
	}
	name, ok := egressProfileName(w, r)
	if !ok {
		return
	}
	p, err := h.q.GetEgressProfileByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "egress profile not found")
			return
		}
		slog.Error("get egress profile", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"egress_profile": egressProfileToDTO(p, egressprofile.WarningsFor(p.Hosts, p.MultiPublisherOverride))})
}

// AdminCreateEgressProfile creates a profile. 201 with the stored profile and its
// warnings; 422 with every problem; 409 when the name is taken.
func (h *Handler) AdminCreateEgressProfile(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
	if !ok {
		return
	}
	var req egressProfileCreateRequest
	if err := httpx.DecodeJSONStrict(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	v, probs := egressprofile.Validate(egressprofile.Input{
		Name:                   req.Name,
		Description:            req.Description,
		Hosts:                  req.Hosts,
		MultiPublisherOverride: req.MultiPublisherOverride,
	}, true)
	if len(probs) > 0 {
		writeEgressProfileInvalid(w, probs)
		return
	}
	p, err := h.q.CreateEgressProfile(r.Context(), store.CreateEgressProfileParams{
		Name:                   v.Name,
		Description:            v.Description,
		Hosts:                  v.Hosts,
		MultiPublisherOverride: nonNilStrings(v.MultiPublisherOverride),
		Actor:                  pgconv.UUID(actor.ID),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Error(w, http.StatusConflict, "an egress profile with that name already exists")
			return
		}
		slog.Error("create egress profile", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"egress_profile": egressProfileToDTO(p, v.Warnings)})
}

// AdminUpdateEgressProfile replaces a profile's description, hosts and overrides. The
// name is immutable. 404 for an unknown name; 422 with every problem.
func (h *Handler) AdminUpdateEgressProfile(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
	if !ok {
		return
	}
	name, ok := egressProfileName(w, r)
	if !ok {
		return
	}
	var req egressProfileUpdateRequest
	if err := httpx.DecodeJSONStrict(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	v, probs := egressprofile.Validate(egressprofile.Input{
		Name:                   name,
		Description:            req.Description,
		Hosts:                  req.Hosts,
		MultiPublisherOverride: req.MultiPublisherOverride,
	}, false)
	if len(probs) > 0 {
		writeEgressProfileInvalid(w, probs)
		return
	}
	p, err := h.q.UpdateEgressProfileByName(r.Context(), store.UpdateEgressProfileByNameParams{
		Name:                   name,
		Description:            v.Description,
		Hosts:                  v.Hosts,
		MultiPublisherOverride: nonNilStrings(v.MultiPublisherOverride),
		Actor:                  pgconv.UUID(actor.ID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "egress profile not found")
			return
		}
		slog.Error("update egress profile", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"egress_profile": egressProfileToDTO(p, v.Warnings)})
}

// AdminDeleteEgressProfile deletes a profile by name. 204, or 404 for an unknown name.
// Nothing references a profile yet (runs gain a reference in a later milestone, which
// snapshots the list at claim), so a delete has no dependents to guard.
func (h *Handler) AdminDeleteEgressProfile(w http.ResponseWriter, r *http.Request) {
	if _, ok := egressProfileAdmin(w, r); !ok {
		return
	}
	name, ok := egressProfileName(w, r)
	if !ok {
		return
	}
	n, err := h.q.DeleteEgressProfileByName(r.Context(), name)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == egressProfileRunsFK {
		// PRD #1906 M3: runs.egress_profile_id is ON DELETE RESTRICT, because unbinding a
		// run would let it claim with the full tool set and a forge credential.
		httpx.Error(w, http.StatusConflict, "egress profile is still referenced by runs")
		return
	}
	if err != nil {
		slog.Error("delete egress profile", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		httpx.Error(w, http.StatusNotFound, "egress profile not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeEgressProfileInvalid(w http.ResponseWriter, probs []egressprofile.Problem) {
	httpx.JSON(w, http.StatusUnprocessableEntity, egressProfileInvalidBody{
		Error:    "the egress profile is invalid: see problems",
		Reason:   egressProfileInvalidReason,
		Problems: probs,
	})
}
