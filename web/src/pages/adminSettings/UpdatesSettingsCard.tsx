import { useState } from "react";
import { api, isHttpsUrl, type ReleaseCheckStatus } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { useAsyncData } from "../../lib/useAsyncData";
import { Alert, Badge, Button, Card, SectionTitle, Skeleton } from "../../components/ui";
import { displayVersion, formatDay } from "../../components/BuildInfoPopover";
import { formatAgo } from "../../lib/rateLimits";

// The copyable upgrade runbook (PRD #836 M5). Static, generic ops guidance — a helm
// upgrade for hosted/k8s and a compose pull+up for the laptop loop. Non-exported so
// knip's dead-export tier stays green (an exported const read only here reddens it).
const UPGRADE_RUNBOOK = [
  "# helm (hosted / k8s)",
  "helm upgrade uzi oci://ghcr.io/vtmocanu/uzi",
  "# compose (laptop)",
  "docker compose pull && docker compose up -d",
].join("\n");

// releaseExcerpt renders a short PLAIN-TEXT preview of the raw release markdown
// (never HTML — the body is admin-supplied markdown and must not be injected). It
// collapses to the first few non-empty lines, capped at ~300 chars, with an ellipsis
// when truncated. Each previewed line is tidied — a leading heading marker
// (`#`..`######` + space) and a leading list bullet (`-`/`*`/`+` + space) are
// stripped — so `### Added` shows as `Added` and `- Worker drain…` as `Worker
// drain…`. This is a pure string transform on already-plain text: it strips the
// markers, it does NOT render markdown.
function releaseExcerpt(body: string | undefined, max = 300): string {
  if (!body) return "";
  const trimmed = body.trim();
  const lines = trimmed
    .split("\n")
    .map((l) => l.trim().replace(/^#{1,6}\s+/, "").replace(/^[-*+]\s+/, ""))
    .filter((l) => l !== "")
    .slice(0, 6);
  const joined = lines.join("\n");
  if (joined.length <= max) return joined;
  return `${joined.slice(0, max).trimEnd()}…`;
}

// UpdatesSettingsCard is the admin surface for the upstream release check (PRD #836
// M5) — the destination the sidebar pip points at. It self-loads GET
// /admin/release-check (admin-only by virtue of living on the admin-guarded page):
// the current-vs-latest version delta, the release name + date, a plain-text notes
// excerpt with a link OUT to the full GitHub notes, the copyable upgrade runbook, a
// "Check now" button (POST /admin/release-check), and the two runtime toggles. It is
// the ONLY surface that states the "disabled" (air-gap) / "error" (rate-limited or
// unreachable) / "never" (no check yet) status in words. It renders NO release
// markdown as HTML — the excerpt is a plain React text node.
export function UpdatesSettingsCard() {
  // status is seeded by the load but ALSO updated by checkNow / saveToggle, so it
  // stays local and the fetcher seeds it as a side effect.
  const [status, setStatus] = useState<ReleaseCheckStatus | null>(null);
  const [checking, setChecking] = useState(false);
  // Which toggle is mid-save; null when idle. A save disables BOTH toggle inputs
  // (deliberate concurrent-write protection — see the `disabled` guards below), while
  // this value still records which of the two rows is the one being saved.
  const [savingToggle, setSavingToggle] = useState<"enabled" | "banner" | null>(null);
  const [copied, setCopied] = useState(false);
  // Kept local: checkNow / saveToggle set this on failure, so it is merged with the
  // hook's load error at the one Alert below.
  const [error, setError] = useState("");

  // skeleton: "always" mirrors the old load's setLoading(true) on every call, so a
  // Retry (reload) re-arms the skeleton exactly as it did before.
  const { loading, error: loadError, reload } = useAsyncData(
    async ({ isCurrent }) => {
      const { release_check } = await api.getReleaseCheck();
      if (!isCurrent()) return;
      setStatus(release_check);
    },
    [],
    { fallback: "Failed to load release-check status", skeleton: "always" },
  );

  const checkNow = async () => {
    setError("");
    setChecking(true);
    try {
      const { release_check } = await api.checkReleaseNow();
      setStatus(release_check);
    } catch (err) {
      setError(errorMessage(err, "Failed to run the release check"));
    } finally {
      setChecking(false);
    }
  };

  // Persist a toggle through the settings string-space (release_check_enabled /
  // release_check_banner_enabled), then re-read the status so the derived
  // status/delta reflect the new master-toggle state (off ⇒ "disabled").
  const saveToggle = async (which: "enabled" | "banner", next: boolean) => {
    setError("");
    setSavingToggle(which);
    try {
      const key = which === "enabled" ? "release_check_enabled" : "release_check_banner_enabled";
      await api.updateSettings({ [key]: String(next) });
      // The write succeeded — reflect it immediately so a failed re-read below does not
      // leave the controlled checkbox showing the pre-save value.
      setStatus((prev) =>
        prev === null
          ? prev
          : which === "enabled"
            ? { ...prev, release_check_enabled: next }
            : { ...prev, release_check_banner_enabled: next },
      );
      const { release_check } = await api.getReleaseCheck();
      setStatus(release_check);
    } catch (err) {
      setError(errorMessage(err, "Failed to save update settings"));
    } finally {
      setSavingToggle(null);
    }
  };

  const copyRunbook = async () => {
    try {
      await navigator.clipboard.writeText(UPGRADE_RUNBOOK);
      setCopied(true);
      setTimeout(() => setCopied(false), 1400);
    } catch {
      // Clipboard unavailable (insecure context): the runbook stays visible to copy by hand.
    }
  };

  const deltaBadge = (s: ReleaseCheckStatus) => {
    if (s.security) return <Badge tone="danger">Security release</Badge>;
    if (s.update_available) return <Badge tone="brand">Update available</Badge>;
    return <Badge tone="ok">Up to date</Badge>;
  };

  return (
    <Card className="space-y-5">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <SectionTitle>Updates</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            uzi periodically asks GitHub for the latest{" "}
            <span className="font-medium text-fg">vtmocanu/uzi</span> release and reports whether this
            instance has fallen behind. This card is the only place the check&rsquo;s status is spelled
            out; the sidebar pip points here.
          </p>
        </div>
        {status && (
          <Button
            variant="secondary"
            size="sm"
            onClick={checkNow}
            disabled={checking || !status.release_check_enabled}
          >
            {checking ? "Checking…" : "Check now"}
          </Button>
        )}
      </div>

      {(error || loadError) && <Alert message={error || loadError} />}

      {loading ? (
        <Skeleton className="h-24 w-full" />
      ) : !status ? (
        // Initial load failed (status never populated). Show the error (rendered
        // above) plus a retry path instead of a permanent skeleton — the "Check
        // now" button is gated on `status`, so this is the only way back.
        <div className="space-y-3">
          {!error && !loadError && <Alert message="Failed to load release-check status." />}
          <Button variant="secondary" size="sm" onClick={() => void reload()} disabled={loading}>
            Retry
          </Button>
        </div>
      ) : (
        <>
          {/* Security callout — the loudest thing this card does, per the mockup. */}
          {status.security && (
            <Alert
              tone="danger"
              message={`${displayVersion(status.latest_tag ?? "")} is flagged as a security release — update at the next opportunity.`}
            />
          )}

          {/* Status-in-words for the non-ok states — the air-gap / rate-limited / never-run cases. */}
          {status.status === "disabled" && (
            <div className="rounded-xl border border-edge bg-raised/40 p-4 text-sm text-muted">
              Update checks are turned off (master toggle below). While off, uzi never contacts
              github.com — the air-gap / privacy state.
            </div>
          )}
          {status.status === "never" && (
            <div className="rounded-xl border border-edge bg-raised/40 p-4 text-sm text-muted">
              No release check has run yet. Use <span className="font-medium text-fg">Check now</span> to
              run one.
            </div>
          )}
          {status.status === "error" && (
            <Alert
              tone="warning"
              message={`Release check unavailable${status.message ? ` — ${status.message}` : " — the last check failed (rate-limited or unreachable)."}`}
            />
          )}

          {/* Version delta — always shows the running version; the arrow + latest when a check ran. */}
          {status.status === "ok" && (
            <div className="space-y-3 rounded-xl border border-edge bg-raised/40 p-4">
              <div className="flex flex-wrap items-center gap-3">
                <span className="font-mono text-xl font-semibold text-fg">
                  {displayVersion(status.running_version)}
                </span>
                {status.update_available && status.latest_tag && (
                  <>
                    <span className="text-muted">&rarr;</span>
                    <span
                      className={`font-mono text-xl font-semibold ${
                        status.security ? "text-danger" : "text-brand"
                      }`}
                    >
                      {displayVersion(status.latest_tag)}
                    </span>
                  </>
                )}
                {deltaBadge(status)}
              </div>

              {status.update_available ? (
                <>
                  {(status.latest_name || status.published_at) && (
                    <p className="text-sm text-muted">
                      {status.latest_name && (
                        <span className="font-medium text-fg">{status.latest_name}</span>
                      )}
                      {status.latest_name && status.published_at && " · "}
                      {status.published_at && `released ${formatDay(status.published_at) ?? status.published_at}`}
                    </p>
                  )}

                  {releaseExcerpt(status.body) && (
                    <p className="whitespace-pre-wrap text-sm text-muted">{releaseExcerpt(status.body)}</p>
                  )}

                  {status.notes_url && isHttpsUrl(status.notes_url) && (
                    <a
                      href={status.notes_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="text-sm text-info hover:underline"
                    >
                      Full notes on GitHub &#8599;
                    </a>
                  )}

                  {/* Copyable upgrade runbook. */}
                  <div className="relative rounded-lg border border-edge bg-ink p-3">
                    <button
                      type="button"
                      onClick={copyRunbook}
                      className="absolute right-2 top-2 rounded-md border border-edge bg-raised px-2 py-0.5 text-[11px] text-muted hover:text-fg"
                    >
                      {copied ? "Copied" : "Copy"}
                    </button>
                    <pre className="overflow-x-auto whitespace-pre font-mono text-xs leading-relaxed text-fg">
                      {UPGRADE_RUNBOOK}
                    </pre>
                  </div>
                </>
              ) : (
                <p className="text-sm text-muted">You&rsquo;re running the latest release.</p>
              )}
            </div>
          )}

          {/* aria-live region: a "Check now" / toggle refresh mutates the text
              inside this persistent container, so a screen reader is notified of
              the async update. Single visible copy — no duplicate info. */}
          <div aria-live="polite">
            {status.checked_at && (
              <p className="text-xs text-faint">Checked {formatAgo(status.checked_at)}.</p>
            )}
          </div>

          {/* The two runtime toggles — persisted through the settings string-space. */}
          <div className="space-y-3 border-t border-edge pt-4">
            <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={status.release_check_enabled}
                disabled={savingToggle !== null}
                onChange={(e) => void saveToggle("enabled", e.target.checked)}
                className="h-4 w-4 rounded border-edge accent-brand"
              />
              Enable update checks (contact github.com for the latest release)
            </label>
            <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={status.release_check_banner_enabled}
                disabled={savingToggle !== null}
                onChange={(e) => void saveToggle("banner", e.target.checked)}
                className="h-4 w-4 rounded border-edge accent-brand"
              />
              Show the escalation banner for far-behind / security releases
            </label>
          </div>
        </>
      )}
    </Card>
  );
}
