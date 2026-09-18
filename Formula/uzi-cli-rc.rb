# Homebrew formula for the uzi CLI release-candidate (RC) channel (vtmocanu/uzi#1378).
#
# This is the OPT-IN uzi-cli-rc formula: it tracks the newest uzi release CANDIDATE
# (vX.Y.Z-rc.N) tag, built FROM SOURCE exactly like the stable uzi-cli formula. It is
# mutually exclusive with uzi-cli via `conflicts_with` -- both install a binary named
# `uzi`, so only ONE channel is active at a time (`brew uninstall uzi-cli-rc && brew
# install uzi-cli` switches back to stable). Stable users are never moved onto a
# candidate; a dogfooder opts in with `brew install vtmocanu/tap/uzi-cli-rc`.
#
# This file is the SOURCE OF TRUTH and a TEMPLATE. `task brew-rc:formula` /
# `task brew-rc:publish` render it -- the reusable homebrew-tap.yml from
# github.com/vtmocanu/task, the SAME mechanic uzi-cli.rb and the sibling formulae
# (cc-statusline, fj-queue) use -- which substitutes the url + sha256 placeholder strings
# below for the RC tag's tarball + sha256 and pushes the rendered formula into the shared
# vtmocanu tap (vtmocanu/homebrew-tap). Release CI (.github/workflows/brew.yml) will run
# that render/publish on an RC tag once its RC-channel wiring lands (issue #1378 M3); until
# then the render path is exercised locally (scripts/brew-local-test.sh, `task
# brew-rc:formula`). The tap's Formula/uzi-cli-rc.rb is fully GENERATED from this one on
# each RC render, so edit HERE, never the tap copy (tap edits are overwritten). The
# placeholder strings are valid Ruby, so `task lint:formula` (ruby -c) still parses this.
#
# The render is a GLOBAL substitution, so this header must NOT spell the placeholder
# tokens literally: the url/sha256 lines below are the ONLY places they may appear.
#
# uzi builds FROM SOURCE: `brew install` downloads the RC tag source tarball and runs
# `cd api && go build ./cmd/uzi`. A public tarball needs no repo credentials, so this is
# the ordinary public-tap shape the sibling formulae (cc-statusline, fj-queue) use:
# GitHub's auto-generated /archive/refs/tags tarball, sha256-pinned.
#
# The version is scanned from the RC tag in the rendered url (vX.Y.Z-rc.N, leading v
# stripped); the CLI stamps v#{version} into the binary so `uzi version` reports the
# candidate it was built from.
#
# Local testing without a published tag: scripts/brew-local-test.sh (RC channel) renders
# these placeholders against a throwaway -rc.N tarball of the CURRENT source and asserts
# `uzi version`.
class UziCliRc < Formula
  desc "Terminal control surface for the uzi AI factory"
  homepage "https://github.com/vtmocanu/uzi"
  # Placeholders. task brew-rc:formula substitutes both for the tag's auto-generated source
  # tarball (https://github.com/vtmocanu/uzi/archive/refs/tags/<tag>.tar.gz) + its sha256.
  url "@@URL@@"
  sha256 "@@SHA256@@"
  license "MIT"

  depends_on "go" => :build
  conflicts_with "uzi-cli", because: "both install a `uzi` binary; only one channel can be active at a time"

  def install
    # The Go module is rooted at api/, so build from there. `output: bin/"uzi"` names
    # the binary `uzi` (the formula is uzi-cli, the command is uzi). The version stamp
    # makes `uzi version` report the tag it was built from.
    cd "api" do
      ldflags = "-s -w -X main.version=v#{version}"
      system "go", "build", *std_go_args(output: bin/"uzi", ldflags:), "./cmd/uzi"
    end
  end

  test do
    # `uzi version` is read-only and needs no server/token; it prints the stamped
    # version and exits 0.
    assert_match "v#{version}", shell_output("#{bin}/uzi version")
  end

  # Print-only: `caveats` returns a string brew prints after install/upgrade; it
  # touches nothing. We must NOT wire the Claude Code hook from the formula -- a
  # `post_install` runs sandboxed with an ephemeral `$HOME` and cannot reach the
  # real `~/.claude` (disproven in PRD #86 review), so we only nudge the user.
  def caveats
    <<~EOS
      uzi ships a self-updating skill for both Claude Code
      (~/.claude/skills/uzi-cli/SKILL.md) and Codex CLI
      (~/.agents/skills/uzi-cli/SKILL.md).

      Run `uzi skill install-hook [--target claude|codex|all]` once to wire a
      SessionStart hook so the skill refreshes at session start (otherwise it
      only refreshes on your next `uzi` command). Omit --target to wire every
      harness you have detected; Claude wires ~/.claude/settings.json, Codex
      wires $CODEX_HOME/hooks.json and needs a one-time `/hooks` review in
      Codex to trust it.

      `uzi skill uninstall-hook [--target ...]` removes the hook; `uzi skill
      status [--target ...]` shows whether it is wired, per harness.

      uzi's product docs are embedded in the binary and readable offline:
      `uzi docs list`, `uzi docs search <query>`, `uzi docs show <slug>`.
    EOS
  end
end
