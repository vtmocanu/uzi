package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// syncHealthForLink is the pure server-side sync-health mapper (PRD #576 M2): it turns
// a github_project_links row into the caller-scoped badge DTO. It is called ONLY for a
// repo that has a link row, so Linked is always true and the unlinked→nil case is the
// caller's job (asserted via the absent-key branch in the list handlers). These cases
// pin the healthy / errored contract the badge reads, no DB required.
func TestSyncHealthForLink(t *testing.T) {
	synced := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	ts := pgtype.Timestamptz{Time: synced, Valid: true}

	t.Run("healthy link (no error) -> Healthy true, LastError nil", func(t *testing.T) {
		link := store.GithubProjectLink{
			RepoID:       uuid.New(),
			LastSyncedAt: ts,
			LastError:    pgtype.Text{Valid: false},
		}
		got := syncHealthForLink(link)
		if got == nil {
			t.Fatal("syncHealthForLink returned nil for a present link")
		}
		if !got.Linked {
			t.Error("Linked = false, want true (a present link is always linked)")
		}
		if !got.Healthy {
			t.Error("Healthy = false, want true (no last_error)")
		}
		if got.LastError != nil {
			t.Errorf("LastError = %q, want nil", *got.LastError)
		}
		if got.LastSyncedAt == nil || !got.LastSyncedAt.Equal(synced) {
			t.Errorf("LastSyncedAt = %v, want %v", got.LastSyncedAt, synced)
		}
	})

	t.Run("empty-string error -> Healthy true (empty is not an error)", func(t *testing.T) {
		link := store.GithubProjectLink{
			RepoID:    uuid.New(),
			LastError: pgtype.Text{String: "", Valid: true},
		}
		got := syncHealthForLink(link)
		if !got.Healthy {
			t.Error("Healthy = false, want true (an empty last_error string is not a failure)")
		}
		if got.LastError != nil {
			t.Errorf("LastError = %q, want nil for an empty string", *got.LastError)
		}
	})

	t.Run("errored link (last_error set) -> Healthy false, LastError propagated", func(t *testing.T) {
		link := store.GithubProjectLink{
			RepoID:       uuid.New(),
			LastSyncedAt: ts,
			LastError:    pgtype.Text{String: "boom: field option mismatch", Valid: true},
		}
		got := syncHealthForLink(link)
		if got.Healthy {
			t.Error("Healthy = true, want false (last_error is set)")
		}
		if got.LastError == nil || *got.LastError != "boom: field option mismatch" {
			t.Errorf("LastError = %v, want the stored message", got.LastError)
		}
		if got.LastSyncedAt == nil || !got.LastSyncedAt.Equal(synced) {
			t.Errorf("LastSyncedAt = %v, want %v", got.LastSyncedAt, synced)
		}
	})

	t.Run("never-synced errored link -> LastSyncedAt nil", func(t *testing.T) {
		link := store.GithubProjectLink{
			RepoID:       uuid.New(),
			LastSyncedAt: pgtype.Timestamptz{Valid: false},
			LastError:    pgtype.Text{String: "never ran", Valid: true},
		}
		got := syncHealthForLink(link)
		if got.LastSyncedAt != nil {
			t.Errorf("LastSyncedAt = %v, want nil (never synced)", *got.LastSyncedAt)
		}
		if got.Healthy {
			t.Error("Healthy = true, want false")
		}
	})
}
