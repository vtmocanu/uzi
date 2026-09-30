// Admin → Products → one product's skill set (PRD #1909 M6, D9): where it comes from (a
// git repo on the instance's allowlist, a ref, a write-only clone token), what a sync
// STAGED from it, and what an admin APPROVED. Only the approved set reaches the product's
// jobs, so this panel is an approval review before it is a settings form.
//
// IA: a disclosure at the foot of each live product card, collapsed by default and loaded
// on first open, so the registry page stays one request per list and a product nobody is
// reviewing costs nothing. Inside, the order is the order of the work: source, then what is
// waiting for approval (the decision), then what is live now (the reference).
//
// Trust: names, descriptions and bodies come from an external repo. They render as React
// text only (no markdown, no HTML), and any control or format character (a bidi override, a
// zero-width space) is shown as a visible U+XXXX marker instead of being rendered raw or
// silently dropped: the approving admin must see that the text reads differently than it
// runs. The clone token is never displayed: the api only says whether one is set, and the
// input that sends a new one is a password field cleared once the save lands.

import { useEffect, useId, useState, type FormEvent, type ReactNode } from "react";
import { api, type Product, type ProductPatch, type ProductSkill, type ProductSkillDrop, type ProductSkills } from "../lib/api";
import { ApiError, errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { useAuth } from "../auth/AuthContext";
import { splitUnsafeChars } from "../lib/safeText";
import { Badge, Button, Field, Input, PasswordInput, Spinner, cx } from "./ui";

type Staged = NonNullable<ProductSkills["staged"]>;

const plural = (n: number, noun: string) => `${n} ${noun}${n === 1 ? "" : "s"}`;

// A commit id as people compare it against a forge: the first 12 hex characters. The full
// id stays reachable (title, and the sr-only text) for an exact check.
function Sha({ sha }: { sha: string }) {
  return (
    <code title={sha} className="rounded-sm bg-raised px-1 font-mono text-xs text-fg">
      {sha.slice(0, 12)}
      <span className="sr-only"> (commit {sha})</span>
    </code>
  );
}

const UNSAFE_MARK =
  "mx-px inline-block rounded-sm bg-danger/15 px-0.5 align-baseline font-mono text-[10px] leading-4 text-danger ring-1 ring-danger/40";

// RevealedText renders untrusted text with every control/format character replaced by a
// visible code-point marker. Tabs and newlines stay (the body is pre-wrapped).
function RevealedText({ text }: { text: string }) {
  return (
    <>
      {splitUnsafeChars(text).map((p, i) =>
        p.unsafe ? (
          <span key={i} className={UNSAFE_MARK} data-hidden-char="">
            <span className="sr-only">hidden character </span>
            U+{(p.text.codePointAt(0) ?? 0).toString(16).toUpperCase().padStart(4, "0")}
          </span>
        ) : (
          p.text
        ),
      )}
    </>
  );
}

// countHidden counts the markers RevealedText draws for these strings: the review banner
// sums exactly the text the review renders, so its number matches the red codes on screen.
const countHidden = (texts: readonly string[]) =>
  texts.reduce((n, t) => n + splitUnsafeChars(t).filter((p) => p.unsafe).length, 0);

const skillTexts = (s: ProductSkill) => [s.name, s.description, s.body];

const hiddenCount = (s: ProductSkill) => countHidden(skillTexts(s));

// One skill: its name and description always visible, the SKILL.md body behind a
// disclosure (bodies run to kilobytes; the diff is read name-first).
function SkillEntry({ skill, extra }: { skill: ProductSkill; extra?: ReactNode }) {
  const hidden = hiddenCount(skill);
  return (
    <li className="rounded-lg border border-edge bg-surface px-3 py-2">
      <details className="group">
        <summary className="flex cursor-pointer list-none flex-wrap items-baseline gap-x-2 gap-y-1 marker:content-none">
          <span aria-hidden="true" className="inline-block text-faint group-open:rotate-90 motion-safe:transition-transform">
            ▸
          </span>
          <span className="font-mono text-sm font-medium break-all text-fg">
            <RevealedText text={skill.name} />
          </span>
          {hidden > 0 && (
            <Badge tone="danger">{plural(hidden, "hidden character")}</Badge>
          )}
          <span className="basis-full pl-4 text-xs text-muted">
            {skill.description ? <RevealedText text={skill.description} /> : "No description."}
          </span>
        </summary>
        <pre className="mt-2 max-h-96 overflow-auto rounded-md border border-edge bg-raised p-3 font-mono text-xs leading-relaxed break-words whitespace-pre-wrap text-fg">
          <RevealedText text={skill.body} />
        </pre>
        {extra}
      </details>
    </li>
  );
}

// Why a sync left a skill out, in the admin's terms (apitypes.ProductSkillDropDTO.Reason).
const DROP_REASON: Record<string, string> = {
  invalid: "not a valid SKILL.md (its frontmatter is missing or malformed)",
  too_large: "over the size limit for one skill",
  duplicate: "another skill in the repo has the same name",
  over_limit: "past the limit on skills per product",
  secret: "it contains what looks like a credential",
};

// The texts DropRow renders through RevealedText (an unknown reason is shown as sent).
const dropTexts = (d: ProductSkillDrop) => [d.name, ...(d.reason in DROP_REASON ? [] : [d.reason])];

function DropRow({ d }: { d: ProductSkillDrop }) {
  const reason = DROP_REASON[d.reason] ?? <RevealedText text={d.reason} />;
  if (d.name === "") {
    return (
      <li>
        {d.count ? `${plural(d.count, "more file")} not read: ` : "Some files not read: "}
        {reason}.
      </li>
    );
  }
  return (
    <li>
      <span className="font-mono break-all text-fg">
        <RevealedText text={d.name} />
      </span>
      : {reason}
      {d.count && d.count > 1 ? ` (${d.count} files)` : ""}.
    </li>
  );
}

// The diff group headings. The tone carries the direction of the change; the count is in
// the heading so a screen-reader user hears the size before the list.
const GROUPS = [
  { key: "added", label: "Added", tone: "ok" },
  { key: "changed", label: "Changed", tone: "warning" },
  { key: "removed", label: "Removed", tone: "danger" },
] as const;

function isNoOp(s: Staged) {
  return s.diff.added.length + s.diff.changed.length + s.diff.removed.length === 0;
}

// userLabel names who staged or approved: "you" for the viewer, else the short user id
// (the api returns the id, not a name).
function useUserLabel() {
  const { user } = useAuth();
  return (id: string | null) =>
    id === null ? "an unknown user" : id === user?.id ? "you" : `user ${id.slice(0, 8)}`;
}

// friendly maps a skills write failure to copy that says what to do. 409 and 400 carry a
// specific server sentence (no repo, deleted product, source changed, token unreadable,
// URL no longer allowlisted, staged set moved); 429 and 502 are fixed server texts, so the
// panel words them itself.
function friendly(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    if (err.status === 429) return "Another skills sync is running. Try again in about 30 seconds.";
    if (err.status === 502) {
      return "Could not read the skills repo. Check the repo URL, ref and clone token, then sync again.";
    }
  }
  const msg = errorMessage(err, fallback);
  return msg.charAt(0).toUpperCase() + msg.slice(1) + (/[.!?]$/.test(msg) ? "" : ".");
}

