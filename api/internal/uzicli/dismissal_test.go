package uzicli

import (
	"os"
	"testing"
	"time"
)

// PRD #1251 M1 — the TUI startup update prompt's per-version "don't remind me" dismissal,
// persisted in the same per-server version-check file as the skew cache.

func TestDismissedUpdateRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	const url = "https://uzi.example"

	if got := s.DismissedUpdateTag(url); got != "" {
		t.Fatalf("a fresh store must report no dismissal, got %q", got)
	}
	if err := s.RecordDismissedUpdate(url, "v0.85.0"); err != nil {
		t.Fatalf("RecordDismissedUpdate: %v", err)
	}
	if got := s.DismissedUpdateTag(url); got != "v0.85.0" {
		t.Errorf("DismissedUpdateTag = %q, want v0.85.0", got)
	}
	// Keyed per server: a different URL is unaffected (like the version cache).
	if got := s.DismissedUpdateTag("https://other.example"); got != "" {
		t.Errorf("a dismissal must be per-server, got %q for a different URL", got)
	}
}

func TestDismissedUpdateNilStore(t *testing.T) {
	var s *Store
	if got := s.DismissedUpdateTag("https://uzi.example"); got != "" {
		t.Errorf("a nil store must read as not-dismissed, got %q", got)
	}
	if err := s.RecordDismissedUpdate("https://uzi.example", "v0.85.0"); err == nil {
		t.Error("recording against a nil store should return an error, not panic")
	}
}

// TestDismissedUpdateCoexistsWithVersionCache proves the dismissal and the skew cache share
// one per-server record without clobbering each other in either direction.
func TestDismissedUpdateCoexistsWithVersionCache(t *testing.T) {
	s := NewStore(t.TempDir())
	const url = "https://uzi.example"
	now := time.Now()

	if _, err := s.RecordServerVersion(url, "0.84.0", "v0.83.0", now); err != nil {
		t.Fatalf("RecordServerVersion: %v", err)
	}
	if err := s.RecordDismissedUpdate(url, "v0.85.0"); err != nil {
		t.Fatalf("RecordDismissedUpdate: %v", err)
	}
	if got := s.DismissedUpdateTag(url); got != "v0.85.0" {
		t.Errorf("recording a dismissal did not persist it, got %q", got)
	}
	if v, fresh := s.CachedServerVersion(url, now.Add(time.Minute), VersionCheckTTL); !fresh || v != "0.84.0" {
		t.Errorf("the dismissal clobbered the version cache: v=%q fresh=%v", v, fresh)
	}

	// A later version-check must not wipe the dismissal.
	if _, err := s.RecordServerVersion(url, "0.86.0", "v0.83.0", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("RecordServerVersion (second): %v", err)
	}
	if got := s.DismissedUpdateTag(url); got != "v0.85.0" {
		t.Errorf("a later version-check wiped the dismissal, got %q", got)
	}
}

// TestDismissedUpdateBackCompat proves an older file lacking the dismissed_update_tag field
// decodes fine (reads as not-dismissed), and recording a dismissal preserves the pre-existing
// version reading.
func TestDismissedUpdateBackCompat(t *testing.T) {
	s := NewStore(t.TempDir())
	const url = "https://uzi.example"
	old := `{"servers":{"` + versionCheckKey(url) + `":{"version":"0.84.0","checked_at":"2026-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(s.versionCheckPath(), []byte(old), 0o600); err != nil {
		t.Fatalf("seed older file: %v", err)
	}

	if got := s.DismissedUpdateTag(url); got != "" {
		t.Errorf("an older file lacking the field must read as not-dismissed, got %q", got)
	}
	if err := s.RecordDismissedUpdate(url, "v0.85.0"); err != nil {
		t.Fatalf("RecordDismissedUpdate: %v", err)
	}
	at, err := time.Parse(time.RFC3339, "2026-01-01T00:01:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if v, fresh := s.CachedServerVersion(url, at, VersionCheckTTL); !fresh || v != "0.84.0" {
		t.Errorf("recording a dismissal wiped the pre-existing version: v=%q fresh=%v", v, fresh)
	}
}
