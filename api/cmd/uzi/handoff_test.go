package main

import (
	"context"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// handoffRecorder is a fake Env.Git: it records every git call (full arg list) and,
// interleaved with the client's create/dispatch, an ordered `seq` so a test can prove
// the Decision-6 create → push → dispatch ordering that per-verb captures cannot show.
// gitOut/gitErr are keyed by the space-joined args so a test can canned-reply to
// `remote get-url origin` and inject a push failure.
type handoffRecorder struct {
	seq      []string
	gitCalls [][]string
	gitOut   map[string]string
	gitErr   map[string]error
}

func (r *handoffRecorder) git(_ string, args ...string) (string, error) {
	r.gitCalls = append(r.gitCalls, args)
	r.seq = append(r.seq, "git:"+strings.Join(args, " "))
	key := strings.Join(args, " ")
	if e, ok := r.gitErr[key]; ok {
		return "", e
	}
	return r.gitOut[key], nil
}

// handoffClient wraps a FakeClient to append "create"/"dispatch" to the shared
// recorder's ordered seq, so create/push/dispatch land in one comparable sequence.
type handoffClient struct {
	*uzicli.FakeClient
	rec *handoffRecorder
}

func (c *handoffClient) CreateTaskRun(ctx context.Context, repoID, taskContext, baseBranch string, openMR, reviewRequested, thenFixRequested bool) (apitypes.RunDTO, error) {
	c.rec.seq = append(c.rec.seq, "create")
	return c.FakeClient.CreateTaskRun(ctx, repoID, taskContext, baseBranch, openMR, reviewRequested, thenFixRequested)
}

func (c *handoffClient) DispatchTaskRun(ctx context.Context, runID string) (apitypes.RunDTO, error) {
	c.rec.seq = append(c.rec.seq, "dispatch")
	return c.FakeClient.DispatchTaskRun(ctx, runID)
}

// handoffEnv wires a fake client + a fake Git recorder into an Env.
func handoffEnv(fc *uzicli.FakeClient, rec *handoffRecorder) (Env, *handoffClient) {
	hc := &handoffClient{FakeClient: fc, rec: rec}
	env := fakeEnv(hc)
	env.Git = rec.git
	return env, hc
}

// taskRun builds a created/dispatched task-run DTO with a server-named branch.
func taskRun(id, branch string) apitypes.RunDTO {
	b := branch
	return apitypes.RunDTO{ID: id, Kind: "task", Branch: &b}
}

// TestHandoffHappyPath is the Decision-6 core: a --repo handoff creates the run, pushes
// local HEAD to the created branch, and dispatches — IN THAT ORDER — forwarding the
// exact args at each step.
func TestHandoffHappyPath(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedTaskRun: taskRun("r1", "uzi/task/r1"),
		DispatchedRun:  taskRun("r1", "uzi/task/r1"),
	}
	rec := &handoffRecorder{}
	env, _ := handoffEnv(fc, rec)

	out, _, code := runCLI(t, env, "handoff", "--repo", "p1", "-m", "do X")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}

	// (a) create captured the right args.
	if fc.LastCreateTaskRepoID != "p1" {
		t.Errorf("create repo = %q, want p1", fc.LastCreateTaskRepoID)
	}
	if fc.LastCreateTaskContext != "do X" {
		t.Errorf("create context = %q, want %q", fc.LastCreateTaskContext, "do X")
	}
	if fc.LastCreateTaskBaseBranch != "" {
		t.Errorf("create base = %q, want empty", fc.LastCreateTaskBaseBranch)
	}
	if fc.LastCreateTaskOpenMr {
		t.Errorf("create open_mr = true, want false")
	}
	// (b) push went to origin HEAD:refs/heads/<created branch>.
	wantPush := []string{"push", "origin", "HEAD:refs/heads/uzi/task/r1"}
	if !hasGitCall(rec, wantPush) {
		t.Errorf("no push %v in git calls %v", wantPush, rec.gitCalls)
	}
	// (c) dispatch captured the created run id.
	if fc.LastDispatchRunID != "r1" {
		t.Errorf("dispatch run id = %q, want r1", fc.LastDispatchRunID)
	}
	// (d) ORDER: create < push < dispatch.
	assertSeqOrder(t, rec.seq, "create", "git:push origin HEAD:refs/heads/uzi/task/r1", "dispatch")

	if !strings.Contains(out, "uzi/task/r1") || !strings.Contains(out, "git fetch origin") {
		t.Errorf("human output missing branch/pull hint:\n%s", out)
	}
}

