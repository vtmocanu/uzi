# Landing and publishing a private security advisory fix

A fix for a draft GHSA stays private until the release that carries it: `.github/SECURITY.md`
forbids public reports, and uzi workers cannot see advisories, so the fix is written in a
maintainer session. Use the advisory's temporary private fork. A public PR discloses the
vulnerability through its diff whatever its wording, so take that route only when the user
explicitly authorizes public disclosure before the release.

## Build and review in the advisory fork

- Work in a worktree off `main`. Create the fork with
  `gh api -X POST repos/<owner>/<repo>/security-advisories/<GHSA>/forks`. Creation is
  asynchronous (up to about five minutes): wait until
  `gh api repos/<owner>/<fork>/branches/main` answers, then push the branch and open the PR
  against the fork's `main`.
- Actions, CodeRabbit and Greptile do not run in the fork. The local `task gate:*` targets
  are the CI; a peer session (Codex) is the reviewer. Ask it to review the exact pushed
  head SHA, not the working tree.
- The fork's `main` may lag upstream. The PR diff is computed from the merge-base, so no
  sync is needed.

## Land

- There is no documented API for the advisory page's merge button. Stack the approved
  commits unchanged on current `main` and gate that exact stack, every gate the diff
  touches plus `gate:repo`: `main` may have moved, and separate fixes may never have been
  gated together.
- The landing is an admin fast-forward past `protect-main`. If the harness refuses it
  (auto mode classifies it as a merge without review), stop and hand the user the exact
  command: `! git -C <worktree> push origin <stack-branch>:main`.
- Then watch `main` CI: it is the first real CI these commits get. If a run was cancelled
  because a newer push superseded it, watch the newest run whose head contains the commits
  (`git merge-base --is-ancestor <commit> <head>`); otherwise investigate the cancellation.
- CHANGELOG entries stay neutral ("Security: ..."), with no GHSA id or exploit detail,
  until the advisory is published.

## Publish (after the release)

- Cite the first **stable** release carrying the fix. An RC-only patched version points
  stable-channel users at a build they do not install, so ask `uzi-release` to promote.
- Verify the tag contains every fix commit (`git merge-base --is-ancestor`) and is a
  published non-prerelease.
- Update the advisory with a JSON payload file:
  `gh api -X PATCH repos/<owner>/<repo>/security-advisories/<GHSA> --input <file>`, carrying
  `summary`, the public `description` (Impact, Patches, Workarounds), `cwe_ids`, and full
  `vulnerabilities[]` entries: keep each existing `package` (`ecosystem`, `name`) and set
  `vulnerable_version_range: "< X.Y.Z"` and `patched_versions: "X.Y.Z"`.
- Close every fork PR first; publishing refuses (422, "a workspace with no open pull
  requests") while one is open. Use
  `gh api -X PATCH repos/<owner>/<fork>/pulls/<n> -f state=closed`; `gh pr close --comment`
  fails there on the comment.
- Publish with `-f state=published`. Publishing deletes the fork (its API then returns
  404), so remove the local worktrees and branches afterwards.
