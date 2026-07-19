# Homebrew formula for the uzi CLI (vtmocanu/uzi#64).
#
# This file is the SOURCE OF TRUTH. Release CI (the tag-only `publish_brew` job,
# M10) copies this formula into the shared vtmocanu tap (vtmocanu/homebrew-tap) and
# bumps ONLY the `tag:` line below to the release tag (there is no `version` line —
# Homebrew scans the version from the tag; see below). The tap's
# Formula/uzi-cli.rb is fully GENERATED from this one on every release, so edit
# HERE, never the tap copy (tap edits are overwritten).
#
# Distribution differs from example-app's: example-app vendors a shell SCRIPT into the tap, so
# consumers need no access to the private product repo. uzi builds FROM SOURCE, so
# Homebrew clones vtmocanu/uzi at the pinned `vX.Y.Z` tag over git-over-SSH using
# the teammate's own SSH key (the same key `brew tap` already uses). That means
# consumers DO need group-read on vtmocanu/uzi and the clone pulls the whole product
# source. This is an accepted departure from example-app's "no repo access" property; the
# tap README documents it (PRD #64 Risk 2).
#
# Homebrew scans the version from the `vX.Y.Z` tag (it strips the leading `v`, so
# tag `v0.3.1` => version `0.3.1`; verified with `brew audit`, which flags an
# explicit `version` line as redundant). The CLI stamps `v#{version}` into the
# binary so `uzi version` reports `vX.Y.Z` == the API version it was compiled
# against. Release CI (M10) bumps only the `tag:` below — exactly example-app's model.
#
# `revision:` is deliberately NOT pinned alongside the tag, matching example-app: doing
# so needs a two-commit dance to reference its own commit, and CI owns/protects the
# tags, so the reproducibility gain is marginal. `brew audit --strict` flags this;
# it is an accepted tradeoff for a CI-owned-tag private tap, not a defect.
#
# The module lives at api/go.mod, NOT the repo root, so the build must `cd "api"`
# before `go build ./cmd/uzi` (a repo-root `std_go_args` would not find the CLI).
#
# Local testing without a published tag: run scripts/brew-local-test.sh, which
# builds a throwaway local git repo from the CURRENT source, points `url` at it,
# and asserts `uzi version`.
class UziCli < Formula
  desc "Terminal control surface for the uzi AI factory"
  homepage "https://gitlab.example.com/vtmocanu/uzi"
  # Placeholder tag until the first tagged release wires M10's publish_brew job.
  url "git@gitlab.example.com:vtmocanu/uzi.git", using: :git, tag: "v0.0.0"

  depends_on "go" => :build

  def install
    # The Go module is rooted at api/, so build from there. `output: bin/"uzi"`
    # names the binary `uzi` (the formula is uzi-cli, the command is uzi). The
    # version stamp makes `uzi version` report the tag it was built from.
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
  # touches nothing. We must NOT wire the Claude Code hook from the formula — a
  # `post_install` runs sandboxed with an ephemeral `$HOME` and cannot reach the
  # real `~/.claude` (disproven in PRD #86 review), so we only nudge the user.
  def caveats
    <<~EOS
      uzi ships a self-updating Claude Code skill at
      ~/.claude/skills/uzi-cli/SKILL.md.

      If you use Claude Code, run `uzi skill install-hook` once to wire a
      SessionStart hook so the skill refreshes at session start (otherwise it
      only refreshes on your next `uzi` command).

      `uzi skill uninstall-hook` removes the hook; `uzi skill status` shows
      whether it is wired.
    EOS
  end
end
