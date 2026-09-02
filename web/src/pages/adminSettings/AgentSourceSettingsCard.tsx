import { useCallback, useState, type FormEvent } from "react";
import {
  api,
  ApiError,
  type AgentSourceStaged,
  type AgentSourceView,
  type UpdateSettingsPayload,
} from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { useAsyncData } from "../../lib/useAsyncData";
import {
  Alert,
  Badge,
  type BadgeTone,
  Button,
  Card,
  Field,
  Input,
  SectionTitle,
  Skeleton,
} from "../../components/ui";

// agentSourceActionMeta maps a staged-diff action to its review chip: a tone, a
// short label, and a one-line description of what Approve would do for that role
// (PRD #602 M5). The action set is the server's closed enum; an unknown action
// falls back to a neutral chip so a future action still renders rather than crashing.
function agentSourceActionMeta(action: string): { tone: BadgeTone; label: string; detail: string } {
  switch (action) {
    case "add":
      return { tone: "ok", label: "Add", detail: "New role — added as a shared template." };
    case "override":
      return { tone: "info", label: "Override", detail: "Replaces the shipped builtin body with the source's." };
    case "conflict":
      return {
        tone: "danger",
        label: "Conflict",
        detail: "Name collides with an admin template — skipped, never overwritten.",
      };
    case "remove":
      return {
        tone: "warning",
        label: "Remove",
        detail: "Gone from the source — its synced role is removed (an overridden builtin resets to shipped).",
      };
    case "unchanged":
      return { tone: "neutral", label: "Unchanged", detail: "Already matches — nothing to apply." };
    default:
      // No diff entry for this name (a role-only row: parsed/skipped but nothing to
      // apply). "not applied" is clearer than a bare "—" chip; a future non-empty
      // action string still renders as itself.
      return { tone: "neutral", label: action || "not applied", detail: "" };
  }
}

