package workersvc

import (
	"context"
	"testing"
	"time"
)

// TestSweepExpiresStalledUploadsWhenWindowSet: with a positive
// RecoveryUploadRetryWindow the sweep calls ExpireStalledUploads ONCE, passes the
// configured window as the interval, and reports the row count in RecoveryStalled — the
// live-consumer wiring of UZI_RECOVERY_UPLOAD_RETRY_WINDOW (PRD #1296 D3/D4).
func TestSweepExpiresStalledUploadsWhenWindowSet(t *testing.T) {
	fs := &fakeStore{stalledUploadsRows: 3}
	p := testParams()
	p.RecoveryUploadRetryWindow = 24 * time.Hour
	svc := New(fs, newBox(t), p)

	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.RecoveryStalled != 3 {
		t.Fatalf("RecoveryStalled = %d, want 3 (the ExpireStalledUploads row count)", res.RecoveryStalled)
	}
	if len(fs.expireStalledWindows) != 1 {
		t.Fatalf("ExpireStalledUploads called %d times, want exactly 1 per sweep", len(fs.expireStalledWindows))
	}
	got := fs.expireStalledWindows[0]
	if !got.Valid {
		t.Fatal("ExpireStalledUploads called with an invalid interval; the window must be passed as a valid pgtype.Interval")
	}
	if want := (24 * time.Hour).Microseconds(); got.Microseconds != want {
		t.Fatalf("ExpireStalledUploads window = %d us, want %d us (the configured RecoveryUploadRetryWindow)", got.Microseconds, want)
	}
}

// TestSweepSkipsStalledUploadsWhenWindowDisabled: a non-positive window DISABLES the pass
// (the "0 disables" convention shared with ChatIdleTimeout/ProposalConfirmStuckTimeout) —
// ExpireStalledUploads is never called and RecoveryStalled stays 0, so an operator can turn
// the timed transition off and a Params literal that omits the field is off by default.
func TestSweepSkipsStalledUploadsWhenWindowDisabled(t *testing.T) {
	for _, window := range []time.Duration{0, -time.Hour} {
		fs := &fakeStore{stalledUploadsRows: 5}
		p := testParams()
		p.RecoveryUploadRetryWindow = window
		svc := New(fs, newBox(t), p)

		res, err := svc.Sweep(context.Background())
		if err != nil {
			t.Fatalf("Sweep(window=%s): %v", window, err)
		}
		if res.RecoveryStalled != 0 {
			t.Fatalf("Sweep(window=%s) RecoveryStalled = %d, want 0 (a non-positive window disables the pass)", window, res.RecoveryStalled)
		}
		if len(fs.expireStalledWindows) != 0 {
			t.Fatalf("Sweep(window=%s) called ExpireStalledUploads %d times, want 0 — a disabled pass must never touch the DB", window, len(fs.expireStalledWindows))
		}
	}
}
