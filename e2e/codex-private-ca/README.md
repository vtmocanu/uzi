# Private-CA UID-split proof (in progress)

Run `bash e2e/codex-private-ca/run.sh` with Docker and OpenSSL. The launcher always runs all three rows: stalled TLS, trusted private CA, and no extra CA. It uses the baked production entrypoint, config capture, factory, executor, default launcher, native helpers and pinned runtime. No host credentials, replacement production sources, TLS-disable settings or fixture selectors are passed.

The default image is `cdr-codex-ca:6b4d31d7e`, ID `sha256:eda31cfe5c9f9e9d819174eb33ecd060f69cf5e9c5e239d390ff5c909c4d2b1a`, built from production revision `6b4d31d7e653d30ea43f7ad8bd014a652c63788e`. Image overrides require both `FIXTURE_IMAGE` and `FIXTURE_EXPECTED_ID`.

The initial-response allowance is shared with setup, not a certificate diagnosis. Negative rows require an instance of the image's actual `CodexTransportError` with category `timeout` and the fixed initial-response deadline message, zero HTTP requests and settlement/disposal. The stalled listener accepts TCP and sends no TLS bytes. The no-extra row additionally requires listener-observed failed handshakes. The trusted row completes authenticated Responses and callback continuation. Each row checks worker UID 10001, source ownership `0:10001` with mode `0440`, worker readability and actual runner read denial.

The three-row matrix passed against this image on Linux x64 / Node v26.12.1. The prior 100-second outer watchdog included roughly 85 seconds of container startup, cutting off the third row. Reordering rows did not establish a production cross-run defect. The launcher now records container elapsed time and permits 240 seconds for startup and all rows. Node's 120-second test timeout and production wall/idle settings (20/10 seconds) are unchanged; watchdog expiry always fails. Temporary child-process observation wrappers were removed.

Command-cache retention warnings (`unattested`) remain: accepted settlement/disposal does not prove kernel drainage, root removal, or absence of retained custody. Fixture request bodies are capped at 4 MiB and the HTTP request count at 16; these are not production ingestion bounds.

Remaining M3 work includes the other CA controls (startup-source errors, unrelated trust and vendor controls), CI integration under the existing Codex M4 target, exact staged PEM/environment/mode observations, auth/session/archive exclusion, root cleanup/recreation, later connections and remaining lifecycle proofs. No capability activation follows from this fixture.

Shell syntax and warning-level ShellCheck are available. The image has production dependencies only and no TypeScript compiler; no compiler result is claimed. Before/after CI critical-path timing remains unmeasured. Hosted acceptance remains separately unverified.
