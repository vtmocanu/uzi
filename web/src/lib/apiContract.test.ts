import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import type {
  Run,
  RunListItem,
  Repo,
  RunMessage,
  Schedule,
  ScheduleInput,
  Worker,
  AdminWorker,
  User,
  Memory,
  SecretMeta,
  RunUsage,
  UserSettings,
  CatalogEntry,
  AdminCliToken,
  Board,
  Card,
  BoardColumn,
  Skill,
  SettingsResponse,
  Branding,
  Chat,
  AgentTemplate,
} from "./apiTypes";

import runZero from "../../../fixtures/api-contract/run.zero.json";
import runFull from "../../../fixtures/api-contract/run.full.json";
import runListItemZero from "../../../fixtures/api-contract/run_list_item.zero.json";
import runListItemFull from "../../../fixtures/api-contract/run_list_item.full.json";
import repoZero from "../../../fixtures/api-contract/repo.zero.json";
import repoFull from "../../../fixtures/api-contract/repo.full.json";
import messageZero from "../../../fixtures/api-contract/message.zero.json";
import messageFull from "../../../fixtures/api-contract/message.full.json";
import scheduleZero from "../../../fixtures/api-contract/schedule.zero.json";
import scheduleFull from "../../../fixtures/api-contract/schedule.full.json";
import scheduleInputZero from "../../../fixtures/api-contract/schedule_input.zero.json";
import scheduleInputFull from "../../../fixtures/api-contract/schedule_input.full.json";
import workerZero from "../../../fixtures/api-contract/worker.zero.json";
import workerFull from "../../../fixtures/api-contract/worker.full.json";
import adminWorkerZero from "../../../fixtures/api-contract/admin_worker.zero.json";
import adminWorkerFull from "../../../fixtures/api-contract/admin_worker.full.json";
import userZero from "../../../fixtures/api-contract/user.zero.json";
import userFull from "../../../fixtures/api-contract/user.full.json";
import agentMemoryZero from "../../../fixtures/api-contract/agent_memory.zero.json";
import agentMemoryFull from "../../../fixtures/api-contract/agent_memory.full.json";
import secretZero from "../../../fixtures/api-contract/secret.zero.json";
import secretFull from "../../../fixtures/api-contract/secret.full.json";
import usageZero from "../../../fixtures/api-contract/usage.zero.json";
import usageFull from "../../../fixtures/api-contract/usage.full.json";
import userSettingsZero from "../../../fixtures/api-contract/user_settings.zero.json";
import userSettingsFull from "../../../fixtures/api-contract/user_settings.full.json";
import catalogEntryZero from "../../../fixtures/api-contract/catalog_entry.zero.json";
import catalogEntryFull from "../../../fixtures/api-contract/catalog_entry.full.json";
import cliTokenZero from "../../../fixtures/api-contract/cli_token.zero.json";
import cliTokenFull from "../../../fixtures/api-contract/cli_token.full.json";
import boardZero from "../../../fixtures/api-contract/board.zero.json";
import boardFull from "../../../fixtures/api-contract/board.full.json";
import cardZero from "../../../fixtures/api-contract/card.zero.json";
import cardFull from "../../../fixtures/api-contract/card.full.json";
import columnZero from "../../../fixtures/api-contract/column.zero.json";
import columnFull from "../../../fixtures/api-contract/column.full.json";
import skillZero from "../../../fixtures/api-contract/skill.zero.json";
import skillFull from "../../../fixtures/api-contract/skill.full.json";
import settingsZero from "../../../fixtures/api-contract/settings.zero.json";
import settingsFull from "../../../fixtures/api-contract/settings.full.json";
import brandingZero from "../../../fixtures/api-contract/branding.zero.json";
import brandingFull from "../../../fixtures/api-contract/branding.full.json";
import chatZero from "../../../fixtures/api-contract/chat.zero.json";
import chatFull from "../../../fixtures/api-contract/chat.full.json";
import agentTemplateZero from "../../../fixtures/api-contract/agent_template.zero.json";
import agentTemplateFull from "../../../fixtures/api-contract/agent_template.full.json";

