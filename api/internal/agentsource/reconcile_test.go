package agentsource

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// --- fakes ---------------------------------------------------------------------

type fakeSettings struct {
	enabled        bool
	url            string
	ref            string
	folder         string
	interval       time.Duration
	credential     string
	credErr        error
	lastAppliedSHA string
	invalidated    int
}

func (f *fakeSettings) AgentSourceEnabled(context.Context) (bool, error)   { return f.enabled, nil }
func (f *fakeSettings) AgentSourceRepoURL(context.Context) (string, error) { return f.url, nil }
func (f *fakeSettings) AgentSourceRef(context.Context) (string, error)     { return f.ref, nil }
func (f *fakeSettings) AgentSourceFolder(context.Context) (string, error)  { return f.folder, nil }
func (f *fakeSettings) AgentSourceInterval(context.Context) (time.Duration, error) {
	return f.interval, nil
}
func (f *fakeSettings) AgentSourceCredential(context.Context) (string, error) {
	return f.credential, f.credErr
}
func (f *fakeSettings) AgentSourceLastAppliedSHA(context.Context) (string, error) {
	return f.lastAppliedSHA, nil
}
func (f *fakeSettings) Invalidate() { f.invalidated++ }

type fakeStore struct {
	templates []store.AgentTemplate
	prior     *store.AgentSourceStaged
	upserts   []store.UpsertAgentSourceStagedParams
	appSet    map[string]string
}

func newFakeStore() *fakeStore { return &fakeStore{appSet: map[string]string{}} }

func (f *fakeStore) ListAgentTemplates(context.Context) ([]store.AgentTemplate, error) {
	return f.templates, nil
}
func (f *fakeStore) GetAgentSourceStaged(context.Context) (store.AgentSourceStaged, error) {
	if f.prior == nil {
		return store.AgentSourceStaged{}, pgx.ErrNoRows
	}
	return *f.prior, nil
}
func (f *fakeStore) UpsertAgentSourceStaged(_ context.Context, arg store.UpsertAgentSourceStagedParams) (store.AgentSourceStaged, error) {
	f.upserts = append(f.upserts, arg)
	row := store.AgentSourceStaged{ID: 1, FetchedSha: arg.FetchedSha, SourceUrl: arg.SourceUrl, SourceRef: arg.SourceRef, Roles: arg.Roles, Diff: arg.Diff}
	f.prior = &row
	return row, nil
}
func (f *fakeStore) UpsertAppSetting(_ context.Context, arg store.UpsertAppSettingParams) (store.AppSetting, error) {
	f.appSet[arg.Key] = arg.Value
	return store.AppSetting{}, nil
}

type fakeAllowlist struct{ allowed bool }

func (f fakeAllowlist) AgentSourceBaseURLAllowed(string) bool { return f.allowed }

// stubFetch returns a canned fetch result / error.
func stubFetch(sha string, files []SourceFile, err error) FetchFunc {
	return func(context.Context, CloneOptions) (string, []SourceFile, error) { return sha, files, err }
}

func roleFile(name, body string) SourceFile {
	return SourceFile{Name: name + ".md", Data: []byte("---\nname: " + name + "\ndescription: does " + name + " work\n---\n" + body + "\n")}
}

func newRec(st Store, set SettingsReader, allow bool) *Reconciler {
	r := NewReconciler(st, set, fakeAllowlist{allowed: allow}, nil, nil)
	return r
}

// --- reconcile unit tests ------------------------------------------------------

func TestReconcileDisabledNoOp(t *testing.T) {
	st := newFakeStore()
	set := &fakeSettings{enabled: false, url: "https://ok.test/a.git"}
	res, err := newRec(st, set, true).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != "disabled" {
		t.Errorf("status = %q, want disabled", res.Status)
	}
	if len(st.upserts) != 0 || len(st.appSet) != 0 {
		t.Errorf("disabled reconcile must touch nothing: upserts=%d appSet=%d", len(st.upserts), len(st.appSet))
	}
}

