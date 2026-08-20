package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// realGit is the production Env.Git: it runs `git <args>` in dir, returning the
// trimmed stdout on success. On failure the returned error carries git's stderr
// so the caller can report a clean, actionable message. Stdin and stderr are
// connected to the process so an interactive credential helper can prompt on a
// push/delete (the user's own creds — PRD #400 Decision 6); stdout is captured so
// a value read like `git remote get-url origin` is returned rather than printed.
// stderr is BOTH streamed (so the prompt is visible) and captured (so the error
// message is clean) via an io.MultiWriter.
func realGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// version is stamped at build time via -ldflags "-X main.version=vX.Y.Z"
// (the brew formula does this). It equals the uzi v* tag the binary was built
// from, making `uzi version` == the API version it was compiled against.
var version = "dev"

// Env holds the injectable dependencies of the CLI: where output goes, how the
// API client is built, and the config store. Tests build an Env with a fake
// client and an in-memory store; DefaultEnv wires the real ones.
type Env struct {
	Stdout    io.Writer
	Stderr    io.Writer
	Stdin     io.Reader
	StdoutTTY bool
	// StdinTTY reports whether stdin is a terminal. `uzi auth token` uses it to
	// decide between prompting (TTY) and reading a piped token (non-TTY); the
	// credential is never taken from argv (PRD #64).
	StdinTTY bool

	// NewClient builds the API client from resolved settings. M3 wires the
	// real-client stub; tests inject a fake; M7 replaces the default with the
	// live HTTP client.
	NewClient func(uzicli.Settings) uzicli.Client

	// Git runs `git <args>` in dir and returns its trimmed stdout, or an error
	// whose message includes git's stderr on failure. It is the CLI's one
	// shell-out seam (PRD #400 M3): `uzi handoff` uses it to detect the repo's
	// origin and to push local HEAD to the server-named uzi/task/<id> branch with
	// the USER's own credentials. DefaultEnv wires the real exec.Command (with
	// stdin/stderr connected so a credential helper can prompt); tests inject a
	// fake that records calls and returns canned output/errors, so no command test
	// forks git. Same injection-seam contract as NewClient above.
	Git func(dir string, args ...string) (string, error)

	// Store reads config/credentials. May be nil (e.g. no home dir), in which
	// case only env/flags supply settings.
	Store *uzicli.Store

	// SkillHome is the base dir the bundled skill installs under
	// (SkillHome/.claude/skills/uzi-cli/). Empty means os.UserHomeDir(). This is a
	// TEST/INJECTION SEAM only — DefaultEnv leaves it empty (real home) and it is
	// never populated from a flag/env/config, so the "no user-supplied path
	// component" property (PRD #64) holds. Tests point it at a temp dir.
	SkillHome string

	// AutoUpgradeSkill enables the best-effort skill self-upgrade run before every
	// command. DefaultEnv enables it; command tests leave it false so unrelated
	// tests do no filesystem writes. UZI_SKILL_AUTO_UPGRADE=0 disables it at runtime.
	AutoUpgradeSkill bool

	// CheckServerVersion enables the best-effort CLI-vs-server skew warning run
	// before every command (and inline in `uzi version`). DefaultEnv enables it;
	// command tests leave it false so unrelated tests make no network call and no
	// filesystem write. UZI_VERSION_CHECK=0 disables it at runtime.
	//
	// Same seam and same reasoning as AutoUpgradeSkill, deliberately: a test that has
	// to opt IN to a background behaviour is the only shape under which adding one
	// does not silently rewrite what every existing test exercises.
	CheckServerVersion bool
}

// DefaultEnv wires the real dependencies.
func DefaultEnv() Env {
	store, _ := uzicli.DefaultStore() // nil store tolerated; env/flags still work
	return Env{
		Stdout:             os.Stdout,
		Stderr:             os.Stderr,
		Stdin:              os.Stdin,
		StdoutTTY:          uzicli.IsTerminal(os.Stdout),
		StdinTTY:           uzicli.IsTerminal(os.Stdin),
		NewClient:          func(s uzicli.Settings) uzicli.Client { return uzicli.NewHTTPClient(s) },
		Git:                realGit,
		Store:              store,
		AutoUpgradeSkill:   true,
		CheckServerVersion: true,
	}
}

