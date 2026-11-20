// Admin → Workers: the worker sizes editor (PRD #4213) and the worker-fleet controls that
// used to sit on Admin → Instance (hosted worker quota, custody hold limit, ephemeral workers, worker hibernation,
// Docker worker repo allowlist, capability-aware scheduling). They are the same
// DB-backed app_settings cards; only their home moved. Old /admin/settings#<id> links
// are redirected here by AdminSettings. Admin-only (AdminRoute + the admin-only API).

import { useLocation } from "react-router-dom";
import { useCallback, useEffect, useRef, useState } from "react";
import { api, type AdminWorkerSizes, type AppSettings, type SettingSource, type SettingsResponse } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { Alert, Skeleton } from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import { SectionPills } from "../components/SectionPills";
import { ADMIN_WORKERS_ANCHOR_IDS } from "../components/navSections";
import { DocLink } from "../components/DocLink";
import { DOC_ADMIN_SETTINGS } from "../lib/doclinks";
import { DockerAllowlistCard } from "./adminSettings/DockerAllowlistCard";
import { HostedWorkerQuotaCard } from "./adminSettings/HostedWorkerQuotaCard";
import { CustodyHoldLimitCard } from "./adminSettings/CustodyHoldLimitCard";
import { EphemeralWorkersCard } from "./adminSettings/EphemeralWorkersCard";
import { WorkerHibernationCard } from "./adminSettings/WorkerHibernationCard";
import { CapabilitySchedulingCard } from "./adminSettings/CapabilitySchedulingCard";
import { WorkerSizesCard } from "./adminSettings/WorkerSizesCard";

export function AdminWorkers() {
  const { hash, key } = useLocation();
  const [saved, setSaved] = useState<AppSettings | null>(null);
  const [sources, setSources] = useState<Record<string, SettingSource>>({});
  const [defaults, setDefaults] = useState<Record<string, string> | undefined>(undefined);

  // Same load shape as AdminSettings: every card save returns the full settings map.
  const applyResponse = useCallback((resp: SettingsResponse) => {
    setSaved(resp.settings);
    setSources(resp.sources ?? {});
    setDefaults(resp.defaults);
  }, []);

  // The worker sizes (PRD #4213) are read once here and re-read whenever the sizes editor
  // asks (after each successful write, on Retry and after some refused writes), then
  // handed to the sizes editor AND the two default-size selectors, so an edited quantity
  // shows in those selectors at once. A failed read leaves the last good list.
  const [sizes, setSizes] = useState<AdminWorkerSizes | null>(null);
  const [sizesError, setSizesError] = useState("");
  const mounted = useRef(true);
  const loadSizes = useCallback(async () => {
    try {
      const next = await api.getAdminWorkerSizes();
      if (!mounted.current) return;
      setSizes(next);
      setSizesError("");
    } catch (err) {
      if (mounted.current) setSizesError(errorMessage(err, "Failed to load worker sizes"));
    }
  }, []);
  useEffect(() => {
    mounted.current = true;
    void loadSizes();
    return () => {
      mounted.current = false;
    };
  }, [loadSizes]);
  const sizeOptions = sizes?.sizes;

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
  // Page-owned refs shared with SectionPills: the end sentinel, the deep-link target pending its
  // highlight, and the current-section evaluator the deep-link effect re-runs after scrolling.
  const endRef = useRef<HTMLDivElement>(null);
  const pendingRef = useRef<string | null>(null);
  const evaluateRef = useRef<(() => void) | null>(null);
  useEffect(() => {
    // SPA navigation keeps the previous page's scroll position, so a failed load that
    // arrived via an advertised anchor resets to the top itself.
    if (failed && ADMIN_WORKERS_ANCHOR_IDS.has(hash.slice(1))) window.scrollTo?.(0, 0);
  }, [failed, hash, key]);
  useEffect(() => {
    // Every card renders only after a successful load, so deep links scroll once it settles.
    if (loaded && ADMIN_WORKERS_ANCHOR_IDS.has(hash.slice(1))) {
      // Near the page end the scroll cannot bring the target to the line: remember it so
      // evaluate() lights its own pill.
      const id = hash.slice(1);
      // The section pills mount with this load, so the pinned block grows in the same
      // commit and its ResizeObserver rewrites --pinned-h (the root scroll-padding) only
      // after the next layout. Scroll two frames later, once that offset is current, so
      // the card is not left under the pill row; then evaluate once (a page already at
      // its end does not move, so no observer fires).
      let raf2 = 0;
      const raf = requestAnimationFrame(() => {
        raf2 = requestAnimationFrame(() => {
          // Set right before the jump: an observer evaluation during the delay clears it.
          pendingRef.current = id;
          document.getElementById(id)?.scrollIntoView?.({ block: "start" });
          evaluateRef.current?.();
        });
      });
      return () => {
        cancelAnimationFrame(raf);
        cancelAnimationFrame(raf2);
      };
    }
  }, [loaded, hash, key]);

  const sections: { id: string; label: string }[] = loaded
    ? [
        { id: "docker-allowlist", label: "Docker workers" },
        { id: "worker-sizes", label: "Worker sizes" },
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
      pinned={
        /* The section index, pinned under the tabs as one non-wrapping row, as on
          Admin → Instance. Shown only after a successful load. */
        sections.length > 1 ? (
          <SectionPills
            sections={sections}
            anchorIds={ADMIN_WORKERS_ANCHOR_IDS}
            endRef={endRef}
            pendingRef={pendingRef}
            evaluateRef={evaluateRef}
          />
        ) : undefined
      }
    >
      {loadError && <Alert message={loadError} />}

      {loading && (
        <div className="space-y-4">
          <Skeleton className="h-16 w-full" />
          <Skeleton className="h-16 w-full" />
        </div>
      )}

      {loaded && saved && (
        <>
          <section id="docker-allowlist" className="scroll-mt-6">
            <DockerAllowlistCard settings={saved} sources={sources} onSaved={applyResponse} />
          </section>
          <section id="worker-sizes" className="scroll-mt-6">
            <WorkerSizesCard data={sizes} loadError={sizesError} onChanged={loadSizes} />
          </section>
          <section id="hosted-worker-quota" className="scroll-mt-6">
            <HostedWorkerQuotaCard settings={saved} sources={sources} defaults={defaults} sizeOptions={sizeOptions} onSaved={applyResponse} />
          </section>
          <section id="custody-hold-limit" className="scroll-mt-6">
            <CustodyHoldLimitCard settings={saved} onSaved={applyResponse} />
          </section>
          <section id="ephemeral-workers" className="scroll-mt-6">
            <EphemeralWorkersCard settings={saved} sources={sources} defaults={defaults} sizeOptions={sizeOptions} onSaved={applyResponse} />
          </section>
          <section id="worker-hibernation" className="scroll-mt-6">
            <WorkerHibernationCard settings={saved} onSaved={applyResponse} />
          </section>
          <section id="capability-scheduling" className="scroll-mt-6">
            <CapabilitySchedulingCard settings={saved} sources={sources} onSaved={applyResponse} />
          </section>
        </>
      )}

      {/* End sentinel for the section highlight: zero height, observed while the page end is in view. */}
      <div ref={endRef} aria-hidden="true" className="h-0" />
    </AdminShell>
  );
}
