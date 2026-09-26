# shellcheck shell=bash
# phase:    compose-web-dns
# title:    Compose web re-resolves API DNS for REST and WebSocket
# critical: no
# lane:     any
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  creates isolated throwaway Docker network and containers
# restores: removes only its own exact-name Docker resources
say "Compose web API DNS recovery"
"$ROOT/e2e/compose-web-dns-test.sh" --green || fail "Compose web DNS regression failed"
pass "Compose web recovered both proxy locations after the API address changed"
