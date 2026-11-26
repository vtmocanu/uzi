// THE FILE A MODULE REPO VENDORS: host SDK contract, version 1 (PRD #4686, "Host SDK").
//
// A module's exposed page is a React component that receives one prop, `sdk: HostSdkV1`,
// and the module's component file also exports `hostContract: 1`. Copy this one file, at the
// uzi tag the module targets, into the module repo. It is self-contained: it imports only
// types from `react` and nothing from the host, so it compiles against the module's own
// React types. The host's runtime values (api client, ui components, theme helpers) live in
// sdk.ts and are NOT part of what a module vendors.
//
// Within a major version changes are additive only. The host supports exactly one major.
// sdk.ts assigns the host's real components to `HostSdkV1["ui"]` and checks that every prop key
// named below still exists on the host component, so a host component that drops a prop, narrows
// a prop's type or adds a required prop fails the host's type-check. (Host-only extra optional
// props are allowed.)

import type { ButtonHTMLAttributes, ReactNode, Ref, SelectHTMLAttributes } from "react";

export const HOST_CONTRACT = 1 as const;

export type ModuleHttpMethod = "GET" | "POST" | "PUT" | "PATCH" | "DELETE";

// The polarity of the current theme (light-on-dark or dark-on-light), never a theme name.
export type ThemeMode = "light" | "dark";

export type ButtonVariant = "primary" | "secondary" | "ghost" | "danger" | "dangerSolid";
export type ButtonSize = "sm" | "md";
export type BadgeTone = "neutral" | "queue" | "warning" | "danger" | "ok" | "info" | "brand" | "plan";

export interface CardProps {
  children: ReactNode;
  className?: string;
  // Anchor target for in-page section indexes.
  id?: string;
  // Edge-to-edge table card: no padding, outer table cells padded instead.
  flush?: boolean;
}

export interface CardHeaderProps {
  title: ReactNode;
  level?: 2 | 3;
  badge?: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
}

export type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: ButtonVariant;
  size?: ButtonSize;
  ref?: Ref<HTMLButtonElement>;
};

export type SelectProps = SelectHTMLAttributes<HTMLSelectElement>;

export interface BadgeProps {
  children: ReactNode;
  tone?: BadgeTone;
  title?: string;
  dot?: boolean;
  // pulse animates the dot (dot must also be set).
  pulse?: boolean;
  // wrap opts out of whitespace-nowrap, for a sentence-length string.
  wrap?: boolean;
  "aria-describedby"?: string;
}

// The contract exposes only the run status word; the host's pill accepts more (progress,
// health, recovery fields) that modules cannot rely on.
export interface StatusPillProps {
  status: string;
}

export interface EmptyStateProps {
  icon?: ReactNode;
  title: string;
  description?: ReactNode;
  action?: ReactNode;
}

export interface SpinnerProps {
  className?: string;
}

export interface SkeletonProps {
  className?: string;
}

type Component<P> = (props: P) => ReactNode;

export interface HostSdkV1 {
  contract: typeof HOST_CONTRACT;
  user: { id: string; displayName: string; isAdmin: boolean };
  api: {
    // `path` is relative to /api and must start with "/". Rejects (ApiError) with the
    // server's HTTP status and message; a path that would leave /api/ is refused before
    // any request is made.
    request<T>(method: ModuleHttpMethod, path: string, body?: unknown): Promise<T>;
  };
  ui: {
    Card: Component<CardProps>;
    CardHeader: Component<CardHeaderProps>;
    Button: Component<ButtonProps>;
    Select: Component<SelectProps>;
    Badge: Component<BadgeProps>;
    StatusPill: Component<StatusPillProps>;
    EmptyState: Component<EmptyStateProps>;
    Spinner: Component<SpinnerProps>;
    Skeleton: Component<SkeletonProps>;
  };
  theme: {
    // The current polarity; read it again after a change (it is a live getter).
    readonly mode: ThemeMode;
    // Calls `cb` when the polarity changes; returns the unsubscribe function.
    subscribe(cb: (mode: ThemeMode) => void): () => void;
  };
  // A router helper for links outside the module's own /m/<id> subtree.
  navigate(to: string): void;
}