// AgentSourceStagedReview renders the pending staged snapshot: a count summary, the
// per-role diff (action chip + collapsible sanitized body), and the Approve gate.
// It is a child so the card body stays readable; the parent owns the apply action.
function AgentSourceStagedReview({
  staged,
  applying,
  onApprove,
}: {
  staged: AgentSourceStaged;
  applying: boolean;
  onApprove: () => void;
}) {
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const toggle = (name: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });

  // The diff carries the per-name action; the roles carry the parsed body. Join by
  // name across BOTH so each review row shows what it can: a "remove" has a diff
  // entry and no role, a skipped/failed role has a role and no diff entry, and both
  // must be visible. Diff order first (the applied actions), then any role-only names.
  const roleByName = new Map(staged.roles.map((r) => [r.name, r]));
  const actionByName = new Map(staged.diff.map((d) => [d.name, d]));
  const names = Array.from(new Set([...staged.diff.map((d) => d.name), ...staged.roles.map((r) => r.name)]));

  return (
    <div className="space-y-4 rounded-xl border border-plan/40 bg-plan/5 p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <Badge tone="plan" dot pulse>
            review needed
          </Badge>
          <span className="text-sm font-medium text-fg">Staged changes from the source</span>
        </div>
        <div className="flex items-center gap-3 text-xs text-faint">
          <span>
            <strong className="text-fg">{staged.counts.staged}</strong> staged
          </span>
          <span>
            <strong className="text-fg">{staged.counts.changed}</strong> changed
          </span>
          <span className={staged.counts.failed > 0 ? "text-warn" : undefined}>
            <strong className={staged.counts.failed > 0 ? "text-warn" : "text-fg"}>{staged.counts.failed}</strong>{" "}
            failed
          </span>
        </div>
      </div>

      <p className="text-xs text-muted">
        Fetched{" "}
        {staged.fetched_at ? new Date(staged.fetched_at).toLocaleString() : "just now"} at{" "}
        <code className="rounded bg-raised px-1 py-0.5 font-mono text-fg">{staged.fetched_sha.slice(0, 10)}</code>.
        Nothing below is applied to your agents until you approve it.
      </p>

      <ul className="space-y-2">
        {names.map((name) => {
          const diff = actionByName.get(name);
          const role = roleByName.get(name);
          const meta = agentSourceActionMeta(diff?.action ?? "");
          const isOpen = expanded.has(name);
          const hasBody = Boolean(role?.prompt_body);
          return (
            <li key={name} className="rounded-lg border border-edge bg-surface">
              <div className="flex flex-wrap items-center gap-2 px-3 py-2">
                <Badge tone={meta.tone}>{meta.label}</Badge>
                <code className="font-mono text-sm text-fg">{name}</code>
                {role && !role.ok && (
                  <Badge tone="warning" title={`Skipped: ${role.reason ?? "unparseable"}`}>
                    skipped
                  </Badge>
                )}
                <span className="min-w-0 flex-1 truncate text-xs text-faint">
                  {diff?.detail || meta.detail || role?.reason}
                </span>
                {hasBody && (
                  <Button variant="ghost" onClick={() => toggle(name)} aria-expanded={isOpen}>
                    {isOpen ? "Hide body" : "Show body"}
                  </Button>
                )}
              </div>
              {/* Approval-surface honesty: when the server stripped control/bidi/format
                  chars for display, the preview under-represents the raw body. Flag it
                  ALWAYS (not only when expanded) so the admin never approves blind. */}
              {role?.body_sanitized && (
                <p className="flex items-start gap-1.5 border-t border-edge px-3 py-2 text-xs text-warn">
                  <span aria-hidden="true">⚠</span>
                  <span>
                    Hidden formatting characters were removed from this preview — the raw body is what will run.
                  </span>
                </p>
              )}
              {isOpen && role && (
                <div className="border-t border-edge px-3 py-2">
                  {(role.description || role.model || (role.tools && role.tools.length > 0)) && (
                    <dl className="mb-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
                      {role.description && (
                        <>
                          <dt className="text-faint">Description</dt>
                          <dd className="text-muted">{role.description}</dd>
                        </>
                      )}
                      <dt className="text-faint">Model</dt>
                      <dd className="text-muted">{role.model || "inherit"}</dd>
                      <dt className="text-faint">Tools</dt>
                      <dd className="font-mono text-muted">
                        {role.tools && role.tools.length > 0 ? role.tools.join(", ") : "inherit all"}
                      </dd>
                    </dl>
                  )}
                  {/* prompt_body is ALREADY server-sanitized (termsafe.SanitizeTTY) — a
                      plain text node, never dangerouslySetInnerHTML. */}
                  <pre className="max-h-64 overflow-auto whitespace-pre-wrap rounded-lg border border-edge bg-ink p-3 text-xs text-muted">
                    {role.prompt_body}
                  </pre>
                </div>
              )}
            </li>
          );
        })}
      </ul>

      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-xs text-muted">
          Approving applies every change above at once and records it as the last-applied snapshot.
        </p>
        <Button onClick={onApprove} disabled={applying}>
          {applying ? "Applying…" : "Approve & apply"}
        </Button>
      </div>
    </div>
  );
}

// AgentSourceSettingsCard is the admin surface for the configurable agent-source
// repo (PRD #602 M5). It self-loads a dedicated endpoint (config + last-sync status
// + a STAGED snapshot). The trust model is the whole
// point of the copy: a sync only STAGES what the source repo contains; nothing
// reaches a run until an admin reviews the diff and approves it. A fresh install
// has no URL and is disabled — the card reads as "nothing configured" in that state.
// The one-click preset that follows uzi's canonical roster (PRD #702 M3). The URL and
// folder are the literal published shape of the skills repo; the ref is NOT hardcoded —
// it is resolved to the latest tag at click time via the ls-remote endpoint. Enabling
// the source still needs both URL and ref non-empty, which the resolved tag satisfies.
const SKILLS_PRESET_URL = "https://github.com/vtmocanu/skills";
const SKILLS_PRESET_FOLDER = "product-agents/";

