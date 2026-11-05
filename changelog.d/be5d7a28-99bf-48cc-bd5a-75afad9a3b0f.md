### Security

- **A run can no longer rewrite the repo agents its later claims use (#3913).**
  The repo-agent roster is read from git objects at the commit of the repository's configured default branch, recorded on the run's first claim, never from the working tree; a branch-seeded `mr_rework` or `ci_fix` run gets the default-branch agents. No agent turn starts until the server confirms the recorded commit. A default branch that cannot be resolved, a revision that cannot be recorded or read holds the run, and a hold keeps the resolved commit for the claim after a resume. Only a worker reporting `repo_agent_source_v1` claims a run that reads repo agents, its first claim included. The run view shows the revision.
