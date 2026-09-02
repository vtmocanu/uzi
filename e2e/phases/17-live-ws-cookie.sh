# shellcheck shell=bash
# phase:    live-ws-cookie
# title:    PRD #97 M2: a live /api/ws subscription receives a run_message frame during a run (not REST replay)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# =============================================================================
# PRD #97 M2 — live /api/ws frame assertion + uzi CLI smoke. Two full-wire-only
# consumers no lower layer reaches: the browser's primary real-time transport
# (/api/ws — every OTHER stream assertion in this harness uses the REST ?after=<seq>
# replay path, so the hub-broadcast → client wire is never exercised end to end) and
# the `uzi` CLI (a co-equal API consumer, api/cmd/uzi, that silently rots when a
# route/DTO change updates only web/). Both are separate HTTP clients on the real wire.
#
# PLACEMENT (deliberate): M2 sits BEFORE the M1 git-auth leg, not after it. M2 drives two
# runs to completion, so it must NOT be the phase immediately preceding the timing-sensitive
# PRD #95 steer-queue phase, whose assertion needs a follow-up to still be Queued *before*
# the worker claims+steers the run — a worker freshly freed by an M2 run (warm clone cache)
# claims the next run fast enough to consume the follow-up first (observed: consumed 7ms
# after submit). Landing M2 here keeps the proven M1 → main-reject git-probes → PRD #95
# sequence intact between M2 and PRD #95.
#
# --- Leg 1: live /api/ws frame assertion, COOKIE half ------------------------
# /api/ws accepts EITHER the session JWT cookie (uzi_auth) or a Bearer CLI token: it is a
# GET upgrade behind RequireUser (handler.go, mounted with the run reads since PRD #112
# M1), with an Origin==Host same-origin check (CSWSH defense) and per-run owner/admin
# authz in ServeWS (ws.go). This leg drives the COOKIE half — the browser's path, and the
# one the same-origin check exists for; leg 3 below drives the Bearer half on the same
# route. No new tooling (fable review): the agent container's Node 22 has a
# global WebSocket that honours a {headers} option, so we plumb the admin jar's cookie
# into the upgrade and reach the api directly at ws://api:8080 (UZI_API_URL) — the SAME
# WS server the browser hits through nginx, so this proves the api hub→client wire, not
# nginx. A frame is live-only (no replay), so the probe subscribes FIRST, then on socket
# open approves the parked plan from inside node; the run resumes and broadcasts its
# persisted run_messages only AFTER the subscription exists (mirrors ServeWS's unit test
# "publish after dial"). Receiving a run_message frame proves the wire; a DTO/route drift
# or a broken hub→client path yields none ⇒ TIMEOUT ⇒ FAIL. A no-cookie upgrade is
# separately asserted to be REJECTED, so a "would accept anything" gate cannot pass here.
say "PRD #97 M2: a live /api/ws subscription receives a run_message frame during a run (not REST replay)"

# Build the Cookie header for the upgrade. The auth cookie (uzi_auth) is HttpOnly, so curl
# writes it into the Netscape jar with a "#HttpOnly_" domain prefix — a naive /^#/ skip
# drops it (leaving only the non-HttpOnly uzi_csrf, which RequireAuth then 401s). awk
# field-splits that prefix into $1, so name/value stay $6/$7; extract uzi_auth (authN) and
# uzi_csrf (for the in-probe approve's CSRF check) by NAME, mirroring csrf() ($6=="uzi_csrf").
WS_CSRF="$(csrf)"
WS_AUTH="$(awk '$6=="uzi_auth"{print $7}' "$JAR")"
{ [ -n "$WS_AUTH" ] && [ -n "$WS_CSRF" ]; } || fail "could not read uzi_auth/uzi_csrf from the admin jar ($JAR)"
WS_COOKIE="uzi_auth=$WS_AUTH; uzi_csrf=$WS_CSRF"
WS_API="http://api:8080"                       # the agent reaches the api here (UZI_API_URL)
WS_ORIGIN="http://api:8080"                     # match Host so ServeWS's same-origin Accept passes

