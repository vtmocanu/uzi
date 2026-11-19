package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// providerRunsStore is the runsStore handler fake plus the batched provider-name read, answering
// only for the run ids the handler actually asks about, as the real join does.
type providerRunsStore struct {
	*runsStore
	names map[uuid.UUID]string
}

func (s *providerRunsStore) ListRunProviderNames(_ context.Context, ids []uuid.UUID) ([]store.ListRunProviderNamesRow, error) {
	var out []store.ListRunProviderNamesRow
	for id, n := range s.names {
		if slices.Contains(ids, id) {
			out = append(out, store.ListRunProviderNamesRow{RunID: id, Name: n})
		}
	}
	return out, nil
}

// TestProviderNameReadHandlersWiring pins the PRD #4320 provider_name overlay in every read that
// serves a RunDTO: the owner's GetRun and ListRuns, and AdminListRuns. A provider run carries its
// provider's name and a vendor neighbour carries explicit nulls. The overlay helper has its own
// unit test (TestRunProviderIdentity); this one fails when a handler stops calling it.
func TestProviderNameReadHandlersWiring(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	provRunID, vendorID, providerID := uuid.New(), uuid.New(), uuid.New()
	provRun := store.Run{ID: provRunID, UserID: owner.ID, Harness: "claude", Status: "running",
		ModelProviderID: pgtype.UUID{Bytes: providerID, Valid: true}}
	vendor := store.Run{ID: vendorID, UserID: owner.ID, Harness: "claude", Status: "running"}
	newStore := func() *providerRunsStore {
		return &providerRunsStore{
			runsStore: &runsStore{
				ownerID:    owner.ID,
				run:        provRun,
				userRuns:   []store.ListRunsForUserRow{{Run: provRun}, {Run: vendor}},
				activeRuns: []store.ListActiveRunsAllRow{{Run: provRun, OwnerEmail: "o@x.io"}, {Run: vendor, OwnerEmail: "o@x.io"}},
			},
			names: map[uuid.UUID]string{provRunID: "team-gateway"},
		}
	}
	type runJSON struct {
		ID           string  `json:"id"`
		ProviderID   *string `json:"provider_id"`
		ProviderName *string `json:"provider_name"`
	}
	assertProvider := func(t *testing.T, r runJSON) {
		t.Helper()
		if r.ProviderID == nil || *r.ProviderID != providerID.String() {
			t.Errorf("provider run %s provider_id = %v, want %s", r.ID, r.ProviderID, providerID)
		}
		if r.ProviderName == nil || *r.ProviderName != "team-gateway" {
			t.Errorf("provider run %s provider_name = %v, want team-gateway", r.ID, r.ProviderName)
		}
	}
	assertList := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Runs []runJSON `json:"runs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Runs) != 2 {
			t.Fatalf("runs = %+v, want the provider run and its vendor neighbour", body.Runs)
		}
		for _, r := range body.Runs {
			switch r.ID {
			case provRunID.String():
				assertProvider(t, r)
			case vendorID.String():
				if r.ProviderID != nil || r.ProviderName != nil {
					t.Errorf("vendor run provider = %v/%v, want both null", r.ProviderID, r.ProviderName)
				}
			default:
				t.Fatalf("unexpected run %s", r.ID)
			}
		}
	}

	t.Run("owner GetRun", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newRunsHandler(t, newStore()).GetRun(rec, runReq(owner, provRunID))
		if rec.Code != http.StatusOK {
			t.Fatalf("GetRun = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Run runJSON `json:"run"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		assertProvider(t, body.Run)
	})
	t.Run("owner ListRuns", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
		newRunsHandler(t, newStore()).ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), owner)))
		assertList(t, rec)
	})
	t.Run("AdminListRuns", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/runs", nil)
		newRunsHandler(t, newStore()).AdminListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), admin)))
		assertList(t, rec)
	})
}
