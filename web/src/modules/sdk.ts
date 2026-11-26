// The host's implementation of the SDK contract, version 1 (PRD #4686, "Host SDK"). The
// contract types a module repo vendors live in sdk-contract.ts (self-contained, React types
// only); this file binds them to the host's runtime values (api client, ui components,
// theme helpers) and is NOT vendored.

import type { ComponentProps } from "react";
import type { User } from "../lib/apiTypes";
import { moduleApiRequest } from "../lib/api";
import { Badge, Button, Card, CardHeader, EmptyState, Select, Skeleton, Spinner, StatusPill } from "../components/ui";
import {
  HOST_CONTRACT,
  type BadgeProps,
  type ButtonProps,
  type CardHeaderProps,
  type CardProps,
  type EmptyStateProps,
  type HostSdkV1,
  type ModuleHttpMethod,
  type SelectProps,
  type SkeletonProps,
  type SpinnerProps,
  type StatusPillProps,
} from "./sdk-contract";
import { currentThemeMode, subscribeThemeMode } from "./theme";

export { HOST_CONTRACT, type HostSdkV1 };

// Compile-time conformance, two checks on the host's nine ui components against the contract:
//  1. `hostUi` assignment: a contract prop the host component no longer accepts under that
//     type (a narrowed type, or a required prop the contract does not pass) fails to compile.
//  2. `Covered` below: every KEY the contract names must still exist on the host component's
//     props. The assignment alone does not catch the host dropping an optional prop (the
//     contract's `pulse?` on Badge, say), because a component without it is still assignable.
// What is NOT checked: a prop the host adds beyond the contract (additive, allowed), and a
// prop's meaning.
const hostUi: HostSdkV1["ui"] = { Card, CardHeader, Button, Select, Badge, StatusPill, EmptyState, Spinner, Skeleton };

// Resolves to `true` when every key of the contract props C is a key of the host props H, else
// to the union of the missing keys (never assignable to `true`, so the line below fails).
type MissingKeys<C, H> = Exclude<keyof C, keyof H>;
type Covered<C, H> = [MissingKeys<C, H>] extends [never] ? true : MissingKeys<C, H>;
const coverage: {
  Card: Covered<CardProps, ComponentProps<typeof Card>>;
  CardHeader: Covered<CardHeaderProps, ComponentProps<typeof CardHeader>>;
  Button: Covered<ButtonProps, ComponentProps<typeof Button>>;
  Select: Covered<SelectProps, ComponentProps<typeof Select>>;
  Badge: Covered<BadgeProps, ComponentProps<typeof Badge>>;
  StatusPill: Covered<StatusPillProps, ComponentProps<typeof StatusPill>>;
  EmptyState: Covered<EmptyStateProps, ComponentProps<typeof EmptyState>>;
  Spinner: Covered<SpinnerProps, ComponentProps<typeof Spinner>>;
  Skeleton: Covered<SkeletonProps, ComponentProps<typeof Skeleton>>;
} = { Card: true, CardHeader: true, Button: true, Select: true, Badge: true, StatusPill: true, EmptyState: true, Spinner: true, Skeleton: true };
void coverage;

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