# A run parked at the plan gate, dedicated to this leg (the probe's approve is its real
# approval).
IID_WS="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E ws","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
{ [ -n "$IID_WS" ] && [ "$IID_WS" != null ]; } || fail "could not create the /api/ws issue"
RUN_WS="$(create_run "$REPO_ID" "$IID_WS")" || fail "ws-leg run was not created"
wait_status "$RUN_WS" awaiting_approval

# NEGATIVE control FIRST (run still parked, so a valid run id — the ONLY rejection reason
# is the missing cookie): a no-cookie upgrade must be refused. With no Authorization header
# RequireUser dispatches to its unmodified RequireAuth cookie path, which 401s before any
# upgrade — proving the auth gate is real and the positive assertion non-vacuous.
WS_NEG_PROBE='const wsurl=process.argv[1], origin=process.argv[2];let done=false;const finish=(code,msg)=>{ if(done)return; done=true; console.log(msg); process.exit(code); };const ws=new WebSocket(wsurl,{headers:{Origin:origin}});ws.addEventListener("open",()=>finish(1,"OPENED_WITHOUT_COOKIE"));ws.addEventListener("error",()=>finish(0,"rejected"));setTimeout(()=>finish(2,"NO_REJECTION"),10000);'
if NEG_OUT="$("${COMPOSE[@]}" exec -T agent node -e "$WS_NEG_PROBE" "$WS_API/api/ws?run=$RUN_WS" "$WS_ORIGIN")"; then
  pass "uncredentialed /api/ws upgrade is rejected ($NEG_OUT) — the WS auth gate is real"
else
  # Names the gate this probe actually exercises. The request carries neither a cookie
  # nor a Bearer, so what refused it is RequireUser as a whole — saying "the cookie auth
  # gate" would send the next reader to audit cookie handling for a failure that is just
  # as likely the bearer branch or the route's mount.
  fail "uncredentialed /api/ws upgrade was NOT rejected (probe: ${NEG_OUT:-<none>}) — the RequireUser gate on /ws is broken/vacuous (cookie branch, bearer branch, or the route lost its guard)"
fi

# POSITIVE: subscribe with the admin cookie, approve on open, assert a run_message frame.
WS_PROBE='const wsurl=process.argv[1], cookie=process.argv[2], approveUrl=process.argv[3], csrf=process.argv[4], origin=process.argv[5];let done=false;const finish=(code,msg)=>{ if(done)return; done=true; console.log(msg); process.exit(code); };const ws=new WebSocket(wsurl,{headers:{Cookie:cookie,Origin:origin}});ws.addEventListener("open",()=>{ fetch(approveUrl,{method:"POST",headers:{"Content-Type":"application/json","X-CSRF-Token":csrf,"Cookie":cookie},body:JSON.stringify({kind:"approve_plan",body:""})}).catch(e=>finish(2,"APPROVE_ERR="+e.message)); });ws.addEventListener("message",(ev)=>{ let f; try{ f=JSON.parse(ev.data); }catch(e){ return; } if(f.type==="message"&&f.seq>0){ finish(0,"FRAME type=message seq="+f.seq+(f.agent?(" agent="+f.agent):"")+" kind="+(f.kind||"")); } });ws.addEventListener("error",(e)=>{ fetch(wsurl.replace(/^ws/,"http"),{headers:{Cookie:cookie,Origin:origin}}).then(r=>finish(5,"WS_ERR http_probe_status="+r.status+" msg="+((e&&e.message)||""))).catch(err=>finish(5,"WS_ERR msg="+((e&&e.message)||"")+" (diag_fetch_failed="+err.message+")")); });setTimeout(()=>finish(6,"TIMEOUT no live /api/ws run_message frame"),25000);'
if WS_OUT="$("${COMPOSE[@]}" exec -T agent node -e "$WS_PROBE" \
    "$WS_API/api/ws?run=$RUN_WS" "$WS_COOKIE" "$WS_API/api/runs/$RUN_WS/inputs" "$WS_CSRF" "$WS_ORIGIN")"; then
  pass "live /api/ws frame received during a real run: $WS_OUT"
else
  fail "no live /api/ws run_message frame (probe: ${WS_OUT:-<none>}) — hub-broadcast wire or DTO drift"
fi
# The probe's approve drove the run: confirm it advances to completed (closes the loop and
# leaves RUN_WS terminal, not parked).
wait_status "$RUN_WS" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "the WS-triggered approve drove RUN_WS to completed"