func TestReconcileEmptyURLNoOp(t *testing.T) {
	st := newFakeStore()
	set := &fakeSettings{enabled: true, url: "   "}
	res, _ := newRec(st, set, true).Reconcile(context.Background())
	if res.Status != "disabled" {
		t.Errorf("empty url must be a disabled no-op; got %q", res.Status)
	}
	if len(st.upserts) != 0 {
		t.Errorf("must not stage on empty url")
	}
}

func TestReconcileURLNotAllowlisted(t *testing.T) {
	st := newFakeStore()
	// A prior good snapshot must be left untouched.
	st.prior = &store.AgentSourceStaged{ID: 1, FetchedSha: "good"}
	set := &fakeSettings{enabled: true, url: "https://evil.test/a.git"}
	r := newRec(st, set, false) // allowlist denies
	fetched := 0
	r.fetch = func(context.Context, CloneOptions) (string, []SourceFile, error) {
		fetched++
		return "x", nil, nil
	}
	res, _ := r.Reconcile(context.Background())
	if res.Status != statusError {
		t.Errorf("status = %q, want error", res.Status)
	}
	if fetched != 0 {
		t.Errorf("must NOT clone a non-allowlisted url")
	}
	if len(st.upserts) != 0 {
		t.Errorf("staged snapshot must be untouched on allowlist miss")
	}
	if st.appSet[settingsKeyStatus()] != statusError {
		t.Errorf("last-sync status must be error; got %q", st.appSet[settingsKeyStatus()])
	}
}

func TestReconcileUnreachableKeepsLastGood(t *testing.T) {
	st := newFakeStore()
	st.prior = &store.AgentSourceStaged{ID: 1, FetchedSha: "good-old"}
	set := &fakeSettings{enabled: true, url: "https://ok.test/a.git"}
	r := newRec(st, set, true)
	r.fetch = stubFetch("", nil, errors.New("agentsource: clone failed: dial tcp timeout"))
	res, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile must never return a fatal error; got %v", err)
	}
	if res.Status != statusError {
		t.Errorf("status = %q, want error", res.Status)
	}
	if len(st.upserts) != 0 {
		t.Errorf("unreachable source must leave last-good snapshot untouched")
	}
	if st.prior.FetchedSha != "good-old" {
		t.Errorf("prior snapshot mutated: %q", st.prior.FetchedSha)
	}
	if st.appSet[settingsKeyStatus()] != statusError {
		t.Errorf("status must be recorded as error")
	}
}

func TestReconcileHappyPathStages(t *testing.T) {
	st := newFakeStore()
	set := &fakeSettings{enabled: true, url: "https://ok.test/a.git", ref: "v1.0.0"}
	r := newRec(st, set, true)
	r.fetch = stubFetch("sha-new", []SourceFile{roleFile("coder", "coder body")}, nil)
	res, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != statusOK || !res.Restaged {
		t.Fatalf("want ok+restaged; got %+v", res)
	}
	if res.Staged != 1 || res.Changed != 1 || res.Failed != 0 {
		t.Errorf("counts wrong: %+v", res)
	}
	if len(st.upserts) != 1 {
		t.Fatalf("expected one staged upsert; got %d", len(st.upserts))
	}
	up := st.upserts[0]
	if up.FetchedSha != "sha-new" || up.SourceRef != "v1.0.0" {
		t.Errorf("upsert scalars wrong: %+v", up)
	}
	var roles []StagedRole
	if err := json.Unmarshal(up.Roles, &roles); err != nil {
		t.Fatalf("roles json: %v", err)
	}
	if len(roles) != 1 || roles[0].Name != "coder" || !roles[0].OK {
		t.Errorf("staged roles wrong: %+v", roles)
	}
	var diff []DiffEntry
	if err := json.Unmarshal(up.Diff, &diff); err != nil {
		t.Fatalf("diff json: %v", err)
	}
	if len(diff) != 1 || diff[0].Action != DiffAdd {
		t.Errorf("diff wrong: %+v", diff)
	}
	if st.appSet[settingsKeyStatus()] != statusOK || st.appSet[settingsKeySHA()] != "sha-new" {
		t.Errorf("status keys wrong: %v", st.appSet)
	}
}