// The api ⇄ SPA JSON wire-contract (PRD #982). This is the VITEST HALF; the Go
// half is api/internal/apitypes/contract_test.go. Neither reads the other: each
// side checks the SAME recorded fixtures under fixtures/api-contract/ with its
// OWN production definition (the Go struct, the TS type here), so a failure names
// the side that drifted. The precedent shape is fixtures/run-usage.
//
// Per DTO two fixtures:
//   <stem>.zero.json  == json.Marshal(T{})            — every null the type emits
//   <stem>.full.json  == json.Marshal(populate(T{}))  — key set + value kinds
//
// Three compile-time assertions per DTO (a tsc error here is a red gate:web, so
// the drift is caught by the typechecker, not only by vitest):
//   1. key-set equality, both directions (each `Exclude` must be `never`)
//   2. nullability: every null the zero value emits is accepted (ZeroOf exemptions)
//   3. value kinds: the populated shape is accepted with literal unions widened
// plus a runtime self-check that the fixtures exist and that assertion 2 is not
// vacuous (its zero.json really does carry a null).
//
// 🔴 THE FIXTURES ARE RECORDED, NOT AUTHORED. There is no -update flag; the Go
// half prints the exact JSON on a mismatch so re-recording is a copy-paste. The
// Go half additionally needs `go test -count=1` because fixtures/ sits above
// api/ and does not enter that module's test cache; vitest has no such cache.

// Widen<T> maps every string-literal union member to `string` (and number/boolean
// likewise), recursing through arrays and objects, while leaving null, undefined
// and unknown untouched. A JSON import types the fixture's "queued" as `string`,
// so a raw `= runZero` would false-fail on `status: RunStatus`; Widen keeps the
// nullability and kind checks (`title: string | null` still rejects a fixture
// null in a never-null slot) while ignoring enum narrowing. What it gives up is
// stated in the README under "What this cannot catch": an enum member the server
// adds and the TS union lacks is not caught here.
type Widen<T> = T extends string
  ? string
  : T extends number
    ? number
    : T extends boolean
      ? boolean
      : T extends readonly (infer E)[]
        ? Widen<E>[]
        : T extends object
          ? { [K in keyof T]: Widen<T[K]> }
          : T;

// ZeroOf<T, NeverNull> is Widen<T> with the named fields additionally accepting
// null. It is the per-field, reason-carrying exemption list of Decision 7: the
// zero fixture is json.Marshal(T{}), which emits null for every nil slice, but a
// mapper normalizes some of those to [] on the real wire, so the TS type is right
// to say never-null. Naming the field as a string literal means a rename breaks
// the exemption too, and the README cites the mapper line for each.
type ZeroOf<T, NeverNull extends keyof T = never> = {
  [K in keyof T]: K extends NeverNull ? Widen<T[K]> | null : Widen<T[K]>;
};

// ── Run ─────────────────────────────────────────────────────────────────────
// ZeroOf exemptions for Run (all three normalized to [] by capsOrEmpty in
// runToDTO): plan_changed_files (handler/workers.go:448), required_capabilities
// (:442), required_tools (:443); capsOrEmpty is handler/forge.go:148.
{
  // 1. key-set equality, both directions.
  const _runMissing: never = null as unknown as Exclude<keyof Run, keyof typeof runFull>;
  const _runExtra: never = null as unknown as Exclude<keyof typeof runFull, keyof Run>;
  // 2. nullability with the mapper-normalized exemptions applied.
  const _runZero: ZeroOf<Run, "plan_changed_files" | "required_capabilities" | "required_tools"> = runZero;
  // 3. value kinds, literal unions widened.
  const _runFull: Widen<Run> = runFull;
  void _runMissing;
  void _runExtra;
  void _runZero;
  void _runFull;
}

// ── RunListItem ─────────────────────────────────────────────────────────────
// RunListItem extends Run, so its extra keys inherit the same 7 drift fields; its
// own fields (repo_path, worker_name, owner_email, judge_verdict, judge_todo_count,
// is_revising) match. Same three ZeroOf exemptions, inherited from Run.
{
  const _runListItemMissing: never = null as unknown as Exclude<keyof RunListItem, keyof typeof runListItemFull>;
  const _runListItemExtra: never = null as unknown as Exclude<keyof typeof runListItemFull, keyof RunListItem>;
  const _runListItemZero: ZeroOf<RunListItem, "plan_changed_files" | "required_capabilities" | "required_tools"> =
    runListItemZero;
  const _runListItemFull: Widen<RunListItem> = runListItemFull;
  void _runListItemMissing;
  void _runListItemExtra;
  void _runListItemZero;
  void _runListItemFull;
}