// globalFlags are the persistent flags shared by every command.
type globalFlags struct {
	json    bool
	url     string
	quiet   bool
	noColor bool
	context string
}

// Main builds the command tree, runs it, prints any error to stderr, and maps
// the result to a process exit code.
func Main(env Env, args []string) int {
	root := newRootCmd(env)
	root.SetArgs(args)
	err := root.Execute()
	if err != nil {
		// CellText, and this one line covers more untrusted text than any other in the
		// CLI (#180): statusError folds the server's {"error": "..."} body verbatim into
		// the message, so EVERY failing command prints server-supplied bytes here. It
		// does not go through the Printer, so the boundary in uzicli/output.go cannot
		// reach it.
		//
		// CellText, not SanitizeTTY: this is a one-line report with a fixed "uzi: "
		// prefix, so a newline in a server error would print a second line that carries
		// no prefix and reads as the CLI's own voice.
		_, _ = fmt.Fprintln(env.Stderr, "uzi:", uzicli.CellText(err.Error()))
	}
	return uzicli.ExitCodeFor(err)
}

func newRootCmd(env Env) *cobra.Command {
	gf := &globalFlags{}

	root := &cobra.Command{
		Use:   "uzi",
		Short: "Terminal control of the uzi factory, for humans and agents",
		Long: "uzi drives the factory from the terminal: list and follow runs, approve plan\n" +
			"gates, manage workers, and read admin state. Humans get tables; agents get\n" +
			"--json and documented exit codes.",
		Version: version,
		// We print errors and map exit codes ourselves in Main; silence cobra's
		// own error/usage dumping so output stays under our control.
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	// Best-effort skill self-upgrade before every command, so an agent always
	// drives the CLI with a SKILL.md matching THIS binary. It is deliberately
	// skipped for the `uzi skill ...` verbs (which manage the skill directly, and
	// would otherwise race their own report). Never fatal, never blocking: any
	// error warns on stderr and the real command still runs — a read-only $HOME in
	// CI must not break `uzi run list --json`.
	//
	// 🔴 THIS SEAM IS SINGLE-OCCUPANCY AND NOW HAS TWO CONSUMERS. cobra walks
	// ancestors and BREAKS at the first command carrying a PersistentPreRun, and
	// EnableTraverseRunHooks is unset, so a subcommand that later sets its own hook
	// silently disables BOTH of these with nothing failing anywhere. Add work HERE
	// rather than on a subcommand.
	//
	// The skew check runs SECOND, after the purely local skill work: it is the one
	// that may make a network call, and ordering the local work first means a slow
	// or hanging endpoint cannot delay it.
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		if env.AutoUpgradeSkill && !underSkillCmd(cmd) {
			maybeAutoUpgradeSkill(env)
		}
		maybeWarnVersionSkew(cmd, env, gf)
	}
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	if env.Stdin != nil {
		root.SetIn(env.Stdin)
	}
	// Wrap cobra's flag parse errors (unknown/invalid flag) into an *ExitError so
	// they map to the usage exit code (2), keeping ExitCodeFor's default free to be
	// the generic error (1) for anything that leaks unwrapped.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return uzicli.Exitf(uzicli.ExitUsage, "%v", err)
	})

	pf := root.PersistentFlags()
	pf.BoolVar(&gf.json, "json", false, "machine-readable JSON output (for agents)")
	pf.StringVar(&gf.url, "url", "", "uzi API base URL (overrides config and $UZI_URL)")
	pf.BoolVar(&gf.quiet, "quiet", false, "suppress non-essential output")
	pf.BoolVar(&gf.noColor, "no-color", false, "disable colour output")
	pf.StringVarP(&gf.context, "context", "c", "", "named context to use (overrides $UZI_CONTEXT and config current)")

	root.AddCommand(
		newLoginCmd(env, gf),
		newLogoutCmd(env, gf),
		newAuthCmd(env, gf),
		newWhoamiCmd(env, gf),
		newRunCmd(env, gf),
		newScheduleCmd(env, gf),
		newTUICmd(env, gf),
		newReviewCmd(env, gf),
		newFindingsCmd(env, gf),
		newWorkerCmd(env, gf),
		newTokenCmd(env, gf),
		newMemoryCmd(env, gf),
		newRepoCmd(env, gf),
		newHandoffCmd(env, gf),
		newAdminCmd(env, gf),
		newSkillCmd(env, gf),
		newVersionCmd(env, gf),
	)
	return root
}