func TestReconcilePerRoleFailureRecordedOthersProceed(t *testing.T) {
	st := newFakeStore()
	set := &fakeSettings{enabled: true, url: "https://ok.test/a.git"}
	r := newRec(st, set, true)
	// One valid role, one file with no frontmatter (parses as invalid).
	bad := SourceFile{Name: "bad.md", Data: []byte("no frontmatter here")}
	r.fetch = stubFetch("sha1", []SourceFile{roleFile("coder", "b"), bad}, nil)
	res, _ := r.Reconcile(context.Background())
	if res.Staged != 1 {
		t.Errorf("the valid role must still be staged; staged=%d", res.Staged)
	}
	if res.Failed != 1 {
		t.Errorf("the invalid role must be counted failed; failed=%d", res.Failed)
	}
	var roles []StagedRole
	_ = json.Unmarshal(st.upserts[0].Roles, &roles)
	var okCount, failCount int
	for _, r := range roles {
		if r.OK {
			okCount++
		} else {
			failCount++
			if r.Reason == "" {
				t.Errorf("a failed role must carry a reason: %+v", r)
			}
		}
	}
	if okCount != 1 || failCount != 1 {
		t.Errorf("want 1 ok + 1 failed staged role; got ok=%d fail=%d", okCount, failCount)
	}
}

func TestReconcileIdempotentSameSHA(t *testing.T) {
	st := newFakeStore()
	st.prior = &store.AgentSourceStaged{ID: 1, FetchedSha: "sha-same"}
	set := &fakeSettings{enabled: true, url: "https://ok.test/a.git"}
	r := newRec(st, set, true)
	r.fetch = stubFetch("sha-same", []SourceFile{roleFile("coder", "b")}, nil)
	res, _ := r.Reconcile(context.Background())
	if res.Restaged {
		t.Errorf("same SHA must not re-stage")
	}
	if res.Status != statusOK {
		t.Errorf("same SHA must record status ok; got %q", res.Status)
	}
	if len(st.upserts) != 0 {
		t.Errorf("same SHA must not upsert the snapshot; got %d", len(st.upserts))
	}
}

// TestReconcileIdempotentSameSHACarriesCounts pins FINDING 3: an unchanged (same-SHA)
// tick must report the counts of the CURRENTLY-STAGED snapshot, not blank {0,0,0} — the
// unchanged tick is the steady state, so zeroing would make the counts panel read zero
// almost always.
func TestReconcileIdempotentSameSHACarriesCounts(t *testing.T) {
	roles, err := json.Marshal([]StagedRole{
		{Name: "coder", OK: true},
		{Name: "tester", OK: true},
		{Name: "broken", OK: false, Reason: "invalid"},
	})
	if err != nil {
		t.Fatalf("marshal roles: %v", err)
	}
	diff, err := json.Marshal([]DiffEntry{
		{Name: "coder", Action: DiffOverride},
		{Name: "tester", Action: DiffUnchanged},
	})
	if err != nil {
		t.Fatalf("marshal diff: %v", err)
	}
	st := newFakeStore()
	st.prior = &store.AgentSourceStaged{ID: 1, FetchedSha: "sha-same", Roles: roles, Diff: diff}
	set := &fakeSettings{enabled: true, url: "https://ok.test/a.git"}
	r := newRec(st, set, true)
	r.fetch = stubFetch("sha-same", []SourceFile{roleFile("coder", "b")}, nil)

	res, _ := r.Reconcile(context.Background())
	if res.Restaged {
		t.Errorf("same SHA must not re-stage")
	}
	// 2 OK roles staged, 1 changed (override; unchanged is not counted), 1 failed.
	if res.Staged != 2 || res.Changed != 1 || res.Failed != 1 {
		t.Errorf("carried counts wrong: staged=%d changed=%d failed=%d; want 2/1/1", res.Staged, res.Changed, res.Failed)
	}
	got := st.appSet["agent_source_last_sync_counts"]
	if !strings.Contains(got, `"staged":2`) || !strings.Contains(got, `"changed":1`) || !strings.Contains(got, `"failed":1`) {
		t.Errorf("recorded counts must describe the staged set, not zeros; got %q", got)
	}
}

