import { describe, expect, it } from "vitest";
import type { PlanCrossCheckSummary } from "./apiTypes";
import { readFileSync } from "node:fs";
import type {
  Run,
  RunListItem,
  RunJob,
  Repo,
  RunMessage,
  Schedule,
  ScheduleInput,
  RunNowResponse,
  LastFireStarted,
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
  LatestRun,
  BoardColumn,
  Skill,
  SettingsResponse,
  Branding,
  Chat,
  AgentTemplate,
  SchedulePauseDTO,
  IncidentalFinding,
  Pull,
  PullDetail,
  Check,
  PullReview,
  MergeState,
  CIRun,
  CIRunDetail,
  CIJob,
  CIStep,
  RecoveryArchive,
  RecoveryArchiveSummary,
  RecoveryCustodyHold,
  RecoveryCustodyAggregate,
  RecoveryCustodyHolds,
  GuardrailOverrideRequest,
  CodexAccountRateLimit,
  CodexAdminRateLimitRow,
  HealthDoc,
  BuildInfo,
  Product,
  ProductOAuthClient,
  OAuthAuthorizeRequest,
  OAuthConnection,
  AdminOAuthConnection,
  OAuthRedirect,
  RotateProductClientSecretResponse,
  ProductToken,
  AdminProductToken,
  ProductTokenMint,
  V1Whoami,
  AdminDeleteProductResponse,
  MintableProduct,
  EgressProfile,
  ProductSkills,
} from "./apiTypes";

import buildInfoZero from "../../../fixtures/api-contract/build_info.zero.json";
import buildInfoFull from "../../../fixtures/api-contract/build_info.full.json";
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
import scheduleRunNowZero from "../../../fixtures/api-contract/schedule_run_now.zero.json";
import scheduleRunNowFull from "../../../fixtures/api-contract/schedule_run_now.full.json";
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
import schedulePauseZero from "../../../fixtures/api-contract/schedule_pause.zero.json";
import schedulePauseFull from "../../../fixtures/api-contract/schedule_pause.full.json";
import findingZero from "../../../fixtures/api-contract/finding.zero.json";
import findingFull from "../../../fixtures/api-contract/finding.full.json";
import pullZero from "../../../fixtures/api-contract/pull.zero.json";
import pullFull from "../../../fixtures/api-contract/pull.full.json";
import pullDetailZero from "../../../fixtures/api-contract/pull_detail.zero.json";
import pullDetailFull from "../../../fixtures/api-contract/pull_detail.full.json";
import checkZero from "../../../fixtures/api-contract/check.zero.json";
import checkFull from "../../../fixtures/api-contract/check.full.json";
import pullReviewZero from "../../../fixtures/api-contract/pull_review.zero.json";
import pullReviewFull from "../../../fixtures/api-contract/pull_review.full.json";
import mergeStateZero from "../../../fixtures/api-contract/merge_state.zero.json";
import mergeStateFull from "../../../fixtures/api-contract/merge_state.full.json";
import ciRunZero from "../../../fixtures/api-contract/ci_run.zero.json";
import ciRunFull from "../../../fixtures/api-contract/ci_run.full.json";
import ciRunDetailZero from "../../../fixtures/api-contract/ci_run_detail.zero.json";
import ciRunDetailFull from "../../../fixtures/api-contract/ci_run_detail.full.json";
import ciJobZero from "../../../fixtures/api-contract/ci_job.zero.json";
import ciJobFull from "../../../fixtures/api-contract/ci_job.full.json";
import ciStepZero from "../../../fixtures/api-contract/ci_step.zero.json";
import ciStepFull from "../../../fixtures/api-contract/ci_step.full.json";
import recoveryArchiveZero from "../../../fixtures/api-contract/recovery_archive.zero.json";
import recoveryArchiveFull from "../../../fixtures/api-contract/recovery_archive.full.json";
import recoveryArchiveSummaryZero from "../../../fixtures/api-contract/recovery_archive_summary.zero.json";
import recoveryArchiveSummaryFull from "../../../fixtures/api-contract/recovery_archive_summary.full.json";
import recoveryCustodyHoldZero from "../../../fixtures/api-contract/recovery_custody_hold.zero.json";
import recoveryCustodyHoldFull from "../../../fixtures/api-contract/recovery_custody_hold.full.json";
import recoveryCustodyAggregateZero from "../../../fixtures/api-contract/recovery_custody_aggregate.zero.json";
import recoveryCustodyAggregateFull from "../../../fixtures/api-contract/recovery_custody_aggregate.full.json";
import recoveryCustodyHoldsZero from "../../../fixtures/api-contract/recovery_custody_holds.zero.json";
import recoveryCustodyHoldsFull from "../../../fixtures/api-contract/recovery_custody_holds.full.json";
import guardrailOverrideRequestZero from "../../../fixtures/api-contract/guardrail_override_request.zero.json";
import guardrailOverrideRequestFull from "../../../fixtures/api-contract/guardrail_override_request.full.json";
import codexAccountRateLimitZero from "../../../fixtures/api-contract/codex_account_rate_limit.zero.json";
import codexAccountRateLimitFull from "../../../fixtures/api-contract/codex_account_rate_limit.full.json";
import codexAdminRateLimitRowZero from "../../../fixtures/api-contract/codex_admin_rate_limit_row.zero.json";
import codexAdminRateLimitRowFull from "../../../fixtures/api-contract/codex_admin_rate_limit_row.full.json";
import healthDocZero from "../../../fixtures/api-contract/health_doc.zero.json";
import healthDocFull from "../../../fixtures/api-contract/health_doc.full.json";
import productZero from "../../../fixtures/api-contract/product.zero.json";
import productFull from "../../../fixtures/api-contract/product.full.json";
import rotateProductClientSecretZero from "../../../fixtures/api-contract/rotate_product_client_secret.zero.json";
import rotateProductClientSecretFull from "../../../fixtures/api-contract/rotate_product_client_secret.full.json";
import oauthAuthorizeRequestZero from "../../../fixtures/api-contract/oauth_authorize_request.zero.json";
import oauthAuthorizeRequestFull from "../../../fixtures/api-contract/oauth_authorize_request.full.json";
import oauthRedirectResponseZero from "../../../fixtures/api-contract/oauth_redirect_response.zero.json";
import oauthRedirectResponseFull from "../../../fixtures/api-contract/oauth_redirect_response.full.json";
import oauthConnectionZero from "../../../fixtures/api-contract/oauth_connection.zero.json";
import oauthConnectionFull from "../../../fixtures/api-contract/oauth_connection.full.json";
import adminOauthConnectionZero from "../../../fixtures/api-contract/admin_oauth_connection.zero.json";
import adminOauthConnectionFull from "../../../fixtures/api-contract/admin_oauth_connection.full.json";
import productTokenZero from "../../../fixtures/api-contract/product_token.zero.json";
import runJobZero from "../../../fixtures/api-contract/run_job.zero.json";
import runJobFull from "../../../fixtures/api-contract/run_job.full.json";
import productTokenFull from "../../../fixtures/api-contract/product_token.full.json";
import adminProductTokenZero from "../../../fixtures/api-contract/admin_product_token.zero.json";
import adminProductTokenFull from "../../../fixtures/api-contract/admin_product_token.full.json";
import mintProductTokenZero from "../../../fixtures/api-contract/mint_product_token.zero.json";
import mintProductTokenFull from "../../../fixtures/api-contract/mint_product_token.full.json";
import adminDeleteProductZero from "../../../fixtures/api-contract/admin_delete_product.zero.json";
import adminDeleteProductFull from "../../../fixtures/api-contract/admin_delete_product.full.json";
import mintableProductZero from "../../../fixtures/api-contract/mintable_product.zero.json";
import mintableProductFull from "../../../fixtures/api-contract/mintable_product.full.json";
import v1WhoamiZero from "../../../fixtures/api-contract/v1_whoami.zero.json";
import v1WhoamiFull from "../../../fixtures/api-contract/v1_whoami.full.json";
import egressProfileZero from "../../../fixtures/api-contract/egress_profile.zero.json";
import egressProfileFull from "../../../fixtures/api-contract/egress_profile.full.json";
import productSkillsZero from "../../../fixtures/api-contract/product_skills.zero.json";
import productSkillsFull from "../../../fixtures/api-contract/product_skills.full.json";

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

