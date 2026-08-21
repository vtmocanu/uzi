// uzi's primitive kit. Deliberately small (one file, zero dependencies) but
// shaped after multica's packages/ui: a variant/size matrix on Button
// (ui/button.tsx), a soft destructive style, an Empty component
// (ui/empty.tsx), a Skeleton (ui/skeleton.tsx), a fixed-width unicode Spinner
// (common/unicode-spinner.tsx), and a unified PageHeader
// (views/layout/page-header.tsx + breadcrumb-header.tsx).

import {
  useEffect,
  useState,
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
} from "react";
import { Link } from "react-router-dom";
import { ArrowLeftIcon, EyeIcon, EyeOffIcon } from "./icons";

export function cx(...parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(" ");
}

// ── Surfaces ─────────────────────────────────────────────────────────────────

export function Card({
  children,
  className = "",
  id,
}: {
  children: ReactNode;
  className?: string;
  // Anchor target for in-page section indexes (Admin → Instance); pair it with a
  // scroll-mt-* class so a jump does not clip the card's title.
  id?: string;
}) {
  return (
    <div
      id={id}
      className={cx("rounded-xl border border-edge bg-surface p-5", className)}
    >
      {children}
    </div>
  );
}

// SectionTitle is the small-caps card heading used across settings/detail pages.
export function SectionTitle({
  children,
  className = "",
}: {
  children: ReactNode;
  className?: string;
}) {
  return (
    <h2
      className={cx(
        "text-xs font-semibold uppercase tracking-wider text-faint",
        className,
      )}
    >
      {children}
    </h2>
  );
}

// ── Buttons ──────────────────────────────────────────────────────────────────

