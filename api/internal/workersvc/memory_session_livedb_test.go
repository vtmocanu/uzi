package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
)

func TestMemoryParkPreservesSessionLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previous string
		session  *string
		want     string
	}{
		{"supplied replaces previous", "previous-session", memorySessionPointer("latest-session"), "latest-session"},
		{"supplied initializes session", "", memorySessionPointer("latest-session"), "latest-session"},
		{"absent preserves previous", "previous-session", nil, "previous-session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMemoryFixture(t)
			f.reserve(t, f.b)
			if tc.previous != "" {
				f.e.exec("UPDATE runs SET session_id=$2 WHERE id=$1", f.b.RunID, tc.previous)
			}
			cause := recoveryCauseWorkerMemoryPressure
			req := StateRequest{State: "recovery_wait", RecoveryCause: &cause,
				ClaimGeneration: &f.b.ClaimGeneration, RegisterNonce: f.b.RegisterNonce,
				MemoryEpisode: &f.b.MemoryEpisode, SessionID: tc.session}
			held, applied, err := f.s.SetState(f.e.ctx, f.w, f.b.RunID, req)
			if err != nil || !applied {
				t.Fatalf("park applied=%v err=%v", applied, err)
			}
			if held.SessionID != pgconv.Text(tc.want) || f.run(t).SessionID != pgconv.Text(tc.want) {
				t.Fatalf("park session=%+v, want %q", held.SessionID, tc.want)
			}
			// A duplicate or stale reporter cannot replace the captured session.
			req.SessionID = memorySessionPointer("stale-replacement")
			replay, applied, err := f.s.SetState(f.e.ctx, f.w, f.b.RunID, req)
			if err != nil || applied || replay.SessionID != pgconv.Text(tc.want) {
				t.Fatalf("replay session=%+v applied=%v err=%v", replay.SessionID, applied, err)
			}
			for _, stale := range []string{"nonce", "generation", "episode"} {
				t.Run(stale, func(t *testing.T) {
					changed := req
					n := f.b.ClaimGeneration + 1
					e := f.b.MemoryEpisode + 1
					switch stale {
					case "nonce":
						changed.RegisterNonce = "stale-nonce"
					case "generation":
						changed.ClaimGeneration = &n
					case "episode":
						changed.MemoryEpisode = &e
					}
					_, applied, err := f.s.SetState(f.e.ctx, f.w, f.b.RunID, changed)
					if err == nil || applied {
						t.Fatalf("stale %s applied=%v err=%v", stale, applied, err)
					}
					if f.run(t).SessionID != pgconv.Text(tc.want) {
						t.Fatal("stale park replaced session")
					}
				})
			}
		})
	}
}

func memorySessionPointer(s string) *string { return &s }