// --base main branches from a named ref: it becomes both the push SOURCE and the
// create call's base_branch.
func TestHandoffBaseRef(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedTaskRun: taskRun("r2", "uzi/task/r2"),
		DispatchedRun:  taskRun("r2", "uzi/task/r2"),
	}
	rec := &handoffRecorder{}
	env, _ := handoffEnv(fc, rec)

	_, _, code := runCLI(t, env, "handoff", "--repo", "p1", "-m", "x", "--base", "main")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastCreateTaskBaseBranch != "main" {
		t.Errorf("create base = %q, want main", fc.LastCreateTaskBaseBranch)
	}
	wantPush := []string{"push", "origin", "main:refs/heads/uzi/task/r2"}
	if !hasGitCall(rec, wantPush) {
		t.Errorf("push should use main as source; git calls %v", rec.gitCalls)
	}
}

// --review requests a diff-review; --then-fix additionally requests a chained fix AND
// implies --review (a fix consumes a review's findings). This pins the load-bearing
// `reviewRequested = review || thenFix` wiring in the CLI.
func TestHandoffReviewAndThenFixFlags(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantReview  bool
		wantThenFix bool
	}{
		{"neither", []string{"handoff", "--repo", "p1", "-m", "x"}, false, false},
		{"review only", []string{"handoff", "--repo", "p1", "-m", "x", "--review"}, true, false},
		{"then-fix implies review", []string{"handoff", "--repo", "p1", "-m", "x", "--then-fix"}, true, true},
		{"both explicit", []string{"handoff", "--repo", "p1", "-m", "x", "--review", "--then-fix"}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{
				CreatedTaskRun: taskRun("rt", "uzi/task/rt"),
				DispatchedRun:  taskRun("rt", "uzi/task/rt"),
			}
			rec := &handoffRecorder{}
			env, _ := handoffEnv(fc, rec)

			_, _, code := runCLI(t, env, tc.args...)
			if code != uzicli.ExitOK {
				t.Fatalf("exit = %d, want 0", code)
			}
			if fc.LastCreateTaskReview != tc.wantReview {
				t.Errorf("review_requested = %v, want %v", fc.LastCreateTaskReview, tc.wantReview)
			}
			if fc.LastCreateTaskThenFix != tc.wantThenFix {
				t.Errorf("then_fix_requested = %v, want %v", fc.LastCreateTaskThenFix, tc.wantThenFix)
			}
		})
	}
}

// --mr sets open_mr=true in the create call and the human output states the branch is
// MR-exempt.
func TestHandoffMR(t *testing.T) {
	created := taskRun("r3", "uzi/task/r3")
	created.OpenMr = true
	fc := &uzicli.FakeClient{CreatedTaskRun: created, DispatchedRun: created}
	rec := &handoffRecorder{}
	env, _ := handoffEnv(fc, rec)

	out, _, code := runCLI(t, env, "handoff", "--repo", "p1", "-m", "x", "--mr")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !fc.LastCreateTaskOpenMr {
		t.Errorf("create open_mr = false, want true")
	}
	if !strings.Contains(out, "exempt") {
		t.Errorf("--mr output should note the branch is rm-exempt:\n%s", out)
	}
}

// Repo auto-detect: origin URL → owner/repo → PathWithNamespace match. Covers the https
// and scp-like SSH forms, plus zero-match and ambiguous-match (both ExitUsage).
func TestHandoffRepoAutoDetect(t *testing.T) {
	cases := []struct {
		name       string
		origin     string
		repos      []apitypes.RepoDTO
		wantRepoID string
		wantCode   int
	}{
		{
			name:       "https",
			origin:     "https://github.com/acme/widgets.git",
			repos:      []apitypes.RepoDTO{{ID: "p9", PathWithNamespace: "acme/widgets"}},
			wantRepoID: "p9",
			wantCode:   uzicli.ExitOK,
		},
		{
			name:       "ssh scp form",
			origin:     "git@github.com:acme/widgets.git",
			repos:      []apitypes.RepoDTO{{ID: "p9", PathWithNamespace: "acme/widgets"}},
			wantRepoID: "p9",
			wantCode:   uzicli.ExitOK,
		},
		{
			name:     "zero match",
			origin:   "https://github.com/acme/widgets.git",
			repos:    []apitypes.RepoDTO{{ID: "p9", PathWithNamespace: "other/thing"}},
			wantCode: uzicli.ExitUsage,
		},
		{
			name:   "ambiguous",
			origin: "https://github.com/acme/widgets.git",
			repos: []apitypes.RepoDTO{
				{ID: "p9", PathWithNamespace: "acme/widgets"},
				{ID: "p10", PathWithNamespace: "acme/widgets"},
			},
			wantCode: uzicli.ExitUsage,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{
				Repos:          tc.repos,
				CreatedTaskRun: taskRun("rd", "uzi/task/rd"),
				DispatchedRun:  taskRun("rd", "uzi/task/rd"),
			}
			rec := &handoffRecorder{gitOut: map[string]string{"remote get-url origin": tc.origin}}
			env, _ := handoffEnv(fc, rec)

			_, _, code := runCLI(t, env, "handoff", "-m", "x")
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d", code, tc.wantCode)
			}
			if tc.wantCode == uzicli.ExitOK && fc.LastCreateTaskRepoID != tc.wantRepoID {
				t.Errorf("resolved repo = %q, want %q", fc.LastCreateTaskRepoID, tc.wantRepoID)
			}
		})
	}
}

