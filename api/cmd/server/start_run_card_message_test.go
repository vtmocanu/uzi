package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// A source-locked repo matches ErrRepoNeverEnabled under errors.Is, but enabling it is
// refused, so the Slack answer must not tell the user to enable it.
func TestStartRunCardMessageSourceLockedIsNotNeverEnabled(t *testing.T) {
	locked := startRunCardMessage(fmt.Errorf("start: %w", store.ErrRepoSourceLocked))
	if !strings.Contains(locked, "issue tracker") || strings.Contains(locked, "enable it") {
		t.Fatalf("source-locked message = %q, want the removed issue tracker link wording and no enable advice", locked)
	}
	never := startRunCardMessage(workersvc.ErrRepoNeverEnabled)
	if !strings.Contains(never, "never been enabled") {
		t.Fatalf("never-enabled message = %q", never)
	}
	if errors.Is(workersvc.ErrRepoNeverEnabled, store.ErrRepoSourceLocked) {
		t.Fatal("never-enabled must not match source-locked")
	}
}
