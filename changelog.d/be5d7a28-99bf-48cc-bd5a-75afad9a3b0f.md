### Security

- **A run can no longer rewrite the repo agents its later claims use (#3913).**
  The repo-agent roster is read from git objects at the default-branch commit recorded on the run's first claim, never from the working tree; a branch-seeded `mr_rework` or `ci_fix` run gets the default-branch agents, an unreadable revision holds the run, and the run view shows the revision.
