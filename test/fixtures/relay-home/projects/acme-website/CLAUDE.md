# Acme Website

Marketing site for Acme. Static-rendered, Go backend for the contact form
and a tiny analytics aggregator. No customer PII passes through here.

## Run locally

```bash
go run ./cmd/server
# visits: http://localhost:8080
```

## Layout

- `src/main.go` — HTTP entry point. Wires routes and templates.
- `src/handlers.go` — handler logic for `/contact` and `/health`.
- `docs/architecture.md` — request flow and deploy topology.
- `skills/deploy/SKILL.md` — runs the production deploy.
- `skills/triage-issue/SKILL.md` — walks through a GitHub issue triage.

## Working agreements

- Don't add dependencies without a paragraph in `docs/architecture.md`
  explaining what it earns us.
- Templates live in `src/templates/`. Keep them small; if logic grows,
  push it to handlers and pass plain data structures in.
- Contact form submissions go to the `inbox` channel — never log message
  bodies to stdout (PII risk even though we claim none passes through).