// ── Repo (M2) ───────────────────────────────────────────────────────────────
// ZeroOf exemption: required_capabilities — normalized to [] by capsOrEmpty
// (handler/forge.go:148) inside repoToDTO (handler/forge.go:174), so the wire is
// [] though the zero marshal is null. Every other nil-slice/pointer field is typed
// nullable in TS (default_branch, pipeline, guardrail_override), so no exemption.
{
  const _repoMissing: never = null as unknown as Exclude<keyof Repo, keyof typeof repoFull>;
  const _repoExtra: never = null as unknown as Exclude<keyof typeof repoFull, keyof Repo>;
  const _repoZero: ZeroOf<Repo, "required_capabilities"> = repoZero;
  const _repoFull: Widen<Repo> = repoFull;
  void _repoMissing;
  void _repoExtra;
  void _repoZero;
  void _repoFull;
}

// ── RunMessage (M2) ─────────────────────────────────────────────────────────
// No ZeroOf exemption — every nullable field (agent, agent_instance, agent_label)
// is typed string|null, and payload is `unknown` (Widen<unknown> accepts anything,
// so payload is pinned for presence only — see README "What this cannot catch").
{
  const _messageMissing: never = null as unknown as Exclude<keyof RunMessage, keyof typeof messageFull>;
  const _messageExtra: never = null as unknown as Exclude<keyof typeof messageFull, keyof RunMessage>;
  const _messageZero: ZeroOf<RunMessage> = messageZero;
  const _messageFull: Widen<RunMessage> = messageFull;
  void _messageMissing;
  void _messageExtra;
  void _messageZero;
  void _messageFull;
}

// ── Schedule (M2) ───────────────────────────────────────────────────────────
// ZeroOf exemption: override_subagent_model — a plain bool column the mapper ALWAYS
// sets (handler/schedules.go:1598-1600, "plain bool column ... so always set it"),
// so the wire is a real boolean though the *bool zero marshals null.
//
// 🔴 DISCOVERED DRIFT (not in the M2 plan): schedule.next_fires. The mapper only sets
// NextFires for a recurring schedule with a valid cron (handler/schedules.go:1612-1614);
// a once (or invalid-cron) schedule leaves it nil, so the wire emits `null`, but TS
// types it `next_fires: string[]` (never-null). This is the ONE directive M4 deliberately
// KEEPS (every other #982 directive is removed): widening to `string[] | null` would
// surface two UNGUARDED `next_fires[0]` index sites (DefaultJobs.tsx / Schedules.tsx) as a
// red gate — a latent crash-on-once-schedule that is a BEHAVIOUR fix needing its own
// regression test (filed as its own bug), out of scope for this type-only PRD. labels is
// already typed `string[] | null` in TS, so it needs NO exemption.
{
  const _scheduleMissing: never = null as unknown as Exclude<keyof Schedule, keyof typeof scheduleFull>;
  const _scheduleExtra: never = null as unknown as Exclude<keyof typeof scheduleFull, keyof Schedule>;
  // @ts-expect-error #982: next_fires is null on the wire for once/invalid-cron schedules; widening to string[] | null is deferred to its own bug (two unguarded next_fires[0] index sites, DefaultJobs.tsx / Schedules.tsx, would crash — a behaviour fix with a regression test, not this type-only PRD)
  const _scheduleZero: ZeroOf<Schedule, "override_subagent_model"> = scheduleZero;
  const _scheduleFull: Widen<Schedule> = scheduleFull;
  void _scheduleMissing;
  void _scheduleExtra;
  void _scheduleZero;
  void _scheduleFull;
}

