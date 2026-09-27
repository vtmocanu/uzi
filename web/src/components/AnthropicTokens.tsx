// Settings → Anthropic tokens: the token LIST (PRD #104 M2/M6). A user may hold
// several named credentials, exactly one of which is the default that unbound
// workers and the judge lane spend.
//
// The value is never displayed, never returned by the API, and never re-shown
// after saving — rotation is a re-paste, which is why "Replace value" is a form
// and not an edit-in-place field.

import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { api, type AutoStatus, type SecretMeta } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { isVaultLocked } from "../lib/api";
import { autoChipFor } from "../lib/rateLimits";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { emitSidebarTokensChanged, isShownInSidebar } from "../lib/sidebarTokens";
import { Badge, Button, Card, Field, Input, SectionTitle, Skeleton } from "./ui";
import {
  CheckingUsageBadge,
  DisableCredentialDialog,
  DisabledCredentialRow,
  DisabledSection,
  NoDefaultNotice,
  disableButtonId,
  disabledSince,
  enableButtonId,
  slotHasDefault,
  splitByEnablement,
  useCheckingUsage,
  usePendingFocus,
  useRememberedExpansion,
} from "./CredentialEnablement";
import { DocLink } from "./DocLink";
import { DOC_ANTHROPIC_TOKEN } from "../lib/doclinks";

// vaultLockedMessage is the shared copy for a 409 vault_locked: the global handler
// has already refreshed the session, so the unlock banner is showing above.
const VAULT_LOCKED = "Your vault is locked — unlock it with the banner above, then save again.";

// D6's reason, in one place: it is rendered as a tooltip AND as the screen-reader
// description the disabled-looking Delete points at, and those two must not drift.
const D6_HINT =
  "Make another token the default first — every account needs one default while any token exists.";

function errText(err: unknown, fallback: string): string {
  if (isVaultLocked(err)) return VAULT_LOCKED;
  return errorMessage(err, fallback);
}

// D5 says the delete confirmation must NAME the affected workers, and it says so
// because the generic warning is precisely what it rejects: a silent fallback to
// the default is acceptable behavior and unacceptable surprise. "Any worker bound
// to it" tells a user that something might move; "alpha and beta" tells them what
// did. Bounded at four names so a fleet-sized list stays a sentence.
const MAX_NAMED = 4;
function nameList(names: string[]): string {
  const shown = names.slice(0, MAX_NAMED);
  const rest = names.length - shown.length;
  const joined =
    shown.length === 1
      ? shown[0]
      : `${shown.slice(0, -1).join(", ")} and ${shown[shown.length - 1]}`;
  return rest > 0 ? `${joined} (and ${rest} more)` : joined;
}

// deleteWarning is the confirmation copy, split out so the naming rules are
// readable and testable in one place rather than nested three ternaries deep.
function deleteWarning(
  label: string,
  isDefault: boolean,
  boundWorkers: string[],
  judgeBound: boolean,
  // PRD #111, web-ux F5. An `auto` worker is bound to the POOL AS A SET, not to any
  // one token, so deleting a pooled credential shrinks the candidate set and shifts
  // spend without being bound to anything this function could enumerate. D5's whole
  // argument is that a silent fallback is acceptable BEHAVIOUR and unacceptable
  // SURPRISE — and "nothing is bound to it" was becoming the surprise.
  autoEligible = false,
  // PRD #1732: disabled tokens that stay behind. Deleting the last ENABLED token is
  // allowed while disabled ones remain; it leaves the slot with no default, not with no
  // Anthropic account.
  disabledLeft = 0,
): string {
  if (isDefault) {
    // Reachable only as the last ENABLED token (D6 blocks deleting a default while other
    // enabled tokens exist), so this is the disconnect case, not a fallback case.
    if (disabledLeft > 0) {
      return `Delete “${label}”? This is your last enabled token, so you will have no default Anthropic token until you enable one of your disabled tokens.`;
    }
    return `Delete “${label}”? This is your last token — uzi will no longer be connected to your Anthropic account.`;
  }
  const affected: string[] = [];
  if (boundWorkers.length > 0) {
    affected.push(
      `${boundWorkers.length === 1 ? "Worker" : "Workers"} ${nameList(boundWorkers)}`,
    );
  }
  if (judgeBound) affected.push("your run judge");
  // The pool clause rides on BOTH branches: a token can be pooled and bound, pooled
  // and unbound, or neither, and each combination is a different sentence.
  const poolClause = autoEligible
    ? " It also leaves your auto-selection pool, so workers set to auto will no longer spend it."
    : "";
  if (affected.length === 0) {
    return autoEligible
      ? `Delete “${label}”? No worker or judge names it directly, but it is in your auto-selection pool — workers set to auto will no longer spend it.`
      : `Delete “${label}”? Nothing is bound to it, so no worker or judge setting changes.`;
  }
  const subject = affected.join(" and ");
  const verb = boundWorkers.length + (judgeBound ? 1 : 0) === 1 ? "falls" : "fall";
  return `Delete “${label}”? ${subject} ${verb} back to your default token.${poolClause}`;
}

