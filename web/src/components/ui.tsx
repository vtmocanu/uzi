// uzi's primitive kit. Deliberately small (one file, zero dependencies) but
// shaped after multica's packages/ui: a variant/size matrix on Button
// (ui/button.tsx), a soft destructive style, an Empty component
// (ui/empty.tsx), a Skeleton (ui/skeleton.tsx), a fixed-width unicode Spinner
// (common/unicode-spinner.tsx), and a unified PageHeader
// (views/layout/page-header.tsx + breadcrumb-header.tsx).

import { useEffect, useState, type ButtonHTMLAttributes, type InputHTMLAttributes, type ReactNode, type SelectHTMLAttributes, type TextareaHTMLAttributes } from "react";
import { Link } from "react-router-dom";
import { ArrowLeftIcon } from "./icons";

export function cx(...parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(" ");
}

// ── Surfaces ─────────────────────────────────────────────────────────────────

export function Card({ children, className = "" }: { children: ReactNode; className?: string }) {
  return (
    <div className={cx("rounded-xl border border-edge bg-surface p-5", className)}>{children}</div>
  );
}

// SectionTitle is the small-caps card heading used across settings/detail pages.
export function SectionTitle({ children, className = "" }: { children: ReactNode; className?: string }) {
  return (
    <h2 className={cx("text-xs font-semibold uppercase tracking-wider text-faint", className)}>
      {children}
    </h2>
  );
}

// ── Buttons ──────────────────────────────────────────────────────────────────

type ButtonVariant = "primary" | "secondary" | "ghost" | "danger" | "dangerSolid";
type ButtonSize = "sm" | "md";

// Variant language follows multica's ui/button.tsx: a single loud primary, a
// bordered secondary, a quiet ghost, and a SOFT destructive
// (bg-destructive/10 text-destructive there; bg-danger/10 text-danger here) so
// routine delete/disable actions stop shouting. dangerSolid is reserved for
// confirmed-destructive moments.
const BUTTON_VARIANTS: Record<ButtonVariant, string> = {
  primary: "bg-brand text-on-brand hover:bg-brand-hover disabled:opacity-50",
  secondary:
    "border border-edge bg-raised text-fg hover:border-edge-strong hover:bg-raised/70 disabled:opacity-50",
  ghost: "text-muted hover:bg-raised hover:text-fg disabled:opacity-50",
  danger: "bg-danger/10 text-danger hover:bg-danger/20 disabled:opacity-50",
  dangerSolid: "bg-danger text-ink hover:bg-danger/80 disabled:opacity-50",
};

const BUTTON_SIZES: Record<ButtonSize, string> = {
  sm: "h-7 px-2.5 text-xs gap-1",
  md: "h-9 px-4 text-sm gap-1.5",
};

export function Button({
  children,
  variant = "primary",
  size = "md",
  className = "",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: ButtonVariant; size?: ButtonSize }) {
  return (
    <button
      className={cx(
        "inline-flex shrink-0 select-none items-center justify-center rounded-lg font-medium transition-colors disabled:cursor-not-allowed",
        BUTTON_VARIANTS[variant],
        BUTTON_SIZES[size],
        className,
      )}
      {...props}
    >
      {children}
    </button>
  );
}

// ── Forms ────────────────────────────────────────────────────────────────────

export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="block space-y-1.5">
      <span className="text-sm font-medium text-muted">{label}</span>
      {children}
    </label>
  );
}

// The brand-tinted border marks the focused field on any focus (incl. mouse);
// the keyboard-only ring comes from the global :focus-visible rule (index.css).
const INPUT_CLASS =
  "w-full rounded-lg border border-edge bg-raised px-3 py-2 text-sm text-fg placeholder:text-faint outline-none focus:border-brand/70";

export function Input(props: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={INPUT_CLASS} {...props} />;
}

export function Select(props: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select className={INPUT_CLASS} {...props} />;
}

export function Textarea({ className = "", ...props }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea className={cx(INPUT_CLASS, className)} {...props} />;
}

// ── Feedback ─────────────────────────────────────────────────────────────────

type AlertTone = "danger" | "success" | "warning" | "info";

const ALERT_TONES: Record<AlertTone, string> = {
  danger: "border-danger/40 bg-danger/10 text-danger",
  success: "border-ok/40 bg-ok/10 text-ok",
  warning: "border-warn/40 bg-warn/10 text-warn",
  info: "border-info/40 bg-info/10 text-info",
};

export function Alert({ message, tone = "danger" }: { message: string; tone?: AlertTone }) {
  return (
    <div role={tone === "danger" ? "alert" : "status"} className={cx("rounded-lg border px-3 py-2 text-sm", ALERT_TONES[tone])}>
      {message}
    </div>
  );
}

export type BadgeTone = "neutral" | "warning" | "danger" | "ok" | "info" | "brand";

const BADGE_TONES: Record<BadgeTone, string> = {
  neutral: "border-edge bg-raised text-muted",
  warning: "border-warn/40 bg-warn/10 text-warn",
  danger: "border-danger/40 bg-danger/10 text-danger",
  ok: "border-ok/40 bg-ok/10 text-ok",
  info: "border-info/40 bg-info/10 text-info",
  brand: "border-brand/40 bg-brand/10 text-brand",
};

