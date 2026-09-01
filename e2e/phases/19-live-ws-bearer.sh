# shellcheck shell=bash
# phase:    live-ws-bearer
# title:    PRD #112 M1: a Bearer (uzc_) /api/ws subscription receives a live run_message frame
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# --- Leg 3: the same /api/ws over a Bearer CLI token (PRD #112 M1) -----------
# M1 moved /ws out of the cookie-only tail into a RequireUser mount, so the headless
# `uzi tui` can subscribe with the $UZI_TOKEN minted above instead of a browser session.
# Leg 1 proves the COOKIE half of that mount still works; this proves the BEARER half on
# the same wire, and it is the only place the two credential classes are exercised
# against one route end to end.
#
# NO Origin header is set, and that omission IS the mechanism under test (PRD #112 D2):
# coder/websocket's authenticateOrigin returns nil for an empty Origin
# (v1.8.14 accept.go:228-232), so a browser-less client passes the unchanged same-origin
# Accept with nothing skipped — no InsecureSkipVerify, no OriginPatterns widening. Node's
# global WebSocket sends no Origin unless one is passed in {headers} (verified on node 22
# and 26), so this probe is genuinely the browser-less shape; if a future runtime starts
# sending one, the upgrade 403s and the diagnostic below prints that status rather than a
# bare timeout.
#
# Own run, own approval, like leg 1: the frame is live-only (no replay), so the probe must
# be subscribed BEFORE the run resumes — it subscribes first and approves from inside the
# socket's open handler. The approve rides the SAME Bearer token (POST /api/runs/{id}/inputs
# is RequireUser and takes no CSRF on the bearer path), so one credential drives both the
# subscribe and the steer.
say "PRD #112 M1: a Bearer (uzc_) /api/ws subscription receives a live run_message frame"

# MEASURE that Node sends no Origin, rather than asserting it by omission. The leg's
# whole point is the empty-Origin exemption (coder/websocket accept.go:228-232) — if a
# future Node emitted an Origin equal to the Host, every assertion below would still
# pass while never exercising the property the leg is named after. The probe opens a
# throwaway HTTP listener in-process, points a WebSocket at it, and reports the Origin
# header the runtime actually sent.
ORIGIN_PROBE='const http=require("http");const srv=http.createServer();srv.on("upgrade",(req,sock)=>{ console.log("ORIGIN="+JSON.stringify(req.headers.origin===undefined?null:req.headers.origin)); sock.destroy(); srv.close(); process.exit(0); });srv.listen(0,"127.0.0.1",()=>{ const p=srv.address().port; const ws=new WebSocket("ws://127.0.0.1:"+p+"/probe",{headers:{Authorization:"Bearer probe"}}); ws.addEventListener("error",()=>{}); });setTimeout(()=>{ console.log("ORIGIN_PROBE_TIMEOUT"); process.exit(1); },10000);'
# stderr is merged deliberately — the captured text is what the failure message prints,
# and a warning line is useful context. But the VERDICT must be a match, not equality:
# any incidental stderr (an ExperimentalWarning, a deprecation notice, a docker banner)
# would fail `=` and redden this leg with "sends an Origin header", naming a cause that
# did not occur. Green today; a base-image node bump turns it red for the wrong reason.
ORIGIN_OUT="$("${COMPOSE[@]}" exec -T agent node -e "$ORIGIN_PROBE" 2>&1 || true)"
printf '%s\n' "$ORIGIN_OUT" | grep -q '^ORIGIN=null$' \
  || fail "the agent runtime sends an Origin header on a headers-only WebSocket (probe: ${ORIGIN_OUT:-<none>}) — the Bearer leg below would then pass WITHOUT exercising the empty-Origin exemption it exists to prove; see PRD #112 D2"
pass "the agent runtime sends NO Origin on a Bearer WebSocket ($ORIGIN_OUT) — the empty-Origin exemption is genuinely what this leg exercises"

# Leg 3 mints its OWN credential (issue #126). It used to consume the token Leg 2 mints,
# which put M1's Bearer-WS auth evidence DOWNSTREAM of `command -v go` and
# `go build ./cmd/uzi`: a runner without go, or a compile error anywhere in that package,
# aborted in Leg 2 and took the auth evidence with it as collateral. That coupling got
# worse on PRD #112 — M3 and M4 added ~2,300 lines to `api/cmd/uzi/`, so the auth gate
# became both the most important leg and the most downstream. There is no cap on
# per-user CLI tokens (CreateCLIToken validates only the name), so a second mint is free.
#
# Deliberately NOT asserting that one credential drives both the CLI and the WS: that was
# a side effect of variable reuse, never a designed invariant, and the point of this
# change is to stop the two legs sharing state at all.
WSB_TOKEN_VAL="$(apipost "/api/me/cli-tokens" '{"name":"e2e-m1-ws-bearer"}' | jq -r '.token')"
{ [ -n "$WSB_TOKEN_VAL" ] && [ "$WSB_TOKEN_VAL" != null ] && [ "${WSB_TOKEN_VAL#uzc_}" != "$WSB_TOKEN_VAL" ]; } \
  || fail "did not mint a uzc_ for the Bearer WS leg (got '${WSB_TOKEN_VAL:-<none>}')"

# Belt-and-braces only, and no longer the thing standing between a reorder and a
# misattributed verdict: the cross-leg variable this used to guard is gone, so the
# hazard is structurally absent rather than merely checked for.
{ [ -n "${WSB_TOKEN_VAL:-}" ] && [ "${WSB_TOKEN_VAL#uzc_}" != "$WSB_TOKEN_VAL" ]; } \
  || fail "Leg 3's own uzc_ mint did not produce a usable token (got '${WSB_TOKEN_VAL-<unset>}')"