const originOf = (raw: string) => {
  try {
    return new URL(raw).origin;
  } catch {
    return null;
  }
};

export function ProductSkillsPanel({ product }: { product: Product }) {
  const [open, setOpen] = useState(false);
  const { data, loading, error: loadError, reload } = useAsyncData<ProductSkills>(
    () => api.adminGetProductSkills(product.id),
    [product.id],
    { enabled: open, fallback: "Failed to load this product’s skills" },
  );
  // A write's own response (sync, apply) is the freshest view, but only over the load it
  // superseded: once a newer load lands (a reload, or a reopen's refetch) the load wins, and
  // closing the panel drops it outright.
  const [fresh, setFresh] = useState<{ view: ProductSkills; over: ProductSkills | null } | null>(null);
  const view = fresh && fresh.over === data ? fresh.view : data;
  const onFresh = (v: ProductSkills) => setFresh({ view: v, over: data });
  const refetch = async () => {
    setFresh(null);
    await reload();
  };

  const staged = view?.staged ?? null;

  return (
    <details
      className="group/skills border-t border-edge pt-3"
      onToggle={(e) => {
        const isOpen = (e.currentTarget as HTMLDetailsElement).open;
        setOpen(isOpen);
        if (!isOpen) setFresh(null);
      }}
    >
      <summary className="flex cursor-pointer list-none flex-wrap items-center gap-x-2 gap-y-1 text-sm font-medium text-fg marker:content-none">
        <span
          aria-hidden="true"
          className="inline-block text-faint group-open/skills:rotate-90 motion-safe:transition-transform"
        >
          ▸
        </span>
        Skills
        {view && view.config.enabled && (
          <span className="text-xs font-normal text-muted">
            {view.applied.sha ? plural(view.applied.skills.length, "approved skill") : "none approved"}
          </span>
        )}
        {staged && (
          <Badge tone="warning" dot>
            Update waiting for approval
          </Badge>
        )}
        <span className="basis-full pl-4 text-xs font-normal text-faint">
          The playbooks this product’s jobs receive, synced from its repo and live only once you approve them.
        </span>
      </summary>

      {open && (
        <div className="mt-3 space-y-4 pl-4">
          {loadError && !view && (
            <p role="alert" className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
              {loadError}
            </p>
          )}
          {loading && !view ? (
            <p className="flex items-center gap-2 text-sm text-muted">
              <Spinner /> Loading skills…
            </p>
          ) : view ? (
            <SkillsBody product={product} view={view} onFresh={onFresh} onRefetch={refetch} />
          ) : null}
        </div>
      )}
    </details>
  );
}

