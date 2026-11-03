package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agentsource"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Keep the real settings cache for last-applied SHA/invalidation; only the local
// source configuration differs from production.
type modelPinsSourceSettings struct {
	agentsource.SettingsReader
	url string
}

func (s modelPinsSourceSettings) AgentSourceEnabled(context.Context) (bool, error) {
	return true, nil
}
func (s modelPinsSourceSettings) AgentSourceRepoURL(context.Context) (string, error) {
	return s.url, nil
}
func (modelPinsSourceSettings) AgentSourceRef(context.Context) (string, error) {
	return "", nil
}
func (modelPinsSourceSettings) AgentSourceFolder(context.Context) (string, error) {
	return ".claude/agents", nil
}
func (modelPinsSourceSettings) AgentSourceCredential(context.Context) (string, error) {
	return "", nil
}

type modelPinsLocalAllowlist string

func (a modelPinsLocalAllowlist) AgentSourceBaseURLAllowed(raw string) bool {
	return raw == string(a)
}

func TestAgentSourceModelPinsLiveDB(t *testing.T) {
	ctx := context.Background()
	assertPins := func(t *testing.T, f agentSourceFixture, name, claude, codex string) {
		t.Helper()
		var gotClaude, gotCodex pgtype.Text
		var origin string
		if err := f.pool.QueryRow(ctx,
			"SELECT model, codex_model, origin FROM agent_templates WHERE name=$1", name).
			Scan(&gotClaude, &gotCodex, &origin); err != nil {
			t.Fatalf("read pins %s: %v", name, err)
		}
		if gotClaude.String != claude || gotClaude.Valid != (claude != "") ||
			gotCodex.String != codex || gotCodex.Valid != (codex != "") || origin != "synced" {
			t.Fatalf("%s stored pins: Claude=%+v Codex=%+v origin=%q; want %q / %q synced",
				name, gotClaude, gotCodex, origin, claude, codex)
		}
	}
	assertDTO := func(t *testing.T, f agentSourceFixture, name, raw, claude, codex string) {
		t.Helper()
		dto := f.getDTO(ctx, t)
		if dto.Staged == nil || !dto.Staged.Pending {
			t.Fatal("expected pending HTTP approval snapshot")
		}
		role := dto.stagedRole(t, name)
		if !role.OK || role.Model != raw || role.ClaudeModel != claude || role.CodexModel != codex {
			t.Fatalf("%s approval pins: raw=%q Claude=%q Codex=%q OK=%v; want %q / %q / %q",
				name, role.Model, role.ClaudeModel, role.CodexModel, role.OK, raw, claude, codex)
		}
	}

	t.Run("pre-upgrade-raw-snapshot", func(t *testing.T) {
		f := newAgentSourceFixture(ctx, t)
		// Literal old wire shape: only the single raw model existed before upgrade.
		_, err := f.q.UpsertAgentSourceStaged(ctx, store.UpsertAgentSourceStagedParams{
			FetchedAt:  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
			FetchedSha: "legacy-model-pins", SourceUrl: "https://example.com/agents.git",
			Roles: []byte(`[{"name":"endpoint-role","ok":true,"description":"legacy","model":"gpt-6-luna","tools":["Read"],"prompt_body":"legacy body\n"}]`),
			Diff:  []byte(`[]`),
		})
		if err != nil {
			t.Fatal(err)
		}
		assertDTO(t, f, "endpoint-role", "gpt-6-luna", "", "gpt-6-luna")
		result, err := f.rec.Apply(ctx, pgconv.UUID(f.admin), "legacy-model-pins")
		if err != nil || result.Applied != 1 || result.AlreadyApplied {
			t.Fatalf("legacy apply: %+v, %v", result, err)
		}
		assertPins(t, f, "endpoint-role", "", "gpt-6-luna")
		t.Log("executed legacy snapshot scenario")
	})

	t.Run("real-preview-apply-preview", func(t *testing.T) {
		f := newAgentSourceFixture(ctx, t)
		// All subprocess Git configuration is explicit and inherited GIT_* is
		// removed, including GIT_DIR/GIT_WORK_TREE from an outer test harness.
		scratch, err := filepath.Abs(os.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		scratch, err = filepath.EvalSymlinks(scratch)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", scratch)
		t.Setenv("GIT_CEILING_DIRECTORIES", scratch)
		repo := t.TempDir()
		gitEnv := make([]string, 0, len(os.Environ()))
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "GIT_") {
				gitEnv = append(gitEnv, entry)
			}
		}
		gitEnv = append(gitEnv, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CEILING_DIRECTORIES="+scratch,
			"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
			"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.com",
			"GIT_TERMINAL_PROMPT=0")
		git := func(t *testing.T, args ...string) {
			t.Helper()
			cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: literal git and test-owned local fixture arguments.
			cmd.Dir, cmd.Env = repo, gitEnv
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fixture git %v: %v\n%s", args, err, out)
			}
		}
		git(t, "init", "--initial-branch=main")
		folder := filepath.Join(repo, ".claude", "agents")
		if err := os.MkdirAll(folder, 0o700); err != nil {
			t.Fatal(err)
		}
		sourceURL := (&url.URL{Scheme: "file", Path: repo}).String()
		rec := agentsource.NewReconciler(f.q,
			modelPinsSourceSettings{SettingsReader: f.h.settings, url: sourceURL},
			modelPinsLocalAllowlist(sourceURL), f.pool, nil)
		f.h.agentSource = rec

		f.seedTemplate(ctx, t, "coder", "builtin", "embedded", true, "old", "old body\n")
		f.seedTemplate(ctx, t, "changed-synced-global", "global", "synced", false, "old", "old body\n")
		// Distinct stale values make both replacement and clearing observable.
		mustExecT(ctx, t, f.pool, `UPDATE agent_templates SET model='opus', codex_model='gpt-6-astra'
			WHERE name IN ('coder','changed-synced-global')`)

		type roleCase struct {
			name, raw, claude, codex, preview string
			applied                           agentsource.ApplyAction
		}
		rounds := []struct {
			name  string
			roles []roleCase
		}{
			{"codex-all-three-writers", []roleCase{
				{"coder", "gpt-6-sol", "", "gpt-6-sol", agentsource.DiffOverride, agentsource.ActionOverrideBuiltin},
				{"endpoint-role", "gpt-6-luna", "", "gpt-6-luna", agentsource.DiffAdd, agentsource.ActionAddGlobal},
				{"changed-synced-global", "gpt-6-sol", "", "gpt-6-sol", agentsource.DiffOverride, agentsource.ActionUpdateGlobal},
			}},
			{"codex-to-claude-and-empty", []roleCase{
				{"coder", "claude-custom-v2", "claude-custom-v2", "", agentsource.DiffOverride, agentsource.ActionOverrideBuiltin},
				{"endpoint-role", "", "", "", agentsource.DiffOverride, agentsource.ActionUpdateGlobal},
				{"changed-synced-global", "opus", "opus", "", agentsource.DiffOverride, agentsource.ActionUpdateGlobal},
			}},
		}
		preview := func(t *testing.T, wantChanged int) agentsource.Result {
			t.Helper()
			result, err := rec.Reconcile(ctx)
			if err != nil || result.Status != "ok" || !result.Restaged ||
				result.Failed != 0 || result.Staged != 3 || result.Changed != wantChanged {
				t.Fatalf("real reconcile: %+v, %v; want 3 roles, %d changes", result, err, wantChanged)
			}
			return result
		}
		assertDiff := func(t *testing.T, roles []roleCase, unchanged bool) {
			t.Helper()
			dto := f.getDTO(ctx, t)
			if dto.Staged == nil || len(dto.Staged.Diff) != len(roles) {
				t.Fatal("preview diff missing roles or contains unexpected removals")
			}
			actions := map[string]string{}
			for _, d := range dto.Staged.Diff {
				actions[d.Name] = d.Action
			}
			for _, role := range roles {
				want := role.preview
				if unchanged {
					want = agentsource.DiffUnchanged
				}
				if actions[role.name] != want {
					t.Fatalf("%s preview action=%q; want %q", role.name, actions[role.name], want)
				}
			}
		}
		scenarios := 0
		for _, round := range rounds {
			t.Run(round.name, func(t *testing.T) {
				for _, role := range round.roles {
					content := fmt.Sprintf("---\nname: %s\ndescription: source role\nmodel: %q\ntools: Read\n---\nsource body\n",
						role.name, role.raw)
					if err := os.WriteFile(filepath.Join(folder, role.name+".md"), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				git(t, "add", "--", ".claude/agents")
				git(t, "commit", "-m", "fixture role pins")
				first := preview(t, 3)
				assertDiff(t, round.roles, false)
				staged, err := f.q.GetAgentSourceStaged(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var rawRoles []map[string]json.RawMessage
				if err := json.Unmarshal(staged.Roles, &rawRoles); err != nil {
					t.Fatal(err)
				}
				if len(rawRoles) != len(round.roles) {
					t.Fatalf("raw staged roles=%d; want %d", len(rawRoles), len(round.roles))
				}
				for _, role := range round.roles {
					found := false
					for _, raw := range rawRoles {
						var name, model string
						if err := json.Unmarshal(raw["name"], &name); err != nil {
							t.Fatal(err)
						}
						if name != role.name {
							continue
						}
						found = true
						if value, ok := raw["model"]; ok {
							if err := json.Unmarshal(value, &model); err != nil {
								t.Fatal(err)
							}
						}
						_, hasClaude := raw["claude_model"]
						_, hasCodex := raw["codex_model"]
						if model != role.raw || hasClaude || hasCodex {
							t.Fatalf("%s raw snapshot model=%q derived fields=%v/%v; want raw %q only",
								name, model, hasClaude, hasCodex, role.raw)
						}
					}
					if !found {
						t.Fatalf("raw snapshot missing %s", role.name)
					}
					assertDTO(t, f, role.name, role.raw, role.claude, role.codex)
				}
				applied, err := rec.Apply(ctx, pgconv.UUID(f.admin), first.SHA)
				if err != nil || applied.Applied != 3 || applied.AlreadyApplied || len(applied.Outcomes) != 3 {
					t.Fatalf("apply: %+v, %v", applied, err)
				}
				for _, role := range round.roles {
					found := false
					for _, outcome := range applied.Outcomes {
						if outcome.Name == role.name && outcome.Action == role.applied {
							found = true
						}
					}
					if !found {
						t.Fatalf("%s missing apply outcome %s", role.name, role.applied)
					}
					assertPins(t, f, role.name, role.claude, role.codex)
					scenarios++
				}
				// Same SHA intentionally skips recomputation. A new commit with
				// identical files forces a genuine preview against applied rows.
				git(t, "commit", "--allow-empty", "-m", "force fresh preview")
				second := preview(t, 0)
				if second.SHA == first.SHA {
					t.Fatal("empty source commit did not advance fetched SHA")
				}
				assertDiff(t, round.roles, true)
				for _, role := range round.roles {
					assertDTO(t, f, role.name, role.raw, role.claude, role.codex)
				}
			})
		}
		if scenarios != 6 {
			t.Fatalf("executed %d role scenarios; want 6", scenarios)
		}
		t.Logf("executed %d preview/apply/fresh-preview role scenarios", scenarios)
	})
}
