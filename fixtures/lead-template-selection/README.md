# Lead template selection contract

`cases.json` has schema
`{ "cases": [{ "id": string, "names": string[], "selected_index": number | null }] }`.

Names are already in delivered order. Select the first case-insensitive exact
`lead` or `orchestrator`; null means no lead. Selection never sorts the names.
All lead-shaped own templates are withheld from subagents, including later matches.
These cases describe own templates, not repo-sourced agents.

Case IDs are unique and stable. Names within each case are distinct so LiveDB
readers can seed them. The Go reader is `TestSelectLeadFixture`; agent and LiveDB
readers consume this same table. Run Go tests with `-count=1` because the fixture
sits outside the Go module.