function SkillsBody({
  product,
  view,
  onFresh,
  onRefetch,
}: {
  product: Product;
  view: ProductSkills;
  onFresh: (v: ProductSkills) => void;
  onRefetch: () => Promise<void>;
}) {
  const userLabel = useUserLabel();
  const [syncing, setSyncing] = useState(false);
  const [applying, setApplying] = useState(false);
  const [actionError, setActionError] = useState("");
  const [notice, setNotice] = useState("");
  const { config, applied, staged } = view;

  const sync = async () => {
    setSyncing(true);
    setActionError("");
    setNotice("");
    try {
      const next = await api.adminSyncProductSkills(product.id);
      onFresh(next);
      if (next.staged) {
        setNotice(
          isNoOp(next.staged) && next.staged.dropped.length === 0
            ? "Synced. The repo matches the approved set; nothing new to review."
            : "Synced. Review the changes below, then approve them.",
        );
      }
    } catch (err) {
      setActionError(friendly(err, "Sync failed"));
    } finally {
      setSyncing(false);
    }
  };

  const approve = async (sha: string) => {
    setApplying(true);
    setActionError("");
    setNotice("");
    try {
      const next = await api.adminApplyProductSkills(product.id, sha);
      onFresh(next);
      setNotice(
        `Approved ${sha.slice(0, 12)}. ${plural(next.applied.skills.length, "skill")} now reach${
          next.applied.skills.length === 1 ? "es" : ""
        } this product’s jobs.`,
      );
    } catch (err) {
      setActionError(friendly(err, "Approve failed"));
      // A 409 means the staged set moved (or vanished) since this review: show the current one.
      if (err instanceof ApiError && err.status === 409) await onRefetch();
    } finally {
      setApplying(false);
    }
  };

  return (
    <>
      {!config.enabled && (
        <p className="rounded-lg border border-info/40 bg-info/10 px-3 py-2 text-sm text-info">
          Product skill sets are off on this instance: no repo host is allowed yet. Whoever runs
          uzi turns them on by listing allowed hosts in{" "}
          <code className="font-mono text-xs">UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS</code>.
        </p>
      )}

      <SourceForm
        productId={product.id}
        productName={product.name}
        config={config}
        busy={syncing || applying}
        onSaved={async (msg) => {
          setActionError("");
          setNotice(msg);
          await onRefetch();
        }}
        onSync={sync}
        syncing={syncing}
      />

      {actionError && (
        <p role="alert" className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          {actionError}
        </p>
      )}
      <p role="status" className={cx("text-sm text-ok", !notice && "sr-only")}>
        {notice}
      </p>

      {staged && (
        <StagedReview
          staged={staged}
          applied={applied}
          stagedBy={userLabel(staged.staged_by)}
          canApprove={config.enabled}
          applying={applying}
          busy={syncing}
          onApprove={() => approve(staged.sha)}
        />
      )}

      <AppliedSet applied={applied} approvedBy={userLabel(applied.applied_by)} />
    </>
  );
}

