// SidebarUsageLimits is the usage micro-meters under the signed-in user block (PRD
// #1653 D-W2): ONE list of accounts, the shown Claude tokens first, then the shown
// Codex accounts, each in server order (default first). It replaces the two stacked
// per-provider blocks (PRD #53 / #104 for Claude, PRD #1209 M3 for Codex), which named
// accounts under two different schemes.
//
// Every account gets a header row, the provider logo then the account's own name,
// ALWAYS, however many accounts are shown: adding a second account never renames the
// first. No provider word is drawn; the logo's `title` names the provider on hover and
// the account's group carries it in its accessible name ("Claude account team").
//
// WHICH accounts ride the rail is the USER'S choice, not a heuristic, and is unchanged
// from the per-provider blocks: the default token / default Codex account always, plus
// any account the user checked "Show in sidebar and TUI" on in Settings
// (UserSettings.sidebar_token_ids / sidebar_codex_account_ids). Everything else stays
// reachable through ONE "+N more accounts in Settings" link, N counting both providers.
// The rail does not self-escalate when an unchecked account runs hot; the app-wide
// RateLimitAnnouncer still announces Claude tone crossings for every token.
//
// Hidden while loading and when neither provider has a readable account (no dead
// chrome). Only readable accounts count: a Claude token with limits.status "ok", a
// Codex account whose status carries a reading (fresh/stale). A stale reading is dimmed.

import { useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { api, type CodexAccountRateLimit, type TokenRateLimits } from "../lib/api";
import { isShownInSidebar, onSidebarTokensChanged } from "../lib/sidebarTokens";
import {
  codexAccountLabel,
  hasCodexMicroReading,
  hasCodexReading,
  isCodexAccountShownInSidebar,
} from "../lib/codexRateLimits";
import { useNow } from "../lib/rateLimits";
import { TokenMicroMeters, useMyRateLimits } from "./RateLimitMeters";
import { CodexAccountMicroMeters, useMyCodexRateLimits } from "./CodexRateLimitMeters";
import { ClaudeIcon, OpenAIIcon } from "./icons";

// useSidebarSelection is the user's chosen extras for both providers, fetched on mount
// and again on the Settings toggle's change event, so a save on the Settings page
// reaches this separate mount immediately.
function useSidebarSelection(): { tokenIds: string[]; codexAccountIds: string[] } {
  const [tokenIds, setTokenIds] = useState<string[]>([]);
  const [codexAccountIds, setCodexAccountIds] = useState<string[]>([]);
  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .getMySettings()
        .then(({ settings }) => {
          if (!alive) return;
          setTokenIds(settings.sidebar_token_ids ?? []);
          setCodexAccountIds(settings.sidebar_codex_account_ids ?? []);
        })
        // A failed fetch keeps the last known set — degrading to default-only on a
        // transient error would look like the user's choice being reverted.
        .catch(() => {});
    load();
    const off = onSidebarTokensChanged(load);
    return () => {
      alive = false;
      off();
    };
  }, []);
  return { tokenIds, codexAccountIds };
}

// AccountGroup is one account in the list: a role="group" named "<Provider> account
// <name>", holding the header row (16px logo + the name in the eyebrow style) and the
// account's meter rows.
function AccountGroup({
  provider,
  name,
  children,
}: {
  provider: "Claude" | "Codex";
  name: string;
  children: ReactNode;
}) {
  return (
    <div role="group" aria-label={`${provider} account ${name}`} className="space-y-1.5">
      <div className="flex min-w-0 items-center gap-1.5">
        <span title={provider} className="flex h-4 w-4 flex-none items-center justify-center">
          {provider === "Claude" ? (
            <ClaudeIcon className="h-4 w-4" />
          ) : (
            <OpenAIIcon className="h-4 w-4 text-fg" />
          )}
        </span>
        <span className="min-w-0 truncate text-[10px] font-medium uppercase tracking-wide text-faint">
          {name}
        </span>
      </div>
      {children}
    </div>
  );
}

export function SidebarUsageLimits() {
  const { tokens } = useMyRateLimits(60_000);
  const { accounts } = useMyCodexRateLimits(60_000);
  const now = useNow();
  const { tokenIds, codexAccountIds } = useSidebarSelection();

  const readableTokens: TokenRateLimits[] = (tokens ?? []).filter((t) => t.limits.status === "ok");
  const readableAccounts: CodexAccountRateLimit[] = (accounts ?? []).filter((a) =>
    hasCodexReading(a.status),
  );
  if (readableTokens.length === 0 && readableAccounts.length === 0) return null;

  const shownTokens = readableTokens.filter((t) => isShownInSidebar(t, tokenIds));
  const shownAccounts = readableAccounts.filter((a) => isCodexAccountShownInSidebar(a, codexAccountIds));
  const hidden =
    readableTokens.length - shownTokens.length + (readableAccounts.length - shownAccounts.length);
  // A shown Codex account with no window reading (a fresh/stale status whose buckets
  // report no percentage) would be a header over no rows: it is not drawn. It still
  // counts as shown, not hidden, since the user did choose it.
  const drawnAccounts = shownAccounts.filter(hasCodexMicroReading);
  if (shownTokens.length === 0 && drawnAccounts.length === 0 && hidden === 0) return null;

  return (
    <div className="mt-2 space-y-1.5" aria-label="Usage limits">
      <div className="space-y-2.5">
        {shownTokens.map((t) => (
          <AccountGroup key={`claude:${t.secret_id}`} provider="Claude" name={t.label}>
            <TokenMicroMeters token={t} now={now} />
          </AccountGroup>
        ))}
        {drawnAccounts.map((a) => (
          <AccountGroup key={`codex:${a.account_id}`} provider="Codex" name={codexAccountLabel(a)}>
            <CodexAccountMicroMeters account={a} now={now} />
          </AccountGroup>
        ))}
      </div>
      {hidden > 0 && (
        <Link
          to="/settings"
          className="block text-[11px] font-medium text-faint transition-colors hover:text-fg"
        >
          +{hidden} more account{hidden === 1 ? "" : "s"} in Settings
        </Link>
      )}
    </div>
  );
}
