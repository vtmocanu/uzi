### Fixed

- **Checked plans on non-issue runs no longer carry milestones (#4555).**
  When a plan cross-check runs on a run that is not an issue run (for example an MR rework, CI fix or prompt run), the server drops any milestone list the lead submits instead of refusing the plan. The checked plan is reviewed and stored with no milestones, so a stray or malformed list no longer sends an auto-approved run to the human gate. Issue runs keep their milestone validation.