// ── ScheduleInput (M2, a REQUEST body) ──────────────────────────────────────
// The contract that bites here is the Go half's DisallowUnknownFields round-trip (a
// TS key the Go struct lacks is a runtime 400). For the zero-nullability check the
// direction is reversed: ScheduleRequest is CLIENT-produced, so json.Marshal of the Go
// zero value is not a real wire sample. Its tri-state *bool / non-omitempty slice fields
// marshal `null`, but the TS client omits them rather than sending null. So the
// never-null-optional fields (labels, auto_approve, wait_on_limit, enabled,
// override_subagent_model, sibling_group_id) are Omit-ted from the zero check (NOT given
// a false `| null` exemption); the genuinely `X | null` request fields stay checked.
{
  const _scheduleInputMissing: never = null as unknown as Exclude<keyof ScheduleInput, keyof typeof scheduleInputFull>;
  const _scheduleInputExtra: never = null as unknown as Exclude<keyof typeof scheduleInputFull, keyof ScheduleInput>;
  const _scheduleInputZero: ZeroOf<
    Omit<
      ScheduleInput,
      "labels" | "auto_approve" | "wait_on_limit" | "enabled" | "override_subagent_model" | "sibling_group_id"
    >
  > = scheduleInputZero;
  const _scheduleInputFull: Widen<ScheduleInput> = scheduleInputFull;
  void _scheduleInputMissing;
  void _scheduleInputExtra;
  void _scheduleInputZero;
  void _scheduleInputFull;
}

// ── Worker (M2) ─────────────────────────────────────────────────────────────
// ZeroOf exemption: capabilities — the text[] column yields a non-nil empty slice from
// pgx (WorkerDTO.Capabilities doc; passed through at handler/workers.go:203), so the wire
// is [] though the nil-slice zero marshals null.
//
// worker.docker: the mapper emits null for an external worker (boolPtrValue,
// handler/workers.go:201). DISCOVERED in M2 as a drift against the then-`docker?: boolean`
// TS type; RECONCILED in M4 — TS is now `docker?: boolean | null`, so the null is accepted
// and no directive or exemption is needed. Every other pointer field is typed X|null in TS.
{
  const _workerMissing: never = null as unknown as Exclude<keyof Worker, keyof typeof workerFull>;
  const _workerExtra: never = null as unknown as Exclude<keyof typeof workerFull, keyof Worker>;
  const _workerZero: ZeroOf<Worker, "capabilities"> = workerZero;
  const _workerFull: Widen<Worker> = workerFull;
  void _workerMissing;
  void _workerExtra;
  void _workerZero;
  void _workerFull;
}

// ── AdminWorker (M2) ────────────────────────────────────────────────────────
// AdminWorker extends Worker + owner_email; the embedded WorkerDTO fields marshal inline,
// so full.json carries the Worker keys AND owner_email. Same capabilities exemption and the
// same inherited worker.docker null, now reconciled in M4 (docker?: boolean | null) — no directive.
{
  const _adminWorkerMissing: never = null as unknown as Exclude<keyof AdminWorker, keyof typeof adminWorkerFull>;
  const _adminWorkerExtra: never = null as unknown as Exclude<keyof typeof adminWorkerFull, keyof AdminWorker>;
  const _adminWorkerZero: ZeroOf<AdminWorker, "capabilities"> = adminWorkerZero;
  const _adminWorkerFull: Widen<AdminWorker> = adminWorkerFull;
  void _adminWorkerMissing;
  void _adminWorkerExtra;
  void _adminWorkerZero;
  void _adminWorkerFull;
}

// ── User (M2) ───────────────────────────────────────────────────────────────
// No ZeroOf exemption — every nullable field (display_name, judge_anthropic_secret_id,
// judge_anthropic_secret_label, last_login) is typed X|null in TS.
{
  const _userMissing: never = null as unknown as Exclude<keyof User, keyof typeof userFull>;
  const _userExtra: never = null as unknown as Exclude<keyof typeof userFull, keyof User>;
  const _userZero: ZeroOf<User> = userZero;
  const _userFull: Widen<User> = userFull;
  void _userMissing;
  void _userExtra;
  void _userZero;
  void _userFull;
}

