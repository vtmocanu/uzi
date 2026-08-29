// ScheduleModal — the create/edit modal for scheduled runs (PRD #241 M5, mock §2).
//
// One modal serves both timing modes (Once / Recurring) and all three targets
// (Issue / Label sweep / Prompt); the two segmented pickers swap the fields below
// them. A live "Next fires" preview calls POST /api/schedules/preview (debounced)
// so it always matches server truth. The cron STRING is canonical (Decision 6):
// presets translate to it, editing the raw cron flips the preset to "Custom", and
// only the cron string is ever sent — never a preset label.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  api,
  ApiError,
  type Repo,
  type Schedule,
  type ScheduleInput,
  type ScheduleTarget,
  type ScheduleTiming,
} from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import {
  Alert,
  Badge,
  Button,
  Field,
  Input,
  Select,
  Textarea,
  Toggle,
  cx,
} from "./ui";
import { SweepLabelWarn } from "./SweepLabelWarn";
import { RepoMultiSelect } from "./RepoMultiSelect";
import { LockIcon } from "./icons";
import { XIcon, TrashIcon, CopyIcon } from "./icons";
import { ModelSelect } from "./ModelSelect";
import { modelFieldWarning } from "../lib/agentTemplates";
import {
  cronFromPreset,
  DEFAULT_PRESET_STATE,
  hhmm,
  humanizeCron,
  parseHHMM,
  presetFromCron,
  PRESET_OPTIONS,
  type CronPreset,
  type PresetState,
} from "../lib/schedulePresets";

// PinnedIssue locks the modal to one issue (the issue-view "Schedule…" entry,
// mock §3): the target is forced to "issue" and the repo/issue are not editable.
export interface PinnedIssue {
  repoId: string;
  repoPath: string;
  issueIid: number;
}

const TARGET_OPTIONS: { value: ScheduleTarget; title: string; desc: string }[] = [
  { value: "issue", title: "Issue", desc: "Pin one PRD issue" },
  { value: "sweep", title: "Label sweep", desc: "Issues matching a label" },
  { value: "prompt", title: "Prompt", desc: "No issue → opens an MR" },
];

const TIMING_OPTIONS: { value: ScheduleTiming; title: string; desc: string }[] = [
  { value: "once", title: "Once", desc: "A single future moment" },
  { value: "recurring", title: "Recurring", desc: "On a repeating cadence" },
];

const COMMON_TIMEZONES = ["UTC", "Europe/Bucharest", "America/New_York", "Europe/London"];

function browserTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

// toLocalInput / fromLocalInput bridge an ISO instant and a <input type="datetime-local">
// value (which is a wall-clock string with no zone). We treat the picker as the
// browser's local zone, which is the least-surprising default for a once schedule.
function toLocalInput(iso: string | null): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const p = (n: number) => n.toString().padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

function fromLocalInput(v: string): string | null {
  if (!v) return null;
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return null;
  return d.toISOString();
}

// SegmentedControl is the radio-group picker the mock uses for Target and Timing.
function SegmentedControl<T extends string>({
  label,
  value,
  onChange,
  options,
  disabled = false,
}: {
  label: string;
  value: T;
  onChange: (v: T) => void;
  options: { value: T; title: string; desc: string }[];
  disabled?: boolean;
}) {
  return (
    <div className="space-y-2" role="radiogroup" aria-label={label}>
      <span className="text-xs font-semibold uppercase tracking-wide text-muted">{label}</span>
      <div className="grid grid-flow-col auto-cols-fr gap-1.5">
        {options.map((o) => {
          const on = o.value === value;
          return (
            <button
              key={o.value}
              type="button"
              role="radio"
              aria-checked={on}
              disabled={disabled}
              onClick={() => onChange(o.value)}
              className={cx(
                "rounded-lg border px-3 py-2.5 text-left transition-colors disabled:cursor-not-allowed disabled:opacity-60",
                on ? "border-brand bg-brand/10" : "border-edge bg-raised hover:border-edge-strong",
              )}
            >
              <span className="flex items-center gap-2 text-[13px] font-semibold text-fg">
                <span
                  aria-hidden="true"
                  className={cx(
                    "h-3 w-3 shrink-0 rounded-full border-2",
                    on ? "border-brand bg-brand" : "border-edge-strong",
                  )}
                />
                {o.title}
              </span>
              <span className="mt-1 block pl-5 text-[11px] text-faint">{o.desc}</span>
            </button>
          );
        })}
      </div>
    </div>
  );
}

