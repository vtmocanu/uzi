import { useRef, useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Input, SectionTitle } from "../../components/ui";
import { PlusIcon, TrashIcon } from "../../components/icons";
import {
  MAX_TRUSTED_BOTS,
  baseUrlProblem,
  entryKey,
  splitTrustedBots,
  userIdProblem,
} from "../../lib/trustedReviewBots";

// One editable allowlist row. `saved` is the token a row was loaded from (undefined for
// a row the admin added), so a malformed saved entry is flagged at once rather than
// after the admin touches it; `bare` marks a saved token that had no '#' at all, so it
// serializes back verbatim.
type Row = {
  key: number;
  base: string;
  id: string;
  saved?: string;
  bare: boolean;
  touched: boolean;
};

const blank = (row: Row) => row.base.trim() === "" && row.id.trim() === "";

function serialize(row: Row): string {
  if (row.bare && row.id.trim() === "") return row.base.trim();
  return entryKey(row.base, row.id);
}

// normalize is the order- and duplicate-insensitive form used for dirty-checking.
function normalize(entries: string[]): string {
  return [...new Set(entries)].sort().join(",");
}

let nextKey = 0;

// rowsFrom splits a stored value into rows. Every token becomes a row, valid or not:
// a malformed saved entry is shown (flagged) exactly as saved, never dropped.
function rowsFrom(value: string): Row[] {
  return splitTrustedBots(value).map((tok) => {
    const hash = tok.indexOf("#");
    return {
      key: nextKey++,
      base: hash < 0 ? tok : tok.slice(0, hash),
      id: hash < 0 ? "" : tok.slice(hash + 1),
      saved: tok,
      bare: hash < 0,
      touched: false,
    };
  });
}

type RowErrors = { base?: string; fix?: string; id?: string; duplicate?: boolean };

function rowErrors(rows: Row[]): Map<number, RowErrors> {
  const out = new Map<number, RowErrors>();
  const seen = new Set<string>();
  for (const row of rows) {
    if (blank(row)) continue;
    const errs: RowErrors = {};
    const b = baseUrlProblem(row.base);
    if (b) {
      errs.base = b.message;
      errs.fix = b.fix;
    }
    const i = userIdProblem(row.id);
    if (i) errs.id = i;
    const k = serialize(row);
    if (!b && !i && seen.has(k)) errs.duplicate = true;
    seen.add(k);
    if (errs.base || errs.id || errs.duplicate) out.set(row.key, errs);
  }
  return out;
}

