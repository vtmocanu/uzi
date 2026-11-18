package main

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const sshCmdKey = "config --get core.sshCommand"

func TestParseRemote(t *testing.T) {
	ok := []struct {
		in   string
		want remoteInfo
	}{
		{"https://github.com/acme/widgets.git", remoteInfo{form: formHTTPS, host: "github.com"}},
		{"http://git.example/acme/widgets", remoteInfo{form: formHTTPS, host: "git.example"}},
		{"https://GitHub.COM/acme/widgets", remoteInfo{form: formHTTPS, host: "github.com"}},
		{"ssh://git@github.com:2222/acme/widgets.git", remoteInfo{form: formSSHURL, host: "github.com", user: "git", port: "2222", hasUser: true, hasPort: true}},
		{"ssh://github.com/acme/widgets.git", remoteInfo{form: formSSHURL, host: "github.com"}},
		{"git@github.com:acme/widgets.git", remoteInfo{form: formSCP, host: "github.com", user: "git", hasUser: true}},
		{"github.com:acme/widgets.git", remoteInfo{form: formSCP, host: "github.com"}},
		{"git@GitHub.COM:acme/widgets.git", remoteInfo{form: formSCP, host: "github.com", user: "git", hasUser: true}},
		{"git@github.com.:acme/widgets.git", remoteInfo{form: formSCP, host: "github.com.", user: "git", hasUser: true}},
		{"a@b@host:acme/widgets", remoteInfo{form: formSCP, host: "host", user: "a@b", hasUser: true}},
	}
	for _, tc := range ok {
		got, err := parseRemote(tc.in)
		if err != nil {
			t.Errorf("parseRemote(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseRemote(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	if a, _ := parseRemote("ssh://git@h/x"); a.form == formSCP {
		t.Errorf("ssh:// must not classify as scp")
	}
	if !formSCP.isSSH() || !formSSHURL.isSSH() || formHTTPS.isSSH() {
		t.Errorf("isSSH classification wrong")
	}

	for _, in := range []string{
		"ssh://host:/x", "ssh://@host/x", "[::1]:x", "./a:b", "/srv/a:b", "a/b", "",
		"git://github.com/acme/widgets.git", "file:///srv/repo", "git+ssh://git@github.com/acme/w",
		"ssh+git://git@github.com/acme/w", "ftp://h/x", "@host:x", "git@[::1]:x",
	} {
		if got, err := parseRemote(in); err == nil {
			t.Errorf("parseRemote(%q) = %+v, want refusal", in, got)
		} else if in != "" && !strings.Contains(err.Error(), in) {
			t.Errorf("refusal for %q should name the origin: %v", in, err)
		}
	}
}

func TestHostsMatch(t *testing.T) {
	cases := []struct {
		form          remoteForm
		resolved, web string
		want          bool
	}{
		{formHTTPS, "github.com", "github.com", true},
		{formHTTPS, "github.com", "gitlab.com", false},
		{formHTTPS, "", "github.com", false},
		{formHTTPS, "github.com", "", false},
		{formHTTPS, "ssh.github.com", "github.com", false},
		{formSCP, "ssh.github.com", "github.com", true},
		{formSSHURL, "ssh.github.com", "github.com", true},
		{formSSHURL, "ssh.github.com", "gitlab.com", false},
		{formSCP, "github.com", "ssh.github.com", false},
	}
	for _, tc := range cases {
		if got := hostsMatch(tc.form, tc.resolved, tc.web); got != tc.want {
			t.Errorf("hostsMatch(%v,%q,%q) = %v, want %v", tc.form, tc.resolved, tc.web, got, tc.want)
		}
	}
}

// hostCase drives the full auto-match handoff against one origin.
type hostCase struct {
	name       string
	origin     string
	web        string // repo WebURL; default https://github.com/acme/widgets
	stubOut    string // default "hostname github.com\n"
	stubErr    error
	stubFn     func(ctx context.Context, args []string) (string, error)
	nilSeam    bool
	env        map[string]string
	coreSSH    *string // non-nil: core.sshCommand present with this value
	coreSSHErr error   // non-nil: core.sshCommand lookup fails with this
	wantMatch  bool
	wantCalls  int
	wantArgv   []string
}

func runHostCase(t *testing.T, tc hostCase, extra ...string) (*uzicli.FakeClient, *handoffRecorder, string, int) {
	t.Helper()
	web := tc.web
	if web == "" {
		web = "https://github.com/acme/widgets"
	}
	fc := &uzicli.FakeClient{
		Repos:          []apitypes.RepoDTO{{ID: "p1", PathWithNamespace: "acme/widgets", WebURL: web}},
		CreatedTaskRun: taskRun("rh", "uzi/task/rh"),
		DispatchedRun:  taskRun("rh", "uzi/task/rh"),
	}
	rec := &handoffRecorder{gitOut: map[string]string{"remote get-url origin": tc.origin}}
	env, _ := handoffEnv(fc, rec)
	if tc.stubOut != "" {
		rec.sshCfg.out = tc.stubOut
	}
	rec.sshCfg.err = tc.stubErr
	rec.sshCfg.fn = tc.stubFn
	if tc.nilSeam {
		env.SSHConfig = nil
	}
	envVars := tc.env
	env.LookupEnv = func(k string) (string, bool) { v, ok := envVars[k]; return v, ok }
	if tc.coreSSH != nil {
		delete(rec.gitErr, sshCmdKey)
		rec.gitOut[sshCmdKey] = *tc.coreSSH
	}
	if tc.coreSSHErr != nil {
		rec.gitErr[sshCmdKey] = tc.coreSSHErr
	}
	args := append([]string{"handoff", "-m", "x"}, extra...)
	_, stderr, code := runCLI(t, env, args...)
	return fc, rec, stderr, code
}

func sshCmdVal(s string) *string { return &s }

func TestHandoffOriginHostResolution(t *testing.T) {
	bigOut := "hostname github.com\n" + strings.Repeat("x", sshConfigMaxOutput)
	cases := []hostCase{
		{name: "https same host", origin: "https://github.com/acme/widgets.git", wantMatch: true, wantCalls: 0},
		{name: "https uppercase host", origin: "https://GITHUB.com/acme/widgets.git", wantMatch: true},
		{name: "https other host", origin: "https://other.example/acme/widgets.git", wantMatch: false},
		{name: "https ignores GIT_SSH and core.sshCommand", origin: "https://github.com/acme/widgets.git",
			env: map[string]string{"GIT_SSH": "x", "GIT_SSH_COMMAND": ""}, coreSSH: sshCmdVal("x"), wantMatch: true},
		{name: "https ssh.github.com vs github.com", origin: "https://ssh.github.com/acme/widgets.git", wantMatch: false},
		{name: "scp equal host resolves same", origin: "git@github.com:acme/widgets.git", wantMatch: true, wantCalls: 1,
			wantArgv: []string{"-G", "-l", "git", "--", "github.com"}},
		{name: "scp no user omits -l", origin: "github.com:acme/widgets.git", wantMatch: true, wantCalls: 1,
			wantArgv: []string{"-G", "--", "github.com"}},
		{name: "scp equal raw host, stub resolves elsewhere", origin: "git@github.com:acme/widgets.git",
			stubOut: "hostname evil.example\n", wantMatch: false, wantCalls: 1},
		{name: "ssh:// equal raw host, stub resolves elsewhere", origin: "ssh://git@github.com/acme/widgets.git",
			stubOut: "hostname evil.example\n", wantMatch: false, wantCalls: 1},
		{name: "scp alias to web host", origin: "git@work:acme/widgets.git", stubOut: "user git\nhostname github.com\n",
			wantMatch: true, wantCalls: 1, wantArgv: []string{"-G", "-l", "git", "--", "work"}},
		{name: "ssh:// ssh.github.com:443", origin: "ssh://git@ssh.github.com:443/acme/widgets.git", stubOut: "hostname ssh.github.com\n",
			wantMatch: true, wantCalls: 1, wantArgv: []string{"-G", "-l", "git", "-p", "443", "--", "ssh.github.com"}},
		{name: "ssh:// port only", origin: "ssh://github.com:2222/acme/widgets.git", wantMatch: true, wantCalls: 1,
			wantArgv: []string{"-G", "-p", "2222", "--", "github.com"}},
		{name: "ssh.github.com alias not for other web host", origin: "git@x:acme/widgets.git", stubOut: "hostname ssh.github.com\n",
			web: "https://gitlab.example/acme/widgets", wantMatch: false, wantCalls: 1},
		{name: "trailing dot passed to stub", origin: "git@github.com.:acme/widgets.git", wantMatch: true, wantCalls: 1,
			wantArgv: []string{"-G", "-l", "git", "--", "github.com."}},
		{name: "uppercase origin and HostName normalized", origin: "git@GitHub.COM:acme/widgets.git", stubOut: "HostName GitHub.COM\n",
			wantMatch: true, wantCalls: 1, wantArgv: []string{"-G", "-l", "git", "--", "github.com"}},
		{name: "stub error", origin: "git@github.com:acme/widgets.git", stubErr: errors.New("boom"), wantMatch: false, wantCalls: 1},
		{name: "ssh not found", origin: "git@github.com:acme/widgets.git", stubErr: exec.ErrNotFound, wantMatch: false, wantCalls: 1},
		{name: "oversize output", origin: "git@github.com:acme/widgets.git", stubOut: bigOut, wantMatch: false, wantCalls: 1},
		{name: "no hostname line", origin: "git@github.com:acme/widgets.git", stubOut: "user git\nport 22\n", wantMatch: false, wantCalls: 1},
		{name: "leading dash host", origin: "ssh://git@-oProxyCommand=x/acme/widgets.git", wantMatch: false},
		{name: "leading dash user", origin: "-oProxyCommand=x@github.com:acme/widgets.git", wantMatch: false},
		{name: "leading dash port", origin: "ssh://github.com:-1/acme/widgets.git", wantMatch: false},
		{name: "GIT_SSH set", origin: "git@github.com:acme/widgets.git", env: map[string]string{"GIT_SSH": "/bin/x"}, wantMatch: false},
		{name: "GIT_SSH set empty", origin: "git@github.com:acme/widgets.git", env: map[string]string{"GIT_SSH": ""}, wantMatch: false},
		{name: "GIT_SSH_COMMAND set", origin: "ssh://git@github.com/acme/widgets.git", env: map[string]string{"GIT_SSH_COMMAND": "ssh -x"}, wantMatch: false},
		{name: "GIT_SSH_COMMAND set empty", origin: "git@github.com:acme/widgets.git", env: map[string]string{"GIT_SSH_COMMAND": ""}, wantMatch: false},
		{name: "core.sshCommand set", origin: "git@github.com:acme/widgets.git", coreSSH: sshCmdVal("ssh -x"), wantMatch: false},
		{name: "core.sshCommand set empty", origin: "git@github.com:acme/widgets.git", coreSSH: sshCmdVal(""), wantMatch: false},
		{name: "core.sshCommand sentinel value", origin: "git@github.com:acme/widgets.git", coreSSH: sshCmdVal("uzi-handoff-core-sshcommand-unset"), wantMatch: false},
		{name: "core.sshCommand exit 2", origin: "git@github.com:acme/widgets.git", coreSSHErr: exitCodeErr{2}, wantMatch: false},
		{name: "core.sshCommand plain error", origin: "git@github.com:acme/widgets.git", coreSSHErr: errors.New("git broke"), wantMatch: false},
		{name: "nil SSHConfig seam", origin: "git@github.com:acme/widgets.git", nilSeam: true, wantMatch: false},
		{name: "local path refused", origin: "/srv/acme/widgets.git", wantMatch: false},
		{name: "git:// refused", origin: "git://github.com/acme/widgets.git", wantMatch: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc, rec, _, code := runHostCase(t, tc)
			if tc.wantMatch {
				if code != uzicli.ExitOK || fc.LastCreateTaskRepoID != "p1" {
					t.Fatalf("exit=%d repo=%q, want a p1 match", code, fc.LastCreateTaskRepoID)
				}
			} else if code != uzicli.ExitUsage || fc.LastCreateTaskRepoID != "" {
				t.Fatalf("exit=%d repo=%q, want usage refusal", code, fc.LastCreateTaskRepoID)
			}
			if got := len(rec.sshCfg.calls); got != tc.wantCalls {
				t.Errorf("ssh -G calls = %d, want %d (%v)", got, tc.wantCalls, rec.sshCfg.calls)
			}
			if tc.wantArgv != nil && !reflect.DeepEqual(rec.sshCfg.calls[0], tc.wantArgv) {
				t.Errorf("ssh argv = %v, want %v", rec.sshCfg.calls[0], tc.wantArgv)
			}
		})
	}
}

// A connected repo whose WebURL is empty or invalid never matches, for either origin form.
func TestHandoffOriginHostBadWebURL(t *testing.T) {
	for _, web := range []string{"", "://bad", "not a url"} {
		for _, origin := range []string{"https://github.com/acme/widgets.git", "git@github.com:acme/widgets.git"} {
			fc := &uzicli.FakeClient{
				Repos:          []apitypes.RepoDTO{{ID: "p1", PathWithNamespace: "acme/widgets", WebURL: web}},
				CreatedTaskRun: taskRun("rh", "uzi/task/rh"),
				DispatchedRun:  taskRun("rh", "uzi/task/rh"),
			}
			rec := &handoffRecorder{gitOut: map[string]string{"remote get-url origin": origin}}
			env, _ := handoffEnv(fc, rec)
			if _, _, code := runCLI(t, env, "handoff", "-m", "x"); code != uzicli.ExitUsage || fc.LastCreateTaskRepoID != "" {
				t.Errorf("WebURL %q origin %q: exit=%d repo=%q, want refusal", web, origin, code, fc.LastCreateTaskRepoID)
			}
		}
	}
}

func TestHandoffOriginHostTimeout(t *testing.T) {
	old := sshConfigTimeout
	sshConfigTimeout = 30 * time.Millisecond
	t.Cleanup(func() { sshConfigTimeout = old })
	tc := hostCase{
		origin: "git@github.com:acme/widgets.git",
		stubFn: func(ctx context.Context, _ []string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
	fc, rec, stderr, code := runHostCase(t, tc)
	if code != uzicli.ExitUsage || fc.LastCreateTaskRepoID != "" {
		t.Fatalf("exit=%d repo=%q, want refusal after the ssh -G timeout", code, fc.LastCreateTaskRepoID)
	}
	if len(rec.sshCfg.calls) != 1 {
		t.Errorf("ssh calls = %d, want 1", len(rec.sshCfg.calls))
	}
	if !strings.Contains(stderr, "ssh -G") {
		t.Errorf("no-match error should mention the ssh -G failure:\n%s", stderr)
	}
}

func TestHandoffMixedHostSamePath(t *testing.T) {
	pA := apitypes.RepoDTO{ID: "pA", PathWithNamespace: "acme/widgets", WebURL: "https://gitlab.example/acme/widgets"}
	pB := apitypes.RepoDTO{ID: "pB", PathWithNamespace: "acme/widgets", WebURL: "https://github.com/acme/widgets"}
	orders := map[string][]apitypes.RepoDTO{"AB": {pA, pB}, "BA": {pB, pA}}
	origins := []string{"git@github.com:acme/widgets.git", "https://github.com/acme/widgets.git"}
	for name, repos := range orders {
		for _, origin := range origins {
			t.Run(name+" "+origin, func(t *testing.T) {
				mk := func() (*uzicli.FakeClient, *handoffRecorder, Env) {
					fc := &uzicli.FakeClient{
						Repos:          repos,
						CreatedTaskRun: taskRun("rm", "uzi/task/rm"),
						DispatchedRun:  taskRun("rm", "uzi/task/rm"),
					}
					rec := &handoffRecorder{gitOut: map[string]string{"remote get-url origin": origin}}
					env, _ := handoffEnv(fc, rec)
					return fc, rec, env
				}
				wantCalls := 0
				if strings.HasPrefix(origin, "git@") {
					wantCalls = 1
				}

				fc, rec, env := mk()
				if _, _, code := runCLI(t, env, "handoff", "-m", "x"); code != uzicli.ExitOK || fc.LastCreateTaskRepoID != "pB" {
					t.Fatalf("auto: exit=%d repo=%q, want pB", code, fc.LastCreateTaskRepoID)
				}
				if got := len(rec.sshCfg.calls); got != wantCalls {
					t.Errorf("auto: ssh calls = %d, want %d", got, wantCalls)
				}

				fc, _, env = mk()
				_, stderr, code := runCLI(t, env, "handoff", "--repo", "pA", "-m", "x")
				if code != uzicli.ExitUsage || fc.LastCreateTaskRepoID != "" {
					t.Fatalf("--repo pA: exit=%d repo=%q, want refusal", code, fc.LastCreateTaskRepoID)
				}
				if !strings.Contains(stderr, "gitlab.example") || !strings.Contains(stderr, "github.com") {
					t.Errorf("--repo pA error should name both hosts:\n%s", stderr)
				}

				fc, _, env = mk()
				if _, _, code := runCLI(t, env, "handoff", "--repo", "pB", "-m", "x"); code != uzicli.ExitOK || fc.LastCreateTaskRepoID != "pB" {
					t.Fatalf("--repo pB: exit=%d repo=%q, want pB", code, fc.LastCreateTaskRepoID)
				}
			})
		}
	}
}

// The classification of `git config --get core.sshCommand` must hold for the real git:
// unset is exit status 1 (absent); set to the empty string succeeds (present).
func TestSSHCommandConfiguredRealGit(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	if _, err := realGit(dir, "init", "-q"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}

	out, err := realGit(dir, "config", "--get", "core.sshCommand")
	var ec interface{ ExitCode() int }
	if err == nil || !errors.As(err, &ec) || ec.ExitCode() != 1 {
		t.Fatalf("unset core.sshCommand: err = %v, want an ExitCode()==1 error", err)
	}
	if present, cerr := sshCommandConfigured(out, err); present || cerr != nil {
		t.Errorf("unset classified present=%v err=%v, want absent", present, cerr)
	}

	if _, err := realGit(dir, "config", "core.sshCommand", ""); err != nil {
		t.Fatalf("setting core.sshCommand: %v", err)
	}
	out, err = realGit(dir, "config", "--get", "core.sshCommand")
	if err != nil {
		t.Fatalf("empty core.sshCommand: err = %v, want nil", err)
	}
	if present, cerr := sshCommandConfigured(out, err); !present || cerr != nil {
		t.Errorf("empty-set classified present=%v err=%v, want present", present, cerr)
	}

	if present, cerr := sshCommandConfigured("", exitCodeErr{2}); present || cerr == nil {
		t.Errorf("exit 2 must be an error, got present=%v err=%v", present, cerr)
	}
}

func TestSSHConfigCapture(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &sshCapture{max: 8, cancel: cancel}
	if n, err := c.Write([]byte("12345")); n != 5 || err != nil {
		t.Fatalf("first write = %d, %v", n, err)
	}
	if _, err := c.Write([]byte("67890")); !errors.Is(err, errSSHConfigOverflow) {
		t.Fatalf("overflowing write err = %v, want overflow", err)
	}
	if ctx.Err() == nil {
		t.Errorf("overflow must cancel the context")
	}
	if got := c.buf.String(); got != "12345678" {
		t.Errorf("captured %q, want exactly the cap", got)
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, errSSHConfigOverflow) {
		t.Errorf("writes after overflow must fail, got %v", err)
	}
}

func TestNewSSHConfigCmd(t *testing.T) {
	cmd := newSSHConfigCmd(context.Background(), []string{"-G", "--", "host"})
	if cmd.Dir != "" {
		t.Errorf("Dir = %q, want unset (inherit the push's cwd)", cmd.Dir)
	}
	if cmd.WaitDelay <= 0 {
		t.Errorf("WaitDelay = %v, want > 0", cmd.WaitDelay)
	}
	if !reflect.DeepEqual(cmd.Args, []string{"ssh", "-G", "--", "host"}) {
		t.Errorf("Args = %v", cmd.Args)
	}
	if cmd.Stdin != nil || cmd.Stderr != nil {
		t.Errorf("stdin and stderr must stay unconnected")
	}
}
