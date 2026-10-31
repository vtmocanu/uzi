package workersvc

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestValidatePublicationPark(t *testing.T) {
	good := pubParkReport(3, "git", "push")
	if err := validatePublicationPark(good); err != nil {
		t.Fatalf("good report: %v", err)
	}
	if err := validatePublicationPark(fpForgeParkReport()); err != nil {
		t.Fatalf("a report without publication fields is not validated here: %v", err)
	}
	mut := func(f func(*StateRequest)) StateRequest {
		r := pubParkReport(3, "api", "mr_create")
		f(&r)
		return r
	}
	for name, req := range map[string]StateRequest{
		"unknown phase":      mut(func(r *StateRequest) { r.RecoveryPhase = strPtr("clone") }),
		"unknown surface":    mut(func(r *StateRequest) { r.PublicationSurface = strPtr("ssh") }),
		"unknown step":       mut(func(r *StateRequest) { r.PublicationStep = strPtr("fetch") }),
		"missing surface":    mut(func(r *StateRequest) { r.PublicationSurface = nil }),
		"missing step":       mut(func(r *StateRequest) { r.PublicationStep = nil }),
		"phase on a limit":   mut(func(r *StateRequest) { r.RecoveryCause = strPtr("provider_outage") }),
		"phase on a running": mut(func(r *StateRequest) { r.State = "running" }),
		"no generation":      mut(func(r *StateRequest) { r.ClaimGeneration = nil }),
		"short tip":          mut(func(r *StateRequest) { r.HandoffCheckpointTip = strPtr("abc") }),
		"upper-case tip":     mut(func(r *StateRequest) { r.HandoffCheckpointTip = strPtr("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") }),
		"fields without phase": {State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1),
			PublicationSurface: strPtr("git"), PublicationStep: strPtr("push")},
	} {
		if err := validatePublicationPark(req); !errors.Is(err, ErrInvalidState) {
			t.Errorf("%s: err=%v, want ErrInvalidState", name, err)
		}
	}
	tip := mut(func(r *StateRequest) { r.HandoffCheckpointTip = strPtr(pubHeadC) })
	if err := validatePublicationPark(tip); err != nil {
		t.Errorf("valid tip: %v", err)
	}
}

func TestPublicationHandoffRequiresThePublishedTip(t *testing.T) {
	run := store.Run{ID: uuid.New(), CheckpointTip: pgtype.Text{String: pubHeadC, Valid: true}}
	with := func(tip string) StateRequest { return StateRequest{HandoffCheckpointTip: &tip} }
	if h := publicationHandoff(run, StateRequest{}); h != nil {
		t.Fatalf("no tip reported: %s", h)
	}
	if h := publicationHandoff(run, with(pubHeadB)); h != nil {
		t.Fatalf("a tip that is not the published one was accepted: %s", h)
	}
	if h := publicationHandoff(store.Run{ID: run.ID}, with(pubHeadC)); h != nil {
		t.Fatalf("a run with no published tip accepted a handoff: %s", h)
	}
	var got map[string]string
	if err := json.Unmarshal(publicationHandoff(run, with(pubHeadC)), &got); err != nil || got["checkpoint_tip"] != pubHeadC || len(got) != 1 {
		t.Fatalf("handoff=%v err=%v", got, err)
	}
}

func TestPublicationParkRefusalByCapability(t *testing.T) {
	capable := store.Worker{ProtocolCapabilities: []string{capability.PublicationResumeV1}}
	legacy := store.Worker{ProtocolCapabilities: []string{capability.RecoveryArchiveV1}}
	typed := pubParkReport(1, "git", "push")
	if err := publicationParkRefusal(capable, fpForgeParkReport()); !errors.Is(err, ErrPublicationEnrolled) {
		t.Fatalf("capable untyped: %v", err)
	}
	if err := publicationParkRefusal(capable, typed); err != nil {
		t.Fatalf("capable typed: %v", err)
	}
	if err := publicationParkRefusal(legacy, fpForgeParkReport()); err != nil {
		t.Fatalf("incapable untyped is redirected, not refused: %v", err)
	}
}

func TestActiveSnapshotPublicationResumeDecode(t *testing.T) {
	run := uuid.New()
	var snap ActiveSnapshot
	body := `{"active":[],"publication_resume":[{"run_id":"` + run.String() + `","claim_generation":2,"enrollment_seq":3,"candidate_seq":1}]}`
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.PublicationResume) != 1 || snap.PublicationResume[0].EnrollmentSeq != 3 || snap.publicationResumeMalformed {
		t.Fatalf("decoded=%+v", snap)
	}
	var bad ActiveSnapshot
	if err := json.Unmarshal([]byte(`{"active":[],"publication_resume":"nope","finalize_resume":[]}`), &bad); err != nil {
		t.Fatalf("a malformed optional list must not fail the snapshot: %v", err)
	}
	if bad.PublicationResume != nil || !bad.publicationResumeMalformed {
		t.Fatalf("malformed list kept: %+v", bad)
	}
}

func TestWithoutPublicationResume(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	svc := New(nil, nil, Params{ActiveSnapshotMaxEntries: 4})
	in := validatedFinalizeResume{ids: []uuid.UUID{a, b}, generations: []int64{1, 2}}
	entry := func(id uuid.UUID) PublicationResumeEntry {
		return PublicationResumeEntry{RunID: id.String(), ClaimGeneration: 1, EnrollmentSeq: 1}
	}
	out, ok := svc.withoutPublicationResume(store.Worker{}, &ActiveSnapshot{PublicationResume: []PublicationResumeEntry{entry(a)}}, in, true)
	if !ok || len(out.ids) != 1 || out.ids[0] != b || out.generations[0] != 2 {
		t.Fatalf("listed run kept: %+v ok=%v", out, ok)
	}
	out, ok = svc.withoutPublicationResume(store.Worker{}, &ActiveSnapshot{PublicationResume: []PublicationResumeEntry{entry(a), entry(b)}}, in, true)
	if ok || len(out.ids) != 0 {
		t.Fatalf("every run listed, finalize list must be empty: %+v ok=%v", out, ok)
	}
	// An invalid or malformed list is ignored, leaving the finalize list as it was.
	for name, snap := range map[string]*ActiveSnapshot{
		"nil":       nil,
		"empty":     {},
		"bad uuid":  {PublicationResume: []PublicationResumeEntry{{RunID: "x", EnrollmentSeq: 1}}},
		"bad seq":   {PublicationResume: []PublicationResumeEntry{{RunID: a.String(), EnrollmentSeq: 0}}},
		"malformed": {publicationResumeMalformed: true},
	} {
		out, ok = svc.withoutPublicationResume(store.Worker{}, snap, in, true)
		if !ok || len(out.ids) != 2 {
			t.Errorf("%s: finalize list changed: %+v ok=%v", name, out, ok)
		}
	}
}