// A push failure must STOP before dispatch: the run is created but not dispatched, so no
// worker claims it. Non-vacuous: it asserts DispatchTaskRun was never called.
func TestHandoffPushFailureDoesNotDispatch(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedTaskRun: taskRun("r4", "uzi/task/r4"),
		DispatchedRun:  taskRun("r4", "uzi/task/r4"),
	}
	rec := &handoffRecorder{
		gitErr: map[string]error{"push origin HEAD:refs/heads/uzi/task/r4": errPushRejected},
	}
	env, _ := handoffEnv(fc, rec)

	_, _, code := runCLI(t, env, "handoff", "--repo", "p1", "-m", "x")
	if code == uzicli.ExitOK {
		t.Fatalf("push failure must be non-zero exit, got %d", code)
	}
	// The whole point: dispatch was NOT reached.
	if fc.LastDispatchRunID != "" {
		t.Errorf("dispatch ran after a failed push (run id %q); the run must NOT become claimable", fc.LastDispatchRunID)
	}
	for _, s := range rec.seq {
		if s == "dispatch" {
			t.Errorf("dispatch appears in the call sequence after a failed push: %v", rec.seq)
		}
	}
	// And the create DID happen (so the error message's cleanup hint is real).
	if fc.LastCreateTaskRepoID != "p1" {
		t.Errorf("create should have run before the push; repo = %q", fc.LastCreateTaskRepoID)
	}
}

// Empty context with a TTY stdin (no -m, no -f, no pipe) is a usage error.
func TestHandoffEmptyContextExit2(t *testing.T) {
	fc := &uzicli.FakeClient{}
	rec := &handoffRecorder{}
	env, _ := handoffEnv(fc, rec)
	env.StdinTTY = true // a terminal: no piped context to fall back to

	_, _, code := runCLI(t, env, "handoff", "--repo", "p1")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastCreateTaskRepoID != "" {
		t.Errorf("no run should be created for an empty context")
	}
}

