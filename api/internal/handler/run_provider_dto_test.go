package handler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// providerNameStore answers only the batched provider-name read; anything else panics through the
// embedded nil interface. names stands for the join result: a run whose provider row is gone has
// no entry, exactly as the SQL returns no row for it.
type providerNameStore struct {
	workersvc.Store
	names map[uuid.UUID]string
	err   error
	calls [][]uuid.UUID
}

func (s *providerNameStore) ListRunProviderNames(_ context.Context, ids []uuid.UUID) ([]store.ListRunProviderNamesRow, error) {
	s.calls = append(s.calls, ids)
	if s.err != nil {
		return nil, s.err
	}
	var rows []store.ListRunProviderNamesRow
	for _, id := range ids {
		if n, ok := s.names[id]; ok {
			rows = append(rows, store.ListRunProviderNamesRow{RunID: id, Name: n})
		}
	}
	return rows, nil
}

func providerRunDTO(id uuid.UUID, provider uuid.UUID) apitypes.RunDTO {
	return runToDTO(store.Run{ID: id, Harness: "claude", Status: "completed",
		ModelProviderID: pgtype.UUID{Bytes: provider, Valid: true}}, "normal", 0, 0, 0, dtoTestNow, 0)
}

// wireProvider returns the run's provider_id and provider_name exactly as they serialize, so an
// absent key and an explicit null are told apart.
func wireProvider(t *testing.T, d apitypes.RunDTO) (id, name any) {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"provider_id", "provider_name"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("key %s missing from the run JSON; it must be an explicit null for a vendor run", k)
		}
	}
	return m["provider_id"], m["provider_name"]
}

// TestRunProviderIdentity: a vendor run carries provider_id and provider_name as explicit nulls,
// without touching the store; a provider run carries both; a run whose provider was deleted keeps
// its id and has a null name.
func TestRunProviderIdentity(t *testing.T) {
	vendorRun, boundRun, orphanRun := uuid.New(), uuid.New(), uuid.New()
	boundProvider, deletedProvider := uuid.New(), uuid.New()
	st := &providerNameStore{names: map[uuid.UUID]string{boundRun: "corp <b>gateway</b>"}}
	h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})}

	vendor := runToDTO(store.Run{ID: vendorRun, Harness: "claude", Status: "completed"}, "normal", 0, 0, 0, dtoTestNow, 0)
	h.overlayProviderNames(context.Background(), &vendor)
	if len(st.calls) != 0 {
		t.Fatalf("a vendor-only page read %v, want no provider-name query", st.calls)
	}
	if id, name := wireProvider(t, vendor); id != nil || name != nil {
		t.Fatalf("vendor run provider = %v / %v, want null / null", id, name)
	}

	bound, orphan := providerRunDTO(boundRun, boundProvider), providerRunDTO(orphanRun, deletedProvider)
	h.overlayProviderNames(context.Background(), &vendor, &bound, &orphan)
	if len(st.calls) != 1 || len(st.calls[0]) != 2 {
		t.Fatalf("reads = %v, want one batched read of exactly the two bound runs", st.calls)
	}
	if id, name := wireProvider(t, bound); id != boundProvider.String() || name != "corp <b>gateway</b>" {
		t.Fatalf("provider run = %v / %v, want %s / the stored name verbatim", id, name, boundProvider)
	}
	if id, name := wireProvider(t, orphan); id != deletedProvider.String() || name != nil {
		t.Fatalf("deleted-provider run = %v / %v, want %s / null", id, name, deletedProvider)
	}
	if id, name := wireProvider(t, vendor); id != nil || name != nil {
		t.Fatalf("vendor run beside provider runs = %v / %v, want null / null", id, name)
	}
}

// TestOverlayProviderNamesLookupFailure: a failed read degrades to a null name and keeps the id.
func TestOverlayProviderNamesLookupFailure(t *testing.T) {
	runID, provider := uuid.New(), uuid.New()
	st := &providerNameStore{err: errors.New("db down")}
	h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})}
	d := providerRunDTO(runID, provider)
	h.overlayProviderNames(context.Background(), &d)
	if d.ProviderID == nil || *d.ProviderID != provider.String() || d.ProviderName != nil {
		t.Fatalf("after a failed read id=%v name=%v, want the id kept and a null name", d.ProviderID, d.ProviderName)
	}
}