// TestReconcileDiffClassification exercises override/conflict/unchanged/remove against
// a set of current templates.
func TestReconcileDiffClassification(t *testing.T) {
	st := newFakeStore()
	st.templates = []store.AgentTemplate{
		mkTemplate("builtin-role", "builtin", "embedded", "old desc", "old body", nil),             // -> override
		mkTemplate("admin-global", "global", "", "admin desc", "admin body", nil),                  // -> conflict
		mkTemplate("synced-gone", "global", "synced", "d", "b", nil),                               // absent -> remove
		mkTemplate("synced-same", "global", "synced", "does synced-same work", "same body\n", nil), // unchanged
	}
	set := &fakeSettings{enabled: true, url: "https://ok.test/a.git"}
	r := newRec(st, set, true)
	r.fetch = stubFetch("sha1", []SourceFile{
		roleFile("builtin-role", "new body"),
		roleFile("admin-global", "x"),
		roleFile("synced-same", "same body"),
		roleFile("fresh-role", "y"),
	}, nil)
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("err: %v", err)
	}
	var diff []DiffEntry
	_ = json.Unmarshal(st.upserts[0].Diff, &diff)
	got := map[string]string{}
	for _, d := range diff {
		got[d.Name] = d.Action
	}
	want := map[string]string{
		"builtin-role": DiffOverride,
		"admin-global": DiffConflict,
		"synced-same":  DiffUnchanged,
		"fresh-role":   DiffAdd,
		"synced-gone":  DiffRemove,
	}
	for name, act := range want {
		if got[name] != act {
			t.Errorf("%s: action = %q, want %q (all: %v)", name, got[name], act, got)
		}
	}
}

// TestRunnerTick covers the interval trigger's gate: a disabled tick reconciles
// nothing; an enabled tick runs one reconcile; a panicking reconcile is recovered
// (never crashes the process).
func TestRunnerTick(t *testing.T) {
	// Disabled: tick is a no-op.
	stOff := newFakeStore()
	setOff := &fakeSettings{enabled: false, url: "https://ok.test/a.git"}
	recOff := newRec(stOff, setOff, true)
	recOff.fetch = stubFetch("sha", []SourceFile{roleFile("coder", "b")}, nil)
	NewRunner(recOff, setOff, nil).tick(context.Background())
	if len(stOff.upserts) != 0 || len(stOff.appSet) != 0 {
		t.Errorf("a disabled tick must do nothing; upserts=%d appSet=%d", len(stOff.upserts), len(stOff.appSet))
	}

	// Enabled: tick runs one reconcile.
	stOn := newFakeStore()
	setOn := &fakeSettings{enabled: true, url: "https://ok.test/a.git"}
	recOn := newRec(stOn, setOn, true)
	recOn.fetch = stubFetch("sha", []SourceFile{roleFile("coder", "b")}, nil)
	NewRunner(recOn, setOn, nil).tick(context.Background())
	if len(stOn.upserts) != 1 {
		t.Errorf("an enabled tick must reconcile once; upserts=%d", len(stOn.upserts))
	}

	// A panic in the reconcile path is recovered (tick returns without re-panicking).
	stPanic := newFakeStore()
	setPanic := &fakeSettings{enabled: true, url: "https://ok.test/a.git"}
	recPanic := newRec(stPanic, setPanic, true)
	recPanic.fetch = func(context.Context, CloneOptions) (string, []SourceFile, error) {
		panic("boom")
	}
	NewRunner(recPanic, setPanic, nil).tick(context.Background()) // must not panic the test
}

