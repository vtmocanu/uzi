// Settings → Workers: register workers, show the one-time join token, and list
// the fleet with live status. Inside SettingsShell.

import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, type SecretMeta, type Worker } from "../lib/api";
import { Alert, Badge, Button, Card, EmptyState, Field, Input, SectionTitle, Select, Skeleton } from "../components/ui";
import { SettingsShell } from "../components/SettingsShell";
import { FleetUpgradePanel, WorkerUpgradeBadge, WorkerUpgradeDetail } from "../components/WorkerUpgradeBadge";
import { useAppVersion } from "../components/AppShell";
import { ServerIcon } from "../components/icons";
import { DEFAULT_WORKER_TEMPLATE, WORKER_TEMPLATES, hasTemplateDrift } from "../lib/workerTemplates";
import { HostedWorkers } from "../components/HostedWorkers";
import { WorkerRunBadge } from "../components/WorkerRunBadge";
import { WorkerStatGauges } from "../components/WorkerStats";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";

// Stable per-row ids: the delete button is a focus target after a dismissed confirm,
// and the warning is the confirm group's aria-description (PRD #58).
const deleteButtonId = (workerId: string) => `worker-delete-${workerId}`;
const deleteWarningId = (workerId: string) => `worker-delete-warning-${workerId}`;

