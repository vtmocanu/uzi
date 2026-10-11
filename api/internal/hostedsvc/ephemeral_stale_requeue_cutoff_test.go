package hostedsvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// argRecorder is a store.DBTX that records each Query's arguments and returns no rows, so a
// provision pass can be driven without a database.
type argRecorder struct {
	calls map[string][]any
}

func (r *argRecorder) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (r *argRecorder) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	for _, name := range []string{"ListUnplaceableQueuedRunsForEphemeral", "ListSaturationQueuedRunsForEphemeral"} {
		if strings.Contains(sql, "-- name: "+name+" ") {
			r.calls[name] = args
		}
	}
	return emptyRows{}, nil
}

func (r *argRecorder) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

// emptyRows yields no rows. It embeds pgx.Rows (nil) so only the methods the generated
// :many loop calls (Next, Close, Err) are implemented; any other call would panic loudly.
type emptyRows struct{ pgx.Rows }

func (emptyRows) Close()     {}
func (emptyRows) Err() error { return nil }
func (emptyRows) Next() bool { return false }

// TestProvisionPassPassesStaleRequeueCutoffs pins the #2705 wiring: both provisioning demand
// queries receive the grace cutoff (now - WorkerStaleRequeueGrace, invalid at grace 0) and the
// claim affinity cutoff (now - WorkerAffinityCeiling), the first two positional arguments in
// the generated calls. The LiveDB tests pass these parameters directly, so only this test
// fails when ProvisionPass stops setting one.
func TestProvisionPassPassesStaleRequeueCutoffs(t *testing.T) {
	now := time.Date(2026, 10, 10, 17, 11, 7, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		grace     time.Duration
		wantStale pgtype.Timestamptz
	}{
		{"grace on", 10 * time.Minute, pgtype.Timestamptz{Time: now.Add(-10 * time.Minute), Valid: true}},
		{"grace off", 0, pgtype.Timestamptz{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &argRecorder{calls: map[string][]any{}}
			p := NewEphemeralProvisioner(nil, store.New(rec), nil, &dockerSettingsProbe{enabled: true}, EphemeralConfig{
				WorkerStaleRequeueGrace: tc.grace,
				WorkerAffinityCeiling:   2 * time.Hour,
			})
			p.now = func() time.Time { return now }
			if _, err := p.ProvisionPass(context.Background()); err != nil {
				t.Fatalf("ProvisionPass: %v", err)
			}
			wantCeiling := pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true}
			for _, name := range []string{"ListUnplaceableQueuedRunsForEphemeral", "ListSaturationQueuedRunsForEphemeral"} {
				args, ok := rec.calls[name]
				if !ok {
					t.Fatalf("%s was not queried", name)
				}
				if got, _ := args[0].(pgtype.Timestamptz); got != tc.wantStale {
					t.Errorf("%s stale_requeue_cutoff = %+v, want %+v", name, got, tc.wantStale)
				}
				if got, _ := args[1].(pgtype.Timestamptz); got != wantCeiling {
					t.Errorf("%s affinity_cutoff = %+v, want %+v", name, got, wantCeiling)
				}
			}
		})
	}
}