// ── Memory (M2, agent memory) ───────────────────────────────────────────────
// AgentMemoryDTO has NO nullable field, so its zero.json carries no null (declared
// nullable:false below). repo_id/repo_name are `omitempty` in Go: the /me/memory mapper
// ALWAYS sets them (handler/memory.go:37) so the real wire carries them and TS is right to
// require them, but json.Marshal(AgentMemoryDTO{}) drops an empty omitempty string, so the
// zero fixture legitimately lacks them. Their presence is verified via full.json (_missing);
// Omit them from the zero check rather than assert a key the zero-marshal correctly omits.
{
  const _agentMemoryMissing: never = null as unknown as Exclude<keyof Memory, keyof typeof agentMemoryFull>;
  const _agentMemoryExtra: never = null as unknown as Exclude<keyof typeof agentMemoryFull, keyof Memory>;
  const _agentMemoryZero: ZeroOf<Omit<Memory, "repo_id" | "repo_name">> = agentMemoryZero;
  const _agentMemoryFull: Widen<Memory> = agentMemoryFull;
  void _agentMemoryMissing;
  void _agentMemoryExtra;
  void _agentMemoryZero;
  void _agentMemoryFull;
}

// ── SecretMeta (M2) ─────────────────────────────────────────────────────────
// All-scalar (declared nullable:false below): its zero.json legitimately has no null.
{
  const _secretMissing: never = null as unknown as Exclude<keyof SecretMeta, keyof typeof secretFull>;
  const _secretExtra: never = null as unknown as Exclude<keyof typeof secretFull, keyof SecretMeta>;
  const _secretZero: ZeroOf<SecretMeta> = secretZero;
  const _secretFull: Widen<SecretMeta> = secretFull;
  void _secretMissing;
  void _secretExtra;
  void _secretZero;
  void _secretFull;
}

// ── RunUsage (M2) ───────────────────────────────────────────────────────────
// All-scalar (five numbers; declared nullable:false below): its zero.json has no null
// and its nullability pin is legitimately vacuous — recorded, not a failure.
{
  const _usageMissing: never = null as unknown as Exclude<keyof RunUsage, keyof typeof usageFull>;
  const _usageExtra: never = null as unknown as Exclude<keyof typeof usageFull, keyof RunUsage>;
  const _usageZero: ZeroOf<RunUsage> = usageZero;
  const _usageFull: Widen<RunUsage> = usageFull;
  void _usageMissing;
  void _usageExtra;
  void _usageZero;
  void _usageFull;
}

// ── UserSettings (M2) ───────────────────────────────────────────────────────
// ZeroOf exemption: sidebar_token_ids — the handler mapper runs uuidStrings, which returns
// a non-nil [] (handler/user_settings.go:65), so the wire is [] though the nil-slice zero
// marshals null. Every other field is typed X|null in TS.
{
  const _userSettingsMissing: never = null as unknown as Exclude<keyof UserSettings, keyof typeof userSettingsFull>;
  const _userSettingsExtra: never = null as unknown as Exclude<keyof typeof userSettingsFull, keyof UserSettings>;
  const _userSettingsZero: ZeroOf<UserSettings, "sidebar_token_ids"> = userSettingsZero;
  const _userSettingsFull: Widen<UserSettings> = userSettingsFull;
  void _userSettingsMissing;
  void _userSettingsExtra;
  void _userSettingsZero;
  void _userSettingsFull;
}

// ── CatalogEntry (M2 drift, RECONCILED M4) ──────────────────────────────────
// TS CatalogEntry lacked BOTH selector_kind (schedule.go:211) and mr_rework_enabled
// (schedule.go:221); M4 added them (optional, to avoid the scheduleCatalog mock cascade),
// so _catalogEntryExtra is now `never` and the directive is gone. labels is typed
// `string[] | null` in TS, so it needs no ZeroOf exemption.
{
  const _catalogEntryMissing: never = null as unknown as Exclude<keyof CatalogEntry, keyof typeof catalogEntryFull>;
  const _catalogEntryExtra: never = null as unknown as Exclude<keyof typeof catalogEntryFull, keyof CatalogEntry>;
  const _catalogEntryZero: ZeroOf<CatalogEntry> = catalogEntryZero;
  const _catalogEntryFull: Widen<CatalogEntry> = catalogEntryFull;
  void _catalogEntryMissing;
  void _catalogEntryExtra;
  void _catalogEntryZero;
  void _catalogEntryFull;
}