// TokenRow is one stored credential: its label, the default badge, when it was
// last updated, and the per-row actions. Rename and set-default are inline; the
// value rotation opens the shared form below (a value is pasted, never edited).
function TokenRow({
  secret,
  busy,
  soleToken,
  disabledLeft,
  judgeBound,
  autoStatus,
  autoFetchState,
  sidebarShown,
  onToggleSidebar,
  checkingUsage,
  onDisable,
  onChanged,
  onError,
  onNotice,
}: {
  secret: SecretMeta;
  busy: boolean;
  // The only ENABLED token (PRD #1732): the server lets the default go only when no
  // other enabled token remains, whatever sits on the Disabled shelf.
  soleToken: boolean;
  // Disabled tokens that stay behind, for the last-token delete warning.
  disabledLeft: number;
  judgeBound: boolean;
  // The SERVER's live eligibility answer for this token (PRD #111 M2), or
  // undefined while the meters have not loaded (or failed to). Never re-derived
  // here — see lib/rateLimits.ts for why that is a design rule and not a habit.
  autoStatus: AutoStatus | undefined;
  // Whether the meters fetch has resolved (web-ux F9). Without it a pooled token
  // renders NO chip while loading and again on failure — visually identical to a
  // healthy one, which is the silent no-op D11 exists to prevent, arriving through a
  // spinner and an error path rather than through the poller.
  autoFetchState: "pending" | "ready" | "failed";
  // Whether this token's meters ride the sidebar rail. The default token is
  // always shown, so its box renders checked and disabled; for the rest this is
  // the user's explicit choice (round-3 feedback: control, not an automatic
  // hottest-token pick).
  sidebarShown: boolean;
  onToggleSidebar: (id: string, shown: boolean) => Promise<void>;
  // PRD #1732: just enabled, and no fresh reading has landed yet.
  checkingUsage: boolean;
  // Opens the Disable dialog for this token (owned by the card, which knows the slot).
  onDisable: () => void;
  onChanged: () => Promise<void>;
  onError: (m: string) => void;
  onNotice: (m: string) => void;
}) {
  const [renaming, setRenaming] = useState(false);
  const [label, setLabel] = useState(secret.label);
  const [rowBusy, setRowBusy] = useState(false);
  // The Delete click awaits a worker read before confirming; a row that unmounted
  // meanwhile (the user left the page) must not confirm, delete or report.
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  const disabled = busy || rowBusy;
  const blockedByD6 = secret.is_default && !soleToken;
  const d6HintId = `d6-${secret.id}`;
  const autoHintId = `auto-${secret.id}`;

  const run = async (fn: () => Promise<unknown>, ok: string, fallback: string) => {
    onError("");
    onNotice("");
    setRowBusy(true);
    try {
      await fn();
      onNotice(ok);
      await onChanged();
    } catch (err) {
      onError(errText(err, fallback));
    } finally {
      setRowBusy(false);
    }
  };

  const rename = async (e: FormEvent) => {
    e.preventDefault();
    const next = label.trim();
    if (next === "" || next === secret.label) {
      setRenaming(false);
      return;
    }
    await run(
      () => api.patchAnthropicToken(secret.id, { label: next }),
      `Renamed to “${next}”.`,
      "Failed to rename the token",
    );
    setRenaming(false);
  };

  return (
    <div className="rounded-lg border border-edge bg-raised/60 px-4 py-3" data-testid={`token-${secret.id}`}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        {renaming ? (
          <form onSubmit={rename} className="flex items-center gap-2">
            <Input
              autoFocus
              value={label}
              aria-label={`Rename ${secret.label}`}
              onChange={(e) => setLabel(e.target.value)}
              className="h-8 w-48"
            />
            <Button type="submit" size="sm" disabled={disabled}>
              Save
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => {
                setLabel(secret.label);
                setRenaming(false);
              }}
            >
              Cancel
            </Button>
          </form>
        ) : (
          <div className="flex min-w-0 items-center gap-2">
            {/* web-ux F12: the label is user-authored and reaches this renderer
                without necessarily having passed the server validator (rows written
                before it landed are never re-validated). React escaping does not
                touch a bidi override — see lib/sanitizeLabel. */}
            <span className="truncate font-medium text-fg">{sanitizeLabel(secret.label)}</span>
            {secret.is_default && <Badge tone="ok">default</Badge>}
            {checkingUsage && <CheckingUsageBadge />}
          </div>
        )}
        {!renaming && (
          <div className="flex flex-wrap items-center gap-2">
            <Button variant="ghost" size="sm" disabled={disabled} onClick={() => setRenaming(true)}>
              Rename
            </Button>
            {!secret.is_default && (
              <Button
                variant="ghost"
                size="sm"
                disabled={disabled}
                onClick={() =>
                  run(
                    () => api.patchAnthropicToken(secret.id, { default: true }),
                    `“${secret.label}” is now your default token.`,
                    "Failed to set the default token",
                  )
                }
              >
                Make default
              </Button>
            )}
            {/* PRD #1732: reversible suspension, beside Rename / Make default / Delete. */}
            <Button
              id={disableButtonId(secret.id)}
              variant="ghost"
              size="sm"
              disabled={disabled}
              onClick={onDisable}
              aria-label={`Disable ${sanitizeLabel(secret.label)}`}
            >
              Disable
            </Button>
            <Button
              variant="danger"
              size="sm"
              // The default cannot be deleted while other tokens exist (D6). It is
              // aria-disabled rather than `disabled` DELIBERATELY: a `disabled`
              // button leaves the tab order entirely, so a keyboard or screen-reader
              // user met a control they could not reach and a reason that existed
              // only in a hover tooltip (web-ux D3). aria-disabled keeps it
              // focusable, aria-describedby carries the reason to a screen reader,
              // and the click is refused here instead of by the server's 409 — a
              // 409 tells a user it failed, not what to do instead.
              // `busy` still uses real `disabled`: that one is transient and has
              // nothing to explain.
              disabled={disabled}
              aria-disabled={blockedByD6 || undefined}
              aria-describedby={blockedByD6 ? d6HintId : undefined}
              title={blockedByD6 ? D6_HINT : undefined}
              className={blockedByD6 ? "cursor-not-allowed opacity-50" : undefined}
              onClick={async () => {
                if (blockedByD6) return;
                // Deleting a bound token silently returns its workers to the
                // default (D5), so the confirmation must NAME them rather than let a
                // user discover the move from a meter. The bindings are read fresh
                // at the click, never from a mount-time snapshot: a snapshot still
                // loading (or failed) would say "Nothing is bound" about a token
                // that is bound. The last token names nobody, so it skips the read.
                let boundWorkers: string[] = [];
                if (!secret.is_default) {
                  onError("");
                  setRowBusy(true);
                  try {
                    const { workers } = await api.listWorkers();
                    boundWorkers = workers
                      .filter((w) => w.anthropic_secret_id === secret.id)
                      .map((w) => w.name);
                  } catch (err) {
                    if (!mounted.current) return;
                    // Without the names the D5 warning cannot be honest, so refuse
                    // rather than delete on a guess.
                    onError(
                      errText(err, "Could not check which workers use this token, so it was not deleted"),
                    );
                    return;
                  } finally {
                    if (mounted.current) setRowBusy(false);
                  }
                  if (!mounted.current) return;
                }
                if (
                  !window.confirm(
                    deleteWarning(
                      sanitizeLabel(secret.label),
                      secret.is_default,
                      boundWorkers,
                      judgeBound,
                      secret.auto_eligible,
                      disabledLeft,
                    ),
                  )
                )
                  return;
                void run(
                  () => api.deleteAnthropicTokenById(secret.id),
                  `Deleted “${secret.label}”.`,
                  "Failed to delete the token",
                );
              }}
            >
              Delete
            </Button>
            {blockedByD6 && (
              <span id={d6HintId} className="sr-only">
                {D6_HINT}
              </span>
            )}
          </div>
        )}
      </div>
      {/* Auto-selection pool (PRD #111 M2, D2). The toggle and the live status sit
          together on purpose: opting a token in whose gauge has never polled is a
          silent no-op — it looks active and can never be picked (R7) — so the
          consequence has to be visible at the moment of the choice, not one card
          down. The chip renders the SERVER's answer verbatim. */}
      <div className="mt-2 flex flex-wrap items-center gap-2">
        {/* h-4 w-4 / text-sm to match autopilot, judge and Slack on this same page
            (web-ux F7). The control deciding which account gets billed was the most
            de-emphasised thing in the card, and 14px in a 16px row is under WCAG
            2.5.8's 24×24 target minimum vertically. */}
        <label className="flex items-center gap-2 text-sm text-muted">
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={secret.auto_eligible}
            disabled={disabled}
            // Named per TOKEN (web-ux F6). Every checkbox previously carried the
            // identical accessible name, so a screen-reader user heard N identical
            // controls and could not tell which credential each one spends — on the
            // control that decides which account gets billed. Mirrors the rename
            // input's `aria-label` two blocks up.
            aria-label={`Auto-select from ${sanitizeLabel(secret.label)}`}
            aria-describedby={secret.auto_eligible ? autoHintId : undefined}
            onChange={(e) => {
              const next = e.target.checked;
              void run(
                () => api.setTokenAutoEligible(secret.id, next),
                next
                  ? `“${sanitizeLabel(secret.label)}” is now in the auto-selection pool.`
                  : `“${sanitizeLabel(secret.label)}” is no longer in the auto-selection pool.`,
                next
                  ? "Failed to add the token to the pool"
                  : "Failed to remove the token from the pool",
              );
            }}
          />
          <span aria-hidden="true">Auto-select from this token</span>
        </label>
        {/* Sidebar rail opt-in. Same control conventions as the pool toggle above
            (h-4 w-4, per-token accessible name). The DEFAULT token is pinned: the
            rail always shows it, so the box is checked and disabled rather than
            hidden — hiding it would make the pinning unreadable. */}
        <label
          className="flex items-center gap-2 text-sm text-muted"
          title={secret.is_default ? "The default token always shows in the sidebar." : undefined}
        >
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={sidebarShown}
            disabled={disabled || secret.is_default}
            aria-label={`Show ${sanitizeLabel(secret.label)} in the sidebar`}
            onChange={(e) => void onToggleSidebar(secret.id, e.target.checked)}
          />
          <span aria-hidden="true">Show in sidebar</span>
        </label>
        {/* The chip, decided by autoChipFor rather than here: a pooled token whose
            status says `not_pooled` is two sources DISAGREEING, and showing a checked
            box beside "not in pool" asserts a contradiction (web-ux F1). Pending and
            failed fetches get their own honest chips rather than degrading to the
            pre-feature appearance (F9). */}
        {(() => {
          const decision = autoChipFor(secret.auto_eligible, autoStatus, autoFetchState);
          if (decision.kind === "hidden") return null;
          const skipped = decision.chip.tone === "warning";
          return (
            <>
              <Badge tone={decision.chip.tone} title={decision.chip.hint}>
                {decision.chip.label}
              </Badge>
              {/* web-ux F3: the chip states a DIAGNOSIS; none of the labels says
                  whether the token can actually be picked. That consequence used to
                  live only in a `title`, which reaches no keyboard user, no touch
                  user and no screen reader reliably — D11's failure mode one level
                  up, the state rendered and the meaning withheld. It is visible copy
                  now, and the sr-only description mirrors D6_HINT's pattern from the
                  Delete button above. */}
              {/* 🔴 `text-warn`, not `text-warning` — web-ux F22. `warning` is not a
                  class in this project; the tailwind token is `warn`. The computed
                  colour was rgb(228,232,240), the inherited body --fg, beside a chip at
                  rgb(251,191,36): the amber pairing the F3 fix intended silently did not
                  happen. A tailwind class that does not resolve fails COMPLETELY
                  silently — nothing errors, the element just inherits — which is why it
                  survived review, and why six other instances of the same typo exist
                  elsewhere in this codebase. Those predate this PRD and are being
                  raised separately rather than ridden in on a token-selection change. */}
              {skipped && (
                <span className="text-xs text-warn">— auto-selection skips it</span>
              )}
              <span id={autoHintId} className="sr-only">
                {decision.chip.hint}
              </span>
            </>
          );
        })()}
      </div>
      <div className="mt-1 text-xs text-faint">
        updated {new Date(secret.updated_at).toLocaleString()}
      </div>
    </div>
  );
}

