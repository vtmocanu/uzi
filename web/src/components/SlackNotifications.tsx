// Settings → Notifications (PRD #25 M3): the current user's own Slack linking.
// Shows the link status (unlinked / pending confirmation / confirmed), a per-user
// enable toggle (default on), a manual member-ID override for when the email
// auto-match misses, and a self-test DM. All writes are self-scoped — a user can
// only ever touch their own mapping.

import { useState } from "react";
import { api, type SlackLink } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { Alert, Badge, type BadgeTone, Button, Card, Field, Input, SectionTitle, Skeleton } from "./ui";
import { DocLink } from "./DocLink";
import { DOC_SLACK } from "../lib/doclinks";

const STATE_META: Record<SlackLink["state"], { label: string; tone: BadgeTone }> = {
  unlinked: { label: "Not linked", tone: "neutral" },
  pending: { label: "Pending confirmation", tone: "warning" },
  confirmed: { label: "Linked", tone: "ok" },
};

// WORKSPACE_ALERT maps the server-derived Slack workspace state (PRD #56) to a
// non-blocking info banner that explains why the card behaves the way it does.
// "connected" carries no banner — the link-state helpers below take over. All
// three non-connected states use tone="info" (error is deliberately softer than
// tone="danger": a temporary outage, not a user-facing failure).
const WORKSPACE_ALERT: Record<Exclude<SlackLink["workspace"], "connected">, string> = {
  unconfigured:
    "Slack isn't connected on this uzi instance yet, so notifications can't be delivered. An admin can set it up under Admin Settings → Slack.",
  connecting: "Slack is reconnecting…",
  error: "Slack is temporarily unavailable — an admin can check Admin Settings → Slack.",
};

export function SlackNotifications() {
  const [link, setLink] = useState<SlackLink | null>(null);
  const [busy, setBusy] = useState(false);
  const [override, setOverride] = useState("");
  const [notice, setNotice] = useState("");

  // `link` is also written by the mutation handlers below (a save reflects the new
  // state without a refetch), so it stays local state and the fetcher sets it as a
  // side effect rather than being bundled into the hook's `data`.
  const { loading, error: loadError } = useAsyncData(
    async ({ isCurrent }) => {
      const { slack } = await api.getMySlack();
      if (!isCurrent()) return;
      setLink(slack);
      setOverride(slack.member_id ?? "");
    },
    [],
    { fallback: "Failed to load notification settings" },
  );
  const [error, setError] = useState("");

  const toggleNotify = async (notify: boolean) => {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const { slack } = await api.setMySlackNotify(notify);
      setLink(slack);
    } catch (err) {
      setError(errorMessage(err, "Failed to update notifications"));
    } finally {
      setBusy(false);
    }
  };

  const saveOverride = async () => {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const { slack } = await api.setMySlackOverride(override.trim() || null);
      setLink(slack);
      setOverride(slack.member_id ?? "");
      setNotice(
        slack.member_id
          ? "Override saved. Check Slack for a confirmation DM — notifications start once you confirm it."
          : "Override cleared. uzi will fall back to matching you by email.",
      );
    } catch (err) {
      setError(errorMessage(err, "Failed to save the override"));
    } finally {
      setBusy(false);
    }
  };

  const sendTest = async () => {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      await api.testMySlackDM();
      setNotice("Test DM sent. Check your Slack messages from uzi.");
    } catch (err) {
      setError(errorMessage(err, "Failed to send the test DM"));
    } finally {
      setBusy(false);
    }
  };

  const overrideDirty = link !== null && override.trim() !== (link.member_id ?? "");
  // When Slack was never configured on this instance, no control can do anything
  // useful, so every write path is disabled (the card stays visible). All other
  // workspace states leave the controls to their normal link-state rules.
  const controlsDisabled = link?.workspace === "unconfigured";

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Notifications</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Get a Slack DM when a run you own starts, needs your approval, finishes with a merge
          request, or fails. uzi matches you by your account email; if that misses, set your Slack
          member ID below. Notifications only start once you confirm the link in Slack.{" "}See the{" "}
          <DocLink slug={DOC_SLACK}>Slack notifications</DocLink> guide.
        </p>
      </div>

      {(error || loadError) && <Alert message={error || loadError} />}
      {notice && <Alert tone="success" message={notice} />}

      {loading || link === null ? (
        <Skeleton className="h-24 w-full" />
      ) : (
        <>
          {link.workspace !== "connected" && (
            <Alert tone="info" message={WORKSPACE_ALERT[link.workspace]} />
          )}

          <div className="flex flex-wrap items-center gap-2 text-sm">
            <span className="text-muted">Link status</span>
            <Badge tone={STATE_META[link.state].tone} dot>
              {STATE_META[link.state].label}
            </Badge>
            {link.resolved_id && <code className="rounded bg-raised px-1 py-0.5 text-faint">{link.resolved_id}</code>}
          </div>

          {link.state === "unlinked" && (
            <p className="text-sm text-muted">
              No Slack account resolved yet. If uzi can't match your email, paste your Slack member ID
              below (Slack profile → ⋯ → Copy member ID).
            </p>
          )}

          {link.state === "pending" && link.workspace === "connected" && (
            <p className="text-sm text-muted">
              Check Slack for a confirmation DM from uzi — notifications start once you press Confirm.
            </p>
          )}

          <label className="flex items-center gap-3 text-sm">
            <input
              type="checkbox"
              className="h-4 w-4 accent-brand"
              checked={link.notify}
              disabled={busy || controlsDisabled}
              onChange={(e) => toggleNotify(e.target.checked)}
            />
            <span className="text-fg">Send me Slack notifications about my runs</span>
          </label>

          <div className="space-y-3">
            <Field label="Slack member ID override" htmlFor="slack-override">
              <Input
                id="slack-override"
                placeholder="e.g. U0123ABCD — leave blank to match by email"
                value={override}
                disabled={busy || controlsDisabled}
                onChange={(e) => setOverride(e.target.value)}
              />
            </Field>
            <div className="flex flex-wrap gap-2">
              <Button type="button" disabled={busy || !overrideDirty || controlsDisabled} onClick={saveOverride}>
                Save override
              </Button>
              <Button
                type="button"
                variant="secondary"
                disabled={busy || !link.resolved_id || controlsDisabled}
                onClick={sendTest}
              >
                Send test DM
              </Button>
            </div>
            {link.state === "unlinked" && link.workspace === "connected" && (
              <p className="text-sm text-muted">
                Test DMs become available once uzi resolves your Slack account — by email match or the
                override above.
              </p>
            )}
          </div>
        </>
      )}
    </Card>
  );
}