IID_WSB="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E ws bearer","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
{ [ -n "$IID_WSB" ] && [ "$IID_WSB" != null ]; } || fail "could not create the Bearer /api/ws issue"
RUN_WSB="$(create_run "$REPO_ID" "$IID_WSB")" || fail "bearer-ws-leg run was not created"
wait_status "$RUN_WSB" awaiting_approval

# NEGATIVE control FIRST (run still parked, so a valid run id — the ONLY rejection reason
# is the junk credential): a bogus Bearer must be refused. Without this the positive
# assertion below would also pass against a route that admits anything.
#
# It is only HALF a control on its own: an "error" event fires for ANY connect failure —
# the agent container being down, node missing, a syntax error in the probe — so this
# passing means "the socket did not open", not "the gate refused it". It is valid only
# PAIRED with the positive assertion below, which cannot pass unless the route really
# works. Neither is evidence alone.
WSB_NEG_PROBE='const wsurl=process.argv[1];let done=false;const finish=(code,msg)=>{ if(done)return; done=true; console.log(msg); process.exit(code); };const ws=new WebSocket(wsurl,{headers:{Authorization:"Bearer uzc_not-a-real-token"}});ws.addEventListener("open",()=>finish(1,"OPENED_WITH_BOGUS_BEARER"));ws.addEventListener("error",()=>finish(0,"rejected"));setTimeout(()=>finish(2,"NO_REJECTION"),10000);'
if NEGB_OUT="$("${COMPOSE[@]}" exec -T agent node -e "$WSB_NEG_PROBE" "$WS_API/api/ws?run=$RUN_WSB")"; then
  pass "bogus-Bearer /api/ws upgrade is rejected ($NEGB_OUT) — the RequireUser bearer gate is real"
else
  fail "bogus-Bearer /api/ws upgrade was NOT rejected (probe: ${NEGB_OUT:-<none>}) — the bearer auth gate is broken/vacuous"
fi

# POSITIVE: subscribe with the uzc_ token and no Origin, approve on open over the same
# token, assert a run_message frame.
WSB_PROBE='const wsurl=process.argv[1], token=process.argv[2], approveUrl=process.argv[3];let done=false;const finish=(code,msg)=>{ if(done)return; done=true; console.log(msg); process.exit(code); };const auth="Bearer "+token;const ws=new WebSocket(wsurl,{headers:{Authorization:auth}});ws.addEventListener("open",()=>{ fetch(approveUrl,{method:"POST",headers:{"Content-Type":"application/json","Authorization":auth},body:JSON.stringify({kind:"approve_plan",body:""})}).then(r=>{ if(!r.ok) finish(3,"APPROVE_STATUS="+r.status); }).catch(e=>finish(2,"APPROVE_ERR="+e.message)); });ws.addEventListener("message",(ev)=>{ let f; try{ f=JSON.parse(ev.data); }catch(e){ return; } if(f.type==="message"&&f.seq>0){ finish(0,"FRAME type=message seq="+f.seq+(f.agent?(" agent="+f.agent):"")+" kind="+(f.kind||"")); } });ws.addEventListener("error",(e)=>{ fetch(wsurl.replace(/^ws/,"http"),{headers:{Authorization:auth}}).then(r=>finish(5,"WS_ERR http_probe_status="+r.status+" msg="+((e&&e.message)||""))).catch(err=>finish(5,"WS_ERR msg="+((e&&e.message)||"")+" (diag_fetch_failed="+err.message+")")); });setTimeout(()=>finish(6,"TIMEOUT no live /api/ws run_message frame over Bearer"),25000);'
if WSB_OUT="$("${COMPOSE[@]}" exec -T agent node -e "$WSB_PROBE" \
    "$WS_API/api/ws?run=$RUN_WSB" "$WSB_TOKEN_VAL" "$WS_API/api/runs/$RUN_WSB/inputs")"; then
  pass "live /api/ws frame received over a Bearer uzc_ token, no Origin sent: $WSB_OUT"
else
  # The diagnostic in WS_ERR is a PLAIN GET (wsurl with ws:// swapped for http://, no
  # upgrade headers), so it can only observe what the route answers BEFORE any
  # handshake. Naming a status it cannot produce would send the next reader hunting a
  # gate the probe never reached:
  #   401 — authN refused: /ws is back in the cookie-only tail, or the token is bad.
  #   404 — ServeWS's per-run authz refused; it runs BEFORE websocket.Accept.
  #   426 — the route is HEALTHY and this is the expected answer: a plain GET carries no
  #         `Connection: Upgrade`, which coder/websocket rejects first (accept.go:189-192)
  #         with Upgrade Required. A 426 here means auth and authz both PASSED, so the
  #         fault is in the socket or frame path, not the gate.
  # 403 is deliberately NOT listed: the origin check lives inside websocket.Accept, which
  # this probe never reaches, so it can never be the answer here.
  fail "no live /api/ws run_message frame over Bearer (probe: ${WSB_OUT:-<none>}). Read the probe output first: APPROVE_STATUS=<n> means the socket opened and the Bearer approve was refused (409 = the run already left the gate — a timing problem, not an auth one); APPROVE_ERR=<msg> means the approve request never completed; WS_ERR carries http_probe_status where 401 = authN refused (is /ws back in the cookie-only tail?), 404 = per-run authz refused, 426 = route healthy so look at the socket/frame path; TIMEOUT with no status = the upgrade succeeded but no frame arrived; probe:<none> means docker compose exec itself failed (agent down, no node, bad JS) and NOTHING about auth was tested"
fi
# The probe's Bearer approve drove the run: confirm it advances to completed.
wait_status "$RUN_WSB" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "the Bearer-WS-triggered approve drove RUN_WSB to completed"