// --json emits the dispatched run DTO as valid JSON.
func TestHandoffJSON(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedTaskRun: taskRun("r5", "uzi/task/r5"),
		DispatchedRun:  taskRun("r5", "uzi/task/r5"),
	}
	rec := &handoffRecorder{}
	env, _ := handoffEnv(fc, rec)

	out, _, code := runCLI(t, env, "handoff", "--repo", "p1", "-m", "x", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"id": "r5"`) || !strings.Contains(out, `"branch": "uzi/task/r5"`) {
		t.Errorf("--json did not emit the run DTO:\n%s", out)
	}
}

// rm on a no-MR task deletes the remote branch client-side.
func TestHandoffRmDeletesBranch(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"r6": taskRun("r6", "uzi/task/r6"),
	}}
	rec := &handoffRecorder{}
	env, _ := handoffEnv(fc, rec)

	out, _, code := runCLI(t, env, "handoff", "rm", "r6")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	wantDelete := []string{"push", "origin", "--delete", "uzi/task/r6"}
	if !hasGitCall(rec, wantDelete) {
		t.Errorf("rm should push --delete the branch; git calls %v", rec.gitCalls)
	}
	if !strings.Contains(out, "deleted uzi/task/r6") {
		t.Errorf("rm output missing the deleted line:\n%s", out)
	}
}

// rm on a --mr task that has NOT actually opened an MR yet (OpenMr intent set, but
// MrWebURL still nil — e.g. a --mr handoff that failed at push/dispatch) PROCEEDS:
// the exemption keys on an actually-opened MR, so an orphaned branch stays deletable.
func TestHandoffRmOpenMrIntentWithoutMRProceeds(t *testing.T) {
	run := taskRun("r7b", "uzi/task/r7b")
	run.OpenMr = true // intent only; MrWebURL stays nil (no MR opened)
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r7b": run}}
	rec := &handoffRecorder{}
	env, _ := handoffEnv(fc, rec)

	_, _, code := runCLI(t, env, "handoff", "rm", "r7b")
	if code != uzicli.ExitOK {
		t.Fatalf("rm of a --mr task with no opened MR must proceed, got exit %d", code)
	}
	wantDelete := []string{"push", "origin", "--delete", "uzi/task/r7b"}
	if !hasGitCall(rec, wantDelete) {
		t.Errorf("rm should delete the orphaned branch; git calls %v", rec.gitCalls)
	}
}

// rm on a task that opened an MR is refused, and does NOT delete the branch.
func TestHandoffRmMRExempt(t *testing.T) {
	mrURL := "https://forge/mr/1"
	run := taskRun("r7", "uzi/task/r7")
	run.MrWebURL = &mrURL
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r7": run}}
	rec := &handoffRecorder{}
	env, _ := handoffEnv(fc, rec)

	_, _, code := runCLI(t, env, "handoff", "rm", "r7")
	if code == uzicli.ExitOK {
		t.Fatalf("rm of an MR branch must be refused, got exit 0")
	}
	if len(rec.gitCalls) != 0 {
		t.Errorf("an MR-exempt rm must not run git: %v", rec.gitCalls)
	}
}

// A --base that starts with '-' is rejected (it would be misparsed by git push as an
// option in the refspec argv element): usage error, and nothing is pushed or dispatched.
func TestHandoffBaseLeadingDashRejected(t *testing.T) {
	fc := &uzicli.FakeClient{
		CreatedTaskRun: taskRun("r9", "uzi/task/r9"),
		DispatchedRun:  taskRun("r9", "uzi/task/r9"),
	}
	rec := &handoffRecorder{}
	env, _ := handoffEnv(fc, rec)

	_, _, code := runCLI(t, env, "handoff", "--repo", "p1", "-m", "x", "--base", "--receive-pack=evil")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	// Rejected BEFORE create, so no orphaned run and nothing pushed/dispatched.
	if fc.LastCreateTaskRepoID != "" {
		t.Errorf("a leading-dash --base must be rejected before create, but create ran (repo=%q)", fc.LastCreateTaskRepoID)
	}
	for _, g := range rec.gitCalls {
		if len(g) > 0 && g[0] == "push" {
			t.Errorf("a leading-dash --base must not reach a git push: %v", rec.gitCalls)
		}
	}
	if fc.LastDispatchRunID != "" {
		t.Errorf("dispatch must not run after a rejected --base, got %q", fc.LastDispatchRunID)
	}
}

// rm on a non-task run is refused.
func TestHandoffRmNonTaskRefused(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"r8": {ID: "r8", Kind: "issue"},
	}}
	rec := &handoffRecorder{}
	env, _ := handoffEnv(fc, rec)

	_, _, code := runCLI(t, env, "handoff", "rm", "r8")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if len(rec.gitCalls) != 0 {
		t.Errorf("a non-task rm must not run git: %v", rec.gitCalls)
	}
}

// rm on a missing run is ExitNotFound (the fake 404s an absent id).
func TestHandoffRmMissingExit4(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{}}
	rec := &handoffRecorder{}
	env, _ := handoffEnv(fc, rec)

	_, _, code := runCLI(t, env, "handoff", "rm", "nope")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found)", code, uzicli.ExitNotFound)
	}
}

// TestParseRepoPath pins the origin-URL → owner/repo parser across the forms the
// auto-detect relies on.
func TestParseRepoPath(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/widgets.git":       "acme/widgets",
		"https://github.com/acme/widgets":           "acme/widgets",
		"git@github.com:acme/widgets.git":           "acme/widgets",
		"git@github.com:acme/widgets":               "acme/widgets",
		"ssh://git@github.com:22/acme/widgets.git":  "acme/widgets",
		"https://gitlab.com/acme/group/widgets.git": "acme/group/widgets",
	}
	for in, want := range cases {
		if got := parseRepoPath(in); got != want {
			t.Errorf("parseRepoPath(%q) = %q, want %q", in, got, want)
		}
	}
}

var errPushRejected = uzicli.Exitf(uzicli.ExitGeneric, "remote rejected")

// hasGitCall reports whether the recorder saw a git call with exactly these args.
func hasGitCall(rec *handoffRecorder, want []string) bool {
	for _, got := range rec.gitCalls {
		if len(got) != len(want) {
			continue
		}
		eq := true
		for i := range got {
			if got[i] != want[i] {
				eq = false
				break
			}
		}
		if eq {
			return true
		}
	}
	return false
}

// assertSeqOrder checks that want's entries appear in seq in the given relative order.
func assertSeqOrder(t *testing.T, seq []string, want ...string) {
	t.Helper()
	idx := 0
	for _, s := range seq {
		if idx < len(want) && s == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Errorf("call sequence %v does not contain %v in order (matched %d/%d)", seq, want, idx, len(want))
	}
}