// TrustedReviewBotsCard is the admin surface for `mr_review_trusted_bots` (issue #2347).
// Review comments from authors who are not collaborators on a repository are withheld
// from automatic MR rework; third-party review bots are never collaborators, so an admin
// lists the ones uzi should hear from here, by forge and numeric user id (never by
// login). Listing a bot authorizes ingestion only: its comments stay untrusted evidence,
// and its summary or walkthrough comments never start a rework. An empty list is the
// fail-closed default: every third-party bot is withheld.
export function TrustedReviewBotsCard({
  settings,
  sources,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const stored = settings.mr_review_trusted_bots ?? "";
  const [rows, setRows] = useState<Row[]>(() => rowsFrom(stored));
  const [submitted, setSubmitted] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const listRef = useRef<HTMLUListElement>(null);

  const isEnv = sources["mr_review_trusted_bots"] === "env";

  const errors = rowErrors(rows);
  const filled = rows.filter((r) => !blank(r));
  const entries = filled.map(serialize);
  const dirty = normalize(entries) !== normalize(splitTrustedBots(stored));
  const tooMany = new Set(entries).size > MAX_TRUSTED_BOTS;
  const malformedSaved = rows.filter((r) => r.saved !== undefined && errors.has(r.key)).length;
  const trustedCount = filled.filter((r) => !errors.has(r.key)).length;

  const update = (key: number, patch: Partial<Row>) =>
    setRows((prev) => prev.map((r) => (r.key === key ? { ...r, ...patch } : r)));

  const remove = (key: number) => {
    setNotice("");
    setRows((prev) => prev.filter((r) => r.key !== key));
  };

  const add = () => {
    setNotice("");
    const key = nextKey++;
    setRows((prev) => [...prev, { key, base: "", id: "", bare: false, touched: false }]);
    // Move focus into the new row so a keyboard user lands where they are about to type.
    requestAnimationFrame(() => {
      listRef.current?.querySelector<HTMLInputElement>(`[data-row="${key}"] input`)?.focus();
    });
  };

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (isEnv || !dirty) return;
    setSubmitted(true);
    if (errors.size > 0 || tooMany) {
      setError(
        tooMany
          ? `At most ${MAX_TRUSTED_BOTS} review bots can be trusted. Remove some before saving.`
          : "Fix the highlighted entries before saving.",
      );
      // Focus the first bad field. Found by row key, not aria-invalid: that mark only
      // renders after `submitted` re-renders the rows.
      const first = rows.find((r) => errors.has(r.key));
      if (first) {
        const inputs = listRef.current?.querySelectorAll<HTMLInputElement>(`[data-row="${first.key}"] input`);
        inputs?.[errors.get(first.key)?.base ? 0 : 1]?.focus();
      }
      return;
    }
    const value = [...new Set(entries)].join(",");
    setBusy(true);
    try {
      const resp = await api.updateSettings({ mr_review_trusted_bots: value });
      onSaved(resp);
      setRows(rowsFrom(resp.settings.mr_review_trusted_bots ?? ""));
      setSubmitted(false);
      setNotice(
        value === ""
          ? "Trusted review bots saved. No review bot is trusted, so their comments are withheld."
          : "Trusted review bots saved.",
      );
    } catch (err) {
      setError(errorMessage(err, "Failed to save the trusted review bots"));
    } finally {
      setBusy(false);
    }
  };

  const atCap = rows.length >= MAX_TRUSTED_BOTS;

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Trusted review bots</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Review comments from people who are not collaborators on a repository are withheld from
          automatic MR rework: they never start a rework and never reach the agent. Review bots such
          as CodeRabbit or Greptile are not collaborators either, so list the ones uzi should hear
          from here.
        </p>
        <p className="mt-2 text-sm text-muted">
          Trusting a bot only lets its comments through. The agent still treats them as unverified
          input, and a bot&rsquo;s summary or walkthrough comment never starts a rework.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}
      {isEnv && (
        <Alert tone="info" message="This setting is fixed by an environment variable and cannot be changed here." />
      )}
      {malformedSaved > 0 && (
        <Alert
          tone="warning"
          message={`${malformedSaved} saved ${malformedSaved === 1 ? "entry is" : "entries are"} not in the required format and match no bot. ${malformedSaved === 1 ? "It is" : "They are"} shown below exactly as saved: fix or remove ${malformedSaved === 1 ? "it" : "them"} before saving.`}
        />
      )}

      <form onSubmit={save} className="space-y-4" noValidate>
        <div className="space-y-2">
          {rows.length === 0 ? (
            <div className="rounded-lg border border-dashed border-edge px-4 py-5">
              <p className="text-sm text-fg">Comments from third-party review bots are currently withheld.</p>
              <p className="mt-1 text-sm text-faint">
                Add a bot to let its review comments reach automatic MR rework.
              </p>
            </div>
          ) : (
            <>
              <h3 className="text-sm font-medium text-muted">
                {trustedCount === 0
                  ? "No review bot trusted yet"
                  : `${trustedCount} review bot${trustedCount === 1 ? "" : "s"} trusted`}
              </h3>
              {/* Column captions for wide screens; each input also carries its own
                  accessible name, so these are presentational. */}
              <div aria-hidden="true" className="hidden gap-2 px-0.5 text-xs text-faint sm:flex">
                <span className="flex-1">Forge address</span>
                <span className="w-4" />
                <span className="w-40">Bot user id</span>
                <span className="w-9" />
              </div>
              <ul ref={listRef} className="space-y-2">
                {rows.map((row, idx) => {
                  const errs = errors.get(row.key);
                  const show = !!errs && (row.saved !== undefined || row.touched || submitted);
                  const n = idx + 1;
                  const msgId = `trusted-bot-${row.key}-msg`;
                  const baseBad = show && !!errs?.base;
                  const idBad = show && (!!errs?.id || !!errs?.duplicate);
                  return (
                    <li
                      key={row.key}
                      data-row={row.key}
                      className="space-y-1 rounded-lg border border-edge p-2 sm:border-0 sm:p-0"
                    >
                      <div className="flex flex-wrap items-center gap-2 sm:flex-nowrap">
                        <Input
                          aria-label={`Forge address, bot ${n}`}
                          aria-invalid={baseBad || undefined}
                          aria-describedby={show ? msgId : undefined}
                          value={row.base}
                          disabled={isEnv}
                          placeholder="e.g. https://github.com"
                          autoComplete="off"
                          spellCheck={false}
                          inputMode="url"
                          onChange={(e) => update(row.key, { base: e.target.value })}
                          onBlur={() => update(row.key, { touched: true })}
                          className="min-w-0 flex-1 basis-full aria-invalid:border-danger/70 sm:basis-auto"
                        />
                        {/* The '#' is the separator of the stored form
                            (https://github.com#136622811), so the row reads as one entry. */}
                        <span aria-hidden="true" className="w-4 text-center text-base text-faint">
                          #
                        </span>
                        <Input
                          aria-label={`Bot user id, bot ${n}`}
                          aria-invalid={idBad || undefined}
                          aria-describedby={show ? msgId : undefined}
                          value={row.id}
                          disabled={isEnv}
                          placeholder="e.g. 136622811"
                          autoComplete="off"
                          spellCheck={false}
                          inputMode="numeric"
                          onChange={(e) => update(row.key, { id: e.target.value, bare: false })}
                          onBlur={() => update(row.key, { touched: true })}
                          className="min-w-0 flex-1 tabular-nums aria-invalid:border-danger/70 sm:w-40 sm:flex-none"
                        />
                        {/* A square icon button, not <Button>: its px-4 would win over a
                            px-0 override (cx has no tailwind-merge) and crush the glyph. */}
                        <button
                          type="button"
                          aria-label={`Remove bot ${n}`}
                          title="Remove"
                          disabled={isEnv}
                          onClick={() => remove(row.key)}
                          className="inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-lg text-faint transition-colors hover:bg-danger/10 hover:text-danger disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-transparent disabled:hover:text-faint"
                        >
                          <TrashIcon className="h-4 w-4" />
                        </button>
                      </div>
                      {show && errs && (
                        <div id={msgId} className="space-y-0.5 text-xs">
                          {row.saved !== undefined && serialize(row) === row.saved && (
                            <p className="text-warn">Saved as &ldquo;{row.saved}&rdquo;.</p>
                          )}
                          {errs.base && (
                            <p className="text-danger">
                              Address: {errs.base}
                              {errs.fix && errs.fix !== row.base.trim() && !isEnv && (
                                <>
                                  {" "}
                                  <button
                                    type="button"
                                    className="rounded text-brand underline underline-offset-2 hover:text-brand-hover"
                                    onClick={() => update(row.key, { base: errs.fix!, touched: true })}
                                  >
                                    Use {errs.fix}
                                  </button>
                                </>
                              )}
                            </p>
                          )}
                          {errs.id && <p className="text-danger">User id: {errs.id}</p>}
                          {errs.duplicate && <p className="text-danger">This bot is already listed.</p>}
                        </div>
                      )}
                    </li>
                  );
                })}
              </ul>
            </>
          )}

          {!isEnv && (
            <Button type="button" variant="secondary" size="sm" onClick={add} disabled={atCap}>
              <PlusIcon className="h-3.5 w-3.5" />
              Add review bot
            </Button>
          )}
          {atCap && !isEnv && (
            <p className="text-xs text-faint">The list holds at most {MAX_TRUSTED_BOTS} bots.</p>
          )}
        </div>

        <p className="text-xs text-faint">
          Bots are matched by forge and numeric user id, never by login name, so a renamed or
          look-alike account is not trusted by accident. On GitHub the id is the{" "}
          <code className="text-muted">id</code> field of{" "}
          <code className="text-muted">api.github.com/users/&lt;login&gt;</code>; on GitLab, of{" "}
          <code className="text-muted">/api/v4/users?username=&lt;login&gt;</code>.
        </p>

        <Button type="submit" disabled={!dirty || busy || isEnv}>
          {busy ? "Saving…" : "Save trusted bots"}
        </Button>
      </form>
    </Card>
  );
}
