package schedsvc

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func ciZIP(t *testing.T, texts ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for i, text := range texts {
		w, err := z.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("entry-%d.txt", i), Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func ciArtifactSchedule(h *ciHarness) store.RunSchedule {
	s := h.schedule()
	s.CiArtifacts = []byte(`["logs*"]`)
	return s
}
func ciArtifact(r forge.WorkflowRun, id int64) forge.Artifact {
	return forge.Artifact{ID: id, Name: "logs", RunID: r.ID, RepositoryID: r.RepositoryID, SHA: r.SHA}
}
func seedCIArtifact(t *testing.T, h *ciHarness, r forge.WorkflowRun) {
	h.cf.artifacts = []forge.Artifact{ciArtifact(r, 5)}
	data := ciZIP(t, "evidence")
	h.cf.download = func(ctx context.Context, id, max int64) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Minute || time.Until(deadline) <= 0 {
			t.Fatal("missing collection deadline")
		}
		return data, nil
	}
}
func TestCIArtifactsFirstAttemptAndProvenance(t *testing.T) {
	for _, tc := range []struct {
		name      string
		attempt   *int64
		wantCalls int
	}{{"first", ciAttempt(1), 1}, {"unknown", nil, 0}, {"rerun", ciAttempt(2), 0}} {
		t.Run(tc.name, func(t *testing.T) {
			r := ciRed("wf", 100, 0)
			r.RunAttempt = tc.attempt
			h := newCIHarness(t, r)
			seedCIArtifact(t, h, r)
			out := h.fire(t, ciArtifactSchedule(h))
			if len(out.Started) != 1 || h.cf.artifactCalls != tc.wantCalls {
				t.Fatalf("out=%+v lists=%d", out, h.cf.artifactCalls)
			}
			p := h.runs.ciRepairs[0]
			if p.ArtifactManifest == nil || p.ArtifactManifest.Execution == nil {
				t.Fatal("no observed identity")
			}
			if tc.wantCalls == 1 {
				if len(p.ArtifactInputs) != 1 {
					t.Fatal("no evidence")
				}
			} else if len(p.ArtifactInputs) != 0 || !reflect.DeepEqual(p.ArtifactManifest.Reasons, []string{"attempt_provenance_unavailable"}) {
				t.Fatalf("%+v", p.ArtifactManifest)
			}
		})
	}
}
func ciAttempt(n int64) *int64 { return &n }

func ciMutations() map[string]func(*forge.WorkflowRun) {
	return map[string]func(*forge.WorkflowRun){
		"green":                        func(r *forge.WorkflowRun) { r.Conclusion = "success" },
		"in-progress-stale-conclusion": func(r *forge.WorkflowRun) { r.Status = "in_progress" },
		"completed-other":              func(r *forge.WorkflowRun) { r.Conclusion = "cancelled" },
		"id":                           func(r *forge.WorkflowRun) { r.ID++ },
		"sha":                          func(r *forge.WorkflowRun) { r.SHA = "changed" },
		"attempt":                      func(r *forge.WorkflowRun) { r.RunAttempt = ciAttempt(2) },
		"unknown-attempt":              func(r *forge.WorkflowRun) { r.RunAttempt = nil },
		"revision":                     func(r *forge.WorkflowRun) { r.UpdatedAt = r.UpdatedAt.Add(time.Second) },
		"repo":                         func(r *forge.WorkflowRun) { r.RepositoryID++ },
		"head-repo":                    func(r *forge.WorkflowRun) { r.HeadRepositoryID++ },
		"workflow":                     func(r *forge.WorkflowRun) { r.WorkflowKey = "other" },
		"branch":                       func(r *forge.WorkflowRun) { r.Branch = "other" },
		"event":                        func(r *forge.WorkflowRun) { r.Event = "pull_request" },
	}
}
func TestCIArtifactsFreshnessAtEveryReadBoundary(t *testing.T) {
	for stage := 1; stage <= 4; stage++ {
		for name, mutate := range ciMutations() {
			t.Run(fmt.Sprintf("%d-%s", stage, name), func(t *testing.T) {
				r := ciRed("wf", 100, 0)
				h := newCIHarness(t, r)
				seedCIArtifact(t, h, r)
				h.cf.getRun = func(_ context.Context, _ int64, call int) (forge.WorkflowRun, error) {
					current := r
					if call >= stage {
						mutate(&current)
					}
					return current, nil
				}
				out := h.fire(t, ciArtifactSchedule(h))
				if len(out.Started) != 0 || len(h.runs.ciRepairs) != 0 || len(h.runs.ciTransitions) != 0 {
					t.Fatalf("obsolete candidate wrote: %+v", out)
				}
				if stage == 1 && len(h.cf.jobCalls) != 0 {
					t.Fatal("logs fetched for obsolete candidate")
				}
				if stage <= 2 && h.cf.artifactCalls != 0 {
					t.Fatal("artifacts fetched for obsolete candidate")
				}
			})
		}
	}
}
func TestCIArtifactsCollectionBeforeLoopGuard(t *testing.T) {
	r := ciRed("wf", 100, 0)
	h := newCIHarness(t, r)
	seedCIArtifact(t, h, r)
	h.st.ciEpisodes = map[string]store.CiWatchEpisode{"wf": {Attempts: 2}}
	h.cf.download = func(context.Context, int64, int64) ([]byte, error) {
		h.cf.runs[0] = ciGreenRun("wf", 100, 0)
		return ciZIP(t, "kept before race"), nil
	}
	out := h.fire(t, ciArtifactSchedule(h))
	if len(out.Started) != 0 || len(h.runs.ciTransitions) != 0 || len(h.runs.ciRepairs) != 0 || len(h.cf.downloadCalls) != 1 {
		t.Fatalf("%+v", out)
	}
}
func TestCIArtifactsFreshnessAfterProjectPathAndFallbackHalt(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			r := ciRed("wf", 100, 0)
			h := newCIHarness(t, r)
			if fallback {
				h.runs.ciCreateErr = &workersvc.CIWatchHaltedError{Reason: workersvc.CIHaltNoProgress}
				h.cf.getRun = func(_ context.Context, _ int64, call int) (forge.WorkflowRun, error) {
					if call >= 5 {
						return ciGreenRun("wf", 100, 0), nil
					}
					return r, nil
				}
			} else {
				h.cf.onPath = func() { h.cf.runs[0] = ciGreenRun("wf", 100, 0) }
			}
			h.fire(t, h.schedule())
			wantCreates := 0
			if fallback {
				wantCreates = 1
			}
			if len(h.runs.ciTransitions) != 0 || len(h.runs.ciRepairs) != wantCreates {
				t.Fatal("stale write")
			}
		})
	}
}
func TestCIArtifactsRevalidationErrorsFailClosed(t *testing.T) {
	for stage := 1; stage <= 5; stage++ {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			r := ciRed("wf", 100, 0)
			h := newCIHarness(t, r)
			if stage == 5 {
				h.runs.ciCreateErr = &workersvc.CIWatchHaltedError{Reason: workersvc.CIHaltNoProgress}
			}
			want := errors.New("read failed")
			h.cf.getRun = func(_ context.Context, _ int64, n int) (forge.WorkflowRun, error) {
				if n == stage {
					return forge.WorkflowRun{}, want
				}
				return r, nil
			}
			_, err := h.sched.fireOne(context.Background(), h.schedule())
			if !errors.Is(err, want) || len(h.runs.ciTransitions) != 0 {
				t.Fatalf("err=%v transitions=%v", err, h.runs.ciTransitions)
			}
			if stage < 5 && len(h.runs.ciRepairs) != 0 {
				t.Fatal("run created after unknown freshness")
			}
		})
	}
}
func TestCIArtifactsEmptyAndMalformedSelectors(t *testing.T) {
	for _, raw := range []string{"null", "[]", `["CaseSensitive"]`, `["logs","logs"]`, `{"bad":true}`, `["["]`} {
		t.Run(raw, func(t *testing.T) {
			h := newCIHarness(t, ciRed("wf", 100, 0))
			s := h.schedule()
			s.CiArtifacts = []byte(raw)
			_, err := h.sched.fireOne(context.Background(), s)
			invalid := raw == `{"bad":true}` || raw == `["["]`
			if (err != nil) != invalid {
				t.Fatalf("err=%v", err)
			}
			if raw == "null" || raw == "[]" || invalid {
				if h.cf.artifactCalls != 0 {
					t.Fatal("unexpected artifact list")
				}
			}
			if len(h.cf.downloadCalls) != 0 {
				t.Fatal("unexpected download")
			}
		})
	}
}
func TestCIArtifactsDownloadBudgetAndPartialErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		first     int
		err       error
		wantCalls int
		reason    string
	}{
		{"exact", 64 << 20, nil, 1, "download_budget"},
		{"remaining", 64<<20 - 13, nil, 2, ""},
		{"overflow", 64<<20 + 1, nil, 1, "download_budget"},
		{"partial", 0, errors.New("unknown partial read"), 1, "download_unknown"},
		{"budget", 0, forge.ErrArtifactBudget, 1, "download_budget"},
		{"denied", 0, forge.ErrArtifactDenied, 3, "denied"},
		{"missing", 0, forge.ErrArtifactMissing, 3, "missing"},
		{"expired", 0, forge.ErrArtifactExpired, 3, "expired"},
		{"deadline", 0, context.DeadlineExceeded, 1, "deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ciRed("wf", 100, 0)
			h := newCIHarness(t, r)
			h.cf.artifacts = []forge.Artifact{ciArtifact(r, 1), ciArtifact(r, 2), ciArtifact(r, 3)}
			n := 0
			h.cf.download = func(_ context.Context, _ int64, max int64) ([]byte, error) {
				n++
				if n == 1 {
					if max != 64<<20 {
						t.Fatalf("max=%d", max)
					}
					return make([]byte, tc.first), tc.err
				}
				if tc.name == "remaining" {
					if max != 13 {
						t.Fatalf("remaining=%d", max)
					}
					return make([]byte, 13), nil
				}
				return nil, tc.err
			}
			h.fire(t, ciArtifactSchedule(h))
			if n != tc.wantCalls {
				t.Fatalf("downloads=%d want %d", n, tc.wantCalls)
			}
			raw, _ := json.Marshal(h.runs.ciRepairs[0].ArtifactManifest)
			if tc.reason != "" && !strings.Contains(string(raw), tc.reason) {
				t.Fatalf("%s", raw)
			}
		})
	}
}
func TestCIArtifactsAssociationDedupAndEarlierKept(t *testing.T) {
	r := ciRed("wf", 100, 0)
	h := newCIHarness(t, r)
	good := ciArtifact(r, 1)
	bad := ciArtifact(r, 2)
	bad.SHA = "old"
	expired := ciArtifact(r, 3)
	expired.Expired = true
	h.cf.artifacts = []forge.Artifact{expired, bad, good, good, ciArtifact(r, 4), ciArtifact(r, 5)}
	h.cf.download = func(_ context.Context, id, max int64) ([]byte, error) {
		if id == 1 {
			return ciZIP(t, "kept"), nil
		}
		return nil, errors.New("secret upstream error")
	}
	h.fire(t, ciArtifactSchedule(h))
	p := h.runs.ciRepairs[0]
	if len(p.ArtifactInputs) != 1 || !reflect.DeepEqual(h.cf.downloadCalls, []int64{1, 4}) {
		t.Fatalf("inputs=%d calls=%v", len(p.ArtifactInputs), h.cf.downloadCalls)
	}
	raw, _ := json.Marshal(p.ArtifactManifest)
	if bytes.Contains(raw, []byte("secret upstream")) || !bytes.Contains(raw, []byte("association")) {
		t.Fatalf("%s", raw)
	}
}
func TestCIArtifactsCollectionDeadlineInjected(t *testing.T) {
	r := ciRed("wf", 100, 0)
	h := newCIHarness(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	h.cf.artifacts = []forge.Artifact{ciArtifact(r, 1), ciArtifact(r, 2)}
	h.cf.download = func(downloadCtx context.Context, _, _ int64) ([]byte, error) {
		cancel()
		<-downloadCtx.Done()
		return nil, downloadCtx.Err()
	}
	m, inputs := h.sched.ciCollectArtifacts(ctx, ciArtifactSchedule(h), h.st.repoRow, h.cf, CIRepairConfig{}, &ciKey{run: r})
	if m == nil || len(inputs) != 0 || len(h.cf.downloadCalls) != 1 || !reflect.DeepEqual(m.Reasons, []string{"deadline"}) {
		t.Fatalf("deadline result: %+v", m)
	}
}

func TestCIArtifactsListedMatchSurvivesDownloadStop(t *testing.T) {
	r := ciRed("wf", 100, 0)
	h := newCIHarness(t, r)
	later := ciArtifact(r, 2)
	later.Name = "later"
	h.cf.artifacts = []forge.Artifact{ciArtifact(r, 1), later}
	h.cf.download = func(context.Context, int64, int64) ([]byte, error) {
		return nil, forge.ErrArtifactBudget
	}
	s := ciArtifactSchedule(h)
	s.CiArtifacts = []byte(`["logs", "later"]`)
	h.fire(t, s)
	m := h.runs.ciRepairs[0].ArtifactManifest
	for _, row := range m.Entries {
		if row.Reason == "missing" {
			t.Fatal("listed matching artifact falsely reported missing")
		}
	}
	if !m.Uninspected || len(h.cf.downloadCalls) != 1 {
		t.Fatal("unknown uninspected siblings not reported")
	}
}

func TestCIArtifactsDescriptionReserve(t *testing.T) {
	h := newCIHarness(t, ciRed("wf", 100, 0))
	seedCIArtifact(t, h, h.cf.runs[0])
	h.cf.tails[1001] = strings.Repeat("logs", 100000)
	h.fire(t, ciArtifactSchedule(h))
	p := h.runs.ciRepairs[0]
	for i := range p.ArtifactManifest.Entries {
		if p.ArtifactManifest.Entries[i].Status == "kept" {
			p.ArtifactManifest.Entries[i].ManifestID = "00000000-0000-0000-0000-000000000000"
		}
	}
	if _, err := p.ArtifactManifest.RenderWithin(workersvc.MaxIssueDescriptionBytes - len(p.Description) - 10); err != nil {
		t.Fatalf("actual manifest cannot fit: %v", err)
	}
	large := newCIHarness(t, ciRed("wf", 100, 0))
	seedCIArtifact(t, large, large.cf.runs[0])
	s := ciArtifactSchedule(large)
	s.Origin = "user"
	s.Prompt.String = strings.Repeat("p", workersvc.MaxIssueDescriptionBytes-(32<<10))
	s.Prompt.Valid = true
	out := large.fire(t, s)
	if len(out.Started) != 1 || !strings.HasPrefix(large.runs.ciRepairs[0].Description, s.Prompt.String) {
		t.Fatal("small manifest refused or clipped trusted prefix")
	}
}