export function ScheduleModal({
  pinned,
  editing,
  onClose,
  onSaved,
  onCloneToEdit,
}: {
  // When set, the modal is pre-pinned to this issue (issue-view entry, mock §3).
  pinned?: PinnedIssue;
  // When set, the modal edits an existing schedule instead of creating one.
  editing?: Schedule;
  onClose: () => void;
  // Called after a successful create/update/delete so the caller can refresh.
  onSaved: () => void;
  // When editing a catalog default (PRD #589): the "Clone to edit" affordance, which
  // hands the read-only default off to the caller to clone into an editable user row.
  onCloneToEdit?: (s: Schedule) => void;
}) {
  const { uziLabel } = useAuth();
  const isEdit = !!editing;
  // A catalog default (PRD #589) is catalog-owned: its target/prompt/labels/guidance are
  // read-only (shown as the baked, sealed values) and only the cadence, model, and run
  // flags are editable. override_subagent_model is non-editable on a default too. To get
  // the full editable set, the owner clones it to a user row (origin='user').
  const isDefault = editing?.origin === "default";

  const [repos, setRepos] = useState<Repo[]>([]);
  // Edit mode targets exactly one repo (the repoint select, PRD #344); this is that repo.
  const [repoId, setRepoId] = useState<string>(
    editing?.repo_id ?? pinned?.repoId ?? "",
  );
  // Create mode targets N repos (PRD #636 Decision 6): a multi-select whose fan-out makes
  // one independent schedule per repo. Seeded from a pinned repo when the modal is pinned
  // to an issue (the picker is hidden then); otherwise seeded to the first repo on load.
  const [selectedRepoIds, setSelectedRepoIds] = useState<string[]>(
    pinned?.repoId ? [pinned.repoId] : [],
  );

  const [target, setTarget] = useState<ScheduleTarget>(
    editing?.target ?? (pinned ? "issue" : "issue"),
  );
  const [issueIid, setIssueIid] = useState<string>(
    editing?.issue_iid != null
      ? String(editing.issue_iid)
      : pinned
        ? String(pinned.issueIid)
        : "",
  );
  const [labels, setLabels] = useState<string[]>(editing?.labels ?? []);
  const [labelInput, setLabelInput] = useState("");
  const [prompt, setPrompt] = useState(editing?.prompt ?? "");

  const [timing, setTiming] = useState<ScheduleTiming>(editing?.timing ?? "recurring");

  // Cadence: the canonical cron string plus the preset sub-state derived from it.
  const initialCron = editing?.timing === "recurring" ? editing.cron_expr : cronFromPreset(DEFAULT_PRESET_STATE);
  const [cron, setCron] = useState<string>(initialCron || cronFromPreset(DEFAULT_PRESET_STATE));
  const [presetState, setPresetState] = useState<PresetState>(
    presetFromCron(initialCron || cronFromPreset(DEFAULT_PRESET_STATE)),
  );

  const [runAtLocal, setRunAtLocal] = useState<string>(toLocalInput(editing?.run_at ?? null));
  const [timezone, setTimezone] = useState<string>(
    editing?.timezone ?? browserTimezone(),
  );
  const [advancedOpen, setAdvancedOpen] = useState(false);

  const [waitOnLimit, setWaitOnLimit] = useState<boolean>(editing?.wait_on_limit ?? true);
  const [autoApprove, setAutoApprove] = useState<boolean>(editing?.auto_approve ?? true);
  // Create-only enabled/disabled toggle (PRD #344 Feature B): lets a schedule be created
  // already paused. Edit-mode enable/disable stays the job of pause/resume, so this state
  // seeds from editing but is only ever SENT on create (see buildInput).
  const [enabled, setEnabled] = useState<boolean>(editing?.enabled ?? true);
  // Sweep-only cap on issues per fire; null = unlimited. New sweeps default to 10
  // (agreeing with the server), an edit reflects the stored value (null included).
  const [maxIssues, setMaxIssues] = useState<number | null>(editing ? editing.max_issues : 10);
  // Optional owner guidance for issue/sweep targets and a prompt-target default;
  // a string in state ("" = none).
  // Steers HOW a run approaches the task — the issue body stays the task.
  const [guidance, setGuidance] = useState<string>(editing?.guidance ?? "");
  // A sweep default's baked catalog guidance is read-only and travels in a SEPARATE
  // DTO field (issue #675); the editable `guidance` state above is the owner overlay.
  const bakedGuidance = editing?.baked_guidance ?? "";
  // Per-schedule model override; "" = inherit the owner's per-user Worker default.
  // Applies to ALL targets (unlike guidance). Injection-suspect custom IDs are gated
  // below (modelWarning) mirroring the server's ValidateModel reject.
  const [model, setModel] = useState<string>(editing?.model ?? "");
  const modelWarning = modelFieldWarning(model);
  // PRD #305: apply the run model to every subagent (overriding pins). Default off.
  const [overrideSubagentModel, setOverrideSubagentModel] = useState<boolean>(
    editing?.override_subagent_model ?? false,
  );

  const [fires, setFires] = useState<string[]>(editing?.next_fires ?? []);
  const [previewError, setPreviewError] = useState(false);
  const [saving, setSaving] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState("");

  // Focus management (a11y): on open we move focus into the dialog container
  // (tabIndex={-1} makes it programmatically focusable) so Tab starts inside and
  // a screen reader lands on the dialog. On close we restore focus to whatever
  // was focused when the dialog opened — captured here without threading a ref
  // from every caller, mirroring how CliTokens focuses a container ref on mount.
  const dialogRef = useRef<HTMLDivElement>(null);
  const restoreFocusRef = useRef<Element | null>(null);
  useEffect(() => {
    restoreFocusRef.current = document.activeElement;
    dialogRef.current?.focus();
    return () => {
      const prev = restoreFocusRef.current;
      if (prev instanceof HTMLElement) prev.focus();
    };
  }, []);

  // Escape closes (same handler as the × button) and Tab is trapped so focus
  // cycles within the dialog instead of reaching the list behind it. The trap is
  // a boundary wrap at the first/last focusable element — no new dependency.
  const onDialogKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.stopPropagation();
      onClose();
      return;
    }
    if (e.key !== "Tab") return;
    const root = dialogRef.current;
    if (!root) return;
    const focusable = Array.from(
      root.querySelectorAll<HTMLElement>(
        'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
      ),
    ).filter((el) => el.offsetParent !== null || el === document.activeElement);
    if (focusable.length === 0) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    const active = document.activeElement;
    if (e.shiftKey) {
      if (active === first || active === root) {
        e.preventDefault();
        last.focus();
      }
    } else if (active === last) {
      e.preventDefault();
      first.focus();
    }
  };

  // Load repos for the picker (create and edit; not pinned). Failure is non-fatal.
  // In edit mode the `cur ||` below preserves the edit-seeded current repo.
  useEffect(() => {
    if (pinned) return;
    let alive = true;
    api
      .listRepos()
      .then(({ repos }) => {
        if (!alive) return;
        setRepos(repos);
        setRepoId((cur) => cur || repos[0]?.id || "");
        // Create mode seeds the multi-select to the first repo so the form is submittable
        // out of the box (parity with the old single-select default); edit mode leaves it.
        setSelectedRepoIds((cur) => (cur.length > 0 ? cur : repos[0] ? [repos[0].id] : []));
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [pinned]);

  // ── Preset ↔ cron keep-in-sync (Decision 6) ────────────────────────────────
  const applyPreset = useCallback((next: PresetState) => {
    setPresetState(next);
    if (next.preset !== "custom") setCron(cronFromPreset(next));
  }, []);

  const onPresetChange = (preset: CronPreset) => {
    if (preset === "custom") {
      setPresetState((s) => ({ ...s, preset }));
      return;
    }
    applyPreset({ ...presetState, preset });
  };
  const onTimeChange = (v: string) => {
    const t = parseHHMM(v);
    if (!t) return;
    applyPreset({ ...presetState, ...t });
  };
  const onEveryNChange = (v: string) => {
    const n = Number(v);
    if (!Number.isFinite(n) || n < 1 || n > 23) return;
    applyPreset({ ...presetState, everyN: n });
  };
  const onRawCronChange = (v: string) => {
    setCron(v);
    setPresetState(presetFromCron(v, presetState));
  };

  // ── Live "Next fires" preview (debounced), always from the server ───────────
  const debounceRef = useRef<number | undefined>(undefined);
  useEffect(() => {
    window.clearTimeout(debounceRef.current);
    const runAtISO = fromLocalInput(runAtLocal);
    // Nothing to preview until the timing's key field is present.
    if (timing === "recurring" && cron.trim() === "") {
      setFires([]);
      setPreviewError(false);
      return;
    }
    if (timing === "once" && !runAtISO) {
      setFires([]);
      setPreviewError(false);
      return;
    }
    debounceRef.current = window.setTimeout(() => {
      api
        .previewSchedule({
          timing,
          cron_expr: timing === "recurring" ? cron : undefined,
          run_at: timing === "once" ? runAtISO : undefined,
          timezone,
          n: 3,
        })
        .then(({ fires }) => {
          setFires(fires);
          setPreviewError(false);
        })
        .catch(() => {
          setFires([]);
          setPreviewError(true);
        });
    }, 400);
    return () => window.clearTimeout(debounceRef.current);
  }, [timing, cron, runAtLocal, timezone]);

  const repoPath = useMemo(() => {
    if (editing) return editing.repo_path;
    if (pinned) return pinned.repoPath;
    // Create mode: the header line is cosmetic over N repos, so it names the first.
    return repos.find((r) => r.id === selectedRepoIds[0])?.path_with_namespace ?? "";
  }, [editing, pinned, repos, selectedRepoIds]);

  const addLabel = () => {
    const t = labelInput.trim();
    if (t && !labels.includes(t)) setLabels((ls) => [...ls, t]);
    setLabelInput("");
  };

  const canSubmit = (): boolean => {
    // Edit repoints one repo; create fans out over the multi-select (≥1 required).
    if (isEdit ? !repoId : selectedRepoIds.length === 0) return false;
    // A catalog default is always recurring: only its cron and model gate submit; the
    // catalog-owned target fields are read-only and always valid.
    if (isDefault) {
      if (cron.trim() === "") return false;
      if (modelWarning !== "") return false;
      if (target === "sweep" && maxIssues != null && !(Number.isInteger(maxIssues) && maxIssues > 0))
        return false;
      return true;
    }
    if (target === "issue" && !(Number(issueIid) > 0)) return false;
    if (target === "prompt" && prompt.trim() === "") return false;
    // Blank max_issues (null) = unlimited and is valid; a set value must be a
    // positive integer. The server validates too.
    if (target === "sweep" && maxIssues != null && !(Number.isInteger(maxIssues) && maxIssues > 0))
      return false;
    if (timing === "recurring" && cron.trim() === "") return false;
    if (timing === "once" && !fromLocalInput(runAtLocal)) return false;
    // An injection-suspect custom model ID blocks the form (mirrors ValidateModel).
    if (modelWarning !== "") return false;
    return true;
  };

  // A catalog default only sends its EDITABLE fields (PRD #589): cadence, timezone,
  // model, the two run flags, and (sweep) max_issues. Everything catalog-owned is
  // omitted entirely, NOT restated — the server (patchDefaultScheduleConfig) 400s ANY
  // default patch whose body carries a catalog-owned field, and `timing` is one of them:
  // it keeps timing="recurring" itself, so sending it (even as the correct value) is
  // rejected. The catalog-owned set the server rejects is target/prompt/labels/repo_id/
  // issue_iid/timing/run_at — none of these appear below.
  //
  // Guidance is owner-editable for a PROMPT default (issue #662) and a SWEEP default
  // (issue #675): the owner steers the baked prompt/guidance via an OVERLAY the server
  // appends to the catalog value at fire time, so it is owner-editable there. We send it
  // with replace-semantics (value, or explicit null to clear) each time. For issue/
  // self_improve defaults guidance stays catalog-owned and MUST be omitted (the server
  // 400s a default patch that carries it for those targets).
  const buildDefaultInput = (): ScheduleInput => ({
    cron_expr: cron.trim(),
    timezone,
    // A self_improve run is always auto-approved (the server forces auto_approve=true on
    // every path), so omit it here rather than send a client-chosen value the server ignores.
    auto_approve: target === "self_improve" ? undefined : autoApprove,
    wait_on_limit: waitOnLimit,
    max_issues: target === "sweep" ? maxIssues : undefined,
    model: model.trim() === "" ? null : model,
    guidance:
      target === "prompt" || target === "sweep"
        ? guidance.trim() === ""
          ? null
          : guidance
        : undefined,
  });

  const buildInput = (): ScheduleInput => ({
    target,
    issue_iid: target === "issue" ? Number(issueIid) : undefined,
    labels: target === "sweep" ? labels : undefined,
    // Sweep-only cap; send explicit null (not undefined) so clearing the field
    // clears the stored value to unlimited. Omitted on non-sweep targets.
    max_issues: target === "sweep" ? maxIssues : undefined,
    // Owner guidance on issue/sweep only; a blank/cleared textarea sends explicit
    // null (clear to none). Omitted (undefined) on prompt so the server never rejects it.
    guidance:
      target === "issue" || target === "sweep"
        ? guidance.trim() === ""
          ? null
          : guidance
        : undefined,
    prompt: target === "prompt" ? prompt : undefined,
    timing,
    cron_expr: timing === "recurring" ? cron.trim() : undefined,
    run_at: timing === "once" ? fromLocalInput(runAtLocal) : undefined,
    timezone,
    // A self_improve run is always auto-approved (the server forces auto_approve=true on
    // every path), so omit it here rather than send a client-chosen value the server ignores.
    auto_approve: target === "self_improve" ? undefined : autoApprove,
    wait_on_limit: waitOnLimit,
    // Model override on EVERY target (all-targets field, unlike guidance); an empty
    // control sends explicit null to clear-to-inherit (replace-semantics).
    model: model.trim() === "" ? null : model,
    // PRD #305: apply the run model to every subagent. Always sent (replace-semantics).
    override_subagent_model: overrideSubagentModel,
    // Sent only on create; on edit, enable/disable is pause/resume, so leave it absent
    // (undefined) here to avoid re-flipping enabled during a config edit.
    enabled: isEdit ? undefined : enabled,
    // Repoint (edit only): send the selected repo so a changed selection moves the
    // schedule. Omitted on create (repo comes from the URL) AND on an issue-target edit —
    // an issue schedule is repo-relative and the server 422s a repoint, so gate on `target`
    // (which is editable in edit mode) rather than only the selector's disabled state, or a
    // user who picks a repo then switches target to issue would provoke that 422. Omitting
    // it is safe: the server re-seeds repo_id from the current row (keep-on-empty).
    repo_id: isEdit && target !== "issue" ? repoId : undefined,
  });

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!canSubmit()) return;
    setError("");
    setSaving(true);
    try {
      if (editing) {
        await api.updateSchedule(editing.id, isDefault ? buildDefaultInput() : buildInput());
        onSaved();
      } else {
        await createFanOut();
      }
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not save the schedule");
    } finally {
      setSaving(false);
    }
  };

  // Create fan-out (PRD #636 Decision 4 + 7). A single-repo create is a plain standalone
  // schedule with no group id. A multi-repo create generates ONE client-side sibling group
  // id and issues N independent createSchedule calls that all carry it, so the rows share a
  // display group; the calls run via Promise.allSettled (mirroring enableDefault) so a
  // mid-fan-out failure does not roll back the rows that landed (Decision 7). On full
  // success the modal closes and the list refreshes (onSaved); on partial or full failure
  // the modal stays open with a "created N of M, K failed" message.
  const createFanOut = async () => {
    const repoIds = selectedRepoIds;
    const base = buildInput();
    if (repoIds.length === 1) {
      await api.createSchedule(repoIds[0], base);
      onSaved();
      return;
    }
    const groupId = crypto.randomUUID();
    const results = await Promise.allSettled(
      repoIds.map((rid) => api.createSchedule(rid, { ...base, sibling_group_id: groupId })),
    );
    const ok = results.filter((r) => r.status === "fulfilled").length;
    const failed = results.length - ok;
    if (failed > 0) {
      setError(`Created ${ok} of ${repoIds.length} schedules; ${failed} failed.`);
      return;
    }
    onSaved();
  };

  const remove = async () => {
    if (!editing) return;
    setError("");
    setDeleting(true);
    try {
      await api.deleteSchedule(editing.id);
      onSaved();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not delete the schedule");
      setDeleting(false);
    }
  };

  // Optional guidance textarea. Rendered for the issue and sweep targets, and for a
  // prompt-target DEFAULT (issue #662: its baked prompt is catalog-owned, but owner
  // guidance overlays it at fire time) and a sweep-target DEFAULT (issue #675: its baked
  // catalog guidance is read-only, but this owner OVERLAY is appended at fire time) — NOT
  // for a user prompt schedule, which carries its own editable prompt text. Mirrors the
  // prompt textarea's markup.
  // A prompt-target default overlays this guidance on its baked catalog prompt, and a
  // sweep-target default overlays it on its baked catalog guidance (issue #675), so the
  // issue-centric wording ("every issue this schedule runs") does not apply to either;
  // give each default case its own help text stating the baked value stays in effect. A
  // user issue/sweep schedule keeps the issue-centric wording.
  const guidanceHelp =
    target === "prompt"
      ? "Steers how the run approaches the task. It's appended to the baked prompt each time this schedule fires; the baked prompt stays the task."
      : isDefault && target === "sweep"
        ? "Steers how the run approaches each swept issue. It's appended to the baked guidance each time this schedule fires; the baked guidance stays in effect."
        : "Steers how the run approaches the task, e.g. “always add a failing test first”. Applied to every issue this schedule runs; the issue itself stays the task.";
  const guidanceField = (
    <Field label="Guidance (optional)" htmlFor="sched-guidance">
      <Textarea
        id="sched-guidance"
        rows={3}
        maxLength={8192}
        value={guidance}
        onChange={(e) => setGuidance(e.target.value)}
        placeholder="always add a failing test first"
      />
      <p className="mt-1 text-[11px] text-faint">{guidanceHelp}</p>
    </Field>
  );

  const footerSummarySegments = [
    timing === "once" ? "One time" : "Recurring",
    target === "sweep"
      ? "sweep"
      : target === "prompt"
        ? "prompt"
        : target === "self_improve"
          ? "self-improve"
          : "issue",
  ];
  // A self_improve run is always auto-approved (the server forces it), so drop the
  // approve segment entirely there — it is a fixed value, not a user choice.
  if (target !== "self_improve") {
    footerSummarySegments.push(autoApprove ? "auto-approve" : "manual approve");
  }
  const footerSummary = footerSummarySegments.join(" · ");

  return (
    <div
      ref={dialogRef}
      tabIndex={-1}
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/60 p-4 outline-hidden sm:items-center"
      role="dialog"
      aria-modal="true"
      // Match the visible <h2> so the SR accessible name tracks the title: a catalog
      // default shows "Default job", not "Edit schedule".
      aria-label={isDefault ? "Default job" : isEdit ? "Edit schedule" : "New schedule"}
      onKeyDown={onDialogKeyDown}
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <form
        onSubmit={submit}
        className="my-8 w-full max-w-xl overflow-hidden rounded-2xl border border-edge-strong bg-surface shadow-2xl"
      >
        {/* Header */}
        <div className="flex items-start justify-between gap-3 border-b border-edge px-5 py-4">
          <div>
            <div className="flex items-center gap-2">
              <h2 className="text-base font-semibold">
                {isDefault ? "Default job" : isEdit ? "Edit schedule" : "New schedule"}
              </h2>
              {isDefault && (
                <Badge tone="brand" dot>
                  DEFAULT
                </Badge>
              )}
            </div>
            <p className="mt-0.5 text-xs text-muted">
              {isDefault
                ? target === "self_improve"
                  ? `${repoPath} · cadence & run options are editable; the improvement directive is baked in worker-side`
                  : `${repoPath} · cadence & run options are editable; the prompt is baked in`
                : `${repoPath || "Pick a repo"} · fires as you, on your Anthropic token`}
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="rounded-md p-1 text-muted hover:bg-raised hover:text-fg"
          >
            <XIcon />
          </button>
        </div>

        {/* Body */}
        <div className="max-h-[70vh] space-y-5 overflow-y-auto px-5 py-5">
          {error && <Alert message={error} />}

          {/* Repo picker (non-pinned). Edit mode is a single repoint <Select> (PRD #344),
              except for an issue target which is repo-relative and cannot move. Create mode
              is a multi-repo picker (PRD #636 Decision 6): N repos → N independent schedules. */}
          {!pinned &&
            (isEdit ? (
              <Field label="Repo" htmlFor="sched-repo">
                <Select
                  id="sched-repo"
                  value={repoId}
                  onChange={(e) => setRepoId(e.target.value)}
                  disabled={isDefault || target === "issue"}
                >
                  {repos.length === 0 && <option value="">No repos available</option>}
                  {repos.map((r) => (
                    <option key={r.id} value={r.id}>
                      {r.path_with_namespace}
                    </option>
                  ))}
                </Select>
                {target === "issue" && (
                  <p className="mt-1 text-[11px] text-faint">
                    An issue-target schedule can't be repointed — its issue number is
                    repo-specific. Delete and recreate it on the new repo.
                  </p>
                )}
              </Field>
            ) : (
              <div className="space-y-1.5">
                <RepoMultiSelect
                  id="sched-repos"
                  label="Repos"
                  repos={repos}
                  selected={selectedRepoIds}
                  onChange={setSelectedRepoIds}
                />
                <p className="text-right text-[12px] text-faint">
                  {selectedRepoIds.length === 0
                    ? "Pick at least one repo"
                    : `${selectedRepoIds.length} repo${selectedRepoIds.length === 1 ? "" : "s"} → ${selectedRepoIds.length} schedule${selectedRepoIds.length === 1 ? "" : "s"}`}
                </p>
              </div>
            ))}

          {/* Target — a catalog default's target/prompt/labels/guidance are catalog-owned,
              so they render read-only (the "baked" values); a user schedule keeps the full
              editable segmented control + fields. */}
          {isDefault ? (
            <>
              <BakedTargetDetail
                target={target}
                prompt={prompt}
                labels={labels}
                bakedGuidance={bakedGuidance}
              />
              {/* A PROMPT default's guidance (issue #662) and a SWEEP default's guidance
                  (issue #675) are owner-editable: the baked prompt/guidance stays read-only
                  above, but this OVERLAY is appended to it at fire time. issue/self_improve
                  defaults keep their guidance catalog-owned (baked, shown read-only). */}
              {(target === "prompt" || target === "sweep") && guidanceField}
            </>
          ) : (
            <>
              <SegmentedControl
                label="Target"
                value={target}
                onChange={setTarget}
                options={TARGET_OPTIONS}
                disabled={!!pinned}
              />

              {target === "issue" && (
                <>
                  <Field label="Issue number" htmlFor="sched-issue">
                    <Input
                      id="sched-issue"
                      type="number"
                      min={1}
                      value={issueIid}
                      disabled={!!pinned}
                      onChange={(e) => setIssueIid(e.target.value)}
                      placeholder="e.g. 142"
                    />
                  </Field>
                  {guidanceField}
                </>
              )}

              {target === "sweep" && (
                <div className="space-y-2">
                  <span className="text-sm font-medium text-muted">Labels</span>
                  <div className="flex flex-wrap items-center gap-1.5">
                    {labels.map((l) => (
                      <span
                        key={l}
                        className="inline-flex items-center gap-1 rounded-md border border-brand/40 bg-brand/10 px-2 py-0.5 text-[11px] text-brand"
                      >
                        {l}
                        <button
                          type="button"
                          aria-label={`Remove ${l}`}
                          onClick={() => setLabels((ls) => ls.filter((x) => x !== l))}
                          className="text-brand/70 hover:text-brand"
                        >
                          ✕
                        </button>
                      </span>
                    ))}
                    <input
                      value={labelInput}
                      onChange={(e) => setLabelInput(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === "Enter" || e.key === ",") {
                          e.preventDefault();
                          addLabel();
                        }
                      }}
                      onBlur={addLabel}
                      placeholder="add label…"
                      className="min-w-[120px] flex-1 rounded-md border border-edge bg-raised px-2 py-1 text-xs text-fg placeholder:text-faint outline-hidden focus:border-brand/70"
                    />
                  </div>
                  <p className="text-[11px] text-faint">
                    Empty ⇒ the <span className="font-medium text-muted">{uziLabel}</span> label. A selector chooses
                    candidates; a candidate fires only when it also carries the{" "}
                    <span className="font-medium text-muted">{uziLabel}</span> label.
                  </p>
                  {/* Sweep-warn (success criterion 6): a selector label missing on a chosen
                      repo means the sweep matches nothing — advisory, never blocking. Edit
                      mode checks the single repoint target; create mode checks EACH selected
                      repo (PRD #636 Decision 8), since the fan-out lands one schedule per repo. */}
                  {isEdit ? (
                    <SweepLabelWarn repoId={repoId} repoPath={repoPath} labels={labels} />
                  ) : (
                    selectedRepoIds.map((rid) => (
                      <SweepLabelWarn
                        key={rid}
                        repoId={rid}
                        repoPath={repos.find((r) => r.id === rid)?.path_with_namespace}
                        labels={labels}
                      />
                    ))
                  )}
                  <Field label="Max issues per run" htmlFor="sched-max-issues">
                    <Input
                      id="sched-max-issues"
                      type="number"
                      min={1}
                      value={maxIssues ?? ""}
                      onChange={(e) => {
                        const v = e.target.value.trim();
                        setMaxIssues(v === "" ? null : Number(v));
                      }}
                      placeholder="unlimited"
                    />
                    <p className="mt-1 text-[11px] text-faint">
                      Oldest issues first. Leave blank for unlimited.
                    </p>
                  </Field>
                  {guidanceField}
                </div>
              )}

              {target === "prompt" && (
                <>
                  <Field label="Prompt" htmlFor="sched-prompt">
                    <Textarea
                      id="sched-prompt"
                      rows={3}
                      value={prompt}
                      onChange={(e) => setPrompt(e.target.value)}
                      placeholder="hunt for flaky tests and open an MR"
                    />
                  </Field>
                  <p className="-mt-2 text-[11px] text-faint">
                    No forge issue — runs against the repo and opens an MR. Bypasses the PRD-issue gate by design;
                    <code className="mx-1 rounded bg-raised px-1">main</code> stays protected by the unchanged guardrails.
                  </p>
                </>
              )}
            </>
          )}

          {/* A default's editable cadence/model/run flags still need max_issues for a
              sweep, since it IS editable on a default (unlike labels/guidance). */}
          {isDefault && target === "sweep" && (
            <Field label="Max issues per run" htmlFor="sched-max-issues">
              <Input
                id="sched-max-issues"
                type="number"
                min={1}
                value={maxIssues ?? ""}
                onChange={(e) => {
                  const v = e.target.value.trim();
                  setMaxIssues(v === "" ? null : Number(v));
                }}
                placeholder="unlimited"
              />
              <p className="mt-1 text-[11px] text-faint">Oldest issues first. Leave blank for unlimited.</p>
            </Field>
          )}

          {/* Timing */}
          <SegmentedControl label="Timing" value={timing} onChange={setTiming} options={TIMING_OPTIONS} />

          {timing === "recurring" ? (
            <Field label="Cadence" htmlFor="sched-preset">
              <div className="grid grid-cols-2 gap-2">
                <Select
                  id="sched-preset"
                  value={presetState.preset}
                  onChange={(e) => onPresetChange(e.target.value as CronPreset)}
                >
                  {PRESET_OPTIONS.map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.label}
                    </option>
                  ))}
                </Select>
                {presetState.preset === "everyNHours" ? (
                  <Input
                    type="number"
                    min={1}
                    max={23}
                    aria-label="Every N hours"
                    value={presetState.everyN}
                    onChange={(e) => onEveryNChange(e.target.value)}
                  />
                ) : presetState.preset === "custom" ? (
                  <div className="flex items-center px-1 text-xs text-faint">Set the cron below</div>
                ) : (
                  <Input
                    type="time"
                    aria-label="Time of day"
                    value={hhmm(presetState)}
                    onChange={(e) => onTimeChange(e.target.value)}
                  />
                )}
              </div>
            </Field>
          ) : (
            <Field label="Fire at" htmlFor="sched-runat">
              <Input
                id="sched-runat"
                type="datetime-local"
                value={runAtLocal}
                onChange={(e) => setRunAtLocal(e.target.value)}
              />
            </Field>
          )}

          {/* Advanced: raw cron + timezone */}
          <details
            open={advancedOpen}
            onToggle={(e) => setAdvancedOpen((e.currentTarget as HTMLDetailsElement).open)}
            className="border-t border-dashed border-edge pt-3"
          >
            <summary className="cursor-pointer text-xs font-medium text-brand">
              Advanced — raw cron &amp; timezone
            </summary>
            <div className="mt-3 space-y-3">
              {timing === "recurring" && (
                <Field label="Cron expression" htmlFor="sched-cron">
                  <Input
                    id="sched-cron"
                    className="font-mono"
                    value={cron}
                    onChange={(e) => onRawCronChange(e.target.value)}
                    placeholder="0 2 * * 1-5"
                  />
                  <p className="mt-1 text-[11px] text-faint">
                    Standard 5-field cron. The preset stays in sync; edit here to go beyond it
                    {presetState.preset === "custom" ? " (currently custom)" : ""}.
                  </p>
                </Field>
              )}
              <Field label="Timezone" htmlFor="sched-tz">
                <Input
                  id="sched-tz"
                  list="sched-tz-list"
                  value={timezone}
                  onChange={(e) => setTimezone(e.target.value)}
                  placeholder="UTC"
                />
                <datalist id="sched-tz-list">
                  {[browserTimezone(), ...COMMON_TIMEZONES].map((tz) => (
                    <option key={tz} value={tz} />
                  ))}
                </datalist>
              </Field>
            </div>
          </details>

          {/* Model override — shown for ALL targets (unlike guidance), in the common area. */}
          <Field label="Model (optional)" htmlFor="sched-model">
            <ModelSelect id="sched-model" value={model} onChange={setModel} />
            <p className="mt-1 text-[11px] text-faint">
              Runs fired by this schedule use this model on every target. Leave on Inherit to
              use your per-user Worker default.
            </p>
          </Field>
          {modelWarning && <Alert message={modelWarning} tone="warning" />}

          {/* PRD #305: apply the run model to every subagent. Always enabled — first-class
              on Inherit (the applied model is the same one the lead resolves). Hidden on a
              catalog default (PRD #589): the server treats it as non-editable there. */}
          {!isDefault && (
            <div className="flex items-start gap-3">
              <Toggle
                checked={overrideSubagentModel}
                onChange={setOverrideSubagentModel}
                label="Apply model also to agents"
              />
              <span className="text-[13px] text-fg">
                Apply model also to agents
                <span className="block text-[11px] text-faint">
                  Subagents run on the same model as the lead — overrides each agent's own model.
                </span>
              </span>
            </div>
          )}

          {/* Options */}
          <div className="space-y-3">
            <span className="text-xs font-semibold uppercase tracking-wide text-muted">Run options</span>
            <div className="flex items-start gap-3">
              <Toggle checked={waitOnLimit} onChange={setWaitOnLimit} label="Wait on usage limit" />
              <span className="text-[13px] text-fg">
                Wait on usage limit
                <span className="block text-[11px] text-faint">
                  Park the run until the Anthropic window reopens instead of failing it
                </span>
              </span>
            </div>
            {/* A self_improve run is always auto-approved (the server forces auto_approve=true),
                so hide the toggle rather than misrepresent a fixed value as a user choice. */}
            {target !== "self_improve" && (
              <div className="flex items-start gap-3">
                <Toggle checked={autoApprove} onChange={setAutoApprove} label="Auto-approve the plan" />
                <span className="text-[13px] text-fg">
                  Auto-approve the plan
                  <span className="block text-[11px] text-faint">
                    Skip the approval gate so unattended runs proceed (like autopilot). Off = the run waits at the gate.
                  </span>
                </span>
              </div>
            )}
            {!isEdit && (
              <div className="flex items-start gap-3">
                <Toggle checked={enabled} onChange={setEnabled} label="Enabled" />
                <span className="text-[13px] text-fg">
                  Enabled
                  <span className="block text-[11px] text-faint">
                    Off = create the schedule paused; it won't fire until you resume it.
                  </span>
                </span>
              </div>
            )}
          </div>

          {/* Next fires preview */}
          <div className="rounded-lg border border-edge bg-ink/50 px-3 py-2.5">
            <p className="mb-1.5 font-mono text-[10px] font-semibold uppercase tracking-wider text-muted">
              Next fires
            </p>
            {previewError ? (
              <p className="text-xs text-danger">This timing is not valid yet.</p>
            ) : fires.length === 0 ? (
              <p className="text-xs text-faint">Nothing to preview yet.</p>
            ) : (
              <ul className="space-y-1">
                {fires.map((f) => (
                  <li key={f} className="flex gap-2 font-mono text-xs tabular-nums text-fg">
                    <span>{formatFire(f, timezone)}</span>
                    <span className="text-faint">{relativeFromNow(f)}</span>
                  </li>
                ))}
              </ul>
            )}
            {timing === "recurring" && presetState.preset !== "custom" && (
              <p className="mt-1.5 text-[11px] text-faint">{humanizeCron(cron)} · {timezone}</p>
            )}
          </div>
        </div>

        {/* Footer */}
        <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-2 border-t border-edge bg-ink/40 px-5 py-3.5">
          <span className="hidden min-w-0 flex-1 truncate text-[11px] text-faint sm:block">{footerSummary}</span>
          <div className="ml-auto flex min-w-0 max-w-full flex-wrap items-center justify-end gap-2">
            {isEdit && (
              <Button type="button" variant="danger" size="sm" onClick={remove} disabled={deleting || saving}>
                <TrashIcon /> {deleting ? "Deleting…" : "Delete"}
              </Button>
            )}
            {isDefault && onCloneToEdit && editing && (
              <Button
                type="button"
                variant="secondary"
                size="sm"
                onClick={() => onCloneToEdit(editing)}
                disabled={saving || deleting}
              >
                <CopyIcon /> Clone to edit
              </Button>
            )}
            <Button type="button" variant="secondary" onClick={onClose} disabled={saving}>
              Cancel
            </Button>
            <Button type="submit" disabled={saving || !canSubmit()}>
              {saving ? "Saving…" : isEdit ? "Save changes" : "Create schedule"}
            </Button>
          </div>
        </div>
      </form>
    </div>
  );
}

