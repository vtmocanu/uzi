// SiteListEditor: the create/edit form for one egress profile ("site list", PRD #1906
// M1w). Hosts are typed one per line; beside them the ENTRY LEDGER mirrors every
// non-empty line with its line number and its verdict: a refusal the api returned for
// it, a stored-entry warning, or the explicit consent a multi-publisher host needs.
//
// The consent is per entry and never implied: an entry the api flags as multi-publisher
// gets a checkbox the admin must tick, and only ticked entries are sent in
// multi_publisher_override. Every server string (problem and warning messages, echoed
// entries) renders as a React text node, never as HTML.

import { useEffect, useId, useMemo, useRef, useState, type FormEvent } from "react";
import {
  api,
  egressProfileProblems,
  type EgressProfile,
  type EgressProfileProblem,
  type EgressProfileWarning,
} from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Field, Input, Textarea, cx } from "../../components/ui";

// entryKey is the identity a verdict and a consent attach to: the entry as typed, trimmed,
// lowercased, one trailing dot dropped. It is a display key only; the api normalizes for
// real (IDNA, de-duplication) and stays the source of truth.
function entryKey(raw: string): string {
  return raw.trim().toLowerCase().replace(/\.$/, "");
}

interface Line {
  lineNo: number;
  text: string;
}

function parseLines(text: string): Line[] {
  return text
    .split("\n")
    .map((t, i) => ({ lineNo: i + 1, text: t.trim() }))
    .filter((l) => l.text !== "");
}

const MULTI_PUBLISHER_CODES = new Set(["multi_publisher_needs_override", "multi_publisher_override"]);

// scopeHint says, in words, what an entry will match — the wildcard rule is the one most
// often misread (a wildcard does not cover its apex).
function scopeHint(text: string): string {
  const k = entryKey(text);
  if (k.startsWith("*.") && k.length > 2) {
    const base = k.slice(2);
    return `Any subdomain of ${base}, not ${base} itself`;
  }
  return "This host only";
}

type Mode = { kind: "create" } | { kind: "edit"; profile: EgressProfile };