export function WorkersSettings() {
  const [workers, setWorkers] = useState<Worker[]>([]);
  // The control plane's own release, from the same memoised GET /api/version the footer
  // uses — one coordinate, so the panel and the footer cannot disagree.
  const cpVersion = useAppVersion();
  // The user's named tokens, for the per-worker picker (PRD #104 M3/M6). Read
  // alongside the workers so a rebind can offer labels without a second round trip.
  const [tokens, setTokens] = useState<SecretMeta[]>([]);
  // Which worker's rebind is in flight, so only that row's picker disables.
  const [tokenBusy, setTokenBusy] = useState("");
  const [loading, setLoading] = useState(true);
  const [name, setName] = useState("");
  // The declared worker template (PRD #18): what image the user says this worker
  // will run. Defaults to the base image; the worker later self-reports its
  // actual template and any mismatch shows as a drift badge.
  const [template, setTemplate] = useState<string>(DEFAULT_WORKER_TEMPLATE);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // newToken holds the plaintext join token for the just-created worker. It is
  // shown exactly once (only its hash is stored server-side) and cleared on the
  // next action.
  const [newToken, setNewToken] = useState<{ worker: string; token: string } | null>(null);
  const [copied, setCopied] = useState(false);
  // Which hosted worker is armed for deletion (PRD #58). Hosted only — see the Delete
  // button below.
  const [confirmingDelete, setConfirmingDelete] = useState<string | null>(null);
  const confirmRef = useRef<HTMLDivElement>(null);
  // The row whose Delete button should get focus back after a dismissed confirmation.
  const [restoreFocusTo, setRestoreFocusTo] = useState<string | null>(null);
  // ONE announcement slot, shared by provisioning and deleting, and that is why it
  // lives here rather than in HostedWorkers. Two reasons, both structural: a delete has
  // to REPLACE a provision's message (once the row is gone, "it appears in your workers
  // below" is simply false), and HostedWorkers renders nothing at all when hosting is
  // off — which is exactly where external deletes still happen.
  //
  // The OBJECT WRAPPER is the mechanism, and it is not incidental — do not "simplify"
  // this to a bare string. The focus effect below keys on `notice`, so every announce()
  // must produce a value the effect sees as new. A fresh object literal always is. A
  // string would not: derived names are NOT unique ("base (S)" twice is exactly what a
  // quota of 2 produces), so deleting two identically-named workers would set the same
  // string, React would bail out, the effect would not re-fire, and the second
  // announcement would silently never take focus.
  const [notice, setNotice] = useState<{ text: string } | null>(null);
  const noticeRef = useRef<HTMLDivElement>(null);
  const announce = useCallback((text: string) => {
    setNotice({ text });
  }, []);

  const load = useCallback(async () => {
    try {
      const [{ workers }, { secrets }] = await Promise.all([api.listWorkers(), api.listSecrets()]);
      setWorkers(workers);
      setTokens(secrets.filter((s) => s.kind === "anthropic_token"));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load workers");
    } finally {
      setLoading(false);
    }
  }, []);

  // rebind points one worker at a named token, or clears the binding when the
  // picker returns to "default token". The change lands on the worker's NEXT
  // claim — no restart — which the announcement says, because a user who expects
  // to restart something will otherwise go looking for the control to do it.
  const rebind = useCallback(
    async (workerId: string, label: string) => {
      setError("");
      setTokenBusy(workerId);
      try {
        const { worker } = await api.setWorkerToken(workerId, label === "" ? null : label);
        setWorkers((prev) => prev.map((w) => (w.id === worker.id ? worker : w)));
        announce(
          label === ""
            ? `${worker.name} now spends your default token, from its next claim.`
            : `${worker.name} now spends ${label}, from its next claim.`,
        );
      } catch (err) {
        setError(err instanceof ApiError ? err.message : "Failed to change the worker's token");
      } finally {
        setTokenBusy("");
      }
    },
    // announce is stable (a setState wrapper); listing it keeps the lint rule happy
    // without re-creating rebind on every render.
    [],
  );

  useEffect(() => {
    load();
  }, [load]);

  // Liveness (PRD #49): re-fetch the fleet every 10s while the tab is visible — the
  // same rhythm the Dashboard uses — so live CPU/memory gauges refresh without a
  // reload (heartbeat cadence 15s × this 10s poll). A poll error keeps the last-good
  // list (it must never blank the fleet or flash an error), unlike the first load.
  const poll = useCallback(async () => {
    try {
      const { workers } = await api.listWorkers();
      setWorkers(workers);
    } catch {
      // keep the last-good list
    }
  }, []);
  usePollWhileVisible(poll, 10000);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      const { worker, token } = await api.createWorker(name.trim(), template);
      setNewToken({ worker: worker.name, token });
      setCopied(false);
      setName("");
      setTemplate(DEFAULT_WORKER_TEMPLATE);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to create worker");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: string) => {
    // Read the name before the row is gone — after load() it is unfindable.
    const name = workers.find((w) => w.id === id)?.name;
    setError("");
    setConfirmingDelete(null);
    try {
      await api.deleteWorker(id);
      // Both kinds announce. The one-click external delete stays one click — an
      // announcement is feedback AFTER the act, not friction before it, so it costs no
      // clicks and takes nothing back from the asymmetry the confirmation buys. A row
      // that silently vanishes is poor feedback whichever kind it was.
      announce(`Deleted ${name ?? "worker"}.`);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to delete worker");
    }
  };

  // Move focus to whatever we just announced. Deleting destroys the control the user
  // acted through — the row and its button both go — so focus would otherwise land on
  // <body>, and delete→provision is a LOOP: the user deletes in order to provision, so
  // dumping them at the top of the document means tabbing back through the whole
  // sidebar to a form a few hundred pixels above where they were.
  //
  // The notice, deliberately, and not the next row's Delete button — the conventional
  // list-deletion pattern is actively unsafe here. After deleting a hosted worker the
  // remaining rows are mostly external: one click, no confirmation. Focusing the next
  // Delete would park a keyboard user on a live one-click destructor, and anyone
  // double-tapping Enter to get through a confirm would destroy a second worker having
  // intended one action. The asymmetry we built on purpose is what makes the
  // conventional answer wrong.
  useEffect(() => {
    if (notice) noticeRef.current?.focus();
  }, [notice]);

  // Focus the confirmation when it arms, so a keyboard user meets the warning instead
  // of hunting for it, and a screen reader reads it rather than announcing nothing.
  // Focus lands on the WARNING, never on "Delete anyway": auto-focusing the
  // destructive control is how a confirmation becomes a formality.
  useEffect(() => {
    if (confirmingDelete) confirmRef.current?.focus();
  }, [confirmingDelete]);

  // Backing out of a confirmation must not cost the user their place. Escape or Cancel
  // unmounts the confirm, which would drop focus to <body> and leave a keyboard user
  // tabbing from the top of the document back to the row they were already on — making
  // the escape hatch feel like a punishment for a misclick. So focus goes back to the
  // Delete button that armed it.
  //
  // Via id rather than a ref because `Button` (components/ui.tsx) does not forward one,
  // and teaching a primitive every page uses to forward refs is a wider change than
  // this fix earns. It has to run AFTER the render that re-mounts the button — hence a
  // state flag and an effect, not a .focus() in the handler, which would fire while the
  // button is still unmounted.
  useEffect(() => {
    if (!restoreFocusTo) return;
    // Gone if the fleet poll dropped the row underneath us; then there is nothing to
    // focus and <body> is the honest answer.
    document.getElementById(deleteButtonId(restoreFocusTo))?.focus();
    setRestoreFocusTo(null);
  }, [restoreFocusTo]);

  const dismissConfirm = (id: string) => {
    setConfirmingDelete(null);
    setRestoreFocusTo(id);
  };

  const copy = async () => {
    if (!newToken) return;
    try {
      await navigator.clipboard.writeText(newToken.token);
      setCopied(true);
    } catch {
      // Clipboard may be unavailable (insecure context); the token stays visible
      // to copy manually.
    }
  };

  return (
    <SettingsShell description="Workers are your uzi-agent containers: they claim your runs and stream them back.">
      {error && <Alert message={error} />}

      {newToken && (
        <Card className="space-y-3 border-ok/40">
          <SectionTitle className="text-ok">Join token for “{newToken.worker}”</SectionTitle>
          <p className="text-sm text-muted">
            Copy it now — it is shown once and never again (only its hash is stored). Set it as{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">UZI_WORKER_TOKEN</code> on the
            worker container.
          </p>
          <div className="flex items-center gap-2">
            <code className="flex-1 overflow-x-auto rounded-lg border border-edge bg-ink px-3 py-2 font-mono text-sm text-ok">
              {newToken.token}
            </code>
            <Button variant="secondary" onClick={copy}>
              {copied ? "Copied" : "Copy"}
            </Button>
          </div>
          <div>
            <Button variant="ghost" onClick={() => setNewToken(null)}>
              Done
            </Button>
          </div>
        </Card>
      )}

      <Card className="space-y-4">
        <SectionTitle>Register a worker</SectionTitle>
        <form onSubmit={create} className="flex flex-wrap items-end gap-3">
          <div className="min-w-[16rem] flex-1">
            <Field label="Name">
              <Input
                placeholder="e.g. laptop, ci-runner-1"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </Field>
          </div>
          <div className="min-w-[10rem]">
            <Field label="Template">
              <Select
                aria-label="Worker template"
                value={template}
                onChange={(e) => setTemplate(e.target.value)}
              >
                {WORKER_TEMPLATES.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </Select>
            </Field>
          </div>
          <Button type="submit" disabled={busy || name.trim() === ""}>
            {busy ? "Creating…" : "Generate join token"}
          </Button>
        </form>
        <p className="text-xs text-muted">
          The template is the worker image to build (<code className="rounded bg-raised px-1 py-0.5 text-fg">base</code> plus
          heavy-dependency variants like <code className="rounded bg-raised px-1 py-0.5 text-fg">jvm</code>). Build the
          worker with a matching{" "}
          <code className="rounded bg-raised px-1 py-0.5 text-fg">WORKER_TEMPLATE</code>; if the worker reports a different
          one it is flagged below, never rejected.
        </p>
      </Card>

      {/* Hosted workers (PRD #58): renders itself only when the instance has hosting
          on and self-service quota left. One list below, not two — a hosted worker is
          an ordinary worker whose container the controller runs, so it keeps the same
          status, gauges, run badge and delete rule, and only its origin differs. */}
      <HostedWorkers
        hostedCount={workers.filter((w) => w.kind === "hosted").length}
        onProvisioned={async (worker) => {
          announce(`Provisioned ${worker.name} — it appears in your workers below.`);
          await load();
        }}
      />

      {/* Between the forms and the list: it is what the forms just did and where the
          list just changed, and the delete→provision loop wants the user near both.
          Each announcement replaces the last, so nothing here can outlive its truth —
          which is why there is no dismiss. (The token card above has a Done button for
          a different reason: it holds a SECRET that should not linger on screen.) */}
      {notice && (
        // tabIndex -1: focusable programmatically, never a tab stop of its own.
        <div ref={noticeRef} tabIndex={-1} className="outline-none">
          <Alert tone="success" message={notice.text} />
        </div>
      )}

      <Card className="space-y-3">
        <SectionTitle>Your workers</SectionTitle>
        {loading ? (
          <div className="space-y-2">
            <Skeleton className="h-12" />
            <Skeleton className="h-12" />
          </div>
        ) : workers.length === 0 ? (
          <EmptyState
            icon={<ServerIcon />}
            title="No workers yet"
            description="Generate a join token above, then start the uzi-agent container with it — the worker shows up here when it heartbeats."
          />
        ) : (
          <>
            <FleetUpgradePanel workers={workers} cpVersion={cpVersion} />
            <ul className="space-y-2">
            {workers.map((w) => (
              <li
                key={w.id}
                className="flex flex-col gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2.5 text-sm"
              >
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div>
                    <span className="font-medium text-fg">{w.name}</span>
                    <span className="ml-2 align-middle">
                      <WorkerUpgradeBadge worker={w} />
                    </span>
                    <div className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-faint">
                      {w.template_reported ? (
                        <span>template {w.template_reported}</span>
                      ) : (
                        w.template_declared && <span>template {w.template_declared} (awaiting report)</span>
                      )}
                      {w.version && <span>· v{w.version}</span>}
                      {w.last_heartbeat_at && (
                        <span>· last seen {new Date(w.last_heartbeat_at).toLocaleString()}</span>
                      )}
                    </div>
                    {/* The EFFECTIVE token, always stated (PRD #104 M6), and it has
                        to be right in BOTH directions. Rendering blank for an unbound
                        worker reads as "no token" when the truth is "the default" —
                        but saying "your default token" on an account holding NO tokens
                        is the same failure mirrored: it over-claims a credential that
                        does not exist, on an account where every run will fail, and
                        offers nowhere to go. Three states, not two (web-ux D2).
                        A bound worker is rendered from the LIST payload, which always
                        carries the label alongside the id — never from a source that
                        could supply an id with a null label. */}
                    <div className="mt-1 text-xs text-muted">
                      {w.anthropic_secret_id ? (
                        <>
                          spends{" "}
                          <strong className="font-medium text-fg">
                            {w.anthropic_secret_label ?? "a named token"}
                          </strong>
                        </>
                      ) : tokens.length === 0 ? (
                        <span className="text-warn">
                          no Anthropic token —{" "}
                          <Link to="/settings" className="underline hover:text-fg">
                            add one in Settings
                          </Link>{" "}
                          or its runs will fail
                        </span>
                      ) : (
                        <span>spends your default token</span>
                      )}
                    </div>
                  </div>
                  <div className="flex items-center gap-1.5">
                    {/* Marked by the row's own data, not by the hosting config: if an
                        admin turns hosting off while a user still holds hosted rows,
                        the rows stay listed (nothing may strand them) and must stay
                        honest about what they are. Hiding the badge there would leave
                        them looking like workers the user forgot to start. */}
                    {w.kind === "hosted" && (
                      <>
                        <Badge tone="info" title="Runs in the cluster: the controller starts and stops its container, not you.">
                          hosted
                        </Badge>
                        {/* The docker capability (PRD #83 M3) is real TEXT — the word
                            "docker" — not a letter or an icon. Badge renders a bare
                            <span>, whose ARIA role is `generic`, where naming is
                            PROHIBITED: aria-label is ignored outright and title degrades
                            to a hover tooltip no screen reader is obliged to read, while
                            sr-only would answer the screen reader and still leave a
                            sighted keyboard or touch user a symbol with nothing to hover.
                            A word reaches sighted, keyboard, and screen-reader users at
                            once, and needs no ARIA to do it. Absence needs no badge, so a
                            worker without the sidecar renders nothing. */}
                        {w.docker === true && <Badge>docker</Badge>}
                      </>
                    )}
                    {hasTemplateDrift(w.template_declared, w.template_reported) && (
                      <Badge
                        tone="warning"
                        title={`Declared ${w.template_declared}, but the worker reports ${w.template_reported}. Rebuild it with WORKER_TEMPLATE=${w.template_declared} to match.`}
                      >
                        template drift
                      </Badge>
                    )}
                    <Badge tone={w.status === "online" ? "ok" : "neutral"} dot>
                      {w.status}
                    </Badge>
                    <WorkerRunBadge worker={w} />
                    {/* Hosted deletes confirm; external ones stay one click, and that
                        asymmetry is the whole point. Deleting an EXTERNAL worker
                        revokes a token — the container keeps running and the user
                        re-registers to recover. Deleting a HOSTED one takes its disks
                        with it (PRD: "PVCs accrue cost — deleted with the worker"),
                        which nothing undoes. Same button, wildly different blast
                        radius, so only the expensive one asks. */}
                    {/* Token picker (PRD #104 M3/M6). Rendered only when the user
                        holds more than one token — with one credential there is
                        nothing to choose between, and an always-visible picker would
                        imply otherwise. Takes effect on the worker's NEXT claim: no
                        restart, no re-minted join token. */}
                    {tokens.length > 1 && confirmingDelete !== w.id && (
                      <Select
                        aria-label={`Anthropic token for ${w.name}`}
                        className="h-8 max-w-[11rem] text-xs"
                        value={w.anthropic_secret_label ?? ""}
                        disabled={tokenBusy === w.id}
                        onChange={(e) => void rebind(w.id, e.target.value)}
                      >
                        <option value="">default token</option>
                        {tokens.map((t) => (
                          <option key={t.id} value={t.label}>
                            {t.label}
                            {t.is_default ? " (default)" : ""}
                          </option>
                        ))}
                      </Select>
                    )}
                    {confirmingDelete !== w.id && (
                      <Button
                        id={deleteButtonId(w.id)}
                        variant="danger"
                        size="sm"
                        onClick={() => (w.kind === "hosted" ? setConfirmingDelete(w.id) : remove(w.id))}
                      >
                        Delete
                      </Button>
                    )}
                  </div>
                </div>
                {/* Confirm-in-place, not a modal: same ruling as the provision form —
                    there is no modal primitive in web/ and a row action is not where to
                    invent one. It names what is destroyed rather than asking a
                    content-free "are you sure", because the whole reason it exists is
                    that the cost is invisible at the moment of the click.

                    "Delete is not a restart" leads deliberately: v1 ships no restart
                    endpoint (PRD non-goals: "no restart endpoint (delete +
                    reprovision)"), so Delete is the ONLY lifecycle control a hosted
                    user has, and they will reach for it on a stuck worker. Without
                    this they would silently pay the full /nix re-fetch from
                    cache.nixos.org — the exact off-LAN cost that volume exists to
                    prevent (Decision 7).

                    On today's build no controller exists (M3), so nothing is reaped
                    and there are no disks to lose. The copy states the RULE ("its
                    disks go with it"), which holds either way and becomes operative
                    the moment M3 ships — which is when a real user meets it. Warning
                    early costs a click; warning late costs someone their /nix. */}
                {confirmingDelete === w.id && (
                  <div
                    ref={confirmRef}
                    tabIndex={-1}
                    role="group"
                    aria-label={`Confirm deleting ${w.name}`}
                    // The label alone would DEFEAT this control for a screen reader.
                    // Focusing a named container announces its accessible NAME —
                    // "Confirm deleting base (M), group" — which sounds like a routine
                    // are-you-sure, while the warning below stays untethered text the
                    // user may never hear. The description is what carries the payload,
                    // and the payload is the entire reason the control exists.
                    aria-describedby={deleteWarningId(w.id)}
                    onKeyDown={(e) => {
                      if (e.key === "Escape") dismissConfirm(w.id);
                    }}
                    className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 outline-none"
                  >
                    <p id={deleteWarningId(w.id)} className="text-xs text-warn">
                      Delete is not a restart: a hosted worker’s disks go with it, permanently —{" "}
                      <code className="rounded bg-raised px-1 py-0.5">/data</code> (its workspace) and{" "}
                      <code className="rounded bg-raised px-1 py-0.5">/nix</code> (its cached tools). A replacement
                      re-downloads its tools from the internet.
                    </p>
                    <div className="flex items-center gap-1.5">
                      <Button variant="danger" size="sm" onClick={() => remove(w.id)}>
                        Delete anyway
                      </Button>
                      <Button variant="ghost" size="sm" onClick={() => dismissConfirm(w.id)}>
                        Cancel
                      </Button>
                    </div>
                  </div>
                )}
                {/* Live resource gauges (PRD #49): renders only once the worker has
                    reported a sample, so a worker without stats keeps its old row. */}
                <WorkerStatGauges worker={w} />
                <WorkerUpgradeDetail worker={w} />
              </li>
            ))}
            </ul>
          </>
        )}
      </Card>
    </SettingsShell>
  );
}
