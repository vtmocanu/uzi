// Settings → Notifications (PRD #25 M3): the current user's own Slack linking.
// Shows the link status (unlinked / pending confirmation / confirmed), a per-user
// enable toggle (default on), a manual member-ID override for when the email
// auto-match misses, and a self-test DM. All writes are self-scoped — a user can
// only ever touch their own mapping.

import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type SlackLink } from "../lib/api";
import { Alert, Badge, type BadgeTone, Button, Card, Field, Input, SectionTitle, Skeleton } from "./ui";

const STATE_META: Record<SlackLink["state"], { label: string; tone: BadgeTone }> = {
  unlinked: { label: "Not linked", tone: "neutral" },
  pending: { label: "Pending confirmation", tone: "warning" },
  confirmed: { label: "Linked", tone: "ok" },
};

export function SlackNotifications() {
  const [link, setLink] = useState<SlackLink | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [override, setOverride] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const load = useCallback(async () => {
    try {
      const { slack } = await api.getMySlack();
      setLink(slack);
      setOverride(slack.member_id ?? "");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load notification settings");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const toggleNotify = async (notify: boolean) => {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const { slack } = await api.setMySlackNotify(notify);
      setLink(slack);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to update notifications");
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
      setError(err instanceof ApiError ? err.message : "Failed to save the override");
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
      setError(err instanceof ApiError ? err.message : "Failed to send the test DM");
    } finally {
      setBusy(false);
    }
  };

  const overrideDirty = link !== null && override.trim() !== (link.member_id ?? "");

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Notifications</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Get a Slack DM when a run you own starts, needs your approval, finishes with a merge
          request, or fails. uzi matches you by your account email; if that misses, set your Slack
          member ID below. Notifications only start once you confirm the link in Slack.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      {loading || link === null ? (
        <Skeleton className="h-24 w-full" />
      ) : (
        <>
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

          <label className="flex items-center gap-3 text-sm">
            <input
              type="checkbox"
              className="h-4 w-4 accent-brand"
              checked={link.notify}
              disabled={busy}
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
                disabled={busy}
                onChange={(e) => setOverride(e.target.value)}
              />
            </Field>
            <div className="flex flex-wrap gap-2">
              <Button type="button" disabled={busy || !overrideDirty} onClick={saveOverride}>
                Save override
              </Button>
              <Button
                type="button"
                variant="secondary"
                disabled={busy || !link.resolved_id}
                onClick={sendTest}
              >
                Send test DM
              </Button>
            </div>
          </div>
        </>
      )}
    </Card>
  );
}