export function SiteListEditor({
  mode,
  onSaved,
  onCancel,
}: {
  mode: Mode;
  onSaved: (profile: EgressProfile, created: boolean) => void;
  onCancel: () => void;
}) {
  const editing = mode.kind === "edit" ? mode.profile : null;
  const [name, setName] = useState("");
  const [description, setDescription] = useState(editing?.description ?? "");
  const [hostsText, setHostsText] = useState(editing ? editing.hosts.join("\n") : "");
  const [overrides, setOverrides] = useState<Set<string>>(
    () => new Set((editing?.multi_publisher_override ?? []).map(entryKey)),
  );
  // Verdicts from the last refused save, keyed by entryKey so they stay pinned to their
  // entry while the admin edits other lines, and vanish from an entry whose text changes.
  const [entryProblems, setEntryProblems] = useState<Map<string, EgressProfileProblem[]>>(new Map());
  // Why the api called an entry multi-publisher, remembered ACROSS refused saves (merged,
  // never replaced) while the entry is still listed. A later refusal about something else
  // no longer mentions the entry, but its consent is still sent, so its checkbox and reason
  // must stay on screen: an override is never sent from a control the admin cannot see.
  const [mpReasons, setMpReasons] = useState<Map<string, string>>(new Map());
  const [fieldProblems, setFieldProblems] = useState<EgressProfileProblem[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const summaryRef = useRef<HTMLDivElement>(null);
  const uid = useId();

  // Opening the editor moves focus to its first field (the name, or the description when
  // editing), so a keyboard user lands in the form they just opened.
  const firstFieldId = editing ? `${uid}-desc` : `${uid}-name`;
  useEffect(() => {
    document.getElementById(firstFieldId)?.focus();
  }, [firstFieldId]);

  // Stored-entry warnings (edit mode) by key: overrides already accepted, entries the grown
  // built-in list now flags, and stale entries.
  const storedWarnings = useMemo(() => {
    const m = new Map<string, EgressProfileWarning[]>();
    for (const w of editing?.warnings ?? []) {
      const k = entryKey(w.entry);
      m.set(k, [...(m.get(k) ?? []), w]);
    }
    return m;
  }, [editing]);

  const lines = parseLines(hostsText);
  // A multi-publisher refusal whose box is now ticked is a decision made, not a problem left.
  const problemCount =
    [...entryProblems].reduce(
      (n, [k, ps]) => n + ps.filter((p) => !(MULTI_PUBLISHER_CODES.has(p.code) && overrides.has(k))).length,
      0,
    ) + fieldProblems.length;

  // The multi-publisher reason for an entry, in order of freshness: the last refusal, a
  // remembered earlier refusal, then a stored-entry warning (edit mode).
  const multiPublisherReason = (k: string): string | undefined =>
    entryProblems.get(k)?.find((p) => MULTI_PUBLISHER_CODES.has(p.code))?.message ??
    mpReasons.get(k) ??
    storedWarnings.get(k)?.find((w) => MULTI_PUBLISHER_CODES.has(w.code))?.message;

  const fieldProblem = (field: string) => fieldProblems.filter((p) => p.field === field);

  const toggleOverride = (key: string, on: boolean) =>
    setOverrides((prev) => {
      const next = new Set(prev);
      if (on) next.add(key);
      else next.delete(key);
      return next;
    });

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    const hosts = lines.map((l) => l.text);
    const seen = new Set<string>();
    const override = hosts.filter((h) => {
      const k = entryKey(h);
      if (!overrides.has(k) || seen.has(k)) return false;
      seen.add(k);
      return true;
    });
    const body = { description: description.trim(), hosts, multi_publisher_override: override };
    setBusy(true);
    try {
      const resp = editing
        ? await api.adminUpdateEgressProfile(editing.name, body)
        : await api.adminCreateEgressProfile({ name: name.trim(), ...body });
      setEntryProblems(new Map());
      setMpReasons(new Map());
      setFieldProblems([]);
      onSaved(resp.egress_profile, !editing);
    } catch (err) {
      const problems = egressProfileProblems(err);
      if (!problems) {
        setError(errorMessage(err, editing ? "Failed to save the site list" : "Failed to create the site list"));
        return;
      }
      const byEntry = new Map<string, EgressProfileProblem[]>();
      const rest: EgressProfileProblem[] = [];
      for (const p of problems) {
        const hostIdx = /^hosts\[(\d+)\]$/.exec(p.field);
        const ovIdx = /^multi_publisher_override\[(\d+)\]$/.exec(p.field);
        const text = hostIdx ? hosts[Number(hostIdx[1])] : ovIdx ? override[Number(ovIdx[1])] : undefined;
        if (text === undefined) {
          rest.push(p);
          continue;
        }
        const k = entryKey(text);
        byEntry.set(k, [...(byEntry.get(k) ?? []), p]);
      }
      setEntryProblems(byEntry);
      setMpReasons((prev) => {
        const listed = new Set(hosts.map(entryKey));
        const next = new Map([...prev].filter(([k]) => listed.has(k)));
        for (const [k, ps] of byEntry) {
          const mp = ps.find((p) => MULTI_PUBLISHER_CODES.has(p.code));
          if (mp) next.set(k, mp.message);
        }
        return next;
      });
      setFieldProblems(rest);
      requestAnimationFrame(() => summaryRef.current?.focus());
    } finally {
      setBusy(false);
    }
  };

  const title = editing ? `Edit ${editing.name}` : "New site list";

  return (
    <Card className="space-y-5">
      <form onSubmit={submit} className="space-y-5" aria-label={title} noValidate>
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <h2 className="text-base font-semibold text-fg">{title}</h2>
          {editing && <p className="text-xs text-faint">The name is fixed once a site list is created.</p>}
        </div>

        {error && <Alert message={error} />}
        {problemCount > 0 && (
          <div
            ref={summaryRef}
            tabIndex={-1}
            role="alert"
            className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger outline-hidden focus-visible:outline-2 focus-visible:outline-danger"
          >
            {problemCount === 1
              ? "1 problem to fix before this site list can be saved. Nothing was stored."
              : `${problemCount} problems to fix before this site list can be saved. Nothing was stored.`}
          </div>
        )}

        <div className="grid gap-4 sm:grid-cols-2">
          {!editing && (
            <div className="space-y-1">
              <Field label="Name" htmlFor={`${uid}-name`}>
                <Input
                  id={`${uid}-name`}
                  value={name}
                  placeholder="vendor-x-docs"
                  autoComplete="off"
                  spellCheck={false}
                  className="font-mono"
                  aria-invalid={fieldProblem("name").length > 0}
                  aria-describedby={`${uid}-name-hint`}
                  onChange={(e) => setName(e.target.value)}
                />
              </Field>
              <p id={`${uid}-name-hint`} className="text-xs text-faint">
                Lowercase letters, digits and hyphens. Runs refer to the list by this name.
              </p>
              <ProblemList problems={fieldProblem("name")} />
            </div>
          )}
          <div className={cx("space-y-1", editing && "sm:col-span-2")}>
            <Field label="Description (optional)" htmlFor={`${uid}-desc`}>
              <Input
                id={`${uid}-desc`}
                value={description}
                placeholder="What these sites are, and why a run may read them"
                maxLength={500}
                aria-invalid={fieldProblem("description").length > 0}
                onChange={(e) => setDescription(e.target.value)}
              />
            </Field>
            <ProblemList problems={fieldProblem("description")} />
          </div>
        </div>

        <div className="grid gap-4 lg:grid-cols-[minmax(0,5fr)_minmax(0,7fr)]">
          <div className="space-y-1">
            <Field label="Hosts, one per line" htmlFor={`${uid}-hosts`}>
              <Textarea
                id={`${uid}-hosts`}
                value={hostsText}
                rows={Math.min(14, Math.max(6, hostsText.split("\n").length + 1))}
                spellCheck={false}
                autoCapitalize="off"
                autoComplete="off"
                placeholder={"docs.vendor-x.com\n*.cdn.vendor-x.com"}
                className="font-mono leading-6"
                aria-invalid={fieldProblem("hosts").length > 0 || entryProblems.size > 0}
                aria-describedby={`${uid}-hosts-hint`}
                onChange={(e) => setHostsText(e.target.value)}
              />
            </Field>
            <p id={`${uid}-hosts-hint`} className="text-xs text-faint">
              An exact host, or <code className="font-mono">*.example.com</code> for its subdomains. A wildcard
              does not cover <code className="font-mono">example.com</code> itself: list both to allow both.
            </p>
            <ProblemList problems={fieldProblem("hosts")} />
          </div>

          <section aria-label="Entries" className="min-w-0">
            <p className="mb-1.5 text-sm font-medium text-muted">
              {lines.length === 1 ? "1 entry" : `${lines.length} entries`}
            </p>
            {lines.length === 0 ? (
              <p className="rounded-lg border border-dashed border-edge px-3 py-6 text-center text-sm text-faint">
                Each host you add appears here with what it matches.
              </p>
            ) : (
              <ol className="max-h-[28rem] divide-y divide-edge overflow-y-auto rounded-lg border border-edge">
                {lines.map((l) => (
                  <LedgerRow
                    key={`${l.lineNo}-${l.text}`}
                    line={l}
                    problems={entryProblems.get(entryKey(l.text)) ?? []}
                    warnings={storedWarnings.get(entryKey(l.text)) ?? []}
                    multiPublisherReason={multiPublisherReason(entryKey(l.text))}
                    overridden={overrides.has(entryKey(l.text))}
                    onOverride={(on) => toggleOverride(entryKey(l.text), on)}
                  />
                ))}
              </ol>
            )}
          </section>
        </div>

        <ProblemList problems={fieldProblems.filter((p) => !["name", "description", "hosts"].includes(p.field))} />

        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={busy}>
            {busy ? (editing ? "Saving…" : "Creating…") : editing ? "Save changes" : "Create site list"}
          </Button>
          <Button type="button" variant="ghost" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
        </div>
      </form>
    </Card>
  );
}

