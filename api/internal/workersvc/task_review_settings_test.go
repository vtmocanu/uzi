package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestResolveTaskReview(t *testing.T) {
	cases := []struct {
		name       string
		harness    Harness
		stored     *string
		wantModel  string
		wantSource string
	}{
		{"claude unset is the SDK default", HarnessClaude, nil, "", "builtin"},
		{"claude blank is the SDK default", HarnessClaude, strp("   "), "", "builtin"},
		{"claude alias", HarnessClaude, strp("opus"), "opus", "user_default"},
		{"claude custom id is trimmed", HarnessClaude, strp("  my-custom-model  "), "my-custom-model", "user_default"},
		{"claude refuses a Codex role id", HarnessClaude, strp("gpt-6-astra"), "", "builtin"},
		{"claude refuses a control character", HarnessClaude, strp("op\x00us"), "", "builtin"},
		{"codex unset is the curated review model", HarnessCodex, nil, "gpt-6-sol", "builtin"},
		{"codex blank is the built-in", HarnessCodex, strp(""), "gpt-6-sol", "builtin"},
		{"codex curated", HarnessCodex, strp("gpt-6.1-sol"), "gpt-6.1-sol", "user_default"},
		{"codex role model outside the picker", HarnessCodex, strp("gpt-6-luna"), "gpt-6-luna", "user_default"},
		{"codex trims", HarnessCodex, strp(" gpt-5.6-sol "), "gpt-5.6-sol", "user_default"},
		{"codex custom id falls back", HarnessCodex, strp("my-custom-model"), "gpt-6-sol", "builtin"},
		{"codex Claude alias falls back", HarnessCodex, strp("opus"), "gpt-6-sol", "builtin"},
		{"codex removed model falls back", HarnessCodex, strp("gpt-4-retired"), "gpt-6-sol", "builtin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveTaskReview(tc.harness, TaskReviewInput{Model: tc.stored})
			if got.Model != tc.wantModel || got.ModelSource != tc.wantSource {
				t.Fatalf("got (%q, %q), want (%q, %q)", got.Model, got.ModelSource, tc.wantModel, tc.wantSource)
			}
			wantBuiltin := ""
			if tc.harness == HarnessCodex {
				wantBuiltin = "gpt-6-sol"
			}
			if got.BuiltinModel != wantBuiltin {
				t.Fatalf("builtin = %q, want %q", got.BuiltinModel, wantBuiltin)
			}
			if (got.StoredModel == nil) != (tc.stored == nil) || (tc.stored != nil && *got.StoredModel != *tc.stored) {
				t.Fatalf("stored echo = %v, want %v", got.StoredModel, tc.stored)
			}
		})
	}
}

func TestPredictLeadReviewUsesStoredModel(t *testing.T) {
	f := &predictFake{poolFakeStore: &poolFakeStore{}, taskReview: store.GetUserTaskReviewSettingsRow{
		TaskReviewModel: pgconv.TextOrNull("opus"), CodexTaskReviewModel: pgconv.TextOrNull("gpt-6.1-sol"),
	}}
	claude := predictLead(t, f, HarnessClaude, nil, false, true)
	if claude.Model != "opus" || claude.ModelSource != "user_default" || claude.DefaultModel != "" || claude.DefaultModelSource != "builtin" ||
		claude.LaneModel == nil || *claude.LaneModel != "opus" {
		t.Fatalf("claude review = %+v", claude)
	}
	codex := predictLead(t, f, HarnessCodex, nil, false, true)
	if codex.Model != "gpt-6.1-sol" || codex.ModelSource != "user_default" || codex.DefaultModel != "gpt-6-sol" || codex.DefaultModelSource != "builtin" {
		t.Fatalf("codex review = %+v", codex)
	}
	// A stored value the claim would refuse reads as the built-in in the view too.
	f.taskReview.CodexTaskReviewModel = pgconv.TextOrNull("my-custom-model")
	if got := predictLead(t, f, HarnessCodex, nil, false, true); got.Model != "gpt-6-sol" || got.ModelSource != "builtin" {
		t.Fatalf("invalid stored codex review = %+v", got)
	}
}

func TestPredictLeadReviewReadFailureIsAnError(t *testing.T) {
	f := &predictFake{poolFakeStore: &poolFakeStore{}, taskReviewErr: errors.New("boom")}
	if _, err := (&Service{q: f}).PredictLead(context.Background(), uuid.New(), HarnessClaude, nil, false, true); err == nil {
		t.Fatal("expected the review settings read failure to surface")
	}
}

