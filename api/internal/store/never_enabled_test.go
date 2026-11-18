package store

import (
	"errors"
	"testing"
)

func TestSourceLockedMatchesNeverEnabledButNotTheReverse(t *testing.T) {
	if !errors.Is(ErrRepoSourceLocked, ErrRepoNeverEnabled) {
		t.Fatal("ErrRepoSourceLocked must match ErrRepoNeverEnabled")
	}
	if errors.Is(ErrRepoNeverEnabled, ErrRepoSourceLocked) {
		t.Fatal("ErrRepoNeverEnabled must not match ErrRepoSourceLocked")
	}
}

func TestRefuseNeverEnabledPrecedence(t *testing.T) {
	if err := RefuseNeverEnabled(false, false); err != nil {
		t.Fatalf("neither flag = %v, want nil", err)
	}
	if err := RefuseNeverEnabled(true, false); err != ErrRepoNeverEnabled {
		t.Fatalf("awaiting only = %v, want ErrRepoNeverEnabled", err)
	}
	if err := RefuseNeverEnabled(false, true); err != ErrRepoSourceLocked {
		t.Fatalf("locked only = %v, want ErrRepoSourceLocked", err)
	}
	// Both flags: the never-enabled marker wins (it is checked first).
	if err := RefuseNeverEnabled(true, true); err != ErrRepoNeverEnabled {
		t.Fatalf("both flags = %v, want ErrRepoNeverEnabled to win", err)
	}
}
