// The host's implementation of the SDK contract, version 1 (PRD #4686, "Host SDK"). The
// contract types a module repo vendors live in sdk-contract.ts (self-contained, React types
// only); this file binds them to the host's runtime values (api client, ui components,
// theme helpers) and is NOT vendored.

import type { User } from "../lib/apiTypes";
import { moduleApiRequest } from "../lib/api";
import { Badge, Button, Card, CardHeader, EmptyState, Select, Skeleton, Spinner, StatusPill } from "../components/ui";
import { HOST_CONTRACT, type HostSdkV1, type ModuleHttpMethod } from "./sdk-contract";
import { currentThemeMode, subscribeThemeMode } from "./theme";

export { HOST_CONTRACT, type HostSdkV1 };

// Compile-time conformance: each host component must accept every prop the contract names.
// If ui.tsx changes a component's props incompatibly, this assignment stops compiling.
const hostUi: HostSdkV1["ui"] = { Card, CardHeader, Button, Select, Badge, StatusPill, EmptyState, Spinner, Skeleton };

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
    ui: hostUi,
    theme: {
      get mode() {
        return currentThemeMode();
      },
      subscribe: subscribeThemeMode,
    },
    navigate,
  };
}