// resolveContextName returns the active context name and whether it was chosen
// explicitly. Precedence (PRD #427 D1): --context/-c flag > $UZI_CONTEXT >
// config.Current > "default". An empty flag or empty $UZI_CONTEXT is treated as
// unset (the same `!= ""` "set" test resolveSettings uses for $UZI_URL), so
// `-c ""` falls through. `explicit` is true when the name came from the flag,
// $UZI_CONTEXT, or config.Current, and false only for the implicit "default"
// fallback — D9's existence check keys on that bit.
//
// It resolves the NAME ONLY; it does not validate that the context exists. A
// LoadConfig failure is surfaced as the error return rather than swallowed. When
// env.Store is nil there is no config, so the implicit "default" is returned.
func resolveContextName(env Env, gf *globalFlags) (string, bool, error) {
	if gf.context != "" {
		return gf.context, true, nil
	}
	if v := os.Getenv("UZI_CONTEXT"); v != "" {
		return v, true, nil
	}
	if env.Store == nil {
		return "default", false, nil
	}
	cfg, err := env.Store.LoadConfig()
	if err != nil {
		return "", false, err
	}
	if cfg.Current != "" {
		return cfg.Current, true, nil
	}
	return "default", false, nil
}

// resolveSettings resolves the active context (PRD #427 D1), reads its stored
// URL/token, applies D3 URL inheritance from the default context, then layers
// env vars and flags on top. Precedence for the URL: --url > $UZI_URL > active
// context URL > default context URL; for the token: $UZI_TOKEN > active context
// token. There is deliberately no --token flag — a credential must never land on
// argv (PRD #64).
func resolveSettings(env Env, gf *globalFlags) (uzicli.Settings, error) {
	name, explicit, err := resolveContextName(env, gf)
	if err != nil {
		return uzicli.Settings{}, uzicli.Exitf(uzicli.ExitAuth, "%v", err)
	}

	var s uzicli.Settings
	if env.Store != nil {
		cfg, err := env.Store.LoadConfig()
		if err != nil {
			return s, uzicli.Exitf(uzicli.ExitAuth, "%v", err)
		}
		creds, err := env.Store.LoadCredentials()
		if err != nil {
			return s, uzicli.Exitf(uzicli.ExitAuth, "%v", err)
		}
		// D9: an explicitly named context that exists in neither map is an error
		// before any client is built. The implicit "default" fallback must never
		// error on absence — a fresh user's first `uzi --url … login` has no
		// stored default context yet.
		if explicit {
			_, inCfg := cfg.Contexts[name]
			_, inCreds := creds.Contexts[name]
			if !inCfg && !inCreds {
				return s, uzicli.Exitf(uzicli.ExitUsage, "unknown context %q", name)
			}
		}
		s, err = env.Store.Resolve(name)
		if err != nil {
			return s, uzicli.Exitf(uzicli.ExitAuth, "%v", err)
		}
		// D3: a context with no stored URL inherits the default context's URL
		// (token does NOT inherit).
		if s.URL == "" && name != "default" {
			s.URL = cfg.Contexts["default"].URL
		}
	}
	if v := os.Getenv("UZI_URL"); v != "" {
		s.URL = v
	}
	if v := os.Getenv("UZI_TOKEN"); v != "" {
		s.Token = v
	}
	if gf.url != "" {
		s.URL = gf.url
	}
	return s, nil
}

// client resolves settings and builds the API client.
func (env Env) client(gf *globalFlags) (uzicli.Client, error) {
	s, err := resolveSettings(env, gf)
	if err != nil {
		return nil, err
	}
	return env.NewClient(s), nil
}

// printer builds the output renderer for the current global flags.
func (env Env) printer(gf *globalFlags) *uzicli.Printer {
	return uzicli.NewPrinter(env.Stdout, env.StdoutTTY, gf.json, gf.noColor, gf.quiet)
}
