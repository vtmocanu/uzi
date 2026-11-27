// "Link to an issue tracker" (PRD #4173 M6): the Repos page's dialog that fixes a
// never-enabled repo's issue source to one issue tracker project and eligibility tag.
//
// The link is permanent (no unlink or relink), so the dialog is built around the one
// thing an admin must judge before committing: who can apply the chosen tag. For that
// tag it shows the owner, who can see it, who can apply it, and the two verdicts the
// server judged for the connection's account, in plain words, with what to change in
// YouTrack when either fails. Link stays disabled until both verdicts pass and the admin
// confirms the one thing uzi cannot check (who can edit issues in the project). The
// confirmation is about one project and tag, so changing the connection, project or tag
// clears it.
//
// The server still decides: a refusal (enabled before, already linked, project not
// found, either tag verdict, issue numbers already stored) shows verbatim and every
// selection stays. Every API-provided string is JSX text through stripUnsafeChars.

import { useEffect, useId, useState } from "react";
import { Link } from "react-router-dom";
import {
  api,
  type IssueTrackerConnection,
  type IssueTrackerLinkResult,
  type IssueTrackerProject,
  type IssueTrackerTag,
} from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { stripUnsafeChars } from "../lib/safeText";
import { describeSharing, issueTrackerKindLabel } from "../lib/issueTrackerText";
import { maskHost, maskUsername } from "../lib/demoMask";
import { Alert, Button, cx, Field, Select, Skeleton } from "./ui";
import { Modal } from "./Modal";
import { CheckIcon, XIcon } from "./icons";

type Loadable<T> = { state: "idle" } | { state: "loading" } | { state: "ok"; items: T[] } | { state: "error"; message: string };

