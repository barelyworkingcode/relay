---
name: deploy
description: Roll a production release of the Acme website — tests, image build, orchestrator deploy.
---

# Deploy

Use this when the user says "deploy", "ship it", or "push to prod".

## Steps

1. Confirm the working tree is clean: `git status`. Abort if not.
2. Run `go test ./...` and stop the deploy on any failure.
3. Build the container: `docker build -t acme-website:$(git rev-parse --short HEAD) .`
4. Tag and push to the registry (`registry.internal/acme-website`).
5. Roll the deployment via the orchestrator CLI; wait for healthcheck green.
6. Smoke-test `/health` and `/contact` against the canary endpoint.
7. Post the SHA and rollout link in the team channel.

## Rollback

If smoke fails, the orchestrator CLI's `rollback` subcommand reverts to the
previous tag. Do this before debugging — restore traffic first.
