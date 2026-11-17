// Admin → Workers: the worker-fleet controls that used to sit on Admin → Instance
// (hosted worker quota, custody hold limit, ephemeral workers, worker hibernation,
// Docker worker repo allowlist, capability-aware scheduling). They are the same
// DB-backed app_settings cards; only their home moved. Old /admin/settings#<id> links
// are redirected here by AdminSettings. Admin-only (AdminRoute + the admin-only API).

import { useLocation } from "react-router-dom";
import { useCallback, useEffect, useState } from "react";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../lib/api";
import { useAsyncData } from "../lib/useAsyncData";
import { Alert } from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import { ADMIN_WORKERS_ANCHOR_IDS } from "../components/navSections";
import { DocLink } from "../components/DocLink";
import { DOC_ADMIN_SETTINGS } from "../lib/doclinks";
import { DockerAllowlistCard } from "./adminSettings/DockerAllowlistCard";
import { HostedWorkerQuotaCard } from "./adminSettings/HostedWorkerQuotaCard";
import { CustodyHoldLimitCard } from "./adminSettings/CustodyHoldLimitCard";
import { EphemeralWorkersCard } from "./adminSettings/EphemeralWorkersCard";
import { WorkerHibernationCard } from "./adminSettings/WorkerHibernationCard";
import { CapabilitySchedulingCard } from "./adminSettings/CapabilitySchedulingCard";

export function AdminWorkers() {
  const { hash, key } = useLocation();
  const [saved, setSaved] = useState<AppSettings | null>(null);
  const [sources, setSources] = useState<Record<string, SettingSource>>({});

  // Same load shape as AdminSettings: every card save returns the full settings map.
  const applyResponse = useCallback((resp: SettingsResponse) => {
    setSaved(resp.settings);
    setSources(resp.sources ?? {});
  }, []);

  const { loading, error: loadError } = useAsyncData(
    async ({ isCurrent }) => {
      const resp = await api.getSettings();
      if (isCurrent()) applyResponse(resp);
    },
    [applyResponse],
    { fallback: "Failed to load settings" },
  );

  // Keyed on "loaded", not the `saved` object: a card save replaces `saved` and must not
  // yank the page back to the URL's anchor.
  const loaded = !loading && saved !== null;
  const failed = !loading && saved === null;
  useEffect(() => {
    // SPA navigation keeps the previous page's scroll position, so a failed load that
    // arrived via an advertised anchor resets to the top itself.
    if (failed && ADMIN_WORKERS_ANCHOR_IDS.has(hash.slice(1))) window.scrollTo?.(0, 0);
  }, [failed, hash, key]);
  useEffect(() => {
    // Every card renders only after a successful load, so deep links scroll once it settles.
    if (loaded && ADMIN_WORKERS_ANCHOR_IDS.has(hash.slice(1))) {
      document.getElementById(hash.slice(1))?.scrollIntoView?.({ block: "start" });
    }
  }, [loaded, hash, key]);

  const sections: { id: string; label: string }[] = loaded
    ? [
        { id: "docker-allowlist", label: "Docker workers" },
        { id: "hosted-worker-quota", label: "Hosted worker quota" },
        { id: "custody-hold-limit", label: "Custody hold limit" },
        { id: "ephemeral-workers", label: "Ephemeral workers" },
        { id: "worker-hibernation", label: "Worker hibernation" },
        { id: "capability-scheduling", label: "Capability scheduling" },
      ]
    : [];

  return (
    <AdminShell
      description={
        <>
          Worker fleet controls shared across every user of this uzi instance. See the{" "}
          <DocLink slug={DOC_ADMIN_SETTINGS}>admin settings</DocLink> guide.
        </>
      }
    >
      {sections.length > 1 && (
        <nav aria-label="Sections on this page" className="flex flex-wrap gap-1.5">
          {sections.map((s) => (
            <a
              key={s.id}
              href={`#${s.id}`}
              className="rounded-full border border-edge px-2.5 py-1 text-xs font-medium text-muted transition-colors hover:border-edge-strong hover:text-fg"
            >
              {s.label}
            </a>
          ))}
        </nav>
      )}

      {loadError && <Alert message={loadError} />}

      {loaded && saved && (
        <>
          <section id="docker-allowlist" className="scroll-mt-6">
            <DockerAllowlistCard settings={saved} sources={sources} onSaved={applyResponse} />
          </section>
          <section id="hosted-worker-quota" className="scroll-mt-6">
            <HostedWorkerQuotaCard settings={saved} onSaved={applyResponse} />
          </section>
          <section id="custody-hold-limit" className="scroll-mt-6">
            <CustodyHoldLimitCard settings={saved} onSaved={applyResponse} />
          </section>
          <section id="ephemeral-workers" className="scroll-mt-6">
            <EphemeralWorkersCard settings={saved} onSaved={applyResponse} />
          </section>
          <section id="worker-hibernation" className="scroll-mt-6">
            <WorkerHibernationCard settings={saved} onSaved={applyResponse} />
          </section>
          <section id="capability-scheduling" className="scroll-mt-6">
            <CapabilitySchedulingCard settings={saved} sources={sources} onSaved={applyResponse} />
          </section>
        </>
      )}
    </AdminShell>
  );
}