export function Badge({
  children,
  tone = "neutral",
  title,
  dot = false,
}: {
  children: ReactNode;
  tone?: BadgeTone;
  title?: string;
  dot?: boolean;
}) {
  return (
    <span
      title={title}
      className={cx(
        "inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-[11px] font-medium",
        BADGE_TONES[tone],
      )}
    >
      {dot && <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-current" />}
      {children}
    </span>
  );
}

// StatusPill renders a run status with a colored dot (multica gives every
// status an icon color via issues/components/status-icon.tsx; the pill shape
// follows its chat/components/task-status-pill.tsx).
const RUN_STATUS_TONES: Record<string, { tone: BadgeTone; pulse?: boolean }> = {
  queued: { tone: "neutral" },
  claimed: { tone: "info" },
  running: { tone: "info", pulse: true },
  awaiting_approval: { tone: "warning", pulse: true },
  completed: { tone: "ok" },
  failed: { tone: "danger" },
  cancelled: { tone: "neutral" },
};

export function StatusPill({ status }: { status: string }) {
  const cfg = RUN_STATUS_TONES[status] ?? { tone: "neutral" as BadgeTone };
  return (
    <span
      className={cx(
        "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[11px] font-medium",
        BADGE_TONES[cfg.tone],
      )}
    >
      <span
        aria-hidden="true"
        className={cx("h-1.5 w-1.5 rounded-full bg-current", cfg.pulse && "animate-pulse")}
      />
      {status.replace(/_/g, " ")}
    </span>
  );
}

// Spinner: braille-frame inline spinner in a fixed-width monospace span, so
// frames never reflow neighbouring text — lifted straight from multica's
// UnicodeSpinner rationale (packages/ui/components/common/unicode-spinner.tsx).
const SPINNER_FRAMES = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];

export function Spinner({ className = "" }: { className?: string }) {
  const [frame, setFrame] = useState(0);
  useEffect(() => {
    const id = setInterval(() => setFrame((f) => (f + 1) % SPINNER_FRAMES.length), 80);
    return () => clearInterval(id);
  }, []);
  return (
    <span
      aria-hidden="true"
      className={cx("inline-block min-w-[1ch] text-center font-mono", className)}
    >
      {SPINNER_FRAMES[frame]}
    </span>
  );
}

// ── Loading & empty states ───────────────────────────────────────────────────

export function Skeleton({ className = "" }: { className?: string }) {
  return <div aria-hidden="true" className={cx("animate-pulse rounded-md bg-raised", className)} />;
}

// ListSkeleton: a card of shimmering rows — the standard page-loading state.
export function ListSkeleton({ rows = 4 }: { rows?: number }) {
  return (
    <Card className="space-y-3">
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="flex items-center gap-3">
          <Skeleton className="h-4 w-4 rounded-full" />
          <Skeleton className={i % 2 ? "h-4 w-2/5" : "h-4 w-3/5"} />
          <Skeleton className="ml-auto h-4 w-16" />
        </div>
      ))}
    </Card>
  );
}

// EmptyState: dashed container + icon + title + description + action, after
// multica's ui/empty.tsx (Empty/EmptyHeader/EmptyMedia/EmptyTitle slots,
// flattened into props at this app's scale).
export function EmptyState({
  icon,
  title,
  description,
  action,
}: {
  icon?: ReactNode;
  title: string;
  description?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="flex w-full flex-col items-center justify-center gap-3 rounded-xl border border-dashed border-edge p-8 text-center">
      {icon && (
        <div className="flex h-9 w-9 items-center justify-center rounded-lg bg-raised text-lg text-muted">
          {icon}
        </div>
      )}
      <div className="max-w-sm space-y-1">
        <p className="text-sm font-medium text-fg">{title}</p>
        {description && <p className="text-sm text-faint">{description}</p>}
      </div>
      {action}
    </div>
  );
}

// ── Page chrome ──────────────────────────────────────────────────────────────

// PageHeader: the one way a page opens — optional back-crumb, title, supporting
// line, right-aligned actions. Replaces the per-page hand-rolled headers that
// had drifted (exactly the drift multica's BreadcrumbHeader was built to kill —
// see its packages/views/layout/breadcrumb-header.tsx doc comment).
export function PageHeader({
  title,
  titleNode,
  description,
  actions,
  backTo,
  backLabel,
}: {
  title?: string;
  titleNode?: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  backTo?: string;
  backLabel?: string;
}) {
  return (
    <div className="space-y-1.5">
      {backTo && (
        <Link
          to={backTo}
          className="inline-flex items-center gap-1 text-xs font-medium text-faint transition-colors hover:text-fg"
        >
          <ArrowLeftIcon /> {backLabel ?? "Back"}
        </Link>
      )}
      <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
        <div className="min-w-0">
          {titleNode ?? <h1 className="text-xl font-semibold tracking-tight">{title}</h1>}
          {description && <p className="mt-1 max-w-2xl text-sm text-muted">{description}</p>}
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
    </div>
  );
}

// StatTile: dashboard number tile.
export function StatTile({
  label,
  value,
  hint,
  to,
}: {
  label: string;
  value: ReactNode;
  hint?: string;
  to?: string;
}) {
  const body = (
    <div className="rounded-xl border border-edge bg-surface p-4 transition-colors hover:border-edge-strong">
      <p className="text-xs font-medium uppercase tracking-wider text-faint">{label}</p>
      <p className="mt-1.5 text-2xl font-semibold tabular-nums text-fg">{value}</p>
      {hint && <p className="mt-0.5 text-xs text-faint">{hint}</p>}
    </div>
  );
  return to ? <Link to={to}>{body}</Link> : body;
}
