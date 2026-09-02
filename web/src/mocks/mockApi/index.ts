// The in-browser mock implementation of the API client. Same surface, same
// response shapes, zero network: every method resolves from the in-memory store
// after a small jittered delay (so loading states render believably). Board
// moves, template CRUD, worker tokens, run inputs — all work locally.

import { patchRun } from "../store";
import { agentSourceApi } from "./agentSource";
import { agentsApi } from "./agents";
import { cliTokensApi } from "./cliTokens";
import { forgeApi } from "./forge";
import { judgeApi } from "./judge";
import { memoryApi } from "./memory";
import { notificationsApi } from "./notifications";
import { findingsApi } from "./findings";
import { runsApi } from "./runs";
import { chatApi } from "./chat";
import { secretsApi } from "./secrets";
import { workersApi } from "./workers";
import { settingsApi } from "./settings";
import { boardsApi } from "./boards";
import { usersApi } from "./users";
import { schedulesApi } from "./schedules";

// The mock surface is composed here from one partial object per domain module
// (`<domain>Api`), each owning its own state, helpers and methods. This file only
// spreads them into one object and re-exports the few internals importers reach for.
// Two guards keep the composition honest: `lib/api.ts`'s `api: typeof realApi`
// assignment is the COMPLETENESS guard (every realApi key must exist here), and
// `mockApi.parity.test.ts` is the DUPLICATE-KEY guard (no method contributed by two
// partials, since `{ ...a, ...b }` would let the later silently win).
export const mockApi = {
  ...usersApi,

  ...settingsApi,

  // ── Agent source (PRD #602 M5) ───────────────────────────────────────────────
  ...agentSourceApi,

  ...notificationsApi,

  // ── Secrets ─────────────────────────────────────────────────────────────────
  ...secretsApi,

  ...agentsApi,

  ...forgeApi,

  ...boardsApi,

  // ── Workers ─────────────────────────────────────────────────────────────────
  ...workersApi,

  ...runsApi,

  ...judgeApi,

  ...findingsApi,

  ...chatApi,

  ...cliTokensApi,

  ...memoryApi,

  ...schedulesApi,
};

// A run patch helper other mock surfaces can use (kept for symmetry/tests).
export { patchRun };
// sameContent is imported by lib/agentTemplateDriftContract.test.ts through the index.
export { sameContent } from "./agents";
// Judge-backlog internals imported by the fidelity/truncation fixtures through the index.
export {
  bucketOf,
  filterGroups,
  groupJudgeRecommendations,
  capBacklogRows,
  MOCK_BACKLOG_MAX_ROWS,
  type JudgeBacklogRow,
} from "./judge";