// ── CliToken / AdminCliToken (M2 drift, RECONCILED M4) ──────────────────────
// The cli_token fixture is recorded from Go's AdminCLITokenDTO, so it carries user_id and
// owner_email that the per-user CliToken type lacks. M2 checked it against CliToken and
// carried the two extra keys under a directive; M4 adds AdminCliToken (extends CliToken +
// user_id + owner_email) and checks the admin fixture against it, so _cliTokenExtra is now
// `never` and the directive is gone. (The web SPA does not fetch the admin list today — it
// is CLI-only — so AdminCliToken has no admin-page fetch to retype; it exists for this pin.)
{
  const _cliTokenMissing: never = null as unknown as Exclude<keyof AdminCliToken, keyof typeof cliTokenFull>;
  const _cliTokenExtra: never = null as unknown as Exclude<keyof typeof cliTokenFull, keyof AdminCliToken>;
  const _cliTokenZero: ZeroOf<AdminCliToken> = cliTokenZero;
  const _cliTokenFull: Widen<AdminCliToken> = cliTokenFull;
  void _cliTokenMissing;
  void _cliTokenExtra;
  void _cliTokenZero;
  void _cliTokenFull;
}

// ── Board (M3) ──────────────────────────────────────────────────────────────
// ZeroOf exemptions: columns, cards — the board mapper (buildBoard) always builds
// them with make([]columnDTO, 0, …) (handler/board.go:422) / make([]cardDTO, 0, …)
// (handler/board.go:580), so the wire is [] though boardDTO{} zero-marshals the nil
// slices to null. TS types both never-null (BoardColumn[] / Card[]). pipeline is
// typed PipelineStatus|null and bot_forge_user_id is optional-scalar, so neither
// needs an exemption. No drift: Board's key set matches boardDTO's.
{
  const _boardMissing: never = null as unknown as Exclude<keyof Board, keyof typeof boardFull>;
  const _boardExtra: never = null as unknown as Exclude<keyof typeof boardFull, keyof Board>;
  const _boardZero: ZeroOf<Board, "columns" | "cards"> = boardZero;
  // full.json exercises the nested Card shape too (the populator gives cards one element).
  const _boardFull: Widen<Board> = boardFull;
  void _boardMissing;
  void _boardExtra;
  void _boardZero;
  void _boardFull;
}

// ── Card (M3) ───────────────────────────────────────────────────────────────
// Card is the most-used type in the SPA (268 uses) and the element type of
// Board.cards; it is checked as its own pair here AND, via board.full.json, as the
// nested element above. ZeroOf exemptions: labels, assignee_ids — decodeLabels
// (handler/board.go:519) and decodeAssigneeIDs (handler/board.go:544) each return a
// non-nil [] (verified: []string{} / []int64{} on nil), so the wire is [] though
// cardDTO{} zero-marshals the nil slices to null. TS types labels as string[] and
// assignee_ids as an optional-never-null number[]. author/latest_run/pipeline are
// typed X|null in TS, so they need no exemption. No drift.
{
  const _cardMissing: never = null as unknown as Exclude<keyof Card, keyof typeof cardFull>;
  const _cardExtra: never = null as unknown as Exclude<keyof typeof cardFull, keyof Card>;
  const _cardZero: ZeroOf<Card, "labels" | "assignee_ids"> = cardZero;
  const _cardFull: Widen<Card> = cardFull;
  void _cardMissing;
  void _cardExtra;
  void _cardZero;
  void _cardFull;
}

// ── BoardColumn (M3, columnDTO → BoardColumn) ───────────────────────────────
// All-scalar (label_name, position; declared nullable:false below): its zero.json
// legitimately carries no null, so the null-presence guard is off and the
// nullability pin is vacuous — recorded, not a failure. There is no `Column` type;
// columnDTO maps to BoardColumn, the element type of Board.columns.
{
  const _columnMissing: never = null as unknown as Exclude<keyof BoardColumn, keyof typeof columnFull>;
  const _columnExtra: never = null as unknown as Exclude<keyof typeof columnFull, keyof BoardColumn>;
  const _columnZero: ZeroOf<BoardColumn> = columnZero;
  const _columnFull: Widen<BoardColumn> = columnFull;
  void _columnMissing;
  void _columnExtra;
  void _columnZero;
  void _columnFull;
}

