# Landing and publishing a private security advisory fix

A fix for a draft GHSA stays private until the release that carries it: `.github/SECURITY.md`
forbids public reports, and uzi workers cannot see advisories, so the fix is written in a
maintainer session. Confirm the landing route with the user first. The advisory's temporary
private fork is the default; a public PR with neutral wording is the alternative.

## Build and review in the advisory fork

- Work in a worktree off `main`. Create the fork with
  `gh api -X POST repos/<owner>/<repo>/security-advisories/<GHSA>/forks`, push the branch
  there, and open the PR against the fork's `main`.
- Actions, CodeRabbit and Greptile do not run in the fork. The local `task gate:*` targets
  are the CI; a peer session (Codex) is the reviewer. Ask it to review the exact pushed
  head SHA, not the working tree.
- The fork's `main` may lag upstream. The PR diff is computed from the merge-base, so no
  sync is needed.

## Land

- There is no API for the advisory page's merge button. Stack the approved commits
  unchanged on current `main` and gate that exact stack (`gate:api`, `gate:agent`,
  `gate:repo`): `main` may have moved, and separate fixes may never have been gated
  together.
- The landing is an admin fast-forward past `protect-main`. The auto-mode classifier
  blocks an agent-run bypass push ("Merge Without Review"), so hand the user the exact
  command: `! git -C <worktree> push origin <stack-branch>:main`.
- Then watch `main` CI: it is the first real CI these commits get. A `cancelled` run was
  superseded by a newer push; watch the newest run whose head contains the commits
  (`git merge-base --is-ancestor <commit> <head>`).
- CHANGELOG entries stay neutral ("Security: ..."), with no GHSA id or exploit detail,
  until the advisory is published.

## Publish (after the release)

- Cite the first **stable** release carrying the fix. An RC-only patched version points
  stable-channel users at a build they do not install, so ask `uzi-release` to promote.
- Verify the tag contains every fix commit (`git merge-base --is-ancestor`) and is the
  non-prerelease Latest.
- Update the advisory: `gh api -X PATCH repos/<owner>/<repo>/security-advisories/<GHSA>`
  with `summary`, the public `description` (Impact, Patches, Workarounds), `cwe_ids`, and
  `vulnerabilities[].{vulnerable_version_range: "< X.Y.Z", patched_versions: "X.Y.Z"}`.
- Close every fork PR first; publishing refuses (422) while one is open.
  `gh pr close` fails on workspace repos (comments are forbidden), so use
  `gh api -X PATCH repos/<owner>/<fork>/pulls/<n> -f state=closed`.
- Publish with `-f state=published`. Publishing deletes the fork (its API then returns
  404), so remove the local worktrees and branches afterwards.