function SourceForm({
  productId,
  productName,
  config,
  busy,
  syncing,
  onSaved,
  onSync,
}: {
  productId: string;
  productName: string;
  config: ProductSkills["config"];
  busy: boolean;
  syncing: boolean;
  onSaved: (notice: string) => Promise<void>;
  onSync: () => void;
}) {
  const id = useId();
  const [url, setUrl] = useState(config.skills_repo_url);
  const [ref, setRef] = useState(config.skills_ref);
  const [token, setToken] = useState("");
  const [clearToken, setClearToken] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  // The server's view is the basis once it changes (a save, or another admin's edit).
  useEffect(() => setUrl(config.skills_repo_url), [config.skills_repo_url]);
  useEffect(() => setRef(config.skills_ref), [config.skills_ref]);

  const off = !config.enabled;
  const urlValue = url.trim();
  const tokenValue = token.trim();
  const urlChanged = urlValue !== config.skills_repo_url;
  const refChanged = ref.trim() !== config.skills_ref;
  // With the feature off the server still accepts the two removals (an empty URL passes the
  // allowlist check; clearing the token needs none), so they stay available. Setting a URL,
  // a ref or a new token does not: the patch simply never carries them while off.
  const patch: ProductPatch = {};
  if (urlChanged && (!off || urlValue === "")) patch.skills_repo_url = urlValue;
  if (refChanged && !off) patch.skills_ref = ref.trim();
  if (clearToken) patch.clear_skills_token = true;
  else if (tokenValue !== "" && !off) patch.skills_token = tokenValue;
  const dirty = Object.keys(patch).length > 0;
  // Moving the repo to another origin, or removing it, drops the stored token server-side:
  // say so before the save, not after.
  const fromOrigin = originOf(config.skills_repo_url);
  const dropsToken =
    config.skills_token_set && patch.clear_skills_token === undefined && patch.skills_token === undefined &&
    patch.skills_repo_url !== undefined && fromOrigin !== null && originOf(urlValue) !== fromOrigin;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (!dirty || saving) return;
    const sent = { ...patch };
    const tokenDropped = sent.clear_skills_token === true || dropsToken;
    setSaving(true);
    setError("");
    try {
      await api.adminUpdateProduct(productId, sent);
      setToken("");
      setClearToken(false);
      const parts = ["Source saved."];
      if (sent.skills_repo_url === "") parts.push("The repo URL was removed.");
      if (tokenDropped) parts.push("The clone token was removed.");
      else if (sent.skills_token !== undefined) parts.push("The new clone token is stored and cannot be shown again.");
      await onSaved(parts.join(" "));
    } catch (err) {
      setError(friendly(err, "Failed to save the skills source"));
    } finally {
      setSaving(false);
    }
  };

  const tokenState = clearToken ? "removed on save" : config.skills_token_set ? "set" : "not set";
  const disabledInput = "disabled:cursor-not-allowed disabled:opacity-60";

  return (
    <form onSubmit={save} aria-label={`Skills source for ${productName}`} className="space-y-3">
      <fieldset className="space-y-3">
        <legend className="sr-only">Skills source</legend>
        <div className="grid items-start gap-3 sm:grid-cols-[minmax(0,1fr)_12rem]">
          <Field label="Repo URL" htmlFor={`${id}-url`}>
            <Input
              id={`${id}-url`}
              type="url"
              inputMode="url"
              autoComplete="off"
              spellCheck={false}
              placeholder={off ? "No repo" : "https://github.com/acme/helpdesk-skills"}
              value={url}
              disabled={off}
              className={disabledInput}
              aria-describedby={`${id}-url-hint`}
              onChange={(e) => setUrl(e.target.value)}
            />
            <p id={`${id}-url-hint`} className="text-xs text-faint">
              {off
                ? "A new repo can be set once this instance allows a repo host."
                : "Must be https on a host this instance allows. Every SKILL.md in the repo is read."}
              {dropsToken && (
                <span className="mt-1 block text-warn">
                  {urlValue === ""
                    ? "Removing the repo URL also removes the stored clone token."
                    : "This moves the repo to another host, so saving removes the stored clone token."}
                </span>
              )}
            </p>
            {off && config.skills_repo_url !== "" && (
              <div>
                {url === "" ? (
                  <Button type="button" variant="ghost" size="sm" onClick={() => setUrl(config.skills_repo_url)}>
                    Keep repo URL
                  </Button>
                ) : (
                  <Button type="button" variant="ghost" size="sm" onClick={() => setUrl("")}>
                    Remove repo URL
                  </Button>
                )}
              </div>
            )}
          </Field>
          <Field label="Branch, tag or commit" htmlFor={`${id}-ref`}>
            <Input
              id={`${id}-ref`}
              autoComplete="off"
              spellCheck={false}
              placeholder="default branch"
              value={ref}
              disabled={off}
              className={disabledInput}
              onChange={(e) => setRef(e.target.value)}
            />
          </Field>
        </div>

        <div className="space-y-1.5">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <span className="font-medium text-muted">Clone token</span>
            <Badge tone={clearToken ? "warning" : config.skills_token_set ? "ok" : "neutral"}>
              {tokenState}
            </Badge>
            {config.skills_token_set && !clearToken && (
              <Button type="button" variant="ghost" size="sm" onClick={() => { setClearToken(true); setToken(""); }}>
                Remove token
              </Button>
            )}
            {clearToken && (
              <Button type="button" variant="ghost" size="sm" onClick={() => setClearToken(false)}>
                Keep token
              </Button>
            )}
          </div>
          {!clearToken && !off && (
            <>
              <label htmlFor={`${id}-token`} className="sr-only">
                {config.skills_token_set ? "Replace clone token" : "Clone token"}
              </label>
              <PasswordInput
                id={`${id}-token`}
                autoComplete="new-password"
                spellCheck={false}
                placeholder={config.skills_token_set ? "Paste a new token to replace it" : "Paste a read-only token (leave empty for a public repo)"}
                value={token}
                aria-describedby={`${id}-token-hint`}
                onChange={(e) => setToken(e.target.value)}
              />
              <p id={`${id}-token-hint`} className="text-xs text-faint">
                A read-only deploy token or fine-grained PAT for this repo. It is encrypted and never shown again.
              </p>
            </>
          )}
        </div>
      </fieldset>

      {error && (
        <p role="alert" className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" variant="secondary" size="sm" disabled={!dirty || saving}>
          {saving ? "Saving…" : "Save source"}
        </Button>
        <Button
          type="button"
          size="sm"
          disabled={off || busy || saving || dirty || config.skills_repo_url === ""}
          aria-describedby={`${id}-sync-hint`}
          onClick={onSync}
        >
          {syncing ? "Syncing…" : "Sync from repo"}
        </Button>
        <span id={`${id}-sync-hint`} className="text-xs text-faint">
          {off
            ? ""
            : config.skills_repo_url === ""
              ? "Save a repo URL to sync."
              : dirty
                ? "Save the source before syncing."
                : "Sync stages the repo’s skills for review; nothing changes until you approve."}
        </span>
      </div>
    </form>
  );
}

// ApprovedVersion is the "before" of a changed skill: the approved description and body, so
// a change to either is visible side by side with the staged copy above it.
function ApprovedVersion({ skill }: { skill: ProductSkill }) {
  return (
    <details className="mt-2">
      <summary className="cursor-pointer text-xs text-muted">Show the approved version</summary>
      <p className="mt-2 text-xs text-muted">
        <span className="font-medium">Approved description: </span>
        {skill.description ? <RevealedText text={skill.description} /> : "No description."}
      </p>
      <pre className="mt-2 max-h-72 overflow-auto rounded-md border border-dashed border-edge p-3 font-mono text-xs leading-relaxed break-words whitespace-pre-wrap text-muted">
        <RevealedText text={skill.body} />
      </pre>
    </details>
  );
}

// StagedReview is the decision: what the sync found against what is live, the skills it
// left out and why, and the one Approve that names the exact staged commit (a 409 if the
// staged set moved underneath the reviewer).
function StagedReview({
  staged,
  applied,
  stagedBy,
  canApprove,
  applying,
  busy,
  onApprove,
}: {
  staged: Staged;
  applied: ProductSkills["applied"];
  stagedBy: string;
  canApprove: boolean;
  applying: boolean;
  busy: boolean;
  onApprove: () => void;
}) {
  const headingId = useId();
  const stagedByName = new Map(staged.skills.map((s) => [s.name, s]));
  const appliedByName = new Map(applied.skills.map((s) => [s.name, s]));
  const noOp = isNoOp(staged);
  // What the review renders, derived once and used for both the markup and the hidden-
  // character count: added/changed show the staged copy (a change also the approved
  // description and body), removed the approved copy, unchanged and dropped their names.
  const groups = noOp
    ? []
    : GROUPS.map((g) => ({
        ...g,
        rows: staged.diff[g.key].map((name) => ({
          name,
          skill: g.key === "removed" ? appliedByName.get(name) : stagedByName.get(name),
          before: g.key === "changed" ? appliedByName.get(name) : undefined,
        })),
      })).filter((g) => g.rows.length > 0);
  const unchanged = noOp ? [] : staged.diff.unchanged;
  const hidden = countHidden([
    ...groups.flatMap((g) =>
      g.rows.flatMap((r) =>
        r.skill
          ? [...skillTexts(r.skill), ...(r.before ? [r.before.description, r.before.body] : [])]
          : [r.name],
      ),
    ),
    ...unchanged,
    ...staged.dropped.flatMap(dropTexts),
  ]);

  return (
    <section
      aria-labelledby={headingId}
      className="space-y-3 rounded-xl border border-warn/40 bg-warn/5 p-3 sm:p-4"
    >
      <div className="space-y-1">
        <h4 id={headingId} className="text-sm font-semibold text-fg">
          Waiting for approval
        </h4>
        {/* The commit transition is the one line an approver checks against the forge. */}
        <p className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs text-muted">
          {applied.sha ? <Sha sha={applied.sha} /> : <span>nothing approved</span>}
          <span aria-hidden="true">→</span>
          <span className="sr-only">to</span>
          <Sha sha={staged.sha} />
          <span>
            synced {new Date(staged.staged_at).toLocaleString()} by {stagedBy}
          </span>
        </p>
      </div>

      {noOp ? (
        <p className="text-sm text-muted">
          Same skills as the approved set ({plural(staged.diff.unchanged.length, "skill")}).
        </p>
      ) : (
        groups.map(({ key, label, tone, rows }) => (
          <div key={key} className="space-y-1.5">
            <h5 className="flex items-center gap-2 text-xs font-medium text-muted">
              <Badge tone={tone}>{label}</Badge>
              {plural(rows.length, "skill")}
            </h5>
            <ul className="space-y-1.5">
              {rows.map(({ name, skill, before }) =>
                skill ? (
                  <SkillEntry
                    key={name}
                    skill={skill}
                    extra={before && <ApprovedVersion skill={before} />}
                  />
                ) : (
                  <li key={name} className="font-mono text-sm break-all text-fg">
                    <RevealedText text={name} />
                  </li>
                ),
              )}
            </ul>
          </div>
        ))
      )}

      {unchanged.length > 0 && (
        <p className="text-xs text-muted">
          Unchanged:{" "}
          <span className="font-mono break-all">
            {unchanged.map((name, i) => (
              <span key={name}>
                {i > 0 && ", "}
                <RevealedText text={name} />
              </span>
            ))}
          </span>
        </p>
      )}

      {staged.dropped.length > 0 && (
        <div className="space-y-1">
          <h5 className="text-xs font-medium text-muted">Left out of this sync</h5>
          <ul className="list-disc space-y-0.5 pl-5 text-xs text-muted">
            {staged.dropped.map((d, i) => (
              <DropRow key={`${d.name}-${d.reason}-${i}`} d={d} />
            ))}
          </ul>
        </div>
      )}

      {hidden > 0 && (
        <p className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          This review contains {plural(hidden, "hidden character")}, shown above as red U+ codes.
          Such characters can make text read differently to you than to the model. Check each one
          before approving.
        </p>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <Button type="button" size="sm" disabled={!canApprove || applying || busy} onClick={onApprove}>
          {applying ? "Approving…" : `Approve ${staged.sha.slice(0, 12)}`}
        </Button>
        <span className="text-xs text-faint">
          Replaces the approved set; the product’s next jobs get exactly these{" "}
          {plural(staged.skills.length, "skill")}.
        </span>
      </div>
    </section>
  );
}

// AppliedSet is the reference: the commit that is live, who approved it and when, and the
// skills the product's jobs receive now.
function AppliedSet({ applied, approvedBy }: { applied: ProductSkills["applied"]; approvedBy: string }) {
  const headingId = useId();
  return (
    <section aria-labelledby={headingId} className="space-y-2">
      <h4 id={headingId} className="text-sm font-semibold text-fg">
        Approved set
      </h4>
      {applied.sha === "" ? (
        <p className="text-sm text-faint">
          Nothing approved yet. This product’s jobs run with no skills.
        </p>
      ) : (
        <>
          <p className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs text-muted">
            <Sha sha={applied.sha} />
            <span>
              approved
              {applied.applied_at ? ` ${new Date(applied.applied_at).toLocaleString()}` : ""} by {approvedBy}
            </span>
          </p>
          {applied.skills.length === 0 ? (
            <p className="text-sm text-faint">The approved commit has no skills.</p>
          ) : (
            <ul className="space-y-1.5">
              {applied.skills.map((s) => (
                <SkillEntry key={s.name} skill={s} />
              ))}
            </ul>
          )}
        </>
      )}
    </section>
  );
}
