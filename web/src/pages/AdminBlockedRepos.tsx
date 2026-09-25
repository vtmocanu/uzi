// Admin → Blocked repos (PRD #66 M9, D8): the cross-user list of repos the
// push/merge guardrail refuses right now, plus any an admin has explicitly allowed.
// Admin-only page (gated by AdminRoute). An admin allows any owner's blocked repo
// (with a reason) or revokes an existing override — the same admin-only endpoints the
// Repos page uses inline. Backed by the STORED privilege report, so it inherits R1's
// caveat: if privilege checks were never run, an empty list is "unknown", not "none".

import { useState } from "react";
import { api, type BlockedRepo, type GuardrailOverrideRequest } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { Alert, Badge, Button, Card, EmptyState, ListSkeleton, Textarea } from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import { Modal } from "../components/Modal";
import { BoardIcon, XIcon } from "../components/icons";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail, maskRepoPath } from "../lib/demoMask";

// An override older than this is flagged stale (Q3): visibility, never auto-revocation.
const STALE_DAYS = 30;

function daysSince(iso: string): number {
  return Math.floor((Date.now() - new Date(iso).getTime()) / (1000 * 60 * 60 * 24));
}

export function AdminBlockedRepos() {
  const demo = useDemoMode();
  // repos + checksUnknown are set only by the load, so they ride in the hook's data
  // bundle and are read below with the same initial values the old useState had.
  const { data, loading, error: loadError, reload } = useAsyncData(
    async () => {
      const res = await api.adminListBlockedRepos();
      return { repos: res.repos, checksUnknown: res.checks_unknown, requests: res.requests };
    },
    [],
    { fallback: "Failed to load blocked repos" },
  );
  const repos = data?.repos ?? [];
  const checksUnknown = data?.checksUnknown ?? false;
  // Pending cross-user override requests (issue #1432): members asking to enable a repo
  // the guardrail refused. Always an array from the server (never null).
  const requests = data?.requests ?? [];
  // Kept local: the Allow-anyway / Revoke handlers below still set the page error, so
  // it is merged with the hook's load error at the one page-level Alert.
  const [error, setError] = useState("");
  // The repo whose Allow-anyway modal is open, plus the reason and in-flight POST.
  const [allowRepo, setAllowRepo] = useState<BlockedRepo | null>(null);
  const [allowReason, setAllowReason] = useState("");
  const [allowBusy, setAllowBusy] = useState(false);
  // Submit error for the Allow-anyway modal, rendered INSIDE the dialog (a page-level
  // Alert would sit behind the backdrop, unseen). Distinct from the page `error`.
  const [allowError, setAllowError] = useState("");
  const [revokeBusyId, setRevokeBusyId] = useState<string | null>(null);
  // Pending override-request decisions (issue #1432). Both decisions prompt a note-modal
  // first — approve records the override with an OPTIONAL note, reject keeps the block with
  // an OPTIONAL note — so each carries its request, note, in-flight POST, and inline error.
  // A decision in flight (approveBusy || rejectBusy) locks the row's Approve/Reject buttons.
  const [approveRequest, setApproveRequest] = useState<GuardrailOverrideRequest | null>(null);
  const [approveNote, setApproveNote] = useState("");
  const [approveBusy, setApproveBusy] = useState(false);
  const [approveError, setApproveError] = useState("");
  const [rejectRequest, setRejectRequest] = useState<GuardrailOverrideRequest | null>(null);
  const [rejectNote, setRejectNote] = useState("");
  const [rejectBusy, setRejectBusy] = useState(false);
  const [rejectError, setRejectError] = useState("");

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
      await reload();
    } catch (err) {
      setAllowError(errorMessage(err, "Failed to allow the repo"));
    } finally {
      setAllowBusy(false);
    }
  };

  const revoke = async (repo: BlockedRepo) => {
    setError("");
    setRevokeBusyId(repo.id);
    try {
      await api.clearRepoGuardrailOverride(repo.id);
      await reload();
    } catch (err) {
      setError(errorMessage(err, "Failed to revoke the override"));
    } finally {
      setRevokeBusyId(null);
    }
  };

  // Open the approve-note modal (issue #1432): approval records the override so the OWNER
  // can retry Enable — it does not enable the repo. Resets the optional note/error.
  const openApprove = (req: GuardrailOverrideRequest) => {
    setError("");
    setApproveNote("");
    setApproveError("");
    setApproveRequest(req);
  };

  // Close the approve-note modal. A no-op while a POST is in flight so neither Escape nor a
  // backdrop click can dismiss a submitting form (matching the disabled ×).
  const closeApprove = () => {
    if (approveBusy) return;
    setApproveRequest(null);
    setApproveError("");
  };

  // Approve a pending request with an OPTIONAL note (submit allowed when empty). Sets the
  // override so the OWNER can retry Enable — it does not enable the repo. An empty note is
  // omitted so the endpoint sees no decision_note rather than "". reload after; 409 (already
  // decided) / 404 surface inline in the modal.
  const submitApprove = async () => {
    if (!approveRequest) return;
    const note = approveNote.trim();
    setApproveError("");
    setApproveBusy(true);
    try {
      await api.approveGuardrailOverrideRequest(approveRequest.id, note || undefined);
      setApproveRequest(null);
      setApproveNote("");
      await reload();
    } catch (err) {
      setApproveError(errorMessage(err, "Failed to approve the request"));
    } finally {
      setApproveBusy(false);
    }
  };

  const openReject = (req: GuardrailOverrideRequest) => {
    setError("");
    setRejectNote("");
    setRejectError("");
    setRejectRequest(req);
  };

  // Close the reject-note modal. A no-op while a POST is in flight so neither Escape nor
  // a backdrop click can dismiss a submitting form (matching the disabled ×).
  const closeReject = () => {
    if (rejectBusy) return;
    setRejectRequest(null);
    setRejectError("");
  };

  // Reject a pending request with an OPTIONAL note (submit allowed when empty). An empty
  // note is omitted so the endpoint sees no decision_note rather than "". reload after.
  const submitReject = async () => {
    if (!rejectRequest) return;
    const note = rejectNote.trim();
    setRejectError("");
    setRejectBusy(true);
    try {
      await api.rejectGuardrailOverrideRequest(rejectRequest.id, note || undefined);
      setRejectRequest(null);
      setRejectNote("");
      await reload();
    } catch (err) {
      setRejectError(errorMessage(err, "Failed to reject the request"));
    } finally {
      setRejectBusy(false);
    }
  };

  return (
    <AdminShell description="Every user's repos the push/merge guardrail refuses right now, plus any an admin has explicitly allowed. Fixing branch protection on the forge clears the block on the next sync; an override persists until revoked.">

      {(error || loadError) && <Alert message={error || loadError} />}

      {/* R1 caveat: never let a never-checked connection read as clean. */}
      {checksUnknown && (
        <Alert
          tone="warning"
          message="At least one forge connection has never been privilege-checked (privilege checks may be disabled). This list may be incomplete — an empty or short list here means unknown, not none blocked."
        />
      )}

      {/* Pending override requests (issue #1432): members asking to enable a repo the
          guardrail refused. Rendered above the blocked/overridden table, and outside its
          repos-empty guard so a request shows even when nothing else is blocked. */}
      {!loading && requests.length > 0 && (
        <Card flush>
          <div className="border-b border-edge px-5 py-3">
            <h2 className="text-sm font-semibold text-fg">Pending override requests</h2>
            <p className="mt-0.5 text-xs text-muted">
              Members asking to enable a repo the guardrail refused. Approving records the override so the
              owner can retry Enable — it does not enable the repo for them.
            </p>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-edge text-muted">
                <tr>
                  <th className="px-4 py-3 font-medium">Requested by</th>
                  <th className="px-4 py-3 font-medium">Repo</th>
                  <th className="px-4 py-3 font-medium">Reason</th>
                  <th className="px-4 py-3 font-medium">What blocked it</th>
                  <th className="px-4 py-3 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-edge">
                {requests.map((req) => (
                  <tr key={req.id} className="align-top transition-colors hover:bg-raised/30">
                    <td className="px-4 py-3 text-muted">{maskEmail(req.owner_email, demo)}</td>
                    <td className="px-4 py-3">
                      <div className="font-medium text-fg">{maskRepoPath(req.repo_path, demo)}</div>
                      <div className="font-mono text-xs text-faint">{req.forge_type}</div>
                    </td>
                    <td className="px-4 py-3 text-muted">{req.reason}</td>
                    <td className="px-4 py-3">
                      {req.findings.length > 0 ? (
                        <ul className="list-disc space-y-0.5 pl-5 text-xs text-muted">
                          {req.findings.map((f, i) => (
                            <li key={`${f.code}-${i}`}>{f.message}</li>
                          ))}
                        </ul>
                      ) : (
                        <span className="text-xs text-faint">—</span>
                      )}
                    </td>
                    <td className="px-4 py-3 text-right">
                      <div className="flex justify-end gap-2">
                        <Button size="sm" disabled={approveBusy || rejectBusy} onClick={() => openApprove(req)}>
                          Approve
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={approveBusy || rejectBusy}
                          onClick={() => openReject(req)}
                        >
                          Reject
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
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
        <Card flush>
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
                      <td className="px-4 py-3 text-muted">{maskEmail(r.owner_email, demo)}</td>
                      <td className="px-4 py-3">
                        <div className="font-medium text-fg">{maskRepoPath(r.path, demo)}</div>
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
                              by {maskEmail(ov.by, demo)} · {daysSince(ov.at)}d ago
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
          label={`Allow runs on ${maskRepoPath(allowRepo.path, demo)}`}
          onClose={closeAllow}
          closeOnBackdrop={!allowBusy}
        >
          <div className="my-8 w-full max-w-lg overflow-hidden rounded-2xl border border-edge-strong bg-surface shadow-2xl">
            <div className="flex items-start justify-between gap-3 border-b border-edge px-5 py-4">
              <div>
                <h2 className="text-base font-semibold">Allow runs on this repo?</h2>
                <p className="mt-0.5 text-xs text-muted">
                  {maskRepoPath(allowRepo.path, demo)} · owned by {maskEmail(allowRepo.owner_email, demo)}
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
            <div className="flex items-center justify-end gap-2 border-t border-edge bg-ink/40 inset-panel px-5 py-3.5">
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

      {/* Approve-note modal (issue #1432): the note is OPTIONAL, so the Approve button
          stays enabled with an empty field. Approval records the override so the owner can
          retry Enable — it does not enable the repo, and the server re-checks the live guard
          at retry time, dismissing the request if the repo is no longer blocked. */}
      {approveRequest && (
        <Modal
          label={`Approve the request to enable ${maskRepoPath(approveRequest.repo_path, demo)}`}
          onClose={closeApprove}
          closeOnBackdrop={!approveBusy}
        >
          <div className="my-8 w-full max-w-lg overflow-hidden rounded-2xl border border-edge-strong bg-surface shadow-2xl">
            <div className="flex items-start justify-between gap-3 border-b border-edge px-5 py-4">
              <div>
                <h2 className="text-base font-semibold">Approve this request?</h2>
                <p className="mt-0.5 text-xs text-muted">
                  {maskRepoPath(approveRequest.repo_path, demo)} · requested by {maskEmail(approveRequest.owner_email, demo)}
                </p>
              </div>
              <button
                type="button"
                onClick={closeApprove}
                disabled={approveBusy}
                aria-label="Close"
                className="rounded-md p-1 text-muted hover:bg-raised hover:text-fg"
              >
                <XIcon />
              </button>
            </div>
            <div className="space-y-4 px-5 py-5">
              {approveError && <Alert message={approveError} />}
              <p className="text-sm text-muted">
                This records the override so the owner can retry Enable — it does{" "}
                <span className="font-medium text-fg">not</span> enable the repo for them. On their
                retry the server re-checks the live guardrail and dismisses this request if the repo
                is no longer blocked.
              </p>
              <label className="block space-y-1.5">
                <span className="text-sm font-medium text-fg">Note (optional)</span>
                <Textarea
                  rows={3}
                  value={approveNote}
                  disabled={approveBusy}
                  placeholder="Add a note for the owner (shown to them on the repo)"
                  onChange={(e) => setApproveNote(e.target.value)}
                />
              </label>
            </div>
            <div className="flex items-center justify-end gap-2 border-t border-edge bg-ink/40 inset-panel px-5 py-3.5">
              <Button variant="ghost" size="sm" disabled={approveBusy} onClick={closeApprove}>
                Cancel
              </Button>
              <Button size="sm" disabled={approveBusy} onClick={submitApprove}>
                {approveBusy ? "Approving…" : "Approve"}
              </Button>
            </div>
          </div>
        </Modal>
      )}

      {/* Reject-note modal (issue #1432): the note is OPTIONAL, so the Reject button
          stays enabled with an empty field. Rendered back to the requester when set. */}
      {rejectRequest && (
        <Modal
          label={`Reject the request to enable ${maskRepoPath(rejectRequest.repo_path, demo)}`}
          onClose={closeReject}
          closeOnBackdrop={!rejectBusy}
        >
          <div className="my-8 w-full max-w-lg overflow-hidden rounded-2xl border border-edge-strong bg-surface shadow-2xl">
            <div className="flex items-start justify-between gap-3 border-b border-edge px-5 py-4">
              <div>
                <h2 className="text-base font-semibold">Reject this request?</h2>
                <p className="mt-0.5 text-xs text-muted">
                  {maskRepoPath(rejectRequest.repo_path, demo)} · requested by {maskEmail(rejectRequest.owner_email, demo)}
                </p>
              </div>
              <button
                type="button"
                onClick={closeReject}
                disabled={rejectBusy}
                aria-label="Close"
                className="rounded-md p-1 text-muted hover:bg-raised hover:text-fg"
              >
                <XIcon />
              </button>
            </div>
            <div className="space-y-4 px-5 py-5">
              {rejectError && <Alert message={rejectError} />}
              <p className="text-sm text-muted">
                The member keeps their block. Add a note if you want to tell them why — they see it on the
                repo. It is optional.
              </p>
              <label className="block space-y-1.5">
                <span className="text-sm font-medium text-fg">Note (optional)</span>
                <Textarea
                  rows={3}
                  value={rejectNote}
                  disabled={rejectBusy}
                  placeholder="Why this request is rejected (shown to the requester)"
                  onChange={(e) => setRejectNote(e.target.value)}
                />
              </label>
            </div>
            <div className="flex items-center justify-end gap-2 border-t border-edge bg-ink/40 inset-panel px-5 py-3.5">
              <Button variant="ghost" size="sm" disabled={rejectBusy} onClick={closeReject}>
                Cancel
              </Button>
              <Button variant="danger" size="sm" disabled={rejectBusy} onClick={submitReject}>
                {rejectBusy ? "Rejecting…" : "Reject request"}
              </Button>
            </div>
          </div>
        </Modal>
      )}
    </AdminShell>
  );
}
