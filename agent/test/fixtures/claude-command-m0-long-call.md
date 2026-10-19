# Claude guarded command: long in-process MCP call evidence (issue #2471, M0)

This is a sanitized evidence record for the off-CI, one-shot proofs run with
[claude-command-long-call.manual.ts](../claude-command-long-call.manual.ts) against an
inert sentinel handler. It records what was observed; it is not a recommendation for
production settings.

## What these runs establish, and what they do not

They establish TRANSPORT duration only: the bundled CLI did not cut an in-process MCP
call that ran for 60.1 minutes when the server timeout was 3660000 ms, and the result
reached the model. The handler was inert (it slept, then returned text), so there were no
descendant processes to drain. Descendant-drain evidence is N/A here. Production lane
deadline and drain semantics are covered separately by the M2 scaled tests and the M4
real-subprocess acceptance.

## Off-CI one-shot proofs

Both runs: SDK 0.3.292, CLI 2.1.292, serverTimeoutMs 3660000.

| Field | 11-minute | 60-minute boundary |
|---|---|---|
| durationMs | 660000 | 3606000 |
| queryBudgetMs | 780000 | 3726000 |
| startedAt | 2026-10-19T09:02:53.674Z | 2026-10-19T09:02:55.893Z |
| endedAt | 2026-10-19T09:13:55.126Z | 2026-10-19T10:03:03.021Z |
| totalMs | 661452 | 3607128 |
| handlerCompleted | true | true |
| handlerMs | 660001 | 3606005 |
| resultReachedModel | true | true |
| elapsedHandlerStartToModelMs | 660112 | 3606097 |
| errorToolResultText | null | null |
| queryOutcome | completed | completed |
| ok | true | true |

## CI knob proofs (claude-command-sdk-m0.integration.test.ts)

- A 3 s server timeout cut a 5 s handler at about 3.0 s; the model received the text
  `MCP server "uzi_cmd" tool "bash" timed out after 3s`. The handler kept running after
  the cut and completed: the CLI does not cancel the handler.
- A 15 s handler completed and its result reached the model under a 3660000 ms server
  timeout.

## Abort observation

After the query is aborted with a handler pending, the handler's `extra.signal` aborts and
the query settles, but the CLI keeps sending model requests until its process is torn down.
The first follow-up carries a tool_result marked as interrupted. When the scripted response
contains a further tool call, each later request carries a rejection ("The user doesn't want
to take this action right now"); one run saw dozens of such requests within about two
seconds. The M0 test asserts that a Bash call scripted into those responses reaches neither
a hook nor the handler. The production lane therefore never relies on the CLI to stop
commands: it owns the abort, and the existing executor teardown stops the CLI.
