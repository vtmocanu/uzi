// Build config for the example runtime web module (PRD #4686). A real module repo uses
// this file as a guide (not a drop-in copy): it is the reference for what a federated
// module must set; docs/web-modules-dev.md says what to vendor and what to drop.
//
// NOT SHIPPED. Nothing in a uzi image, chart or workflow builds or publishes this module; it
// is the acceptance fixture for the host (the browser harness web/scripts/modules-browser-test.mjs
// builds every variant below in-process, and the Dockerfile here serves the `example` one).
//
// What a module must do:
//   - use @module-federation/vite as a REMOTE named after its module id, exposing the page
//     (here "./Page") from remoteEntry.js;
//   - share react, react-dom, react-router-dom and react/jsx-runtime as strict singletons
//     with the host's ranges, so the host's copies are used (two Reacts break hooks);
//   - serve under /modules/<id>/ (`base`), the path the host's nginx proxies to the module;
//   - set the same `define` and plugin as the host so the build carries no dynamic-code
//     literal (the host CSP has no 'unsafe-eval').
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { federation } from "@module-federation/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, type InlineConfig } from "vite";
import { mfNoNewFunction } from "../../scripts/vite-mf-no-new-function.mjs";

// Fixture variants the browser harness builds. Each gets its own federation name and base.
//   example     the working module (built twice, v1 and v2, for the stale-chunk scenario)
//   example2    a second module, for the two-remotes scenarios
//   incompat    requires react ^18 (strict): the host's react 19 refuses it
//   contract2   exports hostContract = 2: the host supports only contract 1
//   noexpose    a normal build; its manifest entry names a page the build does not expose
export type ModuleVariant = "example" | "example2" | "incompat" | "contract2" | "noexpose";

const webPackageJson = JSON.parse(readFileSync(new URL("../../package.json", import.meta.url), "utf8")) as {
  dependencies: Record<string, string>;
};

const SHARED = ["react", "react-dom", "react-router-dom", "react/jsx-runtime"] as const;

function requiredVersion(name: (typeof SHARED)[number], variant: ModuleVariant): string {
  const pkg = name === "react/jsx-runtime" ? "react" : name;
  if (variant === "incompat" && (pkg === "react" || pkg === "react-dom")) return "^18.0.0";
  return webPackageJson.dependencies[pkg];
}

export function createModuleConfig({
  id,
  variant = id as ModuleVariant,
  buildTag = "v1",
  outDir,
}: {
  id: string;
  variant?: ModuleVariant;
  buildTag?: string;
  outDir: string;
}): InlineConfig {
  const shared = Object.fromEntries(
    SHARED.map((name) => [name, { singleton: true, strictVersion: true, requiredVersion: requiredVersion(name, variant) }]),
  );
  return {
    root: fileURLToPath(new URL(".", import.meta.url)),
    configFile: false,
    base: `/modules/${id}/`,
    plugins: [
      react(),
      mfNoNewFunction(),
      federation({
        name: id,
        filename: "remoteEntry.js",
        exposes: { "./Page": "./src/Page.tsx" },
        shareStrategy: "loaded-first",
        shared,
        dts: false,
      }),
    ],
    define: {
      FEDERATION_ALLOW_NEW_FUNCTION: "undefined",
      __system_context__: "undefined",
      __MODULE_ID__: JSON.stringify(id),
      __MODULE_CONTRACT__: String(variant === "contract2" ? 2 : 1),
      __MODULE_BUILD_TAG__: JSON.stringify(buildTag),
    },
    build: { target: "esnext", outDir, emptyOutDir: true },
  };
}

// `vite build --config <this file>` (the Dockerfile) builds the `example` module.
export default defineConfig(() =>
  createModuleConfig({
    id: "example",
    buildTag: process.env.MODULE_BUILD_TAG ?? "v1",
    outDir: process.env.MODULE_OUT_DIR ?? "dist",
  }),
);