export function AgentSourceSettingsCard() {
  // view is seeded by the load's resetForm but ALSO updated by refreshView / syncNow /
  // checkForUpdates, so it stays local and the fetcher seeds it as a side effect.
  const [view, setView] = useState<AgentSourceView | null>(null);
  const [url, setUrl] = useState("");
  const [ref, setRef] = useState("");
  // Repo-relative subfolder role files are read from (PRD #702 M1). The server
  // resolves empty/unset to ".claude/agents", so the input defaults to that.
  const [folder, setFolder] = useState(".claude/agents");
  const [enabled, setEnabled] = useState(false);
  const [interval, setIntervalValue] = useState("1h");
  // Write-only credential input: blank means "leave the stored one unchanged".
  const [credential, setCredential] = useState("");
  const [savingConfig, setSavingConfig] = useState(false);
  const [syncing, setSyncing] = useState(false);
  const [applying, setApplying] = useState(false);
  // True while an "update available" check ls-remotes the configured source (PRD #702 M4).
  const [checking, setChecking] = useState(false);
  // True while a "bump pin" write is in flight (PRD #702 M4).
  const [bumping, setBumping] = useState(false);
  // True while the Preset button is resolving the latest tag from the source (PRD #702 M3).
  const [resolving, setResolving] = useState(false);
  // Preset outcome rendered INLINE in the preset block, co-located with the button
  // (PRD #702 M3 UX fix). The card-level error/notice Alerts are pinned at the TOP of
  // this long card, ~793px above the Preset button — on a resolve failure their
  // explanation scrolls off-screen, so the preset's own feedback lives beside the action.
  const [presetMsg, setPresetMsg] = useState<{ tone: "success" | "warning" | "danger"; text: string } | null>(
    null,
  );
  // Check/Bump outcome rendered INLINE beside the Check-for-updates / Bump-pin controls
  // in the Sync-status block (PRD #702 M4 UX fix). Those controls live ~228px below the
  // card-TOP error/notice Alerts, so a "no update found" success or a bump confirmation
  // routed through the card-top banner would render off-screen — this local state keeps
  // their feedback co-located with the buttons. The shared Alert sets role=alert for
  // danger and role=status otherwise, so it stays announced.
  const [updateMsg, setUpdateMsg] = useState<{ tone: "success" | "warning" | "danger"; text: string } | null>(
    null,
  );
  // Kept local: the save / sync / approve / preset / check / bump handlers below all
  // set this, so it is merged with the hook's load error at the one Alert below.
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  // resetForm mirrors the SAVED server config into the controlled inputs. It runs
  // ONLY on the initial load and after an explicit Save — never on a Sync-now or
  // Approve refresh, which act on the saved config and must preserve whatever the
  // admin has typed (a refresh that reset the inputs would silently discard edits).
  const resetForm = useCallback((v: AgentSourceView) => {
    setView(v);
    setUrl(v.config.url);
    setRef(v.config.ref);
    setFolder(v.config.folder || ".claude/agents");
    setEnabled(v.config.enabled);
    setIntervalValue(v.config.interval || "1h");
  }, []);

  // refreshView re-reads only the status + staged panels, leaving the form inputs
  // (and any unsaved edits) untouched. Used after Sync-now and Approve.
  const refreshView = useCallback(async () => {
    const { agent_source } = await api.getAgentSource();
    setView(agent_source);
  }, []);

  // The initial load resets the form (resetForm) — as does an explicit Save via
  // reload() below — while refreshView (used by Sync/Approve/Bump) deliberately does
  // NOT, preserving unsaved edits. The hook owns loading + the load error only.
  const { loading, error: loadError, reload } = useAsyncData(
    async ({ isCurrent }) => {
      const { agent_source } = await api.getAgentSource();
      if (!isCurrent()) return;
      resetForm(agent_source);
    },
    [resetForm],
    { fallback: "Failed to load agent-source settings" },
  );

  // Preset: fill the URL + folder for uzi's canonical roster and resolve its latest tag
  // at click time (PRD #702 M3). The URL/folder are filled IMMEDIATELY so a resolve
  // failure (github.com not on this deployment's allowlist, or an unreachable source)
  // still leaves the admin a filled form to edit and Save. type="button" so it does not
  // submit the surrounding form. The admin still reviews and Saves.
  const fillPreset = async () => {
    // Preset feedback renders INLINE below (presetMsg), not through the card-level
    // error/notice Alerts at the top of the card — clear all three at the start so a
    // prior Save/Sync banner and a stale preset outcome both go away.
    setPresetMsg(null);
    setError("");
    setNotice("");
    setResolving(true);
    setUrl(SKILLS_PRESET_URL);
    setFolder(SKILLS_PRESET_FOLDER);
    try {
      const { latest_ref } = await api.resolveAgentSourceLatest(SKILLS_PRESET_URL);
      if (latest_ref.trim() === "") {
        // The source is reachable but publishes no semver tag yet. Leave URL+folder
        // filled and the ref as-is so the admin can Save or set a ref by hand.
        setPresetMsg({
          tone: "warning",
          text:
            "Preset filled the URL and folder, but the source publishes no semver tag yet — " +
            "it may not tag releases. Set a ref by hand or Save once it does.",
        });
      } else {
        setRef(latest_ref);
        setPresetMsg({
          tone: "success",
          text: `Preset filled — resolved latest tag ${latest_ref}. Review and Save to apply.`,
        });
      }
    } catch (err) {
      // Graceful: URL+folder stay filled so the admin can still edit/Save.
      setPresetMsg({
        tone: "danger",
        text: errorMessage(
          err,
          "Could not resolve the latest tag. The URL and folder are filled — set a ref by hand or try again.",
        ),
      });
    } finally {
      setResolving(false);
    }
  };

  const saveConfig = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (enabled && url.trim() === "") {
      setError("Set a repository URL before enabling the agent source.");
      return;
    }
    setSavingConfig(true);
    try {
      const payload: UpdateSettingsPayload = {
        agent_source_repo_url: url.trim(),
        agent_source_ref: ref.trim(),
        agent_source_folder: folder.trim(),
        agent_source_enabled: String(enabled),
        agent_source_interval: interval.trim(),
      };
      // Send the credential only when the admin typed one — an empty field leaves the
      // stored credential untouched (write-only, like the Slack tokens).
      if (credential.trim() !== "") payload.agent_source_credential = credential;
      await api.updateSettings(payload);
      setCredential("");
      // updateSettings returns the settings envelope, not the agent-source view —
      // re-read so the status/credential-configured/staged panels reflect the save.
      await reload();
      setNotice("Agent-source settings saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save agent-source settings"));
    } finally {
      setSavingConfig(false);
    }
  };

  const syncNow = async () => {
    setError("");
    setNotice("");
    setSyncing(true);
    try {
      const { agent_source } = await api.syncAgentSource();
      // Refresh the status + staged panels only — the sync ran against the SAVED
      // config, so the admin's in-progress form edits must survive untouched.
      setView(agent_source);
      const st = agent_source.status;
      if (st.last_sync_status === "error") {
        setError(st.last_sync_error || "The sync failed. Check the repository URL, ref, and credential.");
      } else if (agent_source.staged?.pending) {
        setNotice("Sync complete. Review the staged changes below, then approve to apply.");
      } else {
        setNotice("Sync complete. The source matches what is applied — nothing to review.");
      }
    } catch (err) {
      setError(errorMessage(err, "Sync failed"));
    } finally {
      setSyncing(false);
    }
  };

  const approve = async () => {
    const staged = view?.staged;
    if (!staged) return;
    setError("");
    setNotice("");
    setApplying(true);
    try {
      const { result } = await api.applyAgentSource(staged.fetched_sha);
      await refreshView();
      setNotice(
        `Applied ${result.applied} change${result.applied === 1 ? "" : "s"}` +
          (result.deprovisioned > 0 ? `, removed ${result.deprovisioned}` : "") +
          (result.conflicts > 0 ? `, skipped ${result.conflicts} conflict${result.conflicts === 1 ? "" : "s"}` : "") +
          ".",
      );
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        // The staged snapshot changed since it was reviewed (a concurrent restage, or
        // it was already applied). Re-read so the admin reviews the current diff, and
        // say so plainly rather than retrying blind.
        await refreshView();
        setError("The staged snapshot changed since you reviewed it. Review the refreshed diff below, then approve again.");
      } else {
        setError(errorMessage(err, "Failed to apply the staged changes"));
      }
    } finally {
      setApplying(false);
    }
  };

  // Check for updates: ls-remote the SAVED configured source and refresh the derived
  // update-available signal (PRD #702 M4). Like Sync-now it acts on the saved config and
  // leaves the form inputs (and any unsaved edits) untouched. The update check reuses the
  // last_sync_error slot for its own error message. The outcome renders INLINE beside the
  // control (updateMsg), not through the card-top banner, so the "no update found" success
  // has a visible on-screen signal rather than one 228px above the button.
  const checkForUpdates = async () => {
    setError("");
    setNotice("");
    setUpdateMsg(null);
    setChecking(true);
    try {
      const { agent_source } = await api.updateCheckAgentSource();
      setView(agent_source);
      const st = agent_source.status;
      if (st.last_sync_status === "error") {
        setUpdateMsg({
          tone: "danger",
          text:
            st.last_sync_error || "The update check failed. Check the repository URL, ref, and credential.",
        });
      } else if (st.update_available === true) {
        setUpdateMsg({ tone: "success", text: "Update check complete." });
      } else {
        setUpdateMsg({ tone: "success", text: "No update available — you're on the latest." });
      }
    } catch (err) {
      setUpdateMsg({ tone: "danger", text: errorMessage(err, "Update check failed") });
    } finally {
      setChecking(false);
    }
  };

  // Bump pin: write the newer tag as the saved ref via the generic settings PUT (PRD #702
  // M4). This ONLY moves the pin — the admin still Syncs → reviews → approves to apply.
  // After the write, refreshView() re-reads the status + staged panels (NOT the form) so
  // the admin's unsaved edits to the other fields survive, then only the ref input is set
  // to the bumped tag. refreshView's GET re-derives the badge against the now-saved ref
  // (the server sees the just-written ref == latest_ref), so the derived update signal
  // self-clears. The outcome renders INLINE beside the control (updateMsg), not through the
  // card-top banner, so the confirmation is on-screen next to the button.
  const bumpPin = async () => {
    const latest = view?.status.latest_ref;
    if (!latest) return;
    setError("");
    setNotice("");
    setUpdateMsg(null);
    setBumping(true);
    try {
      await api.updateSettings({ agent_source_ref: latest });
      await refreshView();
      setRef(latest);
      setUpdateMsg({
        tone: "success",
        text: `Pinned ref updated to ${latest}. Sync to stage it, then review and approve to apply.`,
      });
    } catch (err) {
      setUpdateMsg({
        tone: "danger",
        text: errorMessage(err, "Failed to update the pinned ref"),
      });
    } finally {
      setBumping(false);
    }
  };

  const status = view?.status;
  // Sync-now acts on the SAVED server config, so its enabled state reads that config
  // (view.config.url), never the local `url` input — otherwise typing a URL without
  // saving would arm a sync against the still-empty stored config.
  const configured = (view?.config.url ?? "").trim() !== "";
  // Whether the form holds unsaved edits vs. the saved config. Sync-now stays enabled
  // while dirty (a manual refresh shouldn't force saving unrelated edits first), but a
  // note makes clear it runs against the saved config, not the in-progress edits.
  const dirty =
    view !== null &&
    (url !== view.config.url ||
      ref !== view.config.ref ||
      folder !== (view.config.folder || ".claude/agents") ||
      enabled !== view.config.enabled ||
      interval !== (view.config.interval || "1h") ||
      credential.trim() !== "");
  const syncTone: BadgeTone =
    status?.last_sync_status === "ok" ? "ok" : status?.last_sync_status === "error" ? "danger" : "neutral";
  // Update-availability signal (PRD #702 M4), DERIVED server-side and read straight from
  // the status. `updateAvailable` gates the whole badge; a non-empty `latest_ref` is the
  // tag-pinned "newer tag" case (Bump pin applies), an empty one is the branch "moved"
  // signal (Decision 6). `stagedPending` distinguishes "source moved past what's running"
  // from "a change is already staged awaiting approval" (#602's staged state).
  const updateAvailable = status?.update_available === true;
  const latestRef = (status?.latest_ref ?? "").trim();
  const stagedPending = view?.staged?.pending === true;

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Agent source</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Point uzi at a Git repository of agent definitions to keep your team's agents in step with a shared,
          version-controlled source. A sync only <strong className="text-fg">stages</strong> what the source
          contains &mdash; you review the exact changes and approve them before anything reaches a run.
        </p>
      </div>

      {(error || loadError) && <Alert message={error || loadError} />}
      {notice && <Alert tone="success" message={notice} />}

      {loading ? (
        <Skeleton className="h-24" />
      ) : (
        <>
          {/* Status panel — reads "Never synced" on a fresh install rather than empty. */}
          <div className="space-y-2 rounded-xl border border-edge bg-raised/40 p-4">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <span className="text-sm font-medium text-fg">Sync status</span>
              <Badge tone={syncTone} dot>
                {status?.last_sync_status === "ok"
                  ? "healthy"
                  : status?.last_sync_status === "error"
                    ? "error"
                    : "never synced"}
              </Badge>
            </div>
            {status?.last_sync_status === "error" && status.last_sync_error && (
              <p className="text-xs text-warn">{status.last_sync_error}</p>
            )}
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
              <dt className="text-faint">Last synced</dt>
              <dd className="text-muted">
                {status?.last_sync_at ? new Date(status.last_sync_at).toLocaleString() : "never"}
                {status?.last_sync_sha && (
                  <code className="ml-2 rounded bg-raised px-1 py-0.5 font-mono text-fg">
                    {status.last_sync_sha.slice(0, 10)}
                  </code>
                )}
              </dd>
              <dt className="text-faint">Last applied</dt>
              <dd className="text-muted">
                {status?.last_applied_at ? new Date(status.last_applied_at).toLocaleString() : "never"}
                {status?.last_applied_sha && (
                  <code className="ml-2 rounded bg-raised px-1 py-0.5 font-mono text-fg">
                    {status.last_applied_sha.slice(0, 10)}
                  </code>
                )}
              </dd>
              {status?.counts && (
                <>
                  <dt className="text-faint">Last sync counts</dt>
                  <dd className="text-muted">
                    {status.counts.staged} staged · {status.counts.changed} changed ·{" "}
                    <span className={status.counts.failed > 0 ? "text-warn" : undefined}>
                      {status.counts.failed} failed
                    </span>
                  </dd>
                </>
              )}
            </dl>
            {/* "Update available" signal (PRD #702 M4), derived server-side from the last
                update check. A non-empty latest_ref is the tag-pinned newer-tag case (Bump
                pin below applies it); an empty one is the branch "moved" signal. The copy
                never implies the update is already applied — it must still be Synced,
                reviewed, and approved. When a change is ALSO already staged, a separate note
                distinguishes "source moved past what's running" from "staged awaiting
                approval" (#602's staged state). */}
            {updateAvailable && (
              <div className="flex flex-wrap items-center gap-x-2 gap-y-1 pt-1">
                <Badge tone="warning" dot wrap>
                  {latestRef ? `Update available: ${latestRef}` : "Source moved"}
                </Badge>
                {status?.update_checked_at && (
                  <span className="text-xs text-faint">
                    checked {new Date(status.update_checked_at).toLocaleString()}
                  </span>
                )}
                {/* The branch "moved" case (empty latest_ref) has no Bump-pin control — a
                    branch pin is not a tag — so it needs a one-line what-next caption. */}
                {!latestRef && (
                  <span className="w-full text-xs text-faint">
                    The tracked branch advanced — Sync now to stage it.
                  </span>
                )}
                {stagedPending && (
                  <span className="w-full text-xs text-faint">
                    A change is staged for approval below — review and approve it to apply.
                  </span>
                )}
              </div>
            )}
            <div className="flex flex-wrap items-center gap-2 pt-1">
              <Button variant="ghost" onClick={syncNow} disabled={syncing || !configured}>
                {syncing ? "Syncing…" : "Sync now"}
              </Button>
              <Button variant="ghost" onClick={checkForUpdates} disabled={checking || !configured}>
                {checking ? "Checking…" : "Check for updates"}
              </Button>
              {updateAvailable && latestRef && (
                <Button variant="secondary" onClick={bumpPin} disabled={bumping}>
                  {bumping ? "Bumping…" : `Bump pin to ${latestRef}`}
                </Button>
              )}
              {!configured ? (
                <span className="ml-2 text-xs text-faint">Save a repository URL below to sync.</span>
              ) : dirty ? (
                <span className="ml-2 text-xs text-faint">
                  Sync now uses the saved configuration, not your unsaved edits.
                </span>
              ) : null}
            </div>
            {/* Check/Bump outcome, INLINE beside the controls (PRD #702 M4 UX fix). The
                shared Alert carries role="alert" for danger and role="status" otherwise,
                so a "no update found" success or a bump confirmation is announced here,
                not routed to the card-top banner ~228px above and off-screen. */}
            {updateMsg && <Alert tone={updateMsg.tone} message={updateMsg.text} />}
          </div>

          {/* Staged-diff review + approve — only when a snapshot is pending. */}
          {view?.staged?.pending && (
            <AgentSourceStagedReview staged={view.staged} applying={applying} onApprove={approve} />
          )}

          <form onSubmit={saveConfig} className="space-y-4">
            <div className="space-y-1.5 rounded-lg border border-edge bg-raised/40 p-3">
              <Button type="button" variant="secondary" size="sm" onClick={fillPreset} disabled={resolving}>
                {resolving ? "Resolving…" : "Use uzi skills preset"}
              </Button>
              <p className="text-xs text-faint">
                Points the source at{" "}
                <code className="rounded bg-raised px-1 py-0.5 font-mono text-fg">github.com/vtmocanu/skills</code>{" "}
                and folder{" "}
                <code className="rounded bg-raised px-1 py-0.5 font-mono text-fg">product-agents/</code>, resolving
                the latest tag now. Requires <code className="rounded bg-raised px-1 py-0.5 font-mono text-fg">github.com</code>{" "}
                on this deployment&rsquo;s agent-source allowlist and a public source. Review the filled values and
                Save to apply.
              </p>
              {/* Preset outcome, INLINE beside the button (PRD #702 M3 UX fix). The
                  shared Alert carries role="alert" for danger and role="status"
                  otherwise, so a resolve failure — off-allowlist or no semver tag — is
                  announced right here, not 793px up in the card-level banner. */}
              {presetMsg && <Alert tone={presetMsg.tone} message={presetMsg.text} />}
            </div>

            <div className="space-y-1.5">
              <Field label="Repository URL">
                <Input
                  value={url}
                  autoComplete="off"
                  placeholder="https://github.com/your-org/agents.git"
                  onChange={(e) => setUrl(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                An https Git URL on the agent-source allowlist. Leave empty to disable the source entirely.
              </p>
            </div>

            <Field label="Ref">
              <Input
                value={ref}
                maxLength={255}
                autoComplete="off"
                placeholder="v1.0.0"
                onChange={(e) => setRef(e.target.value)}
              />
            </Field>

            <div className="space-y-1.5">
              <Field label="Source folder">
                <Input
                  value={folder}
                  maxLength={255}
                  autoComplete="off"
                  placeholder=".claude/agents"
                  onChange={(e) => setFolder(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                The repo-relative subfolder to read role files from. Leave empty for the default{" "}
                <code className="rounded bg-raised px-1 py-0.5 font-mono text-fg">.claude/agents</code>.
              </p>
            </div>

            <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={enabled}
                onChange={(e) => setEnabled(e.target.checked)}
                className="h-4 w-4 rounded border-edge accent-brand"
              />
              Sync automatically on the interval below
            </label>

            <div className="space-y-1.5">
              <Field label="Sync interval">
                <Input
                  value={interval}
                  maxLength={32}
                  autoComplete="off"
                  placeholder="1h"
                  onChange={(e) => setIntervalValue(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                How often the source is re-checked, as a Go duration (e.g.{" "}
                <code className="rounded bg-raised px-1 py-0.5 text-fg">1h</code>). Each auto-sync still only
                stages — approval stays manual.
              </p>
            </div>

            <div className="space-y-1.5">
              <Field label="Access credential">
                <Input
                  type="password"
                  value={credential}
                  autoComplete="off"
                  placeholder={
                    view?.config.credential_configured ? "Configured — type to replace" : "For a private repo"
                  }
                  onChange={(e) => setCredential(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                {view?.config.credential_configured ? (
                  <>
                    A credential is <span className="font-medium text-fg">configured</span>. It is never shown;
                    type a new value to replace it, or leave this blank to keep it.
                  </>
                ) : (
                  <>
                    No credential is set. A public repo needs none; for a private repo, paste a token here (stored
                    sealed, never shown again).
                  </>
                )}
              </p>
            </div>

            <Button type="submit" disabled={savingConfig}>
              {savingConfig ? "Saving…" : "Save agent-source settings"}
            </Button>
          </form>
        </>
      )}
    </Card>
  );
}
