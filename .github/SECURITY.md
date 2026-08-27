# Security Policy

## Reporting a vulnerability

Please do not report security vulnerabilities through public issues, pull
requests, or discussions.

Instead, use GitHub's private vulnerability reporting: go to the repository's
**Security** tab and click **Report a vulnerability**
(https://github.com/vtmocanu/uzi/security/advisories/new). This opens a private
channel visible only to the maintainer, so the details are not disclosed before
a fix is available.

Include, where you can:

- the affected component (api, web, controller, agent, or the Helm chart),
- a description of the issue and its impact,
- steps to reproduce or a proof of concept,
- the version or commit you observed it on.

This is a solo-maintained project, so responses are best effort. You can expect
an acknowledgement once the report has been read, and a follow-up once the issue
has been assessed.

## Supported versions

Only the latest released version receives security fixes. Fixes ship in a new
release rather than as patches to older tags.

## Scope

uzi runs as a self-hosted stack (see `ARCHITECTURE.md` for the trust
boundaries). Reports about the code in this repository and its published
container images and Helm chart are in scope. A misconfiguration of a
self-hosted deployment (for example, weak or reused secrets, or exposing a port
beyond loopback) is the operator's responsibility, not a vulnerability in this
project, unless a shipped default is itself unsafe.
