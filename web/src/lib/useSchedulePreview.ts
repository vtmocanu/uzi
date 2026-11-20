// Debounced, latest-wins fetch of the schedule-settings preview (issue #3260).

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { api } from "./api";
import type { ScheduleSettingsPreview, ScheduleSettingsPreviewRequest } from "./apiTypes";
import { schedulePreviewKey, schedulePreviewView, type SchedulePreviewView } from "./schedulePreview";

export interface SchedulePreviewResult {
  status: "idle" | "loading" | "ready" | "error";
  preview: ScheduleSettingsPreview | null;
  view: SchedulePreviewView | null;
}

interface Published {
  key: string;
  error: boolean;
  preview: ScheduleSettingsPreview | null;
}

const IDLE: SchedulePreviewResult = { status: "idle", preview: null, view: null };
const LOADING: SchedulePreviewResult = { status: "loading", preview: null, view: null };
const ERROR: SchedulePreviewResult = { status: "error", preview: null, view: null };

// useSchedulePreview: a null request is idle. A key change (request or refreshKey) shows
// loading at once, and a result is exposed only while its key is the current key. The request is
// sent after debounceMs through setTimeout (also when 0, so a burst of renders in one tick
// coalesces); the timer and any in-flight response are dropped on cleanup, so only the latest
// key publishes.
export function useSchedulePreview(
  request: ScheduleSettingsPreviewRequest | null,
  opts: { debounceMs?: number; refreshKey?: string } = {},
): SchedulePreviewResult {
  const debounceMs = opts.debounceMs ?? 0;
  const key = request ? `${schedulePreviewKey(request)}|${opts.refreshKey ?? ""}` : null;
  const requestRef = useRef(request);
  const currentKey = useRef<string | null>(key);
  const [published, setPublished] = useState<Published | null>(null);

  useEffect(() => {
    requestRef.current = request;
  });

  useLayoutEffect(() => {
    currentKey.current = key;
  }, [key]);

  // Relies on the requestRef effect above having run first: passive effects run in declaration
  // order, so requestRef.current is already this render's request here.
  useEffect(() => {
    const req = requestRef.current;
    if (key === null || !req) return;
    let live = true;
    // A fresh fetch starts: whatever was published (even under this same key) is no longer shown.
    setPublished((p) => (p === null ? p : null));
    const timer = setTimeout(() => {
      void Promise.resolve()
        .then(() => api.previewScheduleSettings(req))
        .then(
          (preview) => ({ error: false, preview }),
          () => ({ error: true, preview: null }),
        )
        .then((r) => {
          if (!live || currentKey.current !== key) return;
          setPublished({ key, error: r.error, preview: r.preview });
        });
    }, debounceMs);
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [key, debounceMs]);

  const visible = published && key !== null && published.key === key ? published : null;
  const view = useMemo(() => (visible?.preview ? schedulePreviewView(visible.preview) : null), [visible]);
  if (key === null) return IDLE;
  if (!visible) return LOADING;
  if (visible.error || !visible.preview) return ERROR;
  return { status: "ready", preview: visible.preview, view };
}