// ── Skill (M3) ──────────────────────────────────────────────────────────────
// No ZeroOf exemption — every nullable field (user_id, updated_by) is typed
// string|null in TS, matching the *string fields on skillDTO. No drift.
{
  const _skillMissing: never = null as unknown as Exclude<keyof Skill, keyof typeof skillFull>;
  const _skillExtra: never = null as unknown as Exclude<keyof typeof skillFull, keyof Skill>;
  const _skillZero: ZeroOf<Skill> = skillZero;
  const _skillFull: Widen<Skill> = skillFull;
  void _skillMissing;
  void _skillExtra;
  void _skillZero;
  void _skillFull;
}

// ── SettingsResponse (M3, map-vs-struct) ────────────────────────────────────
// The ENVELOPE key set (settings, secrets, sources, slack_status, oidc_status,
// oidc_provider_name) matches between Go and TS, so _missing/_extra pin it correctly
// and stay in the full check. The VALUE-level check is where the map-vs-struct
// mismatch bites and is handled specially:
//   • settings — Go is a dynamic map[string]string; the mapper gives the fixture one
//     entry {"x":"x"}. TS settings is the CLOSED AppSettings interface (~20 fixed
//     keys), which {"x":"x"} cannot satisfy. This is inherent (a registry map, not a
//     struct), so settings is Omit-ted from the _zero/_full value assertions. The
//     ENVELOPE shape IS pinned; AppSettings' inner key contract is registry-driven and
//     out of this fixture's scope (see README "What this cannot catch").
//   • secrets, sources — Record<string,…>; Widen<Record<>> accepts {"x":…}, so they
//     stay in the value check. settingsResponse{} zero-marshals the nil maps to null,
//     but newSettingsResponse takes them from settings.AdminView, which builds all
//     three with make(...) (handler/settings.go:49 ← settings.go:1320-1322), so the
//     real wire is never null → secrets/sources get the ZeroOf NeverNull exemption.
{
  const _settingsMissing: never = null as unknown as Exclude<keyof SettingsResponse, keyof typeof settingsFull>;
  const _settingsExtra: never = null as unknown as Exclude<keyof typeof settingsFull, keyof SettingsResponse>;
  const _settingsZero: ZeroOf<Omit<SettingsResponse, "settings">, "secrets" | "sources"> = settingsZero;
  const _settingsFull: Widen<Omit<SettingsResponse, "settings">> = settingsFull;
  void _settingsMissing;
  void _settingsExtra;
  void _settingsZero;
  void _settingsFull;
}

// ── Branding (M3) ───────────────────────────────────────────────────────────
// All-scalar (strings + bools; declared nullable:false below): brandingResponse has
// no nullable field, so its zero.json legitimately carries no null. No drift.
{
  const _brandingMissing: never = null as unknown as Exclude<keyof Branding, keyof typeof brandingFull>;
  const _brandingExtra: never = null as unknown as Exclude<keyof typeof brandingFull, keyof Branding>;
  const _brandingZero: ZeroOf<Branding> = brandingZero;
  const _brandingFull: Widen<Branding> = brandingFull;
  void _brandingMissing;
  void _brandingExtra;
  void _brandingZero;
  void _brandingFull;
}

// ── Chat (M3, chatListDTO → Chat) ───────────────────────────────────────────
// No ZeroOf exemption — every nullable field (title, last_message_at,
// resume_of_run_id) is typed X|null in TS, matching the *string / *time.Time fields
// on chatListDTO. No drift.
{
  const _chatMissing: never = null as unknown as Exclude<keyof Chat, keyof typeof chatFull>;
  const _chatExtra: never = null as unknown as Exclude<keyof typeof chatFull, keyof Chat>;
  const _chatZero: ZeroOf<Chat> = chatZero;
  const _chatFull: Widen<Chat> = chatFull;
  void _chatMissing;
  void _chatExtra;
  void _chatZero;
  void _chatFull;
}

