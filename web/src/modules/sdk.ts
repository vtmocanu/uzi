// The host SDK contract, version 1 (PRD #4686, "Host SDK"). A module's exposed page is a
// React component that receives one prop, `sdk: HostSdkV1`, and the module's component
// file also exports `hostContract: 1`. This file is the single source of the contract
// types: a module repo vendors it at the uzi tag it targets (the `import type` lines
// below name host paths a module repo replaces with its own React types).
//
// Within a major version changes are additive only. The host supports exactly one major.

import type { User } from "../lib/apiTypes";
import { moduleApiRequest } from "../lib/api";
import { Badge, Button, Card, CardHeader, EmptyState, Select, Skeleton, Spinner, StatusPill } from "../components/ui";
import { currentThemeMode, subscribeThemeMode, type ThemeMode } from "./theme";

export const HOST_CONTRACT = 1 as const;

export type ModuleHttpMethod = "GET" | "POST" | "PUT" | "PATCH" | "DELETE";

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
    Card: typeof Card;
    CardHeader: typeof CardHeader;
    Button: typeof Button;
    Select: typeof Select;
    Badge: typeof Badge;
    StatusPill: typeof StatusPill;
    EmptyState: typeof EmptyState;
    Spinner: typeof Spinner;
    Skeleton: typeof Skeleton;
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

type Transport = <T>(method: ModuleHttpMethod, path: string, body?: unknown) => Promise<T>;

export function createHostSdk({
  user,
  navigate,
  transport = moduleApiRequest,
}: {
  user: Pick<User, "id" | "email" | "display_name" | "is_admin">;
  navigate: (to: string) => void;
  // Tests inject a transport; the default is the host's own api client wrapper.
  transport?: Transport;
}): HostSdkV1 {
  return {
    contract: HOST_CONTRACT,
    user: { id: user.id, displayName: user.display_name || user.email, isAdmin: user.is_admin },
    api: { request: (method, path, body) => transport(method, path, body) },
    ui: {
      Card,
      CardHeader,
      Button,
      Select,
      Badge,
      StatusPill,
      EmptyState,
      Spinner,
      Skeleton,
    },
    theme: {
      get mode() {
        return currentThemeMode();
      },
      subscribe: subscribeThemeMode,
    },
    navigate,
  };
}
