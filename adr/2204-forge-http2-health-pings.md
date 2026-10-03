# ADR-2204: HTTP/2 health pings for forge connections

**Status**: Accepted
**Date**: 2026-10-03
**Issue**: [#2204](https://github.com/vtmocanu/uzi/issues/2204)

## Context

A process-local forge timeout incident prompted a transport investigation. Its
protocol and cause remain unproven. A local HTTP/2 reproduction establishes a
responsive TLS connection, then blackholes that exact socket without closing it
or answering pings. Three consecutive calls through rebuilt GitHub drivers
reuse the same connection and each reach their request deadline.

## Decision

Forge API, ancestry and log clients share a package-owned clone of Go's default
transport when it is a standard http.Transport. If a wrapper was installed
before package initialization, use a fresh transport with the standard proxy,
dial, pooling and TLS timeout settings instead of an unchecked type assertion.
Native HTTP2Config sends a health ping after 30 seconds without
received frames and closes the connection after a further 15 seconds without
the ping response. Subsequent calls may establish a fresh connection. Sharing
the transport across driver rebuilds preserves pooling; the process-global
default is unchanged. A package-private override preserves test transport
injection; replacing the process default never disables production health pings.

Cloning retains environment proxy handling, dial behavior, TLS defaults and
HTTP/2 negotiation. Per-call deadlines, redirect policies, GitHub's origin pin
and ETag wrapper, and token redaction remain separate safeguards. A failed
request is still returned to its caller; this adds no application retry loop.

## Validation and limits

Detection takes about 45 seconds after the last received frame (30 seconds idle
plus the 15-second ping timeout), longer than the default 15-second per-call
deadline. Two or three immediately consecutive calls can still time out before
recovery; request timing and scheduling affect the observed delay and count.

The regression uses the real GitHub constructor and trace connection identity.
Each mode runs in a child process with a private pool trusting only the test
certificate; cleanup closes every wrapped socket. Test-only scaled ping
durations prove recovery on a fresh connection. Disabling production ping
enablement makes that recovery fail; a separate defaults test pins 30s and 15s
and checks the inherited transport properties. This demonstrates the mechanism,
not the cause of the historical incident or recovery from every timeout class.