export function IssueTrackerLinkDialog({
  repoId,
  repoLabel,
  demo,
  onLinked,
  onClose,
}: {
  repoId: string;
  // The repo's display path, already masked for demo mode by the caller.
  repoLabel: string;
  demo: boolean;
  onLinked: (link: IssueTrackerLinkResult, connection: IssueTrackerConnection | null) => void;
  onClose: () => void;
}) {
  const ids = useId();
  const [connections, setConnections] = useState<Loadable<IssueTrackerConnection>>({ state: "loading" });
  const [connectionId, setConnectionId] = useState("");
  const [projects, setProjects] = useState<Loadable<IssueTrackerProject>>({ state: "idle" });
  const [tags, setTags] = useState<Loadable<IssueTrackerTag>>({ state: "idle" });
  const [projectId, setProjectId] = useState("");
  const [tagId, setTagId] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [linking, setLinking] = useState(false);
  const [linkError, setLinkError] = useState("");

  useEffect(() => {
    let live = true;
    api.listIssueTrackerConnections().then(
      ({ connections }) => {
        if (!live) return;
        setConnections({ state: "ok", items: connections });
        setConnectionId((prev) => prev || connections[0]?.id || "");
      },
      (err: unknown) => {
        if (live) setConnections({ state: "error", message: stripUnsafeChars(errorMessage(err, "Failed to load issue tracker connections")) });
      },
    );
    return () => {
      live = false;
    };
  }, []);

  // Projects and tags belong to a connection: a new connection reloads both and clears
  // the picks made under the previous one, and the confirmation with them.
  useEffect(() => {
    if (!connectionId) return;
    let live = true;
    setProjects({ state: "loading" });
    setTags({ state: "loading" });
    setProjectId("");
    setTagId("");
    setConfirmed(false);
    api.listIssueTrackerProjects(connectionId).then(
      ({ projects }) => live && setProjects({ state: "ok", items: projects }),
      (err: unknown) =>
        live && setProjects({ state: "error", message: stripUnsafeChars(errorMessage(err, "Failed to load projects")) }),
    );
    api.listIssueTrackerTags(connectionId).then(
      ({ tags }) => live && setTags({ state: "ok", items: tags }),
      (err: unknown) => live && setTags({ state: "error", message: stripUnsafeChars(errorMessage(err, "Failed to load tags")) }),
    );
    return () => {
      live = false;
    };
  }, [connectionId]);

  const connectionList = connections.state === "ok" ? connections.items : [];
  const connection = connectionList.find((c) => c.id === connectionId) ?? null;
  const tag = tags.state === "ok" ? (tags.items.find((t) => t.id === tagId) ?? null) : null;
  const accountLogin = connection ? maskUsername(stripUnsafeChars(connection.account_login), "bot", demo) : "";
  const showLogin = (login: string) => maskUsername(login, "human", demo);
  const tagUsable = tag !== null && tag.account_can_apply && tag.others_can_apply;
  const canLink = !linking && connection !== null && projectId !== "" && tagUsable && confirmed;

  const link = async () => {
    if (!canLink) return;
    setLinking(true);
    setLinkError("");
    try {
      const { link } = await api.linkRepoToIssueTracker(repoId, {
        connection_id: connectionId,
        project_id: projectId,
        tag_id: tagId,
      });
      onLinked(link, connection);
    } catch (err) {
      // Every selection and the confirmation stay, so a fix in YouTrack is one retry.
      setLinkError(stripUnsafeChars(errorMessage(err, "Link failed")));
      setLinking(false);
    }
  };

  const close = () => {
    if (!linking) onClose();
  };

  return (
    <Modal label={`Link ${repoLabel} to an issue tracker`} onClose={close} closeOnBackdrop={!linking}>
      <div className="my-8 w-full max-w-xl overflow-hidden rounded-2xl border border-edge-strong bg-surface shadow-2xl">
        <div className="flex items-start justify-between gap-3 border-b border-edge px-5 py-4">
          <div className="min-w-0">
            <h2 className="text-base font-semibold">Link to an issue tracker</h2>
            <p className="mt-0.5 break-all text-xs text-muted">{repoLabel}</p>
          </div>
          <button
            type="button"
            onClick={close}
            disabled={linking}
            aria-label="Close"
            className="rounded-md p-1 text-muted hover:bg-raised hover:text-fg"
          >
            <XIcon />
          </button>
        </div>

        <div className="space-y-4 px-5 py-5">
          <p className="max-w-prose text-sm text-muted">
            This repo&apos;s issues will come from an issue tracker project. Its code, merge requests and CI stay on
            the forge. Issues carrying the eligibility tag come into uzi.
          </p>

          {connections.state === "loading" && (
            <div className="space-y-3" aria-hidden="true">
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-9 w-full" />
            </div>
          )}
          {connections.state === "error" && <Alert message={connections.message} />}
          {connections.state === "ok" && connectionList.length === 0 && (
            <div className="rounded-lg border border-edge bg-raised/40 px-3 py-3 text-sm text-muted" data-testid="issue-tracker-no-connection">
              No issue tracker connection yet. Connect one in{" "}
              <Link to="/settings/issue-trackers" className="font-medium text-brand hover:text-brand-hover">
                Settings → Issue trackers
              </Link>
              , then link this repo.
            </div>
          )}

          {connectionList.length > 0 && (
            <>
              <Field label="Connection" htmlFor={`${ids}-connection`}>
                <Select
                  id={`${ids}-connection`}
                  value={connectionId}
                  disabled={linking}
                  onChange={(e) => {
                    setConnectionId(e.target.value);
                    setLinkError("");
                  }}
                >
                  {connectionList.map((c) => (
                    <option key={c.id} value={c.id}>
                      {issueTrackerKindLabel(c.kind)} {maskHost(stripUnsafeChars(c.base_url), demo)} (acts as{" "}
                      {maskUsername(stripUnsafeChars(c.account_login), "bot", demo)})
                    </option>
                  ))}
                </Select>
              </Field>

              <Field label="Project" htmlFor={`${ids}-project`}>
                {projects.state === "error" ? (
                  <Alert message={projects.message} />
                ) : (
                  <Select
                    id={`${ids}-project`}
                    value={projectId}
                    disabled={linking || projects.state !== "ok"}
                    onChange={(e) => {
                      setProjectId(e.target.value);
                      setConfirmed(false);
                      setLinkError("");
                    }}
                  >
                    <option value="">{projects.state === "ok" ? "Choose a project" : "Loading projects…"}</option>
                    {projects.state === "ok" &&
                      projects.items.map((p) => (
                        <option key={p.id} value={p.id}>
                          {stripUnsafeChars(p.short_name)} — {stripUnsafeChars(p.name)}
                        </option>
                      ))}
                  </Select>
                )}
              </Field>

              <Field label="Eligibility tag" htmlFor={`${ids}-tag`}>
                {tags.state === "error" ? (
                  <Alert message={tags.message} />
                ) : (
                  <Select
                    id={`${ids}-tag`}
                    value={tagId}
                    disabled={linking || tags.state !== "ok"}
                    onChange={(e) => {
                      setTagId(e.target.value);
                      setConfirmed(false);
                      setLinkError("");
                    }}
                  >
                    <option value="">{tags.state === "ok" ? "Choose a tag" : "Loading tags…"}</option>
                    {tags.state === "ok" &&
                      tags.items.map((t) => (
                        <option key={t.id} value={t.id}>
                          {stripUnsafeChars(t.name)} (owner {showLogin(stripUnsafeChars(t.owner.login))})
                        </option>
                      ))}
                  </Select>
                )}
              </Field>

              {tag && <TagSharing tag={tag} accountLogin={accountLogin} showLogin={showLogin} />}

              {/* The commitment: what cannot be undone, and what uzi cannot check. */}
              <div className="space-y-2 rounded-lg border border-warn/40 bg-warn/5 px-3 py-3 text-sm">
                <p className="font-medium text-fg">
                  This choice cannot be changed afterwards: there is no unlink or relink.
                </p>
                <p className="text-muted">
                  uzi checks who can apply the tag, but not whether those people can edit issues in this project.
                  Confirm they can.
                </p>
                <label className="flex items-start gap-2 pt-1 text-fg">
                  <input
                    type="checkbox"
                    className="mt-0.5 h-4 w-4 shrink-0 accent-brand"
                    checked={confirmed}
                    disabled={linking}
                    onChange={(e) => setConfirmed(e.target.checked)}
                  />
                  <span>They can edit issues in this project</span>
                </label>
              </div>

              {linkError && <Alert message={linkError} />}
            </>
          )}
        </div>

        <div className="flex items-center justify-end gap-2 border-t border-edge bg-ink/40 inset-panel px-5 py-3.5">
          <Button variant="ghost" size="sm" disabled={linking} onClick={close}>
            Cancel
          </Button>
          {connectionList.length > 0 && (
            <Button size="sm" disabled={!canLink} onClick={link}>
              {linking ? "Linking…" : "Link"}
            </Button>
          )}
        </div>
      </div>
    </Modal>
  );
}

