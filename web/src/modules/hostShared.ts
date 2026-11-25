// The host's shared singleton libraries for Module Federation (PRD #4686, "Loader"). A
// module built with the federation plugin shares the host's React, React DOM, router and
// jsx-runtime instead of bundling its own (two Reacts break hooks and the router context).
// `strictVersion` (set in federation.ts) makes a module built against an incompatible range
// fail loudly with a version-refusal error instead of loading a second copy.
//
// This file lives in the entry chunk on purpose and is handed to the lazily loaded
// federation chunk by federationEntry.ts; see the note in federation.ts for why that chunk
// must not import these libraries itself.

import * as React from "react";
import * as ReactDOM from "react-dom";
import * as JsxRuntime from "react/jsx-runtime";
import * as ReactRouterDom from "react-router-dom";
import { version as reactVersion } from "react/package.json";
import { version as reactDomVersion } from "react-dom/package.json";
import { version as routerVersion } from "react-router-dom/package.json";
import { dependencies } from "../../package.json";
import type { HostShared } from "./federation";

export const HOST_SHARED: HostShared = {
  react: { version: reactVersion, lib: React, requiredVersion: dependencies.react },
  "react-dom": { version: reactDomVersion, lib: ReactDOM, requiredVersion: dependencies["react-dom"] },
  "react-router-dom": { version: routerVersion, lib: ReactRouterDom, requiredVersion: dependencies["react-router-dom"] },
  "react/jsx-runtime": { version: reactVersion, lib: JsxRuntime, requiredVersion: dependencies.react },
};

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