// AnthropicTokens is the whole card: the list, the add form, and the rotate form.
// `secrets` and `reload` are owned by Settings so the rate-limit card and this one
// refresh together after a change.
export function AnthropicTokens({
  secrets,
  loading,
  busy,
  reload,
  onError,
  onNotice,
  judgeSecretId,
  sidebarTokenIds,
  onToggleSidebarToken,
}: {
  secrets: SecretMeta[];
  loading: boolean;
  busy: boolean;
  reload: () => Promise<void>;
  onError: (m: string) => void;
  onNotice: (m: string) => void;
  // The owner's judge-lane binding, so a delete can say the judge moves too (D5
  // covers "workers AND the judge"). From Settings, which already holds the user.
  judgeSecretId: string | null;
  // Which non-default tokens also show on the sidebar rail, plus the save
  // handler. Owned by Settings alongside `secrets` (one load, one reload).
  sidebarTokenIds: string[];
  onToggleSidebarToken: (id: string, shown: boolean) => Promise<void>;
}) {
  const [token, setToken] = useState("");
  const [label, setLabel] = useState("");
  const [addBusy, setAddBusy] = useState(false);
  const [rotateFor, setRotateFor] = useState("");
  const [rotateValue, setRotateValue] = useState("");
  // Per-token live auto-selection eligibility (PRD #111 M2), fetched here for the
  // same reason the workers are: the consequence of the toggle belongs beside the
  // toggle, and no caller has this data. Re-fetched whenever `secrets` changes, so
  // opting a token in updates its chip without a page reload. Failure is silent by
  // design, exactly as above — the toggle still works and still says what it did;
  // an error banner over a secondary fetch would be worse than a missing chip.
  const [autoStatuses, setAutoStatuses] = useState<Record<string, AutoStatus>>({});
  // The fetch's own state, tracked rather than inferred from an empty map (web-ux
  // F9). "no statuses yet" and "the fetch failed" and "this token has no meter row"
  // are three different facts that an empty map collapses into one, and the
  // collapsed answer renders as the pre-feature appearance — a pooled-but-unpickable
  // token looking exactly like a healthy one.
  const [autoFetchState, setAutoFetchState] = useState<"pending" | "ready" | "failed">("pending");
  useEffect(() => {
    let cancelled = false;
    setAutoFetchState("pending");
    void api
      .getMyRateLimits()
      .then(({ tokens }) => {
        if (cancelled) return;
        setAutoStatuses(Object.fromEntries(tokens.map((t) => [t.secret_id, t.auto_status])));
        setAutoFetchState("ready");
      })
      .catch(() => {
        if (cancelled) return;
        setAutoStatuses({});
        setAutoFetchState("failed");
      });
    return () => {
      cancelled = true;
    };
  }, [secrets]);

  const first = secrets.length === 0;
  // PRD #1732: live tokens above, suspended ones on the Disabled shelf below.
  const { active, disabled } = splitByEnablement(secrets);
  const hasDefault = slotHasDefault(secrets);
  const [shelfOpen, setShelfOpen] = useRememberedExpansion("uzi.settings.disabledTokensOpen");
  const [disabling, setDisabling] = useState<SecretMeta | null>(null);
  const [shelfBusy, setShelfBusy] = useState(false);
  const shelfToggleId = useId();
  const focusLater = usePendingFocus();
  // "checking usage…" after Enable, until the token's meter carries a fresh reading.
  const { checking, start: startChecking } = useCheckingUsage(async (ids) => {
    const { tokens } = await api.getMyRateLimits();
    return tokens.filter((t) => ids.includes(t.secret_id) && t.limits.status === "ok").map((t) => t.secret_id);
  });

  const confirmDisable = async (newDefaultId: string | undefined) => {
    const target = disabling;
    if (!target) return;
    onError("");
    onNotice("");
    await api.setSecretEnabled(target.kind, target.id, false, newDefaultId);
    setDisabling(null);
    const promoted = newDefaultId ? secrets.find((s) => s.id === newDefaultId) : undefined;
    onNotice(
      promoted
        ? `Disabled “${sanitizeLabel(target.label)}”. “${sanitizeLabel(promoted.label)}” is now your default token.`
        : `Disabled “${sanitizeLabel(target.label)}”. Enable it again from the Disabled section.`,
    );
    emitSidebarTokensChanged();
    // The row now sits behind the shelf toggle; focus goes there, not to <body>.
    focusLater(shelfToggleId);
    await reload();
  };

  const enable = async (s: SecretMeta) => {
    onError("");
    onNotice("");
    setShelfBusy(true);
    try {
      const { secret: updated } = await api.setSecretEnabled(s.kind, s.id, true);
      startChecking(s.id);
      onNotice(
        updated.is_default
          ? `Enabled “${sanitizeLabel(s.label)}”. It is now your default token.`
          : `Enabled “${sanitizeLabel(s.label)}”.`,
      );
      emitSidebarTokensChanged();
      // Back in the live list: focus its Disable, the control that undoes this.
      focusLater(disableButtonId(s.id));
      await reload();
    } catch (err) {
      onError(errText(err, "Failed to enable the token"));
    } finally {
      setShelfBusy(false);
    }
  };

  // Deleting a disabled token keeps D5's promise: the confirmation names the workers
  // bound to it, read fresh at the click. A disabled token is never the default (D4).
  const removeDisabled = async (s: SecretMeta) => {
    onError("");
    onNotice("");
    setShelfBusy(true);
    try {
      let boundWorkers: string[];
      try {
        const { workers } = await api.listWorkers();
        boundWorkers = workers.filter((w) => w.anthropic_secret_id === s.id).map((w) => w.name);
      } catch (err) {
        onError(errText(err, "Could not check which workers use this token, so it was not deleted"));
        return;
      }
      if (
        !window.confirm(
          deleteWarning(sanitizeLabel(s.label), false, boundWorkers, judgeSecretId === s.id, s.auto_eligible),
        )
      )
        return;
      await api.deleteAnthropicTokenById(s.id);
      onNotice(`Deleted “${s.label}”.`);
      await reload();
    } catch (err) {
      onError(errText(err, "Failed to delete the token"));
    } finally {
      setShelfBusy(false);
    }
  };

  // The add form collapses to first-token mode when the last token goes away, and
  // the Name input UNMOUNTS while keeping its state — so a label typed and never
  // submitted would silently become the next token's name, chosen through a field
  // the user cannot see (web-ux D1, reproduced deterministically). Clearing both
  // fields on the transition is the fix; `first` is the only thing that changes
  // which fields exist, so it is the only trigger needed.
  useEffect(() => {
    setToken("");
    setLabel("");
  }, [first]);

  const add = async (e: FormEvent) => {
    e.preventDefault();
    onError("");
    onNotice("");
    setAddBusy(true);
    try {
      // A user's FIRST token is forced default server-side whatever we send, and
      // the Name field does not exist in that mode, so send the literal `default`
      // rather than `label || "default"` — the latter would read a field that is
      // not on screen.
      await api.createAnthropicToken(token, first ? "default" : label.trim(), false);
      setToken("");
      setLabel("");
      onNotice("Token saved. It is sealed with your login password and validated on the first agent run.");
      await reload();
    } catch (err) {
      onError(errText(err, "Failed to save token"));
    } finally {
      setAddBusy(false);
    }
  };

  const rotate = async (e: FormEvent) => {
    e.preventDefault();
    onError("");
    onNotice("");
    setAddBusy(true);
    try {
      await api.patchAnthropicToken(rotateFor, { token: rotateValue });
      setRotateValue("");
      setRotateFor("");
      onNotice("Token value replaced. The new value is used on the next agent run.");
      await reload();
    } catch (err) {
      onError(errText(err, "Failed to replace the token value"));
    } finally {
      setAddBusy(false);
    }
  };

  const anyBusy = busy || addBusy;

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Anthropic tokens</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          uzi runs your agents with your own Anthropic credentials. Paste an OAuth token from{" "}
          <code className="rounded bg-raised px-1 py-0.5 text-fg">claude setup-token</code> or a
          Console API key. Give each one a name so you can point individual workers at it. The{" "}
          <strong className="text-fg">default</strong> is what every unbound worker spends.{" "}
          <DocLink slug={DOC_ANTHROPIC_TOKEN}>How to obtain a token</DocLink>.
        </p>
      </div>

      {/* PRD #1732 D4: a slot whose tokens are all disabled has no default. Said at the
          top of the card, whether or not the Disabled shelf is open. */}
      {!loading && !first && !hasDefault && (
        <NoDefaultNotice
          slot="anthropic"
          expanded={shelfOpen}
          onShow={() => {
            setShelfOpen(true);
            if (disabled[0]) focusLater(enableButtonId(disabled[0].id));
          }}
        />
      )}

      {loading ? (
        <Skeleton className="h-16 w-full" />
      ) : first ? (
        <div className="rounded-lg border border-edge bg-raised/60 px-4 py-3 text-sm">
          <Badge tone="neutral">Not set</Badge>
        </div>
      ) : (
        <div className="space-y-2">
          {active.map((s) => (
            <TokenRow
              key={s.id}
              secret={s}
              busy={anyBusy || shelfBusy}
              soleToken={active.length === 1}
              disabledLeft={disabled.length}
              judgeBound={judgeSecretId === s.id}
              autoStatus={autoStatuses[s.id]}
              autoFetchState={autoFetchState}
              sidebarShown={isShownInSidebar(s, sidebarTokenIds)}
              onToggleSidebar={onToggleSidebarToken}
              checkingUsage={checking.has(s.id)}
              onDisable={() => setDisabling(s)}
              onChanged={reload}
              onError={onError}
              onNotice={onNotice}
            />
          ))}
          <DisabledSection
            count={disabled.length}
            expanded={shelfOpen}
            onToggle={setShelfOpen}
            toggleId={shelfToggleId}
          >
            {disabled.map((s) => (
              <DisabledCredentialRow
                key={s.id}
                testId={`token-${s.id}`}
                secret={s}
                note={disabledSince(s.disabled_at)}
                enableLabel={hasDefault ? "Enable" : "Enable and make default"}
                busy={anyBusy || shelfBusy}
                onEnable={() => void enable(s)}
                onDelete={() => void removeDisabled(s)}
              />
            ))}
          </DisabledSection>
        </div>
      )}

      {disabling && (
        <DisableCredentialDialog
          secret={disabling}
          slot="anthropic"
          replacements={active.filter((s) => s.id !== disabling.id)}
          onClose={() => setDisabling(null)}
          onConfirm={confirmDisable}
        />
      )}

      {/* Rotating a value is a separate form because the value is pasted, never
          edited: the API never returns it, so there is nothing to pre-fill. */}
      {!loading && !first && (
        <div className="space-y-3">
          <Field label="Replace a token's value">
            <select
              aria-label="Token to replace"
              className="h-9 w-full rounded-md border border-edge bg-surface px-2 text-sm text-fg"
              value={rotateFor}
              onChange={(e) => setRotateFor(e.target.value)}
            >
              <option value="">Select a token…</option>
              {secrets.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.label}
                  {s.is_default ? " (default)" : ""}
                  {s.enabled === false ? " (disabled)" : ""}
                </option>
              ))}
            </select>
          </Field>
          {rotateFor !== "" && (
            <form onSubmit={rotate} className="space-y-3">
              <Field label="New value">
                <Input
                  type="password"
                  autoComplete="off"
                  placeholder="Paste the replacement token"
                  value={rotateValue}
                  onChange={(e) => setRotateValue(e.target.value)}
                />
              </Field>
              <Button type="submit" disabled={anyBusy || rotateValue.trim() === ""}>
                Replace value
              </Button>
            </form>
          )}
        </div>
      )}

      <form onSubmit={add} className="space-y-3 border-t border-edge pt-5">
        <Field label={first ? "Token" : "Add another token"}>
          <Input
            type="password"
            autoComplete="off"
            placeholder="Paste your Anthropic token"
            value={token}
            onChange={(e) => setToken(e.target.value)}
          />
        </Field>
        {!first && (
          <Field label="Name">
            <Input
              placeholder="console-key"
              value={label}
              aria-label="Token name"
              onChange={(e) => setLabel(e.target.value)}
            />
          </Field>
        )}
        <p className="text-xs text-faint">
          Encrypted with your login password. If you forget your password this token cannot be
          recovered and must be re-entered.
        </p>
        <Button type="submit" disabled={anyBusy || token.trim() === "" || (!first && label.trim() === "")}>
          {first ? "Save token" : "Add token"}
        </Button>
        {/* No sr-only echo of the error here, and no `error` prop any more. There
            used to be both, from before this card's parent grew its own
            `role=alert` banner — so a duplicate-label failure sat in the DOM twice
            and was announced twice (web-ux D4). The parent's Alert is now the single
            announcement, and this card reports into it via onError. */}
      </form>
    </Card>
  );
}