function ProblemList({ problems }: { problems: EgressProfileProblem[] }) {
  if (problems.length === 0) return null;
  return (
    <ul className="space-y-0.5 text-xs text-danger">
      {problems.map((p, i) => (
        <li key={`${p.field}-${p.code}-${i}`}>{p.message}</li>
      ))}
    </ul>
  );
}

// LedgerRow is one entry of the ledger. A multi-publisher entry (flagged by this or an
// earlier refused save, or by a stored warning) carries the consent checkbox: ticking it
// is the explicit, per-entry override the api requires.
function LedgerRow({
  line,
  problems,
  warnings,
  multiPublisherReason,
  overridden,
  onOverride,
}: {
  line: Line;
  problems: EgressProfileProblem[];
  warnings: EgressProfileWarning[];
  multiPublisherReason: string | undefined;
  overridden: boolean;
  onOverride: (on: boolean) => void;
}) {
  const id = useId();
  const multiPublisher = multiPublisherReason !== undefined;
  const refusals = problems.filter((p) => !MULTI_PUBLISHER_CODES.has(p.code));
  const stale = warnings.filter((w) => w.code === "stale_entry");
  const needsDecision = multiPublisher && !overridden;
  const refused = refusals.length > 0 || (needsDecision && problems.some((p) => MULTI_PUBLISHER_CODES.has(p.code)));

  return (
    <li
      className={cx(
        "grid grid-cols-[2.25rem_minmax(0,1fr)] gap-x-2 px-3 py-2 text-sm",
        multiPublisher && "bg-warn/5",
        refusals.length > 0 && "bg-danger/5",
      )}
    >
      <span className="pt-px text-right font-mono text-xs leading-5 text-faint tabular-nums">
        <span className="sr-only">Line </span>
        {line.lineNo}
      </span>
      <div className="min-w-0 space-y-1">
        <div className="flex flex-wrap items-baseline justify-between gap-x-3">
          <span className="[overflow-wrap:anywhere] font-mono text-fg">{line.text}</span>
          <span className={cx("text-xs", refused ? "text-danger" : needsDecision ? "text-warn" : "text-faint")}>
            {refusals.length > 0
              ? "Refused"
              : needsDecision
                ? "Needs your decision"
                : multiPublisher
                  ? "Every publisher allowed"
                  : scopeHint(line.text)}
          </span>
        </div>
        {refusals.map((p, i) => (
          <p key={`r-${i}`} className="text-xs text-danger">
            {p.message}
          </p>
        ))}
        {stale.map((w, i) => (
          <p key={`s-${i}`} className="text-xs text-warn">
            {w.message}
          </p>
        ))}
        {multiPublisher && refusals.length === 0 && (
          <div className="space-y-1 rounded-md border border-warn/40 px-2.5 py-2">
            <label htmlFor={`${id}-mp`} className="flex cursor-pointer items-start gap-2 text-sm text-fg">
              <input
                id={`${id}-mp`}
                type="checkbox"
                checked={overridden}
                onChange={(e) => onOverride(e.target.checked)}
                aria-describedby={`${id}-mp-why`}
                className="mt-0.5 h-4 w-4 shrink-0 rounded border-edge accent-warn"
              />
              <span>
                Allow every publisher on <span className="[overflow-wrap:anywhere] font-mono">{line.text}</span>
              </span>
            </label>
            <p id={`${id}-mp-why`} className="pl-6 text-xs text-muted">
              {multiPublisherReason}
            </p>
            <p className="pl-6 text-xs text-muted">
              Many unrelated people publish on this host, and the fetch check sees only the host, not the path.
              Ticking this lets a run read anything anyone has put there, not just this vendor's pages.
            </p>
          </div>
        )}
      </div>
    </li>
  );
}
