// The example runtime web module (PRD #4686). It is the page the host loads for /m/<id>/*:
// a default-exported component that receives `sdk: HostSdkV1`, plus the `hostContract` export
// the host checks. The same source builds every fixture variant (vite.config.ts), so the
// module id, contract and build tag come from build-time defines.
import { createElement, lazy, Suspense, useEffect, useState } from "react";
import { jsx } from "react/jsx-runtime";
import { Link, Route, Routes, useNavigate } from "react-router-dom";
import type { HostSdkV1 } from "../../../src/modules/sdk-contract";
import "./module.css";

const SubView = lazy(() => import("./SubView"));

export const hostContract = __MODULE_CONTRACT__;

// Handed to the host's browser harness to prove one React / one router across the host and
// every remote (the harness compares these with the host's own references).
export const reactRefs = { useState, createElement, jsx, useNavigate };

const base = `/m/${__MODULE_ID__}`;

function ApiResult({ sdk }: { sdk: HostSdkV1 }) {
  const [text, setText] = useState("api: loading");
  useEffect(() => {
    let alive = true;
    sdk.api
      .request<{ user?: { email?: string } }>("GET", "/auth/me")
      .then(
        (r) => alive && setText(`api ok: ${r.user?.email ?? "unknown"}`),
        (e: unknown) => alive && setText(`api error: ${e instanceof Error ? e.message : String(e)}`),
      );
    return () => {
      alive = false;
    };
  }, [sdk]);
  return <p data-testid="example-api">{text}</p>;
}

function ThemeMode({ sdk }: { sdk: HostSdkV1 }) {
  const [mode, setMode] = useState(sdk.theme.mode);
  useEffect(() => sdk.theme.subscribe(setMode), [sdk]);
  return <p data-testid="example-theme">theme: {mode}</p>;
}

// Throws on every render until the harness arms success, so React's automatic re-render
// attempts cannot make it recover on their own; Retry only helps once armed.
function FailUntilArmed() {
  if (window.__uziFixtureArmSuccess !== true) throw new Error("fixture: fail-until-armed is not armed");
  return <p data-testid="example-recovered">recovered</p>;
}

function Home({ sdk }: { sdk: HostSdkV1 }) {
  return (
    <sdk.ui.Card>
      <h2 data-testid="example-title">Example module {__MODULE_BUILD_TAG__}</h2>
      <p data-testid="example-user">signed in as {sdk.user.displayName}</p>
      <ThemeMode sdk={sdk} />
      <ApiResult sdk={sdk} />
      <nav>
        <Link to={`${base}/sub`}>Sub view</Link>
        <Link to={`${base}/fail-until-armed`}>Failing view</Link>
        <Link to="/dashboard">Back to the dashboard</Link>
      </nav>
    </sdk.ui.Card>
  );
}

export default function Page({ sdk }: { sdk: HostSdkV1 }) {
  return (
    <div className="uzi-mod-example">
      <Routes>
        <Route index element={<Home sdk={sdk} />} />
        <Route
          path="sub"
          element={
            <Suspense fallback={<p>Loading sub view</p>}>
              <SubView />
            </Suspense>
          }
        />
        <Route path="fail-until-armed" element={<FailUntilArmed />} />
      </Routes>
    </div>
  );
}
