package workersvc

import (
	"context"
	"testing"
	"time"
)

// TestSweepExpiresReadyCapturesWhenRetentionSet: with a positive RecoveryReadyRetention the
// sweep calls ExpireReadyCaptures ONCE, passes a valid clock as the `now` bound, and reports
// the row count in RecoveryExpired — the live enforcement wiring of UZI_RECOVERY_READY_RETENTION
// (PRD #1296 D4). The window itself is baked into expires_at at capture time; this proves the
// enforcement sweep runs.
func TestSweepExpiresReadyCapturesWhenRetentionSet(t *testing.T) {
	fs := &fakeStore{expiredCapturesRows: 4}
	p := testParams()
	p.RecoveryReadyRetention = 7 * 24 * time.Hour
	svc := New(fs, newBox(t), p)

	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.RecoveryExpired != 4 {
		t.Fatalf("RecoveryExpired = %d, want 4 (the ExpireReadyCaptures row count)", res.RecoveryExpired)
	}
	if len(fs.expireReadyNows) != 1 {
		t.Fatalf("ExpireReadyCaptures called %d times, want exactly 1 per sweep", len(fs.expireReadyNows))
	}
	if !fs.expireReadyNows[0].Valid {
		t.Fatal("ExpireReadyCaptures called with an invalid timestamp; the sweep must pass a valid clock as `now`")
	}
}

// TestSweepSkipsReadyCapturesWhenRetentionDisabled: a non-positive retention DISABLES the pass
// (the "0 disables" convention shared with RecoveryUploadRetryWindow / ChatIdleTimeout) —
// ExpireReadyCaptures is never called and RecoveryExpired stays 0, so an operator can turn the
// retention enforcement off and a Params literal that omits the field is off by default.
func TestSweepSkipsReadyCapturesWhenRetentionDisabled(t *testing.T) {
	for _, retention := range []time.Duration{0, -time.Hour} {
		fs := &fakeStore{expiredCapturesRows: 5}
		p := testParams()
		p.RecoveryReadyRetention = retention
		svc := New(fs, newBox(t), p)

		res, err := svc.Sweep(context.Background())
		if err != nil {
			t.Fatalf("Sweep(retention=%s): %v", retention, err)
		}
		if res.RecoveryExpired != 0 {
			t.Fatalf("Sweep(retention=%s) RecoveryExpired = %d, want 0 (a non-positive retention disables the pass)", retention, res.RecoveryExpired)
		}
		if len(fs.expireReadyNows) != 0 {
			t.Fatalf("Sweep(retention=%s) called ExpireReadyCaptures %d times, want 0 — a disabled pass must never touch the DB", retention, len(fs.expireReadyNows))
		}
	}
}
