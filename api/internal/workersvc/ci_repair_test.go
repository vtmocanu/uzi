package workersvc

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

var crT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func crEp(o CIObservation) store.CiWatchEpisode {
	return store.CiWatchEpisode{
		ObservedRunID:     pgtype.Int8{Int64: o.RunID, Valid: true},
		ObservedCreatedAt: pgtype.Timestamptz{Time: o.CreatedAt, Valid: true},
		ObservedUpdatedAt: pgtype.Timestamptz{Time: o.UpdatedAt, Valid: true},
	}
}

// TestClassifyCIObservation pins the forge-specific freshness ordering: GitHub and GitLab order
// by (CreatedAt, ID), Forgejo by ID alone, and the same execution with a later UpdatedAt is a
// re-run revision on every forge.
func TestClassifyCIObservation(t *testing.T) {
	wm := CIObservation{RunID: 100, CreatedAt: crT0, UpdatedAt: crT0.Add(time.Minute)}
	ep := crEp(wm)
	min := func(n int) time.Time { return crT0.Add(time.Duration(n) * time.Minute) }
	for _, tc := range []struct {
		name      string
		forgeType string
		obs       CIObservation
		want      CIFreshness
	}{
		{"github newer by created", "github", CIObservation{RunID: 50, CreatedAt: min(5), UpdatedAt: min(5)}, CIFresh},
		{"github higher id older created is stale", "github", CIObservation{RunID: 900, CreatedAt: min(-5), UpdatedAt: min(-5)}, CIStale},
		{"github same created higher id is fresh", "github", CIObservation{RunID: 101, CreatedAt: crT0, UpdatedAt: crT0}, CIFresh},
		{"github same created lower id is stale", "github", CIObservation{RunID: 99, CreatedAt: crT0, UpdatedAt: crT0}, CIStale},
		{"gitlab ordering matches github", "gitlab", CIObservation{RunID: 900, CreatedAt: min(-5), UpdatedAt: min(-5)}, CIStale},
		{"forgejo higher id wins despite older created", "forgejo", CIObservation{RunID: 900, CreatedAt: min(-5), UpdatedAt: min(-5)}, CIFresh},
		{"forgejo lower id loses despite newer created", "forgejo", CIObservation{RunID: 50, CreatedAt: min(5), UpdatedAt: min(5)}, CIStale},
		{"same id later updated is a revision", "github", CIObservation{RunID: 100, CreatedAt: crT0, UpdatedAt: min(2)}, CIFresh},
		{"same id later updated on forgejo", "forgejo", CIObservation{RunID: 100, CreatedAt: crT0, UpdatedAt: min(2)}, CIFresh},
		{"identical is already handled", "github", wm, CIAlreadyHandled},
		{"identical at sub-microsecond precision", "github", CIObservation{RunID: 100, CreatedAt: crT0.Add(123 * time.Nanosecond), UpdatedAt: wm.UpdatedAt.Add(456 * time.Nanosecond)}, CIAlreadyHandled},
		{"same id earlier update is stale", "github", CIObservation{RunID: 100, CreatedAt: crT0, UpdatedAt: crT0}, CIStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyCIObservation(tc.forgeType, tc.obs, ep); got != tc.want {
				t.Fatalf("classify = %v, want %v", got, tc.want)
			}
		})
	}
	if got := ClassifyCIObservation("github", CIObservation{RunID: 1}, store.CiWatchEpisode{}); got != CIFresh {
		t.Fatalf("no watermark: classify = %v, want fresh", got)
	}
}

// TestCIObservationNewer: discovery uses the same ordering as the lock-time check.
func TestCIObservationNewer(t *testing.T) {
	a := CIObservation{RunID: 200, CreatedAt: crT0, UpdatedAt: crT0}
	b := CIObservation{RunID: 300, CreatedAt: crT0.Add(-time.Hour), UpdatedAt: crT0}
	if CIObservationNewer("github", b, a) || !CIObservationNewer("github", a, b) {
		t.Fatal("github orders by creation first: 200 (created later) is newer than 300")
	}
	if !CIObservationNewer("forgejo", b, a) || CIObservationNewer("forgejo", a, b) {
		t.Fatal("forgejo orders by id alone: 300 is newer than 200")
	}
	rerun := a
	rerun.UpdatedAt = crT0.Add(time.Minute)
	if !CIObservationNewer("github", rerun, a) || CIObservationNewer("github", a, rerun) {
		t.Fatal("the same execution with a later update is the newer observation")
	}
}

func TestCILoopGuardPrecedence(t *testing.T) {
	ep := store.CiWatchEpisode{Attempts: 1, LastSignature: pgtype.Text{String: "sig", Valid: true}}
	if got := CILoopGuard(ep, "sig", 2); got != CIHaltNoProgress {
		t.Fatalf("same signature = %q", got)
	}
	if got := CILoopGuard(ep, "other", 2); got != "" {
		t.Fatalf("different signature under the cap = %q", got)
	}
	ep.Attempts = 2
	if got := CILoopGuard(ep, "sig", 2); got != CIHaltAttemptCap {
		t.Fatalf("cap must win over no-progress, got %q", got)
	}
	if got := CILoopGuard(store.CiWatchEpisode{LastSignature: pgtype.Text{String: "sig", Valid: true}}, "sig", 2); got != "" {
		t.Fatalf("zero attempts never trips no-progress, got %q", got)
	}
}

