package handler

import (
	"log/slog"
	"net/http"
	"slices"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// V1Whoami serves GET /api/v1/whoami (PRD #1907 D13): the caller's user (id and
// display name), the product a uzp_ token is bound to (null for a uzc_ caller acting
// directly), and the scopes the request holds. It builds the DTO field by field from
// the V1Principal RequireV1Caller resolved; the store.User row is never serialized, so
// no email, admin flag or other column can reach the wire.
func (h *Handler) V1Whoami(w http.ResponseWriter, r *http.Request) {
	p, ok := mw.V1PrincipalFromContext(r.Context())
	if !ok {
		// Unreachable behind RequireV1Caller: reaching here means the route was mounted
		// without it (a programming error). Refuse with a 500 that is deliberately NOT
		// RequireV1Caller's 401, so TestV1SubtreeRequiresV1CallerLiveDB tells the two
		// apart.
		slog.Error("v1 whoami: no V1Principal in context; /api/v1 route mounted without RequireV1Caller")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	resp := apitypes.V1WhoamiDTO{
		User: apitypes.V1WhoamiUserDTO{
			ID:          p.User.ID.String(),
			DisplayName: p.User.DisplayName.String,
		},
		// Never null on the wire: the spec declares scopes a non-nullable array.
		Scopes: slices.Clone(p.Scopes),
	}
	if resp.Scopes == nil {
		resp.Scopes = []string{}
	}
	if p.ProductID.Valid {
		resp.Product = &apitypes.V1WhoamiProductDTO{
			ID:   p.ProductID.UUID.String(),
			Name: p.ProductName,
		}
	}
	httpx.JSON(w, http.StatusOK, resp)
}