// --- FetchRoleFiles integration test (real temp git repo over file://) ---------

func TestFetchRoleFilesFromFixtureRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	bare := buildFixtureRepo(t)
	url := "file://" + bare

	// Default branch (empty ref).
	sha, files, err := FetchRoleFiles(context.Background(), CloneOptions{CloneURL: url})
	if err != nil {
		t.Fatalf("fetch default: %v", err)
	}
	if len(sha) != 40 {
		t.Errorf("sha should be a 40-hex commit id; got %q", sha)
	}
	names := fileNames(files)
	if !names["coder.md"] || !names["reviewer.md"] {
		t.Errorf("expected coder.md + reviewer.md; got %v", names)
	}
	if names["notes.txt"] || names["nested"] {
		t.Errorf("must only read top-level .md regular files; got %v", names)
	}

	// Named tag ref resolves via the advertised-refs list.
	shaTag, filesTag, err := FetchRoleFiles(context.Background(), CloneOptions{CloneURL: url, Ref: "v1"})
	if err != nil {
		t.Fatalf("fetch tag: %v", err)
	}
	if len(shaTag) != 40 || len(filesTag) == 0 {
		t.Errorf("tag fetch returned nothing usable: sha=%q files=%d", shaTag, len(filesTag))
	}

	// A 40-hex SHA pin that IS the fetched tip resolves (Depth-1 carries the tip's
	// objects) — exercises classifyRef's isSHA branch and the pin-must-be-tip check on
	// the success path.
	shaPin, filesPin, err := FetchRoleFiles(context.Background(), CloneOptions{CloneURL: url, Ref: sha})
	if err != nil {
		t.Fatalf("fetch by tip SHA: %v", err)
	}
	if shaPin != sha || len(filesPin) == 0 {
		t.Errorf("SHA-pin fetch should resolve to the same tip with files; got sha=%q files=%d", shaPin, len(filesPin))
	}

	// A ref that does not exist errors cleanly (does not hang).
	if _, _, err := FetchRoleFiles(context.Background(), CloneOptions{CloneURL: url, Ref: "nope-branch"}); err == nil {
		t.Errorf("a missing ref must error")
	}

	// An invalid ref name is rejected by classifyRef before any network op.
	if _, _, err := FetchRoleFiles(context.Background(), CloneOptions{CloneURL: url, Ref: "bad ref~with^chars"}); err == nil {
		t.Errorf("an invalid ref name must be rejected")
	}

	// An explicitly-configured folder that does not exist is a typo, not an empty
	// source: it must error rather than silently return zero roles (which would make
	// Reconcile stage a "remove every role" diff with an ok status). PRD #702 CR fix.
	if _, _, err := FetchRoleFiles(context.Background(), CloneOptions{CloneURL: url, Dir: "product-agents"}); err == nil {
		t.Errorf("a missing explicitly-configured folder must error")
	}

	// A valid non-default subfolder still reads its .md files (the configurable-folder
	// happy path): .claude/agents/nested holds inner.md in the fixture.
	_, nestedFiles, err := FetchRoleFiles(context.Background(), CloneOptions{CloneURL: url, Dir: ".claude/agents/nested"})
	if err != nil {
		t.Fatalf("fetch nested folder: %v", err)
	}
	if !fileNames(nestedFiles)["inner.md"] {
		t.Errorf("expected inner.md from the nested folder; got %v", fileNames(nestedFiles))
	}

	// A configured folder that EXISTS but holds no .md files is a valid EMPTY source
	// (distinct from a missing folder, which errors above): no roles, no error.
	if _, emptyFiles, err := FetchRoleFiles(context.Background(), CloneOptions{CloneURL: url, Dir: ".claude/agents/empty-roster"}); err != nil {
		t.Errorf("an existing markdown-free folder must be a valid empty source; got err: %v", err)
	} else if len(emptyFiles) != 0 {
		t.Errorf("expected zero role files from a markdown-free folder; got %d", len(emptyFiles))
	}
}

