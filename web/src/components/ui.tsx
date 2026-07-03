import type {
  ButtonHTMLAttributes,
  InputHTMLAttributes,
  ReactNode,
  SelectHTMLAttributes,
} from "react";

export function Card({ children, className = "" }: { children: ReactNode; className?: string }) {
  return (
    <div className={`rounded-2xl border border-slate-800 bg-panel/70 p-6 shadow-xl ${className}`}>
      {children}
    </div>
  );
}

export function Button({
  children,
  variant = "primary",
  className = "",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: "primary" | "ghost" | "danger" }) {
  const styles = {
    primary: "bg-indigo-500 hover:bg-indigo-400 text-white disabled:opacity-50",
    ghost: "bg-slate-800 hover:bg-slate-700 text-slate-100",
    danger: "bg-rose-600 hover:bg-rose-500 text-white",
  }[variant];
  return (
    <button
      className={`rounded-lg px-4 py-2 text-sm font-medium transition-colors disabled:cursor-not-allowed ${styles} ${className}`}
      {...props}
    >
      {children}
    </button>
  );
}

export function Field({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <label className="block space-y-1.5">
      <span className="text-sm font-medium text-slate-300">{label}</span>
      {children}
    </label>
  );
}

export function Input(props: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      className="w-full rounded-lg border border-slate-700 bg-slate-900 px-3 py-2 text-sm text-slate-100 outline-none focus:border-indigo-400 focus:ring-1 focus:ring-indigo-400"
      {...props}
    />
  );
}

export function Alert({ message }: { message: string }) {
  return (
    <div className="rounded-lg border border-rose-800 bg-rose-950/60 px-3 py-2 text-sm text-rose-200">
      {message}
    </div>
  );
}

export function Select(props: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select
      className="w-full rounded-lg border border-slate-700 bg-slate-900 px-3 py-2 text-sm text-slate-100 outline-none focus:border-indigo-400 focus:ring-1 focus:ring-indigo-400"
      {...props}
    />
  );
}

export function Badge({
  children,
  tone = "neutral",
  title,
}: {
  children: ReactNode;
  tone?: "neutral" | "warning" | "danger";
  title?: string;
}) {
  const styles = {
    neutral: "border-slate-700 bg-slate-800 text-slate-300",
    warning: "border-amber-700 bg-amber-950/60 text-amber-300",
    danger: "border-rose-800 bg-rose-950/60 text-rose-300",
  }[tone];
  return (
    <span
      title={title}
      className={`inline-flex items-center rounded-md border px-1.5 py-0.5 text-[11px] font-medium ${styles}`}
    >
      {children}
    </span>
  );
}