type ButtonVariant =
  "primary" | "secondary" | "ghost" | "danger" | "dangerSolid";
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
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: ButtonVariant;
  size?: ButtonSize;
}) {
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

export function Field({
  label,
  htmlFor,
  children,
}: {
  label: string;
  htmlFor?: string;
  children: ReactNode;
}) {
  // When htmlFor is set the label references a single control by id (a
  // non-wrapping <label>), so a composite child like ModelSelect (a <select>
  // plus a conditional custom <input>) does not put two controls under one
  // wrapping label — which would pollute the select's accessible name. Without
  // htmlFor the label wraps its single child, the default for simple inputs.
  if (htmlFor) {
    return (
      <div className="space-y-1.5">
        <label
          htmlFor={htmlFor}
          className="block text-sm font-medium text-muted"
        >
          {label}
        </label>
        {children}
      </div>
    );
  }
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
  "w-full rounded-lg border border-edge bg-raised px-3 py-2 text-sm text-fg placeholder:text-faint outline-hidden focus:border-brand/70";

export function Input({
  className = "",
  ...props
}: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={cx(INPUT_CLASS, className)} {...props} />;
}

// A masked secret input with a reveal (eye) toggle. Reusable: forge PAT today,
// the other type="password" sites later (PRD #337 Feature B, D6). Defaults to
// masked; the toggle button never submits the form (type="button"), carries a
// toggling aria-label + aria-pressed, and forwards `id` to the inner input so a
// Field htmlFor associates the label with the input, not the composite. The
// focus-visible outline gives the icon-button its own visible keyboard ring
// (the inner Input keeps the global :focus-visible ring from index.css).
export function PasswordInput({
  id,
  value,
  className = "",
  ...props
}: InputHTMLAttributes<HTMLInputElement>) {
  const [revealed, setRevealed] = useState(false);
  // Re-mask when the value is externally cleared (e.g. setToken("") after a
  // successful connect), so a revealed field never shows the NEXT pasted token
  // in clear (D7 leak guard).
  useEffect(() => {
    if (value === "") setRevealed(false);
  }, [value]);
  return (
    <div className="relative">
      <Input
        id={id}
        type={revealed ? "text" : "password"}
        value={value}
        className={cx("pr-10", className)}
        {...props}
      />
      <button
        type="button"
        onClick={() => setRevealed((r) => !r)}
        aria-label={revealed ? "Hide token" : "Show token"}
        aria-pressed={revealed}
        className="absolute inset-y-0 right-0 flex items-center rounded-r-lg px-3 text-muted outline-hidden hover:text-fg focus-visible:outline-2 focus-visible:outline-brand"
      >
        {revealed ? <EyeOffIcon /> : <EyeIcon />}
      </button>
    </div>
  );
}

export function Select({
  className = "",
  ...props
}: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select className={cx(INPUT_CLASS, className)} {...props} />;
}

export function Textarea({
  className = "",
  ...props
}: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea className={cx(INPUT_CLASS, className)} {...props} />;
}

// Toggle: a switch input (on/off), the pill affordance the mock's per-row
// pause/resume and the modal's option toggles reuse (PRD #241). Renders as a real
// <button role="switch"> so it is keyboard-operable and screen-reader legible; the
// visual thumb slides on `checked`. `label` is the accessible name.
export function Toggle({
  checked,
  onChange,
  disabled = false,
  label,
  title,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  disabled?: boolean;
  label: string;
  title?: string;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      title={title ?? label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cx(
        "relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors disabled:cursor-not-allowed disabled:opacity-50",
        checked ? "bg-brand" : "bg-edge-strong",
      )}
    >
      <span
        aria-hidden="true"
        className={cx(
          "inline-block h-3.5 w-3.5 rounded-full bg-white transition-transform",
          checked ? "translate-x-[18px]" : "translate-x-[3px]",
        )}
      />
    </button>
  );
}

// ── Feedback ─────────────────────────────────────────────────────────────────

type AlertTone = "danger" | "success" | "warning" | "info";

const ALERT_TONES: Record<AlertTone, string> = {
  danger: "border-danger/40 bg-danger/10 text-danger",
  success: "border-ok/40 bg-ok/10 text-ok",
  warning: "border-warn/40 bg-warn/10 text-warn",
  info: "border-info/40 bg-info/10 text-info",
};

export function Alert({
  message,
  tone = "danger",
}: {
  message: string;
  tone?: AlertTone;
}) {
  return (
    <div
      role={tone === "danger" ? "alert" : "status"}
      className={cx("rounded-lg border px-3 py-2 text-sm", ALERT_TONES[tone])}
    >
      {message}
    </div>
  );
}

export type BadgeTone =
  "neutral" | "queue" | "warning" | "danger" | "ok" | "info" | "brand" | "plan";

// neutral and queue carry a border/surface/fg triple (theme tokens set all
// three), so ember renders its solid gray pill while a theme can retint just
// these — the others tint a single hue by opacity. See index.css.
const BADGE_TONES: Record<BadgeTone, string> = {
  neutral: "border-neutral-border bg-neutral-surface text-neutral-fg",
  queue: "border-queue-border bg-queue-surface text-queue-fg",
  warning: "border-warn/40 bg-warn/10 text-warn",
  danger: "border-danger/40 bg-danger/10 text-danger",
  ok: "border-ok/40 bg-ok/10 text-ok",
  info: "border-info/40 bg-info/10 text-info",
  brand: "border-brand/40 bg-brand/10 text-brand",
  plan: "border-plan/40 bg-plan/10 text-plan",
};

export function Badge({
  children,
  tone = "neutral",
  title,
  dot = false,
  pulse = false,
  wrap = false,
  ...aria
}: {
  children: ReactNode;
  tone?: BadgeTone;
  title?: string;
  dot?: boolean;
  // pulse animates the dot (dot must also be set) — used for a still-running state,
  // mirroring StatusPill's running pulse.
  pulse?: boolean;
  // wrap opts OUT of whitespace-nowrap (web-ux F20). The default stays nowrap because
  // most badges are one or two words and a wrapped status pill looks broken — but a
  // badge carrying a sentence-length string overflows the viewport instead, measured
  // at 389px on a 375px screen. The caller knows which it is; the component cannot.
  wrap?: boolean;
  "aria-describedby"?: string;
}) {
  return (
    <span
      title={title}
      {...aria}
      className={cx(
        "inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-[11px] font-medium",
        wrap ? "whitespace-normal" : "whitespace-nowrap",
        BADGE_TONES[tone],
      )}
    >
      {dot && (
        <span
          aria-hidden="true"
          className={cx(
            "h-1.5 w-1.5 rounded-full bg-current",
            pulse && "animate-pulse",
          )}
        />
      )}
      {children}
    </span>
  );
}

// StatusPill renders a run status with a colored dot (multica gives every
// status an icon color via issues/components/status-icon.tsx; the pill shape
// follows its chat/components/task-status-pill.tsx).
//
// Exported so runBadge.test.ts can assert the two tone surfaces agree. runBadge.ts
// carries the same taxonomy for the board card and its header says so ("Tones mirror
// StatusPill's RUN_STATUS_TONES (ui.tsx) so one status renders one color
// everywhere") — a claim that was, until PRD #35, enforced by nothing but that
// sentence. Exporting the map is what lets a test fail when the two drift.
export const RUN_STATUS_TONES: Record<
  string,
  { tone: BadgeTone; pulse?: boolean }
> = {
  queued: { tone: "queue" },
  claimed: { tone: "info" },
  running: { tone: "info", pulse: true },
  // issue #321: the pre-approval planning phase, a derived effective status (not a real
  // runs.status value). Indigo `plan` tone, pulsing like `running` — it IS live work,
  // just pre-approval. StatusPill's default label `status.replace(/_/g," ")` already
  // yields "planning", so no RUN_STATUS_LABELS entry is needed.
  planning: { tone: "plan", pulse: true },
  awaiting_approval: { tone: "warning", pulse: true },
  // PRD #88: parked on a clarification question. Deliberately IDENTICAL to
  // awaiting_approval — same shape of debt (a human owes the run an action while a
  // worker is held), and the PRD frames it as the third human-in-the-loop channel
  // alongside the plan gate (D-O #4). The label distinguishes them; the tone must not.
  awaiting_input: { tone: "warning", pulse: true },
  /** PRD #35: parked until the owner's Anthropic usage window reopens. Warn-toned
   *  like the other "blocked on something outside the run" state, and deliberately
   *  NOT pulsing: a pulse reads as live work, and a parked run does nothing at all
   *  for what can be hours. The countdown that makes the wait legible needs
   *  retry_not_before, which only the run view has — the board's LatestRun
   *  projection does not carry it, so this pill is static everywhere it appears. */
  limit_wait: { tone: "warning" },
  completed: { tone: "ok" },
  failed: { tone: "danger" },
  cancelled: { tone: "neutral" },
};

// RUN_STATUS_LABELS overrides the default `status.replace(/_/g," ")` where the raw enum
// makes a poor sentence. It exists because that fallback and runBadge's label switch are
// two independent renderings of one status, and they had silently diverged: runBadge
// deliberately says "needs your answer" for awaiting_input on the reasoning that
// "awaiting input" reads as machine-waiting-for-machine, while this pill printed exactly
// the phrasing that reasoning rejects — on the run header and every Runs-list row.
//
// The divergence was free until now only because `awaiting_approval` happens to read
// fine de-underscored. Keep the two in step for any status added here.
const RUN_STATUS_LABELS: Record<string, string> = {
  awaiting_input: "needs your answer",
};

export function StatusPill({ status }: { status: string }) {
  const cfg = RUN_STATUS_TONES[status] ?? { tone: "neutral" as BadgeTone };
  const label = RUN_STATUS_LABELS[status] ?? status.replace(/_/g, " ");
  return (
    <span
      className={cx(
        "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[11px] font-medium",
        BADGE_TONES[cfg.tone],
      )}
    >
      <span
        aria-hidden="true"
        className={cx(
          "h-1.5 w-1.5 rounded-full bg-current",
          cfg.pulse && "animate-pulse",
        )}
      />
      {label}
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
    const id = setInterval(
      () => setFrame((f) => (f + 1) % SPINNER_FRAMES.length),
      80,
    );
    return () => clearInterval(id);
  }, []);
  return (
    <span
      aria-hidden="true"
      className={cx(
        "inline-block min-w-[1ch] text-center font-mono",
        className,
      )}
    >
      {SPINNER_FRAMES[frame]}
    </span>
  );
}

// ── Loading & empty states ───────────────────────────────────────────────────

export function Skeleton({ className = "" }: { className?: string }) {
  return (
    <div
      aria-hidden="true"
      className={cx("animate-pulse rounded-md bg-raised", className)}
    />
  );
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
        <h3 className="text-sm font-medium text-fg">{title}</h3>
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
          {titleNode ?? (
            <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
          )}
          {description && (
            <p className="mt-1 max-w-2xl text-sm text-muted">{description}</p>
          )}
        </div>
        {actions && (
          <div className="flex flex-wrap items-center gap-2">{actions}</div>
        )}
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
      <p className="text-xs font-medium uppercase tracking-wider text-faint">
        {label}
      </p>
      <p className="mt-1.5 text-2xl font-semibold tabular-nums text-fg">
        {value}
      </p>
      {hint && <p className="mt-0.5 text-xs text-faint">{hint}</p>}
    </div>
  );
  return to ? <Link to={to}>{body}</Link> : body;
}