// reviewClaim claims a Claude review run (the Codex claim path needs a bound credential and is
// covered by TestTaskReviewCodexClaimLiveDB) with the given worker capabilities.
func reviewClaim(t *testing.T, harness string, caps []string, mutate func(*fakeStore)) (ClaimPayload, *fakeStore) {
	t.Helper()
	fs := leadClaimFake(t, "task", func(r *store.Run) {
		r.Harness = harness
		r.ReviewTargetRunID = pgconv.UUID(uuid.New())
		r.LastSeq = 5
		r.ClaimGeneration = 3
	})
	if mutate != nil {
		mutate(fs)
	}
	w := worker()
	w.ProtocolCapabilities = caps
	payload, err := newOffModeService(fs, newBox(t), testParams()).Claim(context.Background(), w, nil)
	if err != nil || payload == nil {
		t.Fatalf("Claim: %v (payload=%v)", err, payload)
	}
	return *payload, fs
}

func omittedNotes(t *testing.T, fs *fakeStore) []taskReviewSettingsOmittedPayload {
	t.Helper()
	var out []taskReviewSettingsOmittedPayload
	for _, m := range fs.insertedMessages {
		var p taskReviewSettingsOmittedPayload
		if m.Kind != "status" || json.Unmarshal(m.Payload, &p) != nil || p.Event != taskReviewSettingsOmittedEvent {
			continue
		}
		if !m.ClaimGeneration.Valid || m.ClaimGeneration.Int64 != 3 {
			t.Fatalf("note is not fenced on claim_generation: %+v", m)
		}
		out = append(out, p)
	}
	return out
}

func TestReviewClaimTaskReviewModel(t *testing.T) {
	capable := []string{capability.TaskReviewSettingsV1}
	cases := []struct {
		name      string
		harness   string
		caps      []string
		claude    string
		codex     string
		want      *string
		wantNotes int
	}{
		{"capable claude with a model", "claude", capable, "opus", "", strp("opus"), 0},
		{"capable claude unset omits the field", "claude", capable, "", "", nil, 0},
		{"capable claude with a Codex id omits it", "claude", capable, "gpt-6-astra", "", nil, 0},
		{"incapable claude with a model is withheld with a notice", "claude", nil, "opus", "", nil, 1},
		{"incapable claude unset has nothing to withhold", "claude", nil, "", "", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, fs := reviewClaim(t, tc.harness, tc.caps, func(fs *fakeStore) {
				fs.taskReviewModel = pgconv.TextOrNull(tc.claude)
				fs.codexTaskReviewModel = pgconv.TextOrNull(tc.codex)
			})
			switch {
			case tc.want == nil && p.TaskReviewModel != nil:
				t.Fatalf("task_review_model = %q, want omitted", *p.TaskReviewModel)
			case tc.want != nil && (p.TaskReviewModel == nil || *p.TaskReviewModel != *tc.want):
				t.Fatalf("task_review_model = %v, want %q", p.TaskReviewModel, *tc.want)
			}
			notes := omittedNotes(t, fs)
			if len(notes) != tc.wantNotes {
				t.Fatalf("notes = %+v, want %d", notes, tc.wantNotes)
			}
			if tc.wantNotes == 1 {
				if !strings.Contains(notes[0].Text, "task review model") || !strings.Contains(notes[0].Text, "built-in") {
					t.Fatalf("note text does not name the withheld setting: %q", notes[0].Text)
				}
				if p.LastSeq != 6 || fs.lastSeqUpdated == nil || *fs.lastSeqUpdated != 6 {
					t.Fatalf("last_seq = %d stored %v, want 6", p.LastSeq, fs.lastSeqUpdated)
				}
			}
		})
	}
}

func TestReviewClaimReadFailureFailsTheClaim(t *testing.T) {
	fs := leadClaimFake(t, "task", func(r *store.Run) { r.ReviewTargetRunID = pgconv.UUID(uuid.New()) })
	fs.taskReviewErr = errors.New("boom")
	if _, err := newOffModeService(fs, newBox(t), testParams()).Claim(context.Background(), worker(), nil); err == nil {
		t.Fatal("expected the review settings read failure to fail the claim")
	}
}

func TestNonReviewClaimNeverCarriesTaskReviewModel(t *testing.T) {
	for _, kind := range []string{"issue", "task", "prompt"} {
		for _, harness := range []string{"claude"} {
			fs := leadClaimFake(t, kind, func(r *store.Run) { r.Harness = harness })
			fs.taskReviewModel = pgconv.TextOrNull("opus")
			fs.codexTaskReviewModel = pgconv.TextOrNull("gpt-6.1-sol")
			w := worker()
			w.ProtocolCapabilities = []string{capability.TaskReviewSettingsV1}
			p, err := newOffModeService(fs, newBox(t), testParams()).Claim(context.Background(), w, nil)
			if err != nil || p == nil {
				t.Fatalf("%s/%s Claim: %v", kind, harness, err)
			}
			if p.TaskReviewModel != nil {
				t.Fatalf("%s/%s claim carries task_review_model %q", kind, harness, *p.TaskReviewModel)
			}
			if n := omittedNotes(t, fs); len(n) != 0 {
				t.Fatalf("%s/%s recorded a notice: %+v", kind, harness, n)
			}
		}
	}
}