// TestCIWatchFingerprintCoversEveryField: changing any configuration field, or the default
// branch, or the resolved body, changes the digest; an untouched copy does not.
func TestCIWatchFingerprintCoversEveryField(t *testing.T) {
	secret := uuid.New()
	base := store.RunSchedule{
		ID: uuid.New(), UserID: uuid.New(), RepoID: uuid.New(), Target: "ci_watch", CiWorkflows: []byte(`["a"]`),
		Origin: "default", CatalogSlug: pgtype.Text{String: "ci-repair", Valid: true}, Prompt: pgtype.Text{String: "p", Valid: true},
		Guidance: pgtype.Text{String: "g", Valid: true}, AutoApprove: false, WaitOnLimit: true,
		MrReworkEnabled: pgtype.Bool{Bool: true, Valid: true}, Model: pgtype.Text{String: "m", Valid: true}, OverrideSubagentModel: true,
		Harness: pgtype.Text{String: "claude", Valid: true}, CredentialOverrideMode: pgtype.Text{String: "pinned", Valid: true},
		CredentialOverrideSecretID: pgtype.UUID{Bytes: secret, Valid: true}, Enabled: true,
	}
	branch := pgtype.Text{String: "main", Valid: true}
	want := CIWatchFingerprint(base, branch, "body")
	if CIWatchFingerprint(base, branch, "body") != want {
		t.Fatal("fingerprint is not deterministic")
	}
	// Fields that must NOT matter: bookkeeping the fire itself rewrites.
	same := base
	same.LastFire = []byte(`{"x":1}`)
	same.NextFireAt = pgtype.Timestamptz{Time: crT0, Valid: true}
	same.UpdatedAt = pgtype.Timestamptz{Time: crT0, Valid: true}
	if CIWatchFingerprint(same, branch, "body") != want {
		t.Fatal("last_fire/next_fire_at/updated_at must not change the fingerprint")
	}
	mut := map[string]func(*store.RunSchedule){
		"repo_id":      func(s *store.RunSchedule) { s.RepoID = uuid.New() },
		"target":       func(s *store.RunSchedule) { s.Target = "prompt" },
		"ci_workflows": func(s *store.RunSchedule) { s.CiWorkflows = []byte(`["a","b"]`) },
		"origin":       func(s *store.RunSchedule) { s.Origin = "user" },
		"catalog":      func(s *store.RunSchedule) { s.CatalogSlug = pgtype.Text{} },
		"prompt":       func(s *store.RunSchedule) { s.Prompt = pgtype.Text{String: "q", Valid: true} },
		"guidance":     func(s *store.RunSchedule) { s.Guidance = pgtype.Text{} },
		"auto":         func(s *store.RunSchedule) { s.AutoApprove = true },
		"wait":         func(s *store.RunSchedule) { s.WaitOnLimit = false },
		"mr_rework":    func(s *store.RunSchedule) { s.MrReworkEnabled = pgtype.Bool{} },
		"model":        func(s *store.RunSchedule) { s.Model = pgtype.Text{} },
		"subagent":     func(s *store.RunSchedule) { s.OverrideSubagentModel = false },
		"harness":      func(s *store.RunSchedule) { s.Harness = pgtype.Text{String: "codex", Valid: true} },
		"cred mode":    func(s *store.RunSchedule) { s.CredentialOverrideMode = pgtype.Text{String: "auto", Valid: true} },
		"cred secret":  func(s *store.RunSchedule) { s.CredentialOverrideSecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true} },
		"enabled":      func(s *store.RunSchedule) { s.Enabled = false },
	}
	for name, m := range mut {
		s := base
		m(&s)
		if CIWatchFingerprint(s, branch, "body") == want {
			t.Errorf("changing %s did not change the fingerprint", name)
		}
	}
	if CIWatchFingerprint(base, pgtype.Text{String: "trunk", Valid: true}, "body") == want {
		t.Error("changing the default branch did not change the fingerprint")
	}
	if CIWatchFingerprint(base, pgtype.Text{}, "body") == want {
		t.Error("clearing the default branch did not change the fingerprint")
	}
	if CIWatchFingerprint(base, branch, "other body") == want {
		t.Error("changing the resolved body did not change the fingerprint")
	}
	if !reflect.DeepEqual(CIRepairConfigFloor, []string{".gitlab-ci.yml", ".gitlab/**", "**/*.gitlab-ci.yml", ".github/workflows/**", ".forgejo/workflows/**"}) {
		t.Fatalf("repair floor = %v", CIRepairConfigFloor)
	}
}

// TestCreateCIRepairRunFailsClosedWithoutTransaction: no tx beginner (or a non-live store)
// refuses rather than inserting a run without the episode locks.
func TestCreateCIRepairRunFailsClosedWithoutTransaction(t *testing.T) {
	svc := New(&fakeStore{}, newBox(t), testParams())
	_, err := svc.CreateCIRepairRun(t.Context(), CIRepairRunParams{UserID: uuid.New(), RepoID: uuid.New(), ScheduleID: uuid.New()})
	if err == nil || err != errCIRepairRequiresTx {
		t.Fatalf("err = %v, want errCIRepairRequiresTx", err)
	}
	if err := svc.TransitionCIWatchEpisode(t.Context(), CIWatchTransition{}); err != errCIRepairRequiresTx {
		t.Fatalf("transition err = %v, want errCIRepairRequiresTx", err)
	}
}
