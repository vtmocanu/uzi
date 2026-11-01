package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1324 D9: completion is Git evidence only. It may clear the Git flag of the reserved
// origin, but it never clears an API failure, and a broker-admitted completion (no claim snapshot)
// touches no cooldown at all.
func TestForgeCompletionNeverClearsAPIFailureLiveDB(t *testing.T) {
	type state struct {
		rows       int
		git, api   bool
		reservedBy bool
	}
	read := func(t *testing.T, f sourcePublicationFixture) state {
		t.Helper()
		var s state
		if err := f.e.pool.QueryRow(f.e.ctx, `SELECT count(*) FROM forge_origin_cooldowns WHERE worker_id=$1`, f.w.ID).Scan(&s.rows); err != nil {
			t.Fatal(err)
		}
		if s.rows == 0 {
			return s
		}
		if err := f.e.pool.QueryRow(f.e.ctx, `SELECT git_unverified, api_unverified, reservation_run_id IS NOT NULL
			FROM forge_origin_cooldowns WHERE worker_id=$1 ORDER BY origin LIMIT 1`, f.w.ID).Scan(&s.git, &s.api, &s.reservedBy); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, mode := range []string{"exact completion", "broker-admitted completion"} {
		t.Run(mode, func(t *testing.T) {
			f := seedForgeCompletion(t, "issue", false)
			// The shared (same-host) row also records a direct API failure.
			f.e.exec(t, "UPDATE forge_origin_cooldowns SET api_unverified=true WHERE worker_id=$1", f.w.ID)
			if mode == "broker-admitted completion" {
				f.e.exec(t, "DELETE FROM run_forge_origins WHERE run_id=$1", f.id)
			}
			result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
			if err != nil || !result.Applied {
				t.Fatalf("completion: %+v %v", result, err)
			}
			got := read(t, f)
			// Without a claim snapshot nothing can match the reservation, so it is not released either.
			if got.rows != 1 || !got.api || got.reservedBy != (mode != "exact completion") {
				t.Fatalf("completion cleared or kept wrong state: %+v", got)
			}
			// Git success evidence is what completion carries: only the exact run clears the Git flag.
			if wantGit := mode != "exact completion"; got.git != wantGit {
				t.Fatalf("git_unverified=%v want %v: %+v", got.git, wantGit, got)
			}
		})
	}
	t.Run("an unrelated API-only row is untouched", func(t *testing.T) {
		f := seedForgeCompletion(t, "issue", false)
		f.e.exec(t, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at,git_unverified,api_unverified)
			VALUES ($1,$2,'https://api.other.example',now()+interval '1 hour','transient_publication_exhausted',now(),false,true)`, f.e.userID, f.w.ID)
		result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
		if err != nil || !result.Applied {
			t.Fatalf("completion: %+v %v", result, err)
		}
		var kept, remaining int
		if err := f.e.pool.QueryRow(f.e.ctx, `SELECT count(*) FILTER (WHERE origin='https://api.other.example' AND api_unverified AND NOT git_unverified),
			count(*) FILTER (WHERE origin<>'https://api.other.example') FROM forge_origin_cooldowns WHERE worker_id=$1`, f.w.ID).Scan(&kept, &remaining); err != nil {
			t.Fatal(err)
		}
		if kept != 1 || remaining != 0 {
			t.Fatalf("API-only row kept=%d, other rows remaining=%d (the exact Git-only row clears)", kept, remaining)
		}
	})
}

// The API reservation is offered only to a worker that negotiated forge_api_success_v1, and only
// for the exact half-open reservation captured on the claim.
func TestForgeAPIOriginReservationTokenLiveDB(t *testing.T) {
	f := seedForgeCompletion(t, "issue", false)
	// Shared-row snapshot: the API reservation mirrors the Git one (bound by the fixture at version 7).
	f.e.exec(t, `UPDATE run_forge_origins SET api_origin=origin, api_reservation_incarnation=reservation_incarnation,
		api_reservation_version=reservation_version WHERE run_id=$1`, f.id)
	run, err := f.e.q.GetRunByID(f.e.ctx, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if tok := f.svc.forgeApiOriginReservation(f.e.ctx, f.w, run); tok != nil {
		t.Fatalf("a worker without forge_api_success_v1 was offered %+v", tok)
	}
	capable := f.w
	capable.ProtocolCapabilities = append(append([]string{}, f.w.ProtocolCapabilities...), ForgeAPISuccessFeature)
	tok := f.svc.forgeApiOriginReservation(f.e.ctx, capable, run)
	if tok == nil || tok.Version != 7 || tok.ObservationVersion != 1 {
		t.Fatalf("API reservation token: %+v", tok)
	}
	var want store.GetForgeApiOriginReservationRow
	want, err = f.e.q.GetForgeApiOriginReservation(f.e.ctx, store.GetForgeApiOriginReservationParams{RunID: f.id, WorkerID: f.w.ID, Generation: 1})
	if err != nil || tok.Incarnation != want.ReservationIncarnation.Bytes {
		t.Fatalf("token does not match the stored reservation: %+v vs %+v (%v)", tok, want, err)
	}
	// A newer failure advanced the observation: the half-open reservation is no longer exact.
	f.e.exec(t, "UPDATE forge_origin_cooldowns SET observation_version=observation_version+1 WHERE worker_id=$1", f.w.ID)
	if tok := f.svc.forgeApiOriginReservation(f.e.ctx, capable, run); tok != nil {
		t.Fatalf("token offered after a newer failure: %+v", tok)
	}
}
