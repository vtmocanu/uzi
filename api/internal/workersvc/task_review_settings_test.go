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

func TestResolveTaskReviewEffort(t *testing.T) {
	cases := []struct {
		name          string
		harness       Harness
		review        *string
		worker        *string
		wantLevel     *string // nil = no level sent (Runtime default)
		wantSource    string
		wantInherited bool
		wantDefault   string // the worker resolution a reset selects
	}{
		{"inherits the worker level", HarnessClaude, nil, strp("high"), strp("high"), "user_default", true, "high"},
		{"inherits the built-in medium when the worker effort is unset", HarnessClaude, nil, nil, strp("medium"), "builtin", true, "medium"},
		{"inherits a Runtime default worker effort: no level", HarnessClaude, nil, strp("runtime"), nil, "user_default", true, ""},
		{"explicit runtime sends no level even with a worker level", HarnessClaude, strp("runtime"), strp("high"), nil, "user_default", false, "high"},
		{"corrupt review effort is the built-in medium, sent", HarnessClaude, strp("HI gh"), strp("low"), strp("medium"), "builtin", false, "low"},
		{"explicit level overrides the worker effort", HarnessClaude, strp("max"), strp("low"), strp("max"), "user_default", false, "low"},
		{"blank review effort inherits", HarnessClaude, strp("   "), strp("xhigh"), strp("xhigh"), "user_default", true, "xhigh"},
		{"codex inherits the codex worker level", HarnessCodex, nil, strp("low"), strp("low"), "user_default", true, "low"},
		{"codex unset inherits the codex built-in", HarnessCodex, nil, nil, strp("medium"), "builtin", true, "medium"},
		{"corrupt worker effort inherits as the built-in", HarnessClaude, nil, strp("bogus"), strp("medium"), "builtin", true, "medium"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveTaskReview(tc.harness, TaskReviewInput{Effort: tc.review, WorkerEffort: tc.worker})
			lvl := got.EffortLevelPtr()
			if (lvl == nil) != (tc.wantLevel == nil) || (lvl != nil && *lvl != *tc.wantLevel) {
				t.Fatalf("level = %v, want %v", lvl, tc.wantLevel)
			}
			if got.Effort.Source != tc.wantSource || got.EffortInherited != tc.wantInherited {
				t.Fatalf("source/inherited = %q/%v, want %q/%v", got.Effort.Source, got.EffortInherited, tc.wantSource, tc.wantInherited)
			}
			if got.WorkerEffort.Level != tc.wantDefault {
				t.Fatalf("worker resolution level = %q, want %q", got.WorkerEffort.Level, tc.wantDefault)
			}
			if (got.StoredEffort == nil) != (tc.review == nil) {
				t.Fatalf("stored echo = %v, want %v", got.StoredEffort, tc.review)
			}
		})
	}
}

func TestPredictLeadReviewEffort(t *testing.T) {
	f := &predictFake{poolFakeStore: &poolFakeStore{}, taskReview: store.GetUserTaskReviewSettingsRow{
		DefaultEffort: pgconv.TextOrNull("high"), CodexTaskReviewEffort: pgconv.TextOrNull("runtime"),
	}}
	claude := predictLead(t, f, HarnessClaude, nil, false, true)
	if claude.Effort == nil || *claude.Effort != "high" || claude.EffortSource != "user_default" || !claude.ReviewEffortInherited ||
		claude.UserEffort != nil || claude.ReviewWorkerEffort.Level != "high" {
		t.Fatalf("claude review effort = %+v", claude)
	}
	codex := predictLead(t, f, HarnessCodex, nil, false, true)
	if codex.Effort != nil || codex.ReviewEffort.Mode != "runtime_default" || codex.ReviewEffortInherited || codex.UserEffort == nil || *codex.UserEffort != "runtime" {
		t.Fatalf("codex review effort = %+v", codex)
	}
}

// TestPredictLeadReviewInheritsTheHarnessWorkerEffort pins which stored worker-effort column each
// harness inherits from: Claude default_effort, Codex default_codex_effort (never the other lane).
func TestPredictLeadReviewInheritsTheHarnessWorkerEffort(t *testing.T) {
	f := &predictFake{poolFakeStore: &poolFakeStore{}, taskReview: store.GetUserTaskReviewSettingsRow{
		DefaultEffort: pgconv.TextOrNull("max"), DefaultCodexEffort: pgconv.TextOrNull("low"),
	}}
	if got := predictLead(t, f, HarnessClaude, nil, false, true); got.Effort == nil || *got.Effort != "max" || !got.ReviewEffortInherited {
		t.Fatalf("claude review effort = %+v, want inherited max", got)
	}
	if got := predictLead(t, f, HarnessCodex, nil, false, true); got.Effort == nil || *got.Effort != "low" || !got.ReviewEffortInherited ||
		got.ReviewWorkerEffort.Level != "low" {
		t.Fatalf("codex review effort = %+v, want inherited low", got)
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
		{"incapable claude unset with a Runtime default worker effort has nothing to withhold", "claude", nil, "", "", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, fs := reviewClaim(t, tc.harness, tc.caps, func(fs *fakeStore) {
				// The owner's worker effort is Runtime default so this model test sends no effort: an
				// unset owner would inherit the built-in medium (TestReviewClaimTaskReviewEffort).
				fs.defaultEffort = pgconv.TextOrNull("runtime")
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
				if !strings.Contains(notes[0].Text, "task review model") || !strings.Contains(notes[0].Text, "runs on its built-in model and sends no effort") {
					t.Fatalf("note text does not name the withheld setting: %q", notes[0].Text)
				}
				if p.LastSeq != 6 || fs.lastSeqUpdated == nil || *fs.lastSeqUpdated != 6 {
					t.Fatalf("last_seq = %d stored %v, want 6", p.LastSeq, fs.lastSeqUpdated)
				}
			}
		})
	}
}