// BakedTargetDetail renders a catalog default's catalog-owned fields read-only (PRD
// #589): the target as a static line, and the sealed prompt (prompt target) or the
// selector labels + baked guidance (sweep target). The 🔒 marker and the mono block
// signal "shipped and sealed" — these are edited by cloning to a user row, not here.
function BakedTargetDetail({
  target,
  prompt,
  labels,
  bakedGuidance,
}: {
  target: ScheduleTarget;
  prompt: string;
  labels: string[];
  bakedGuidance: string;
}) {
  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <span className="text-xs font-semibold uppercase tracking-wide text-muted">Target</span>
        <span className="inline-flex items-center gap-1 text-[13px] font-semibold text-fg">
          <span aria-hidden="true" className="text-muted">
            <LockIcon />
          </span>
          {target === "sweep"
            ? "Label sweep"
            : target === "prompt"
              ? "Prompt"
              : target === "self_improve"
                ? "Self-improvement"
                : "Issue"}
          <span className="text-[11px] font-normal text-faint">· baked into the default</span>
        </span>
      </div>

      {/* A self_improve default carries no prompt, labels, or guidance — its tracking
          issue is resolved at fire time — so there is no baked text to seal. Only the
          cadence/model editors below apply. */}
      {target === "self_improve" && (
        <p className="text-[12px] text-faint">
          No baked prompt or labels — this default audits uzi's own codebase and opens one
          improvement MR per cycle. Edit its cadence and model below.
        </p>
      )}

      {target === "sweep" && (
        <div className="space-y-1.5">
          <span className="block text-sm font-medium text-muted">Labels</span>
          <div className="flex flex-wrap gap-1.5">
            {labels.length === 0 ? (
              <span className="text-[12px] text-faint">the PRD label</span>
            ) : (
              labels.map((l) => (
                <span
                  key={l}
                  className="inline-flex items-center rounded-md border border-edge bg-raised px-2 py-0.5 font-mono text-[11px] text-muted"
                >
                  {l}
                </span>
              ))
            )}
          </div>
        </div>
      )}

      {target !== "self_improve" && (
        <BakedBlock
          label={target === "sweep" ? "Baked guidance" : "Baked prompt"}
          text={target === "sweep" ? bakedGuidance : prompt}
        />
      )}
    </div>
  );
}

