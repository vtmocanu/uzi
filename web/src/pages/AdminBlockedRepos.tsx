// Admin → Blocked repos (PRD #66 M9, D8): the cross-user list of repos the
// push/merge guardrail refuses right now, plus any an admin has explicitly allowed.
// Admin-only page (gated by AdminRoute). An admin allows any owner's blocked repo
// (with a reason) or revokes an existing override — the same admin-only endpoints the
// Repos page uses inline. Backed by the STORED privilege report, so it inherits R1's
// caveat: if privilege checks were never run, an empty list is "unknown", not "none".

import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type BlockedRepo } from "../lib/api";
import { Alert, Badge, Button, Card, EmptyState, ListSkeleton, PageHeader, Textarea } from "../components/ui";
import { Modal } from "../components/Modal";
import { BoardIcon, XIcon } from "../components/icons";

// An override older than this is flagged stale (Q3): visibility, never auto-revocation.
const STALE_DAYS = 30;

function daysSince(iso: string): number {
  return Math.floor((Date.now() - new Date(iso).getTime()) / (1000 * 60 * 60 * 24));
}

export function AdminBlockedRepos() {
  const [repos, setRepos] = useState<BlockedRepo[]>([]);
  const [checksUnknown, setChecksUnknown] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  // The repo whose Allow-anyway modal is open, plus the reason and in-flight POST.
  const [allowRepo, setAllowRepo] = useState<BlockedRepo | null>(null);
  const [allowReason, setAllowReason] = useState("");
  const [allowBusy, setAllowBusy] = useState(false);
  // Submit error for the Allow-anyway modal, rendered INSIDE the dialog (a page-level
  // Alert would sit behind the backdrop, unseen). Distinct from the page `error`.
  const [allowError, setAllowError] = useState("");
  const [revokeBusyId, setRevokeBusyId] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await api.adminListBlockedRepos();
      setRepos(res.repos);
      setChecksUnknown(res.checks_unknown);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load blocked repos");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const openAllow = (repo: BlockedRepo) => {
    setError("");
    setAllowReason("");
    setAllowError("");
    setAllowRepo(repo);
  };

  // Close the Allow-anyway modal. A no-op while a POST is in flight so neither Escape
  // nor a backdrop click can dismiss a submitting form (matching the disabled ×).
  const closeAllow = () => {
    if (allowBusy) return;
    setAllowRepo(null);
    setAllowError("");
  };

  const submitAllow = async () => {
    if (!allowRepo) return;
    const reason = allowReason.trim();
    if (!reason) return;
    setAllowError("");
    setAllowBusy(true);
    try {
      await api.setRepoGuardrailOverride(allowRepo.id, reason);
      setAllowRepo(null);
      setAllowReason("");
      await load();
    } catch (err) {
      setAllowError(err instanceof ApiError ? err.message : "Failed to allow the repo");
    } finally {
      setAllowBusy(false);
    }
  };

  const revoke = async (repo: BlockedRepo) => {
    setError("");
    setRevokeBusyId(repo.id);
    try {
      await api.clearRepoGuardrailOverride(repo.id);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to revoke the override");
    } finally {
      setRevokeBusyId(null);
    }
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title="Blocked repos"
        description="Every user's repos the push/merge guardrail refuses right now, plus any an admin has explicitly allowed. Fixing branch protection on the forge clears the block on the next sync; an override persists until revoked."
      />

      {error && <Alert message={error} />}

      {/* R1 caveat: never let a never-checked connection read as clean. */}
      {checksUnknown && (
        <Alert
          tone="warning"
          message="At least one forge connection has never been privilege-checked (privilege checks may be disabled). This list may be incomplete — an empty or short list here means unknown, not none blocked."
        />
      )}

      {loading ? (
        <ListSkeleton rows={4} />
      ) : repos.length === 0 ? (
        <EmptyState
          icon={<BoardIcon />}
          title={checksUnknown ? "Nothing to show — but checks may be off" : "No blocked or allowed repos"}
          description={
            checksUnknown
              ? "No repo could be evaluated from the stored reports. Enable privilege checks to populate this list."
              : "No repo is currently refused by the guardrail, and no admin override is active."
          }
        />
      ) : (
        <Card className="p-0">
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-edge text-muted">
                <tr>
                  <th className="px-4 py-3 font-medium">Owner</th>
                  <th className="px-4 py-3 font-medium">Repo</th>
                  <th className="px-4 py-3 font-medium">State</th>
                  <th className="px-4 py-3 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-edge">
                {repos.map((r) => {
                  const ov = r.guardrail_override;
                  const stale = ov ? daysSince(ov.at) >= STALE_DAYS : false;
                  return (
                    <tr key={r.id} className="align-top transition-colors hover:bg-raised/30">
                      <td className="px-4 py-3 text-muted">{r.owner_email}</td>
                      <td className="px-4 py-3">
                        <div className="font-medium text-fg">{r.path}</div>
                        <div className="font-mono text-xs text-faint">{r.forge_type}</div>
                      </td>
                      <td className="px-4 py-3">
                        {r.blocked ? (
                          <div className="space-y-1.5">
                            <Badge tone="danger" dot>
                              runs blocked
                            </Badge>
                            {r.block_messages.length > 0 && (
                              <ul className="list-disc space-y-0.5 pl-5 text-xs text-muted">
                                {r.block_messages.map((m, i) => (
                                  <li key={i}>{m}</li>
                                ))}
                              </ul>
                            )}
                          </div>
                        ) : ov ? (
                          <div className="space-y-1">
                            <div className="flex flex-wrap items-center gap-1.5">
                              <Badge tone="warning" dot>
                                allowed by admin
                              </Badge>
                              {stale && (
                                <Badge tone="danger" title={`Set ${daysSince(ov.at)} days ago`}>
                                  stale
                                </Badge>
                              )}
                            </div>
                            <div className="text-xs text-muted">
                              by {ov.by} · {daysSince(ov.at)}d ago
                            </div>
                            <div className="text-xs text-faint">Reason: {ov.reason}</div>
                          </div>
                        ) : (
                          <span className="text-xs text-faint">—</span>
                        )}
                      </td>
                      <td className="px-4 py-3 text-right">
                        <div className="flex justify-end gap-2">
                          {ov ? (
                            <Button
                              variant="ghost"
                              size="sm"
                              disabled={revokeBusyId === r.id}
                              onClick={() => revoke(r)}
                            >
                              {revokeBusyId === r.id ? "Revoking…" : "Revoke"}
                            </Button>
                          ) : (
                            <Button variant="secondary" size="sm" onClick={() => openAllow(r)}>
                              Allow anyway
                            </Button>
                          )}
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </Card>
      )}

      {/* Allow-anyway modal: names the exact block findings and requires a reason. */}
      {allowRepo && (
        <Modal
          label={`Allow runs on ${allowRepo.path}`}
          onClose={closeAllow}
          closeOnBackdrop={!allowBusy}
        >
          <div className="my-8 w-full max-w-lg overflow-hidden rounded-2xl border border-edge-strong bg-surface shadow-2xl">
            <div className="flex items-start justify-between gap-3 border-b border-edge px-5 py-4">
              <div>
                <h2 className="text-base font-semibold">Allow runs on this repo?</h2>
                <p className="mt-0.5 text-xs text-muted">
                  {(allowRepo.path)} · owned by {allowRepo.owner_email}
                </p>
              </div>
              <button
                type="button"
                onClick={closeAllow}
                disabled={allowBusy}
                aria-label="Close"
                className="rounded-md p-1 text-muted hover:bg-raised hover:text-fg"
              >
                <XIcon />
              </button>
            </div>
            <div className="space-y-4 px-5 py-5">
              {allowError && <Alert message={allowError} />}
              <p className="text-sm text-muted">
                This is a per-repo, recorded exception: it accepts the risk that the bot can reach the default
                branch; it does <span className="font-medium text-fg">not</span> change branch protection, and it
                never waives a protection uzi could not read.
              </p>
              {allowRepo.block_messages.length > 0 && (
                <div className="rounded-md border border-danger/40 bg-danger/5 p-3">
                  <h3 className="mb-1.5 text-sm font-semibold text-fg">You are accepting these findings</h3>
                  <ul className="list-disc space-y-1 pl-5 text-sm text-muted">
                    {allowRepo.block_messages.map((m, i) => (
                      <li key={i}>{m}</li>
                    ))}
                  </ul>
                </div>
              )}
              <label className="block space-y-1.5">
                <span className="text-sm font-medium text-fg">
                  Reason <span className="text-danger">*</span>
                </span>
                <Textarea
                  rows={3}
                  value={allowReason}
                  disabled={allowBusy}
                  placeholder="Why this repo is allowed through the guardrail (recorded with your name)"
                  onChange={(e) => setAllowReason(e.target.value)}
                />
              </label>
            </div>
            <div className="flex items-center justify-end gap-2 border-t border-edge bg-bg/40 px-5 py-3.5">
              <Button variant="ghost" size="sm" disabled={allowBusy} onClick={closeAllow}>
                Cancel
              </Button>
              <Button size="sm" disabled={allowBusy || allowReason.trim() === ""} onClick={submitAllow}>
                {allowBusy ? "Allowing…" : "Allow anyway"}
              </Button>
            </div>
          </div>
        </Modal>
      )}
    </div>
  );
}