// TagSharing is the dialog's centre: the tag's owner and sharing, then the two verdicts.
// A verdict that fails says what to change in YouTrack.
function TagSharing({
  tag,
  accountLogin,
  showLogin,
}: {
  tag: IssueTrackerTag;
  accountLogin: string;
  showLogin: (login: string) => string;
}) {
  return (
    <section aria-label={`Who can apply ${stripUnsafeChars(tag.name)}`} className="rounded-lg border border-edge bg-raised/30">
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1.5 px-3 py-3 text-sm">
        <dt className="text-muted">Owner</dt>
        <dd className="break-words text-fg">{showLogin(stripUnsafeChars(tag.owner.login))}</dd>
        <dt className="text-muted">Can see it</dt>
        <dd className="break-words text-fg">{describeSharing(tag.read, showLogin)}</dd>
        <dt className="text-muted">Can apply it</dt>
        <dd className="break-words text-fg">{describeSharing(tag.use, showLogin)}</dd>
      </dl>
      <ul className="space-y-2 border-t border-edge px-3 py-3 text-sm">
        <Verdict ok={tag.account_can_apply}>
          {tag.account_can_apply ? (
            <>The connection&apos;s account, {accountLogin}, can apply this tag.</>
          ) : (
            <>
              The connection&apos;s account, {accountLogin}, cannot apply this tag. In YouTrack, share the tag&apos;s
              use settings with {accountLogin} or with All Users, or pick a tag {accountLogin} owns.
            </>
          )}
        </Verdict>
        <Verdict ok={tag.others_can_apply}>
          {tag.others_can_apply ? (
            <>People other than {accountLogin} can apply it, so they can start work by tagging an issue.</>
          ) : (
            <>
              Nobody other than {accountLogin} can apply it, so nobody could start work by tagging an issue. In
              YouTrack, share the tag&apos;s use settings with named users or with All Users. A named group counts
              only when YouTrack reports at least two members for it.
            </>
          )}
        </Verdict>
      </ul>
    </section>
  );
}

function Verdict({ ok, children }: { ok: boolean; children: React.ReactNode }) {
  return (
    <li className="flex items-start gap-2">
      <span
        className={cx(
          "mt-0.5 flex h-4 w-4 shrink-0 items-center justify-center rounded-full border",
          ok ? "border-ok/50 bg-ok/10 text-ok" : "border-danger/50 bg-danger/10 text-danger",
        )}
      >
        {ok ? <CheckIcon aria-hidden="true" className="h-3 w-3" /> : <XIcon aria-hidden="true" className="h-3 w-3" />}
        <span className="sr-only">{ok ? "Passes:" : "Fails:"}</span>
      </span>
      <span className={ok ? "text-fg" : "text-danger"}>{children}</span>
    </li>
  );
}