// BakedBlock is the read-only, sealed catalog text: a labelled mono panel with a brand
// left-border, distinguishing catalog-owned wording from the editable fields around it.
function BakedBlock({ label, text }: { label: string; text: string }) {
  return (
    <div className="space-y-1.5">
      <span className="flex items-center gap-1.5 text-sm font-medium text-muted">
        <span aria-hidden="true">
          <LockIcon />
        </span>
        {label} <span className="text-[11px] font-normal text-faint">(read-only)</span>
      </span>
      <div className="max-h-52 overflow-y-auto whitespace-pre-wrap rounded-lg border border-edge border-l-2 border-l-brand/60 bg-ink/50 px-3 py-2.5 font-mono text-[12px] leading-relaxed text-muted">
        {text || "—"}
      </div>
      <p className="text-[11px] text-faint">
        This wording ships with the default. To change it, clone the default to an editable copy.
      </p>
    </div>
  );
}

// formatFire renders one preview instant. The API returns UTC; we render in the
// schedule's chosen timezone so the preview reads the way the schedule fires.
function formatFire(iso: string, timezone: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  try {
    return new Intl.DateTimeFormat(undefined, {
      weekday: "short",
      day: "2-digit",
      month: "short",
      hour: "2-digit",
      minute: "2-digit",
      timeZone: timezone,
    }).format(d);
  } catch {
    return d.toISOString();
  }
}

// relativeFromNow renders a compact "in 6h 12m" / "in 2d 6h" hint.
export function relativeFromNow(iso: string): string {
  const d = new Date(iso).getTime();
  if (Number.isNaN(d)) return "";
  const ms = d - Date.now();
  if (ms <= 0) return "now";
  const mins = Math.round(ms / 60000);
  const days = Math.floor(mins / 1440);
  const hours = Math.floor((mins % 1440) / 60);
  const rem = mins % 60;
  if (days > 0) return `in ${days}d ${hours}h`;
  if (hours > 0) return `in ${hours}h ${rem}m`;
  return `in ${rem}m`;
}
