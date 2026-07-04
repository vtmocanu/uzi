# syntax=docker/dockerfile:1
# forge-fake: a throwaway GitLab stand-in for the M6 E2E harness only. Never
# built or shipped as part of the product. Runs as root so it can bind :443
# (the api's SSRF allowlist normalizes the base URL to the default https port).
FROM node:22-alpine@sha256:16e22a550f3863206a3f701448c45f7912c6896a62de43add43bb9c86130c3e2
WORKDIR /app
COPY forge-fake.mjs ./
ENTRYPOINT ["node", "forge-fake.mjs"]