// ProductZero is the recorded zero marshal of a Product (PRD #1910): allowed_job_types and the
// nested oauth_client's two slices are nil-slice nulls the handler normalizes to [].
type ProductZero = Omit<ZeroOf<Product, "allowed_job_types">, "oauth_client"> & {
  oauth_client: ZeroOf<ProductOAuthClient, "redirect_uris" | "scopes">;
};

// ZeroOf<T, NeverNull> is Widen<T> with the named fields additionally accepting
// null. It is the per-field, reason-carrying exemption list of Decision 7: the
// zero fixture is json.Marshal(T{}), which emits null for every nil slice, but a
// mapper normalizes some of those to [] on the real wire, so the TS type is right
// to say never-null. Naming the field as a string literal means a rename breaks
// the exemption too, and the README cites the mapper line for each.
type ZeroOf<T, NeverNull extends keyof T = never> = {
  [K in keyof T]: K extends NeverNull ? Widen<T[K]> | null : Widen<T[K]>;
};

// ── Build info: the public version response, including optional latest_rc. ──
const _buildInfoMissing: never = null as unknown as Exclude<keyof BuildInfo, keyof typeof buildInfoFull>;
const _buildInfoExtra: never = null as unknown as Exclude<keyof typeof buildInfoFull, keyof BuildInfo>;
const _buildInfoZero: ZeroOf<BuildInfo> = buildInfoZero;
const _buildInfoFull: Widen<BuildInfo> = buildInfoFull;
void _buildInfoMissing;
void _buildInfoExtra;
void _buildInfoZero;
void _buildInfoFull;

// ── Run ─────────────────────────────────────────────────────────────────────
// ZeroOf exemptions for Run (all normalized to [] by a mapper in runToDTO):
// plan_changed_files (handler/workers.go:448), required_capabilities (:442),
// required_tools (:443); capsOrEmpty is handler/forge.go:148. completion_unmet (PRD
// #1226 M5, D8) is the fourth — decodeLatestUnmet (handler/runs_dto.go) always returns
// [] (never null), so the TS type is a never-null string[] and the null zero fixture is
// exempted here like the capsOrEmpty trio. completion_deferred / completion_accepted (PRD
// #1227 M1) are the fifth and sixth — workersvc.CompletionScopeView (handler/runs_dto.go)
// always returns non-nil slices, so their TS types are never-null arrays and the null zero
// fixture is exempted the same way. (completion_revision is number|null, so it needs no
// exemption.)
{
  // 1. key-set equality, both directions.
  const _runMissing: never = null as unknown as Exclude<keyof Run, keyof typeof runFull>;
  const _runExtra: never = null as unknown as Exclude<keyof typeof runFull, keyof Run>;
  // 2. nullability with the mapper-normalized exemptions applied.
  const _runZero: ZeroOf<
    Run,
    | "plan_changed_files"
    | "required_capabilities"
    | "required_tools"
    | "completion_unmet"
    | "completion_deferred"
    | "completion_accepted"
    // PRD #1247 M1: credential_epochs is normalized to [] by runToDTO
    // (handler/runs_dto.go) but its zero fixture is a null nil-slice, so it is
    // never-null on the wire and the null zero value is exempted here.
    | "credential_epochs"
    // nonNilStrings in runs_dto.go normalizes the recorded nil slice to [].
    | "auto_approve_blocked_reasons"
  > = runZero;
  // 3. value kinds, literal unions widened.
  const _runFull: Widen<Run> = runFull;
  void _runMissing;
  void _runExtra;
  void _runZero;
  void _runFull;
}

// ── RunJob (PRD #1908 D-D) ──────────────────────────────────────────────────
// ZeroOf exemption: inputs — the mapper always emits [] for a job with no inputs, the null
// in run_job.zero.json is the nil-slice marshal. result and origin's members are typed
// `X | null`, no exemption.
{
  const _runJobMissing: never = null as unknown as Exclude<keyof RunJob, keyof typeof runJobFull>;
  const _runJobExtra: never = null as unknown as Exclude<keyof typeof runJobFull, keyof RunJob>;
  const _runJobZero: ZeroOf<RunJob, "inputs"> = runJobZero;
  const _runJobFull: Widen<RunJob> = runJobFull;
  void _runJobMissing;
  void _runJobExtra;
  void _runJobZero;
  void _runJobFull;
}

