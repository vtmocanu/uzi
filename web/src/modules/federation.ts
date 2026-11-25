// The Module Federation runtime wiring (PRD #4686, "Loader"). The host uses the RUNTIME
// only (no federation build plugin), and this file is loaded lazily by registry.tsx once
// /modules.json lists at least one module, so with zero modules the federation chunk is
// never fetched. It is also the only place that calls the runtime's registerRemotes and
// loadRemote.
//
// Shared singletons: a module built with the federation plugin shares the host's React,
// React DOM, router and jsx-runtime instead of bundling its own (two Reacts break hooks
// and the router context). `strictVersion` makes a module built against an incompatible
// range fail loudly with a version-refusal error instead of loading a second copy.

import * as React from "react";
import * as ReactDOM from "react-dom";
import * as JsxRuntime from "react/jsx-runtime";
import * as ReactRouterDom from "react-router-dom";
import { init, loadRemote, registerRemotes } from "@module-federation/runtime";
import { version as reactVersion } from "react/package.json";
import { version as reactDomVersion } from "react-dom/package.json";
import { version as routerVersion } from "react-router-dom/package.json";
import { dependencies } from "../../package.json";
import type { ModuleEntry } from "./manifest";
import { countModuleStat } from "./stats";

function shareOf(version: string, lib: unknown, requiredVersion: string) {
  return { version, lib: () => lib, shareConfig: { singleton: true, requiredVersion, strictVersion: true } };
}

let initialised = false;

function ensureInit(): void {
  if (initialised) return;
  initialised = true;
  init({
    name: "uzi_host",
    remotes: [],
    shareStrategy: "loaded-first",
    shared: {
      react: shareOf(reactVersion, React, dependencies.react),
      "react-dom": shareOf(reactDomVersion, ReactDOM, dependencies["react-dom"]),
      "react-router-dom": shareOf(routerVersion, ReactRouterDom, dependencies["react-router-dom"]),
      "react/jsx-runtime": shareOf(reactVersion, JsxRuntime, dependencies.react),
    },
  });
}

function entryUrl(id: string, retry?: number): string {
  return `/modules/${id}/remoteEntry.js${retry === undefined ? "" : `?retry=${retry}`}`;
}

// Registers every configured module. Fetches no remote entry: that happens on the first
// loadRemote of a module route.
export function registerModuleRemotes(list: ModuleEntry[]): void {
  ensureInit();
  registerRemotes(
    list.map((m) => ({ name: m.id, entry: entryUrl(m.id), type: "module" as const })),
    { force: true },
  );
  countModuleStat("registerRemotes");
}

// Retry for a transient first-load failure: re-register the remote with a fresh entry URL
// (the failed fetch may be cached by the browser's module map) and `force`.
export function reRegisterModule(id: string, retry: number): void {
  ensureInit();
  registerRemotes([{ name: id, entry: entryUrl(id, retry), type: "module" as const }], { force: true });
  countModuleStat("registerRemotes");
}

export function loadModuleRemote(request: string): Promise<unknown> {
  ensureInit();
  countModuleStat("loadRemote");
  return loadRemote<unknown>(request);
}

// Harness-only (like __uziModuleStats): the host's own React and router references, so the
// browser harness can prove a remote's `reactRefs` export is the SAME function object (one
// React, one router across the host and every remote). Dead-code eliminated from shipped
// builds, where __UZI_TEST_HOOKS__ is false.
declare global {
  interface Window {
    __uziHostReactRefs?: {
      useState: typeof React.useState;
      createElement: typeof React.createElement;
      jsx: typeof JsxRuntime.jsx;
      useNavigate: typeof ReactRouterDom.useNavigate;
    };
  }
}
if (__UZI_TEST_HOOKS__) {
  window.__uziHostReactRefs = {
    useState: React.useState,
    createElement: React.createElement,
    jsx: JsxRuntime.jsx,
    useNavigate: ReactRouterDom.useNavigate,
  };
}