func TestReviewClaimTaskReviewEffort(t *testing.T) {
	capable := []string{capability.TaskReviewSettingsV1}
	cases := []struct {
		name        string
		caps        []string
		review      string
		worker      string
		runOverride string
		want        *string
		wantNote    string // "" = no notice; else the exact withheld list text
	}{
		{"unset inherits the worker level", capable, "", "high", "", strp("high"), ""},
		{"unset inherits the built-in medium", capable, "", "", "", strp("medium"), ""},
		{"unset inherits a Runtime default worker: omitted", capable, "", "runtime", "", nil, ""},
		{"explicit runtime is omitted with a worker level", capable, "runtime", "high", "", nil, ""},
		{"corrupt review effort sends the built-in medium", capable, "HI gh", "low", "", strp("medium"), ""},
		{"explicit level overrides the worker effort", capable, "max", "low", "", strp("max"), ""},
		{"blank review effort inherits", capable, "  ", "xhigh", "", strp("xhigh"), ""},
		{"a run override effort never changes the review effort", capable, "", "low", "max", strp("low"), ""},
		{"a run override effort does not beat an explicit review effort", capable, "medium", "low", "max", strp("medium"), ""},
		{"incapable with explicit runtime: nothing withheld, no notice", nil, "runtime", "high", "", nil, ""},
		{"incapable with a Runtime default worker: no notice", nil, "", "runtime", "", nil, ""},
		{"incapable inheriting the built-in medium is withheld with a notice", nil, "", "", "", nil, "task review effort"},
		{"incapable with an explicit level is withheld with a notice", nil, "high", "", "", nil, "task review effort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, fs := reviewClaim(t, "claude", tc.caps, func(fs *fakeStore) {
				fs.taskReviewEffort = pgconv.TextOrNull(tc.review)
				fs.defaultEffort = pgconv.TextOrNull(tc.worker)
				if tc.runOverride != "" {
					fs.claimRun.Effort = pgconv.TextOrNull(tc.runOverride)
					fs.claimRun.EffortSource = pgconv.TextOrNull("run_override")
				}
			})
			if (p.TaskReviewEffort == nil) != (tc.want == nil) || (p.TaskReviewEffort != nil && *p.TaskReviewEffort != *tc.want) {
				t.Fatalf("task_review_effort = %v, want %v", p.TaskReviewEffort, tc.want)
			}
			notes := omittedNotes(t, fs)
			if tc.wantNote == "" {
				if len(notes) != 0 {
					t.Fatalf("unexpected notice: %+v", notes)
				}
				return
			}
			if len(notes) != 1 || !strings.Contains(notes[0].Text, "("+tc.wantNote+")") {
				t.Fatalf("notes = %+v, want one naming exactly %q", notes, tc.wantNote)
			}
		})
	}
}

func TestReviewClaimWithholdsModelAndEffortTogether(t *testing.T) {
	_, fs := reviewClaim(t, "claude", nil, func(fs *fakeStore) { fs.taskReviewModel = pgconv.TextOrNull("opus") })
	notes := omittedNotes(t, fs)
	if len(notes) != 1 || !strings.Contains(notes[0].Text, "(task review model, task review effort)") {
		t.Fatalf("notes = %+v, want one naming both settings", notes)
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
		// Claude only: this fake claims no Codex run (no Codex binding). The isReviewRun gate does not
		// depend on the harness, so a Codex non-review claim is not separately exercised here.
		for _, harness := range []string{"claude"} {
			fs := leadClaimFake(t, kind, func(r *store.Run) { r.Harness = harness })
			fs.taskReviewModel = pgconv.TextOrNull("opus")
			fs.codexTaskReviewModel = pgconv.TextOrNull("gpt-6.1-sol")
			fs.taskReviewEffort = pgconv.TextOrNull("high")
			w := worker()
			w.ProtocolCapabilities = []string{capability.TaskReviewSettingsV1}
			p, err := newOffModeService(fs, newBox(t), testParams()).Claim(context.Background(), w, nil)
			if err != nil || p == nil {
				t.Fatalf("%s/%s Claim: %v", kind, harness, err)
			}
			if p.TaskReviewModel != nil {
				t.Fatalf("%s/%s claim carries task_review_model %q", kind, harness, *p.TaskReviewModel)
			}
			if p.TaskReviewEffort != nil {
				t.Fatalf("%s/%s claim carries task_review_effort %q", kind, harness, *p.TaskReviewEffort)
			}
			if n := omittedNotes(t, fs); len(n) != 0 {
				t.Fatalf("%s/%s recorded a notice: %+v", kind, harness, n)
			}
		}
	}
}
