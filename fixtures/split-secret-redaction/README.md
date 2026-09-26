# split-secret-redaction: worker redaction versus the terminal renderer

The worker redacts run-owned secrets by value (`agent/src/redact.ts`); only the worker knows those
values. The CLI and TUI render stored payloads through `termsafe.SanitizeTTY`
(`api/internal/termsafe/termsafe.go`), which deletes control (Cc, except `\t` and `\n`) and format
(Cf) characters. A secret split by one of those characters used to escape the worker's exact
match, get stored split, and re-join on screen.

`cases.json` pins both halves to one set of inputs:

- `agent/test/redact.test.ts` asserts the worker turns each `input` into exactly `redacted`.
- `api/internal/termsafe/split_secret_fixture_test.go` runs the production `SanitizeTTY` over the
  same cases: for `rejoins_on_tty` cases the raw `input` really does display the whole `secret`
  (the threat is real), and no `redacted` value ever displays it.

`secret` is an arbitrary synthetic value, not provider-shaped. Hand-authored: edit the cases, not
a generator.