// ── RunListItem ─────────────────────────────────────────────────────────────
// RunListItem extends Run, so its extra keys inherit the same drift fields; its
// own fields (repo_path, worker_name, owner_email, judge_verdict, judge_todo_count,
// is_revising) match. Same six ZeroOf exemptions, inherited from Run.
{
  const _runListItemMissing: never = null as unknown as Exclude<keyof RunListItem, keyof typeof runListItemFull>;
  const _runListItemExtra: never = null as unknown as Exclude<keyof typeof runListItemFull, keyof RunListItem>;
  const _runListItemZero: ZeroOf<
    RunListItem,
    | "plan_changed_files"
    | "required_capabilities"
    | "required_tools"
    | "completion_unmet"
    | "completion_deferred"
    | "completion_accepted"
    | "credential_epochs"
    // nonNilStrings in runs_dto.go normalizes the recorded nil slice to [].
    | "auto_approve_blocked_reasons"
  > = runListItemZero;
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

// ── GuardrailOverrideRequest (issue #1432) ────────────────────────────────────
// One row of the admin cross-user override-request queue. ZeroOf exemption:
// findings — the handler (guardrailOverrideRequestDTO) always emits [] (never nil),
// so the TS type is a never-null GuardrailFinding[], but the zero marshal is a null
// nil-slice. Same capsOrEmpty idiom as Repo.required_capabilities. created_at is a
// non-pointer time.Time, so its zero fixture is the "0001-01-01T…" string, not null.
{
  const _gorMissing: never = null as unknown as Exclude<
    keyof GuardrailOverrideRequest,
    keyof typeof guardrailOverrideRequestFull
  >;
  const _gorExtra: never = null as unknown as Exclude<
    keyof typeof guardrailOverrideRequestFull,
    keyof GuardrailOverrideRequest
  >;
  const _gorZero: ZeroOf<GuardrailOverrideRequest, "findings"> = guardrailOverrideRequestZero;
  const _gorFull: Widen<GuardrailOverrideRequest> = guardrailOverrideRequestFull;
  void _gorMissing;
  void _gorExtra;
  void _gorZero;
  void _gorFull;
}

// ── HealthDoc (PRD #1484 M1) ──────────────────────────────────────────────────
// The admin-health document. ZeroOf exemption: checks — the registry always emits a
// non-nil slice, but json.Marshal(HealthDocDTO{}) is a null nil-slice (same idiom as
// GuardrailOverrideRequest.findings). snoozed_until/episode_id are string|null (present
// as null on the zero value, so they need no exemption — Widen<string|null> accepts
// null). counts is a nested all-number object. The nested HealthCheck/HealthEvidence
// types ride inside checks[0] of the full fixture.
{
  const _healthDocMissing: never = null as unknown as Exclude<keyof HealthDoc, keyof typeof healthDocFull>;
  const _healthDocExtra: never = null as unknown as Exclude<keyof typeof healthDocFull, keyof HealthDoc>;
  const _healthDocZero: ZeroOf<HealthDoc, "checks"> = healthDocZero;
  const _healthDocFull: Widen<HealthDoc> = healthDocFull;
  void _healthDocMissing;
  void _healthDocExtra;
  void _healthDocZero;
  void _healthDocFull;
}

// ── EgressProfile (PRD #1906 M1w) ────────────────────────────────────────────
// The admin egress profile ("site list"). ZeroOf exemptions: hosts,
// multi_publisher_override and warnings — egressProfileToDTO sends nonNilStrings(...) and
// make([]EgressProfileWarningDTO, 0, n) (handler/egress_profiles.go), so the wire always
// carries [], though json.Marshal(EgressProfileDTO{}) is a null nil-slice. created_by /
// updated_by are string|null (present as null on the zero value, no exemption needed). The
// nested EgressProfileWarning rides inside warnings[0] of the full fixture.
{
  const _egressProfileMissing: never = null as unknown as Exclude<keyof EgressProfile, keyof typeof egressProfileFull>;
  const _egressProfileExtra: never = null as unknown as Exclude<keyof typeof egressProfileFull, keyof EgressProfile>;
  const _egressProfileZero: ZeroOf<EgressProfile, "hosts" | "multi_publisher_override" | "warnings"> = egressProfileZero;
  const _egressProfileFull: Widen<EgressProfile> = egressProfileFull;
  void _egressProfileMissing;
  void _egressProfileExtra;
  void _egressProfileZero;
  void _egressProfileFull;
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
// schedule.next_fires reconciliation (issue #1003): the mapper only sets NextFires for a
// recurring schedule with a valid cron (handler/schedules_dto.go:125-126); a once (or
// invalid-cron) schedule leaves it nil, so the wire emits `null`. The type is now widened
// to `string[] | null` and the `next_fires[0]` index site (lib/scheduleList.ts
// nextFireOf) is guarded with `?.`, so `_scheduleZero` type-checks positively
// against the fixture with no directive. labels is likewise typed `string[] | null`.
{
  const _scheduleMissing: never = null as unknown as Exclude<keyof Schedule, keyof typeof scheduleFull>;
  const _scheduleExtra: never = null as unknown as Exclude<keyof typeof scheduleFull, keyof Schedule>;
  const _scheduleZero: ZeroOf<Schedule, "override_subagent_model"> = scheduleZero;
  const _scheduleFull: Widen<Schedule> = scheduleFull;
  void _scheduleMissing;
  void _scheduleExtra;
  void _scheduleZero;
  void _scheduleFull;
}

// runNowResponse in api/internal/handler/schedules.go allocates run_ids, started,
// and skips with make(..., 0, n), so the wire always carries arrays. Only the
// unmapped Go zero fixture has nil slices and needs these ZeroOf exemptions.
{
  const missing: never = null as unknown as Exclude<keyof RunNowResponse, keyof typeof scheduleRunNowFull>;
  const extra: never = null as unknown as Exclude<keyof typeof scheduleRunNowFull, keyof RunNowResponse>;
  const zero: ZeroOf<RunNowResponse, "run_ids" | "started" | "skips"> = scheduleRunNowZero;
  const full: Widen<RunNowResponse> = scheduleRunNowFull;
  const startedMissing: never = null as unknown as Exclude<keyof LastFireStarted, keyof typeof scheduleRunNowFull.started[number]>;
  const startedExtra: never = null as unknown as Exclude<keyof typeof scheduleRunNowFull.started[number], keyof LastFireStarted>;
  const persistedMissing: never = null as unknown as Exclude<keyof LastFireStarted, keyof typeof scheduleFull.last_fire.started[number]>;
  const persistedExtra: never = null as unknown as Exclude<keyof typeof scheduleFull.last_fire.started[number], keyof LastFireStarted>;
  void [missing, extra, zero, full, startedMissing, startedExtra, persistedMissing, persistedExtra];
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
      "labels" | "auto_approve" | "wait_on_limit" | "enabled" | "override_subagent_model" | "sibling_group_id" | "capacity_limit" | "capacity_room_needed" | "remove_label_on_dispatch"
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
//
// reported_runs (PRD #1390 M2c): a handler-overlaid field, so the TS type is OPTIONAL
// (WorkerReportedRun[]?, matching the outbox_* / retaining_unpublished_work overlay siblings) —
// the list/patch overlay and the DTO builders normalize the nil slice to [] on the real wire
// (handler/workers.go reportedRunsByWorker/builders), never null. The exemption is still needed:
// json.Marshal(WorkerDTO{}) emits null for the nil slice, so worker.zero.json carries an explicit
// `null`, which an optional `X[] | undefined` field rejects — the exemption tolerates it exactly
// like capabilities. run_disk (PRD #1809 M6) is the same handler-overlaid shape
// (handler/workers.go runDiskByWorker/builders) and takes the same exemption.
{
  const _workerMissing: never = null as unknown as Exclude<keyof Worker, keyof typeof workerFull>;
  const _workerExtra: never = null as unknown as Exclude<keyof typeof workerFull, keyof Worker>;
  const _workerZero: ZeroOf<Worker, "capabilities" | "reported_runs" | "run_disk"> = workerZero;
  const _workerFull: Widen<Worker> = workerFull;
  void _workerMissing;
  void _workerExtra;
  void _workerZero;
  void _workerFull;
}

// ── AdminWorker (M2) ────────────────────────────────────────────────────────
// AdminWorker extends Worker with owner and disk cleanup state; WorkerDTO fields marshal inline,
// so full.json carries Worker keys, owner_email, disk_pressure_volumes and cleanup_pending.
// Pressure volumes are always an array, including zero.json: no ZeroOf exemption.
// Same capabilities exemption, the same
// reported_runs exemption (PRD #1390 M2c, inherited from Worker), and the same inherited
// worker.docker null, now reconciled in M4 (docker?: boolean | null) — no directive.
{
  const _adminWorkerMissing: never = null as unknown as Exclude<keyof AdminWorker, keyof typeof adminWorkerFull>;
  const _adminWorkerExtra: never = null as unknown as Exclude<keyof typeof adminWorkerFull, keyof AdminWorker>;
  const _adminWorkerZero: ZeroOf<AdminWorker, "capabilities" | "reported_runs" | "run_disk"> = adminWorkerZero;
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
// ZeroOf exemption: sidebar_token_ids and sidebar_codex_account_ids — the handler mapper
// runs uuidStrings on both, which returns a non-nil [] (handler/user_settings.go), so the
// wire is [] though the nil-slice zero marshals null. Every other field is typed X|null in
// TS.
{
  const _userSettingsMissing: never = null as unknown as Exclude<keyof UserSettings, keyof typeof userSettingsFull>;
  const _userSettingsExtra: never = null as unknown as Exclude<keyof typeof userSettingsFull, keyof UserSettings>;
  const _userSettingsZero: ZeroOf<UserSettings, "sidebar_token_ids" | "sidebar_codex_account_ids"> = userSettingsZero;
  const _userSettingsFull: Widen<UserSettings> = userSettingsFull;
  void _userSettingsMissing;
  void _userSettingsExtra;
  void _userSettingsZero;
  void _userSettingsFull;
}

// ── Codex account rate limits (PRD #1209) ───────────────────────────────────
// The per-account meter and its admin row. ZeroOf exemptions: aliases + buckets on the
// account (non-omitempty Go slices whose nil-slice zero marshals null, normalized to [] by
// the M3 mapper), and accounts on the admin row (same nil-slice shape). last_success_at /
// stale are omitempty and simply absent from the zero fixture, so they need no exemption.
// The nested window + bucket DTOs are exercised inside these two fixtures' full values.
{
  const _codexAcctMissing: never = null as unknown as Exclude<
    keyof CodexAccountRateLimit,
    keyof typeof codexAccountRateLimitFull
  >;
  const _codexAcctExtra: never = null as unknown as Exclude<
    keyof typeof codexAccountRateLimitFull,
    keyof CodexAccountRateLimit
  >;
  const _codexAcctZero: ZeroOf<CodexAccountRateLimit, "aliases" | "buckets"> = codexAccountRateLimitZero;
  const _codexAcctFull: Widen<CodexAccountRateLimit> = codexAccountRateLimitFull;
  void _codexAcctMissing;
  void _codexAcctExtra;
  void _codexAcctZero;
  void _codexAcctFull;

  const _codexAdminMissing: never = null as unknown as Exclude<
    keyof CodexAdminRateLimitRow,
    keyof typeof codexAdminRateLimitRowFull
  >;
  const _codexAdminExtra: never = null as unknown as Exclude<
    keyof typeof codexAdminRateLimitRowFull,
    keyof CodexAdminRateLimitRow
  >;
  const _codexAdminZero: ZeroOf<CodexAdminRateLimitRow, "accounts"> = codexAdminRateLimitRowZero;
  const _codexAdminFull: Widen<CodexAdminRateLimitRow> = codexAdminRateLimitRowFull;
  void _codexAdminMissing;
  void _codexAdminExtra;
  void _codexAdminZero;
  void _codexAdminFull;
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
  const _latestRunMissing: never = null as unknown as Exclude<keyof LatestRun, keyof typeof cardFull.latest_run>;
  const _latestRunExtra: never = null as unknown as Exclude<keyof typeof cardFull.latest_run, keyof LatestRun>;
  void _latestRunMissing;
  void _latestRunExtra;
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

// ── SchedulePauseDTO (M2, PRD #1093) ────────────────────────────────────────
// No ZeroOf exemption — `until` is legitimately nullable (string|null, matching the
// Go *time.Time), so the zero value's until:null is accepted directly. `paused` is a
// plain bool. No drift: the key set matches SchedulePauseDTO exactly.
{
  const _schedulePauseMissing: never = null as unknown as Exclude<keyof SchedulePauseDTO, keyof typeof schedulePauseFull>;
  const _schedulePauseExtra: never = null as unknown as Exclude<keyof typeof schedulePauseFull, keyof SchedulePauseDTO>;
  const _schedulePauseZero: ZeroOf<SchedulePauseDTO> = schedulePauseZero;
  const _schedulePauseFull: Widen<SchedulePauseDTO> = schedulePauseFull;
  void _schedulePauseMissing;
  void _schedulePauseExtra;
  void _schedulePauseZero;
  void _schedulePauseFull;
}

// ── IncidentalFinding (PRD #1183 M3) ────────────────────────────────────────
// The first fixture pair for the Findings backlog row. No ZeroOf exemption: every nullable Go
// field is omitempty, so the zero value drops it rather than emitting a null (like AgentMemoryDTO,
// declared nullable:false below), and every required TS field is a non-omitempty Go field present
// in zero.json. The four M3 additions (dismiss_reason, set_via, evidence_preview, occurrences)
// plus finding_id/filed_* are optional in TS, so their absence from zero.json is accepted. No
// drift: the key set matches IncidentalFinding exactly.
{
  const _findingMissing: never = null as unknown as Exclude<keyof IncidentalFinding, keyof typeof findingFull>;
  const _findingExtra: never = null as unknown as Exclude<keyof typeof findingFull, keyof IncidentalFinding>;
  const _findingZero: ZeroOf<IncidentalFinding> = findingZero;
  const _findingFull: Widen<IncidentalFinding> = findingFull;
  void _findingMissing;
  void _findingExtra;
  void _findingZero;
  void _findingFull;
}

// ── Pull (PRD #1255 M2a) ────────────────────────────────────────────────────
// conflicts and run_id are typed `| null`, so the zero value's nulls are accepted with
// no exemption. No slice fields on the list row (checks live only on PullDetail, D4).
{
  const _pullMissing: never = null as unknown as Exclude<keyof Pull, keyof typeof pullFull>;
  const _pullExtra: never = null as unknown as Exclude<keyof typeof pullFull, keyof Pull>;
  const _pullZero: ZeroOf<Pull> = pullZero;
  const _pullFull: Widen<Pull> = pullFull;
  void _pullMissing;
  void _pullExtra;
  void _pullZero;
  void _pullFull;
}

// ── PullDetail (PRD #1255 M2a) ──────────────────────────────────────────────
// PullDetail extends Pull, so its full fixture carries the Pull scalars inline. ZeroOf
// exemptions: checks, reviews — the api always emits [] for an empty forge result, so
// the wire is never null though json.Marshal(PullDetailDTO{}) marshals the nil slices
// to null. merge is a nested object whose conflicts is natively `boolean | null`, so it
// needs no exemption (Widen keeps it nullable).
{
  const _pullDetailMissing: never = null as unknown as Exclude<keyof PullDetail, keyof typeof pullDetailFull>;
  const _pullDetailExtra: never = null as unknown as Exclude<keyof typeof pullDetailFull, keyof PullDetail>;
  const _pullDetailZero: ZeroOf<PullDetail, "checks" | "reviews"> = pullDetailZero;
  const _pullDetailFull: Widen<PullDetail> = pullDetailFull;
  void _pullDetailMissing;
  void _pullDetailExtra;
  void _pullDetailZero;
  void _pullDetailFull;
}

// ── Check (PRD #1255 M2a) ───────────────────────────────────────────────────
// All-scalar (declared nullable:false below): its zero.json legitimately carries no null.
{
  const _checkMissing: never = null as unknown as Exclude<keyof Check, keyof typeof checkFull>;
  const _checkExtra: never = null as unknown as Exclude<keyof typeof checkFull, keyof Check>;
  const _checkZero: ZeroOf<Check> = checkZero;
  const _checkFull: Widen<Check> = checkFull;
  void _checkMissing;
  void _checkExtra;
  void _checkZero;
  void _checkFull;
}

// ── PullReview (PRD #1255 M2a) ──────────────────────────────────────────────
// All-scalar (declared nullable:false below): its zero.json legitimately carries no null.
{
  const _pullReviewMissing: never = null as unknown as Exclude<keyof PullReview, keyof typeof pullReviewFull>;
  const _pullReviewExtra: never = null as unknown as Exclude<keyof typeof pullReviewFull, keyof PullReview>;
  const _pullReviewZero: ZeroOf<PullReview> = pullReviewZero;
  const _pullReviewFull: Widen<PullReview> = pullReviewFull;
  void _pullReviewMissing;
  void _pullReviewExtra;
  void _pullReviewZero;
  void _pullReviewFull;
}

// ── MergeState (PRD #1255 M2a) ──────────────────────────────────────────────
// conflicts is typed `boolean | null`, so the zero value's null is accepted with no
// exemption; the other fields are non-null scalars.
{
  const _mergeStateMissing: never = null as unknown as Exclude<keyof MergeState, keyof typeof mergeStateFull>;
  const _mergeStateExtra: never = null as unknown as Exclude<keyof typeof mergeStateFull, keyof MergeState>;
  const _mergeStateZero: ZeroOf<MergeState> = mergeStateZero;
  const _mergeStateFull: Widen<MergeState> = mergeStateFull;
  void _mergeStateMissing;
  void _mergeStateExtra;
  void _mergeStateZero;
  void _mergeStateFull;
}

// ── CIRun (PRD #1255 M2a) ───────────────────────────────────────────────────
// All-scalar (declared nullable:false below): its zero.json legitimately carries no null.
{
  const _ciRunMissing: never = null as unknown as Exclude<keyof CIRun, keyof typeof ciRunFull>;
  const _ciRunExtra: never = null as unknown as Exclude<keyof typeof ciRunFull, keyof CIRun>;
  const _ciRunZero: ZeroOf<CIRun> = ciRunZero;
  const _ciRunFull: Widen<CIRun> = ciRunFull;
  void _ciRunMissing;
  void _ciRunExtra;
  void _ciRunZero;
  void _ciRunFull;
}

// ── CIRunDetail (PRD #1255 M2a) ─────────────────────────────────────────────
// CIRunDetail extends CIRun (scalars inline in the full fixture). ZeroOf exemption:
// jobs — the api always emits [] for an empty run, so the wire is never null though
// the nil-slice zero marshals to null. unsupported is a non-null scalar.
{
  const _ciRunDetailMissing: never = null as unknown as Exclude<keyof CIRunDetail, keyof typeof ciRunDetailFull>;
  const _ciRunDetailExtra: never = null as unknown as Exclude<keyof typeof ciRunDetailFull, keyof CIRunDetail>;
  const _ciRunDetailZero: ZeroOf<CIRunDetail, "jobs"> = ciRunDetailZero;
  const _ciRunDetailFull: Widen<CIRunDetail> = ciRunDetailFull;
  void _ciRunDetailMissing;
  void _ciRunDetailExtra;
  void _ciRunDetailZero;
  void _ciRunDetailFull;
}

// ── CIJob (PRD #1255 M2a) ───────────────────────────────────────────────────
// ZeroOf exemption: steps — the api always emits [] (GitLab/Forgejo jobs have no
// steps), so the wire is never null though the nil-slice zero marshals to null. This is
// the only null in ci_job.zero.json, so ci_job is declared nullable:true below.
{
  const _ciJobMissing: never = null as unknown as Exclude<keyof CIJob, keyof typeof ciJobFull>;
  const _ciJobExtra: never = null as unknown as Exclude<keyof typeof ciJobFull, keyof CIJob>;
  const _ciJobZero: ZeroOf<CIJob, "steps"> = ciJobZero;
  const _ciJobFull: Widen<CIJob> = ciJobFull;
  void _ciJobMissing;
  void _ciJobExtra;
  void _ciJobZero;
  void _ciJobFull;
}

// ── CIStep (PRD #1255 M2a) ──────────────────────────────────────────────────
// All-scalar (declared nullable:false below): its zero.json legitimately carries no null.
{
  const _ciStepMissing: never = null as unknown as Exclude<keyof CIStep, keyof typeof ciStepFull>;
  const _ciStepExtra: never = null as unknown as Exclude<keyof typeof ciStepFull, keyof CIStep>;
  const _ciStepZero: ZeroOf<CIStep> = ciStepZero;
  const _ciStepFull: Widen<CIStep> = ciStepFull;
  void _ciStepMissing;
  void _ciStepExtra;
  void _ciStepZero;
  void _ciStepFull;
}

// ── RecoveryArchive (PRD #1296 M1) ──────────────────────────────────────────
// All optional fields are omitempty, so the zero.json carries no null (declared
// nullable:false below) — the finding shape.
{
  const _recoveryArchiveMissing: never = null as unknown as Exclude<keyof RecoveryArchive, keyof typeof recoveryArchiveFull>;
  const _recoveryArchiveExtra: never = null as unknown as Exclude<keyof typeof recoveryArchiveFull, keyof RecoveryArchive>;
  const _recoveryArchiveZero: ZeroOf<RecoveryArchive> = recoveryArchiveZero;
  const _recoveryArchiveFull: Widen<RecoveryArchive> = recoveryArchiveFull;
  void _recoveryArchiveMissing;
  void _recoveryArchiveExtra;
  void _recoveryArchiveZero;
  void _recoveryArchiveFull;
}

// ── RecoveryArchiveSummary (PRD #1296 M1) ────────────────────────────────────
// ZeroOf exemption: archives — the summary endpoint (M2) returns [] for a run with no
// captures, so the TS type is a never-null array though the nil-slice zero marshal is
// null (the CIRunDetail.jobs shape). counts is a nested all-scalar object (no null).
{
  const _recoverySummaryMissing: never = null as unknown as Exclude<keyof RecoveryArchiveSummary, keyof typeof recoveryArchiveSummaryFull>;
  const _recoverySummaryExtra: never = null as unknown as Exclude<keyof typeof recoveryArchiveSummaryFull, keyof RecoveryArchiveSummary>;
  const _recoverySummaryZero: ZeroOf<RecoveryArchiveSummary, "archives"> = recoveryArchiveSummaryZero;
  const _recoverySummaryFull: Widen<RecoveryArchiveSummary> = recoveryArchiveSummaryFull;
  void _recoverySummaryMissing;
  void _recoverySummaryExtra;
  void _recoverySummaryZero;
  void _recoverySummaryFull;
}

// ── RecoveryCustodyHold (PRD #1349 M1) ───────────────────────────────────────
// Its only nullable field is released_at (omitempty, dropped on the zero value) and
// worker_name/capture_state are omitempty strings, so the zero.json carries no null
// (declared nullable:false below) — the finding shape.
{
  const _recoveryCustodyHoldMissing: never = null as unknown as Exclude<keyof RecoveryCustodyHold, keyof typeof recoveryCustodyHoldFull>;
  const _recoveryCustodyHoldExtra: never = null as unknown as Exclude<keyof typeof recoveryCustodyHoldFull, keyof RecoveryCustodyHold>;
  const _recoveryCustodyHoldZero: ZeroOf<RecoveryCustodyHold> = recoveryCustodyHoldZero;
  const _recoveryCustodyHoldFull: Widen<RecoveryCustodyHold> = recoveryCustodyHoldFull;
  void _recoveryCustodyHoldMissing;
  void _recoveryCustodyHoldExtra;
  void _recoveryCustodyHoldZero;
  void _recoveryCustodyHoldFull;
}

// ── RecoveryCustodyAggregate (PRD #1349 M1) ──────────────────────────────────
// All four fields are ints, so the zero.json carries no null (declared nullable:false).
{
  const _recoveryCustodyAggregateMissing: never = null as unknown as Exclude<keyof RecoveryCustodyAggregate, keyof typeof recoveryCustodyAggregateFull>;
  const _recoveryCustodyAggregateExtra: never = null as unknown as Exclude<keyof typeof recoveryCustodyAggregateFull, keyof RecoveryCustodyAggregate>;
  const _recoveryCustodyAggregateZero: ZeroOf<RecoveryCustodyAggregate> = recoveryCustodyAggregateZero;
  const _recoveryCustodyAggregateFull: Widen<RecoveryCustodyAggregate> = recoveryCustodyAggregateFull;
  void _recoveryCustodyAggregateMissing;
  void _recoveryCustodyAggregateExtra;
  void _recoveryCustodyAggregateZero;
  void _recoveryCustodyAggregateFull;
}

// ── RecoveryCustodyHolds (PRD #1349 M1) ──────────────────────────────────────
// ZeroOf exemption: holds — the endpoint (M5) returns [] for an owner with no holds, so the
// TS type is a never-null array though the nil-slice zero marshal is null (the archives
// shape). aggregate is a nested all-scalar object (no null).
{
  const _recoveryCustodyHoldsMissing: never = null as unknown as Exclude<keyof RecoveryCustodyHolds, keyof typeof recoveryCustodyHoldsFull>;
  const _recoveryCustodyHoldsExtra: never = null as unknown as Exclude<keyof typeof recoveryCustodyHoldsFull, keyof RecoveryCustodyHolds>;
  const _recoveryCustodyHoldsZero: ZeroOf<RecoveryCustodyHolds, "holds"> = recoveryCustodyHoldsZero;
  const _recoveryCustodyHoldsFull: Widen<RecoveryCustodyHolds> = recoveryCustodyHoldsFull;
  void _recoveryCustodyHoldsMissing;
  void _recoveryCustodyHoldsExtra;
  void _recoveryCustodyHoldsZero;
  void _recoveryCustodyHoldsFull;
}

// ── Product tokens (PRD #1907 M1) ────────────────────────────────────────────
// ZeroOf exemption: scopes — product_tokens.scopes is NOT NULL with a non-empty CHECK
// (migration 00270), so every row the mappers build carries a non-empty array; the null
// is only the nil-slice zero marshal. The nested product_token in the mint response
// carries the same exemption, spelled as a nested ZeroOf. deleted_at, last_used_at,
// last_used_ip, expires_at and the whoami product are typed `X | null`, no exemption.
{
  const _productMissing: never = null as unknown as Exclude<keyof Product, keyof typeof productFull>;
  const _productExtra: never = null as unknown as Exclude<keyof typeof productFull, keyof Product>;
  // allowed_job_types: jobTypesOrEmpty (handler/admin_products.go) normalizes the nil-slice
  // null in product.zero.json to [], so it is never null on the wire.
  const _productZero: ProductZero = productZero;
  const _productFull: Widen<Product> = productFull;
  void _productMissing;
  void _productExtra;
  void _productZero;
  void _productFull;
}
{
  const _productTokenMissing: never = null as unknown as Exclude<keyof ProductToken, keyof typeof productTokenFull>;
  const _productTokenExtra: never = null as unknown as Exclude<keyof typeof productTokenFull, keyof ProductToken>;
  const _productTokenZero: ZeroOf<ProductToken, "scopes"> = productTokenZero;
  const _productTokenFull: Widen<ProductToken> = productTokenFull;
  void _productTokenMissing;
  void _productTokenExtra;
  void _productTokenZero;
  void _productTokenFull;
}
{
  const _adminProductTokenMissing: never = null as unknown as Exclude<keyof AdminProductToken, keyof typeof adminProductTokenFull>;
  const _adminProductTokenExtra: never = null as unknown as Exclude<keyof typeof adminProductTokenFull, keyof AdminProductToken>;
  const _adminProductTokenZero: ZeroOf<AdminProductToken, "scopes"> = adminProductTokenZero;
  const _adminProductTokenFull: Widen<AdminProductToken> = adminProductTokenFull;
  void _adminProductTokenMissing;
  void _adminProductTokenExtra;
  void _adminProductTokenZero;
  void _adminProductTokenFull;
}
{
  const _mintProductTokenMissing: never = null as unknown as Exclude<keyof ProductTokenMint, keyof typeof mintProductTokenFull>;
  const _mintProductTokenExtra: never = null as unknown as Exclude<keyof typeof mintProductTokenFull, keyof ProductTokenMint>;
  const _mintProductTokenZero: { token: string; product_token: ZeroOf<ProductToken, "scopes"> } = mintProductTokenZero;
  const _mintProductTokenFull: Widen<ProductTokenMint> = mintProductTokenFull;
  void _mintProductTokenMissing;
  void _mintProductTokenExtra;
  void _mintProductTokenZero;
  void _mintProductTokenFull;
}
{
  const _v1WhoamiMissing: never = null as unknown as Exclude<keyof V1Whoami, keyof typeof v1WhoamiFull>;
  const _v1WhoamiExtra: never = null as unknown as Exclude<keyof typeof v1WhoamiFull, keyof V1Whoami>;
  const _v1WhoamiZero: ZeroOf<V1Whoami, "scopes"> = v1WhoamiZero;
  const _v1WhoamiFull: Widen<V1Whoami> = v1WhoamiFull;
  void _v1WhoamiMissing;
  void _v1WhoamiExtra;
  void _v1WhoamiZero;
  void _v1WhoamiFull;
}
// PRD #1910 M1: the nested oauth_client and the rotate-secret response. ZeroOf is shallow, so
// the nested oauth_client block carries its own exemption: redirect_uris and scopes are the
// nil-slice nulls in product.zero.json that oauthClientDTO (handler/admin_products.go)
// normalizes to [] (jobTypesOrEmpty), so they are never null on the wire. rotated_at is
// typed `string | null`, no exemption. oauth_client is optional in Product (rollout skew),
// so the checks read it through Required.
{
  const _oauthClientMissing: never = null as unknown as Exclude<keyof ProductOAuthClient, keyof typeof productFull.oauth_client>;
  const _oauthClientExtra: never = null as unknown as Exclude<keyof typeof productFull.oauth_client, keyof ProductOAuthClient>;
  const _oauthClientFull: Widen<ProductOAuthClient> = productFull.oauth_client;
  const _rotateMissing: never = null as unknown as Exclude<keyof RotateProductClientSecretResponse, keyof typeof rotateProductClientSecretFull>;
  const _rotateExtra: never = null as unknown as Exclude<keyof typeof rotateProductClientSecretFull, keyof RotateProductClientSecretResponse>;
  const _rotateZero: { client_secret: string; product: ProductZero } = rotateProductClientSecretZero;
  const _rotateFull: Widen<RotateProductClientSecretResponse> = rotateProductClientSecretFull;
  void _oauthClientMissing;
  void _oauthClientExtra;
  void _oauthClientFull;
  void _rotateMissing;
  void _rotateExtra;
  void _rotateZero;
  void _rotateFull;
}
// PRD #1910 M2: the consent page's metadata and the approve / deny redirect. scopes is the
// nil-slice null in oauth_authorize_request.zero.json that the handler normalizes to [] (never
// null on the wire).
{
  const _oauthRequestMissing: never = null as unknown as Exclude<keyof OAuthAuthorizeRequest, keyof typeof oauthAuthorizeRequestFull>;
  const _oauthRequestExtra: never = null as unknown as Exclude<keyof typeof oauthAuthorizeRequestFull, keyof OAuthAuthorizeRequest>;
  const _oauthRequestZero: ZeroOf<OAuthAuthorizeRequest, "scopes"> = oauthAuthorizeRequestZero;
  const _oauthRequestFull: Widen<OAuthAuthorizeRequest> = oauthAuthorizeRequestFull;
  const _oauthRedirectMissing: never = null as unknown as Exclude<keyof OAuthRedirect, keyof typeof oauthRedirectResponseFull>;
  const _oauthRedirectExtra: never = null as unknown as Exclude<keyof typeof oauthRedirectResponseFull, keyof OAuthRedirect>;
  const _oauthRedirectZero: Widen<OAuthRedirect> = oauthRedirectResponseZero;
  const _oauthRedirectFull: Widen<OAuthRedirect> = oauthRedirectResponseFull;
  void _oauthRequestMissing;
  void _oauthRequestExtra;
  void _oauthRequestZero;
  void _oauthRequestFull;
  void _oauthRedirectMissing;
  void _oauthRedirectExtra;
  void _oauthRedirectZero;
  void _oauthRedirectFull;
}
// PRD #1910 M3: one live OAuth connection. last_used_at and refresh_issued_at are present-as-null
// pointers; scopes is the nil-slice null in the zero fixture that the handler normalizes to [].
{
  const _oauthConnectionMissing: never = null as unknown as Exclude<keyof OAuthConnection, keyof typeof oauthConnectionFull>;
  const _oauthConnectionExtra: never = null as unknown as Exclude<keyof typeof oauthConnectionFull, keyof OAuthConnection>;
  const _oauthConnectionZero: ZeroOf<OAuthConnection, "scopes"> = oauthConnectionZero;
  const _oauthConnectionFull: Widen<OAuthConnection> = oauthConnectionFull;
  void _oauthConnectionMissing;
  void _oauthConnectionExtra;
  void _oauthConnectionZero;
  void _oauthConnectionFull;
}
// PRD #1910 M5: one live connection in the admin product list. last_used_at is a present-as-null
// pointer; scopes is the nil-slice null in the zero fixture that the handler normalizes to [].
{
  const _adminOauthConnectionMissing: never = null as unknown as Exclude<keyof AdminOAuthConnection, keyof typeof adminOauthConnectionFull>;
  const _adminOauthConnectionExtra: never = null as unknown as Exclude<keyof typeof adminOauthConnectionFull, keyof AdminOAuthConnection>;
  const _adminOauthConnectionZero: ZeroOf<AdminOAuthConnection, "scopes"> = adminOauthConnectionZero;
  const _adminOauthConnectionFull: Widen<AdminOAuthConnection> = adminOauthConnectionFull;
  void _adminOauthConnectionMissing;
  void _adminOauthConnectionExtra;
  void _adminOauthConnectionZero;
  void _adminOauthConnectionFull;
}
// PRD #1907 M4/M5: the typed admin delete response (its nested product's deleted_at is
// `string | null`, no exemption) and the user mint-picker entry (all strings).
{
  const _adminDeleteProductMissing: never = null as unknown as Exclude<keyof AdminDeleteProductResponse, keyof typeof adminDeleteProductFull>;
  const _adminDeleteProductExtra: never = null as unknown as Exclude<keyof typeof adminDeleteProductFull, keyof AdminDeleteProductResponse>;
  const _adminDeleteProductZero: {
    product: ProductZero;
    stopped_token_count: number;
    stopped_connection_count: number;
  } = adminDeleteProductZero;
  const _adminDeleteProductFull: Widen<AdminDeleteProductResponse> = adminDeleteProductFull;
  void _adminDeleteProductMissing;
  void _adminDeleteProductExtra;
  void _adminDeleteProductZero;
  void _adminDeleteProductFull;
}
{
  const _mintableProductMissing: never = null as unknown as Exclude<keyof MintableProduct, keyof typeof mintableProductFull>;
  const _mintableProductExtra: never = null as unknown as Exclude<keyof typeof mintableProductFull, keyof MintableProduct>;
  const _mintableProductZero: ZeroOf<MintableProduct> = mintableProductZero;
  const _mintableProductFull: Widen<MintableProduct> = mintableProductFull;
  void _mintableProductMissing;
  void _mintableProductExtra;
  void _mintableProductZero;
  void _mintableProductFull;
}

// PRD #1909 M6: the admin product skill-set view (GET/sync/apply). applied_at/applied_by
// and staged are typed `X | null`, no exemption. applied.skills is the one nil-slice null in
// product_skills.zero.json: productSkillsView (handler/admin_product_skills.go) builds it with
// make(..., 0, n), so it is never null on the wire. ZeroOf is shallow, so the nested applied
// block carries that exemption itself (the admin_delete_product shape).
{
  const _productSkillsMissing: never = null as unknown as Exclude<keyof ProductSkills, keyof typeof productSkillsFull>;
  const _productSkillsExtra: never = null as unknown as Exclude<keyof typeof productSkillsFull, keyof ProductSkills>;
  type Staged = NonNullable<ProductSkills["staged"]>;
  const _productSkillsConfigMissing: never = null as unknown as Exclude<keyof ProductSkills["config"], keyof typeof productSkillsFull.config>;
  const _productSkillsConfigExtra: never = null as unknown as Exclude<keyof typeof productSkillsFull.config, keyof ProductSkills["config"]>;
  const _productSkillsAppliedMissing: never = null as unknown as Exclude<keyof ProductSkills["applied"], keyof typeof productSkillsFull.applied>;
  const _productSkillsAppliedExtra: never = null as unknown as Exclude<keyof typeof productSkillsFull.applied, keyof ProductSkills["applied"]>;
  const _productSkillsStagedMissing: never = null as unknown as Exclude<keyof Staged, keyof typeof productSkillsFull.staged>;
  const _productSkillsStagedExtra: never = null as unknown as Exclude<keyof typeof productSkillsFull.staged, keyof Staged>;
  const _productSkillsZero: {
    config: ZeroOf<ProductSkills["config"]>;
    applied: ZeroOf<ProductSkills["applied"], "skills">;
    staged: null;
  } = productSkillsZero;
  const _productSkillsFull: Widen<ProductSkills> = productSkillsFull;
  void _productSkillsMissing;
  void _productSkillsExtra;
  void _productSkillsConfigMissing;
  void _productSkillsConfigExtra;
  void _productSkillsAppliedMissing;
  void _productSkillsAppliedExtra;
  void _productSkillsStagedMissing;
  void _productSkillsStagedExtra;
  void _productSkillsZero;
  void _productSkillsFull;
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
  // PRD #1908 D-D: inputs (nil slice), origin members and result are null in zero.json.
  { stem: "run_job", nullable: true },
  { stem: "repo", nullable: true },
  { stem: "message", nullable: true },
  { stem: "schedule", nullable: true },
  { stem: "schedule_input", nullable: true },
  { stem: "schedule_run_now", nullable: true },
  { stem: "worker", nullable: true },
  { stem: "admin_worker", nullable: true },
  { stem: "user", nullable: true },
  // All-scalar / no nullable Go field: their zero.json legitimately carries no null,
  // so the null-presence guard is declared off (per the PRD, like RunUsage).
  { stem: "agent_memory", nullable: false },
  // PRD #1732 D10: disabled_at is a present-as-null pointer while the credential is enabled.
  { stem: "secret", nullable: true },
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
  // M2 (PRD #1093): the pause-all singleton. `until` is nullable, so zero.json carries a null.
  { stem: "schedule_pause", nullable: true },
  // All-scalar / no nullable Go field: their zero.json legitimately carries no null.
  { stem: "column", nullable: false },
  { stem: "branding", nullable: false },
  // PRD #1183 M3: IncidentalFindingDTO's nullable fields (finding_id, filed_issue_iid,
  // resolved_at) are ALL omitempty, so its zero.json carries no null — the same shape as
  // AgentMemoryDTO, declared nullable:false rather than manufacturing a null by dropping an
  // existing field's omitempty (which would change the live findings wire).
  { stem: "finding", nullable: false },
  // PRD #1255 M2a — the forge-view read DTOs.
  // pull: conflicts + run_id are nullable, so zero.json carries a null.
  { stem: "pull", nullable: true },
  // pull_detail: conflicts/run_id/checks/reviews + merge.conflicts all null in zero.json.
  { stem: "pull_detail", nullable: true },
  // check / pull_review: all-scalar, zero.json legitimately carries no null.
  { stem: "check", nullable: false },
  { stem: "pull_review", nullable: false },
  // merge_state: conflicts is nullable, so zero.json carries a null.
  { stem: "merge_state", nullable: true },
  // ci_run: all-scalar, no null.
  { stem: "ci_run", nullable: false },
  // ci_run_detail: jobs (nil slice) is null in zero.json.
  { stem: "ci_run_detail", nullable: true },
  // ci_job: steps (nil slice) is null in zero.json.
  { stem: "ci_job", nullable: true },
  // ci_step: all-scalar, no null.
  { stem: "ci_step", nullable: false },
  // PRD #1296 M1: recovery_archive is all-omitempty optionals, so its zero.json carries no
  // null (the finding shape). recovery_archive_summary's archives slice is non-omitempty,
  // so its zero.json carries a null the M2 mapper normalizes to [].
  { stem: "recovery_archive", nullable: false },
  { stem: "recovery_archive_summary", nullable: true },
  // PRD #1349 M1: the owner-facing custody-hold DTOs. recovery_custody_hold's only nullable
  // field (released_at) plus worker_name/capture_state are all omitempty, so its zero.json
  // carries no null (the finding shape); recovery_custody_aggregate is all ints (no null);
  // recovery_custody_holds' holds slice is non-omitempty, so its zero.json carries a null the
  // endpoint normalizes to [].
  { stem: "recovery_custody_hold", nullable: false },
  { stem: "recovery_custody_aggregate", nullable: false },
  { stem: "recovery_custody_holds", nullable: true },
  // issue #1432: the admin override-request queue row. findings is a non-omitempty
  // slice the handler normalizes to [], so its zero.json carries a null (the nil-slice
  // marshal) that the "findings" ZeroOf exemption above accounts for.
  { stem: "guardrail_override_request", nullable: true },
  // PRD #1209 M1: the Codex per-account meter and its admin row. Both carry nil-slice
  // nulls in their zero.json (aliases + buckets on the account; accounts on the admin
  // row), normalized to [] by the M3 mapper — so nullable:true.
  { stem: "codex_account_rate_limit", nullable: true },
  { stem: "codex_admin_rate_limit_row", nullable: true },
  // PRD #1484 M1: the admin-health document. snoozed_until/episode_id are present-as-null
  // pointers and checks is a non-omitempty slice (a null nil-slice marshal the registry
  // normalizes to []), so its zero.json carries nulls.
  { stem: "health_doc", nullable: true },
  { stem: "build_info", nullable: false },
  // PRD #1907 M1: every product/product-token DTO has a present-as-null pointer or a
  // nil-slice scopes in its zero.json, so all five are nullable:true.
  { stem: "product", nullable: true },
  { stem: "product_token", nullable: true },
  { stem: "admin_product_token", nullable: true },
  { stem: "mint_product_token", nullable: true },
  { stem: "v1_whoami", nullable: true },
  // PRD #1907 M4/M5: admin_delete_product nests a product whose deleted_at is null in
  // zero.json; mintable_product is all strings, so its zero.json carries no null.
  { stem: "admin_delete_product", nullable: true },
  { stem: "mintable_product", nullable: false },
  // PRD #1906 M1w: the admin egress profile. created_by/updated_by are present-as-null
  // pointers and the three slices are nil-slice nulls the handler normalizes to [].
  { stem: "egress_profile", nullable: true },
  // PRD #1909 M6: applied_at/applied_by/staged are present-as-null and applied.skills is a
  // nil-slice null in zero.json.
  { stem: "product_skills", nullable: true },
];

describe("admin worker cleanup contract", () => {
  it("keeps required cleanup state admin-only and pressure volumes non-null at zero", () => {
    expect(adminWorkerZero.disk_pressure_volumes).toEqual([]);
    expect(adminWorkerZero.cleanup_pending).toBe(false);
    expect(adminWorkerFull.disk_pressure_volumes).toEqual(["x"]);
    expect(adminWorkerFull.cleanup_pending).toBe(true);
    expect(workerFull).not.toHaveProperty("disk_pressure_volumes");
    expect(workerFull).not.toHaveProperty("cleanup_pending");
  });
});

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

// TestPlanCrossCheckSummaryNullableItems records this accepted server wire case.
// Explicit typing makes a narrower array-only declaration fail typecheck.
it("plan-check findings permit a null item array", () => {
  const summary: PlanCrossCheckSummary = {
    round: 1,
    verdict: "approve",
    reason_class: "approve",
    findings: { summary: "ok", items: null },
    checker_run_id: null,
    checker_model: null,
    checker_effort: null,
    usage: null,
    historical: false,
  };
  expect(summary.findings?.items).toBeNull();
});


describe("worker custody decision contract", () => {
  it("records populated counts while zero DTOs omit the optional field", () => {
    expect(workerFull.custody_decisions_needed).toBe(1);
    expect(adminWorkerFull.custody_decisions_needed).toBe(1);
    expect(workerZero).not.toHaveProperty("custody_decisions_needed");
    expect(adminWorkerZero).not.toHaveProperty("custody_decisions_needed");
  });
});

it("records removal flags and selector snapshots in both fire responses", () => {
  expect(scheduleZero.remove_label_on_dispatch).toBe(false);
  expect(scheduleFull.remove_label_on_dispatch).toBe(true);
  expect(scheduleInputFull.remove_label_on_dispatch).toBe(true);
  expect(scheduleInputZero).not.toHaveProperty("remove_label_on_dispatch");
  for (const started of [scheduleFull.last_fire.started[0], scheduleRunNowFull.started[0]]) {
    expect(started).toMatchObject({ selector_label: "x", label_removed: true, label_remove_failed: true });
  }
});


describe("issue-input history contract", () => {
  it("records Run and list history fields without hand-authored fixtures", () => {
    for (const [zero, full] of [[runZero, runFull], [runListItemZero, runListItemFull]]) {
      expect(zero.auto_approve_blocked_reasons).toBeNull(); // nil slice before mapper normalization
      expect(zero.issue_input_reason).toBeNull();
      expect(full.auto_approve_blocked_reasons).toEqual(["x"]);
      expect(full.issue_input_reason).toBe("x");
    }
  });

  it("records blocked reasons on the card's latest run", () => {
    expect(cardFull.latest_run.auto_approve_blocked_reasons).toEqual(["x"]);
    expect(boardFull.cards[0].latest_run.auto_approve_blocked_reasons).toEqual(["x"]);
  });
});