// TestReconcilePropagatesFolderAsDir asserts Reconcile reads AgentSourceFolder and
// threads its trimmed value into CloneOptions.Dir (PRD #702 M1) — the fetch seam is
// stubbed to capture the options, so no real clone is needed.
func TestReconcilePropagatesFolderAsDir(t *testing.T) {
	st := newFakeStore()
	set := &fakeSettings{enabled: true, url: "https://ok.test/a.git", ref: "v1.0.0", folder: "  product-agents  "}
	r := newRec(st, set, true)
	var gotDir string
	r.fetch = func(_ context.Context, opts CloneOptions) (string, []SourceFile, error) {
		gotDir = opts.Dir
		return "sha-new", []SourceFile{roleFile("coder", "coder body")}, nil
	}
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gotDir != "product-agents" {
		t.Errorf("configured folder must be trimmed and passed as CloneOptions.Dir; got %q", gotDir)
	}
}

func TestScrubberRedactsToken(t *testing.T) {
	s := scrubber("secret-token-123")
	got := s("clone failed for https://x/secret-token-123 boom")
	if strings.Contains(got, "secret-token-123") {
		t.Errorf("token not scrubbed: %q", got)
	}
	if scrubber("")("no token here") != "no token here" {
		t.Errorf("empty-token scrubber must be identity")
	}
}

// --- helpers -------------------------------------------------------------------

func settingsKeyStatus() string { return "agent_source_last_sync_status" }
func settingsKeySHA() string    { return "agent_source_last_sync_sha" }

func mkTemplate(name, scope, origin, desc, body string, tools []byte) store.AgentTemplate {
	tpl := store.AgentTemplate{Name: name, Scope: scope, Description: desc, PromptBody: body, Tools: tools}
	if origin != "" {
		tpl.Origin.String, tpl.Origin.Valid = origin, true
	}
	return tpl
}

func fileNames(files []SourceFile) map[string]bool {
	m := map[string]bool{}
	for _, f := range files {
		m[f.Name] = true
	}
	return m
}

// buildFixtureRepo creates a bare repo with a commit that carries a
// .claude/agents dir (two .md role files, one non-md file, one nested dir, and one
// markdown-free subdir "empty-roster") plus a tag v1, and returns the bare repo
// path for a file:// clone.
func buildFixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	gitRun(t, "", "git", "init", "--bare", "--initial-branch=main", bare)
	gitRun(t, "", "git", "init", "--initial-branch=main", work)
	gitRun(t, work, "git", "config", "user.email", "t@example.com")
	gitRun(t, work, "git", "config", "user.name", "t")
	gitRun(t, work, "git", "config", "commit.gpgsign", "false")
	gitRun(t, work, "git", "remote", "add", "origin", bare)

	mustWrite(t, filepath.Join(work, ".claude/agents/coder.md"), "---\nname: coder\ndescription: builds\n---\nbody\n")
	mustWrite(t, filepath.Join(work, ".claude/agents/reviewer.md"), "---\nname: reviewer\ndescription: reviews\n---\nbody\n")
	mustWrite(t, filepath.Join(work, ".claude/agents/notes.txt"), "not a role file")
	mustWrite(t, filepath.Join(work, ".claude/agents/nested/inner.md"), "---\nname: inner\ndescription: x\n---\nbody\n")
	// An existing folder that holds no .md files — a valid empty source (distinct
	// from a missing folder, which errors).
	mustWrite(t, filepath.Join(work, ".claude/agents/empty-roster/readme.txt"), "no role files here")

	gitRun(t, work, "git", "add", "-A")
	gitRun(t, work, "git", "commit", "-m", "seed")
	gitRun(t, work, "git", "push", "origin", "main:main")
	gitRun(t, work, "git", "tag", "v1")
	gitRun(t, work, "git", "push", "origin", "v1")
	return bare
}

func gitRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
