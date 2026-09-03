package handler

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// normalizeSchedulePause is the read-time expiry rule (PRD #1093 M2): paused AND
// (until NULL OR until > now). A pure function of its three args, so this table is
// clock-independent.
func TestNormalizeSchedulePause(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	ts := func(offset time.Duration) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: now.Add(offset), Valid: true}
	}

	tests := []struct {
		name      string
		paused    bool
		until     pgtype.Timestamptz
		wantPause bool
		wantUntil *time.Time // nil ⇒ expect Until nil
	}{
		{
			name:      "not paused, no until",
			paused:    false,
			until:     pgtype.Timestamptz{},
			wantPause: false,
			wantUntil: nil,
		},
		{
			name:      "paused, no until = indefinite",
			paused:    true,
			until:     pgtype.Timestamptz{},
			wantPause: true,
			wantUntil: nil,
		},
		{
			name:      "paused, future until = still paused, until carried",
			paused:    true,
			until:     ts(time.Hour),
			wantPause: true,
			wantUntil: func() *time.Time { u := now.Add(time.Hour); return &u }(),
		},
		{
			name:      "paused, expired until = not paused, until dropped",
			paused:    true,
			until:     ts(-time.Hour),
			wantPause: false,
			wantUntil: nil,
		},
		{
			name:      "paused, until exactly now = not paused (not After)",
			paused:    true,
			until:     ts(0),
			wantPause: false,
			wantUntil: nil,
		},
		{
			name:      "not paused but future until = not paused",
			paused:    false,
			until:     ts(time.Hour),
			wantPause: false,
			wantUntil: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeSchedulePause(tt.paused, tt.until, now)
			if got.Paused != tt.wantPause {
				t.Errorf("Paused = %v, want %v", got.Paused, tt.wantPause)
			}
			switch {
			case tt.wantUntil == nil && got.Until != nil:
				t.Errorf("Until = %v, want nil", *got.Until)
			case tt.wantUntil != nil && got.Until == nil:
				t.Errorf("Until = nil, want %v", *tt.wantUntil)
			case tt.wantUntil != nil && !got.Until.Equal(*tt.wantUntil):
				t.Errorf("Until = %v, want %v", *got.Until, *tt.wantUntil)
			}
		})
	}
}