// ── AgentTemplate (M3) ──────────────────────────────────────────────────────
// No ZeroOf exemption and — checked against the Tools WARNING — NO drift. decodeTools
// (handler/agent_templates.go:130) returns nil (→ null) for an empty tools column, so
// agentTemplateDTO.Tools CAN be null on the wire; TS ALREADY types it `tools: string[]
// | null` (apiTypes.ts:150), so the null is accepted and no exemption or directive is
// needed. model/user_id/updated_by/origin are all typed X|null in TS too.
{
  const _agentTemplateMissing: never = null as unknown as Exclude<keyof AgentTemplate, keyof typeof agentTemplateFull>;
  const _agentTemplateExtra: never = null as unknown as Exclude<keyof typeof agentTemplateFull, keyof AgentTemplate>;
  const _agentTemplateZero: ZeroOf<AgentTemplate> = agentTemplateZero;
  const _agentTemplateFull: Widen<AgentTemplate> = agentTemplateFull;
  void _agentTemplateMissing;
  void _agentTemplateExtra;
  void _agentTemplateZero;
  void _agentTemplateFull;
}

// ── Runtime self-checks ─────────────────────────────────────────────────────
// A contract that passes on a missing fixture, or on a zero.json with no null in
// it, is the false-green shape this repo documents repeatedly. These fatal
// (never skip) so a gutted fixture reddens instead of quietly passing.

function read(name: string): string {
  const url = new URL(`../../../fixtures/api-contract/${name}`, import.meta.url);
  try {
    return readFileSync(url, "utf8");
  } catch (err) {
    throw new Error(
      `fixture unreadable: ${name}: ${String(err)} -- this contract asserts nothing ` +
        `without it, and skipping would look identical to passing`,
    );
  }
}

function hasNull(v: unknown): boolean {
  if (v === null) return true;
  if (Array.isArray(v)) return v.some(hasNull);
  if (typeof v === "object") return Object.values(v as Record<string, unknown>).some(hasNull);
  return false;
}

// Each DTO stem, and whether its Go struct has at least one nullable field (so
// its zero.json MUST carry a null — assertion 2 would be vacuous otherwise). Both
// hot M1 DTOs have nullable fields; an all-scalar DTO would be declared here with
// `nullable: false` rather than guarded, per the PRD.
const dtos: { stem: string; nullable: boolean }[] = [
  { stem: "run", nullable: true },
  { stem: "run_list_item", nullable: true },
  { stem: "repo", nullable: true },
  { stem: "message", nullable: true },
  { stem: "schedule", nullable: true },
  { stem: "schedule_input", nullable: true },
  { stem: "worker", nullable: true },
  { stem: "admin_worker", nullable: true },
  { stem: "user", nullable: true },
  // All-scalar / no nullable Go field: their zero.json legitimately carries no null,
  // so the null-presence guard is declared off (per the PRD, like RunUsage).
  { stem: "agent_memory", nullable: false },
  { stem: "secret", nullable: false },
  { stem: "usage", nullable: false },
  { stem: "user_settings", nullable: true },
  { stem: "catalog_entry", nullable: true },
  { stem: "cli_token", nullable: true },
  // M3 — the handler-package hot set.
  { stem: "board", nullable: true },
  { stem: "card", nullable: true },
  { stem: "skill", nullable: true },
  { stem: "settings", nullable: true },
  { stem: "chat", nullable: true },
  { stem: "agent_template", nullable: true },
  // All-scalar / no nullable Go field: their zero.json legitimately carries no null.
  { stem: "column", nullable: false },
  { stem: "branding", nullable: false },
];

describe("api-contract fixtures are present and discriminating", () => {
  for (const { stem, nullable } of dtos) {
    it(`${stem}: both fixtures are readable`, () => {
      expect(() => JSON.parse(read(`${stem}.zero.json`))).not.toThrow();
      expect(() => JSON.parse(read(`${stem}.full.json`))).not.toThrow();
    });

    it(`${stem}: zero.json carries a null iff the DTO has a nullable field`, () => {
      const zero = JSON.parse(read(`${stem}.zero.json`));
      if (nullable) {
        expect(hasNull(zero), `${stem}.zero.json has no null -- assertion 2 would be vacuous`).toBe(true);
      } else {
        expect(hasNull(zero), `${stem}.zero.json has a null but the DTO is declared all-scalar`).toBe(false);
      }
    });

    it(`${stem}: full.json contains no null (every field exercised)`, () => {
      const full = JSON.parse(read(`${stem}.full.json`));
      expect(hasNull(full), `${stem}.full.json has a null -- the populator left a field zero`).toBe(false);
    });
  }
});
