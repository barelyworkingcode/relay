# Architecture

```
[Visitor] --HTTP--> [Cloudflare] --HTTP--> [Go server (this repo)]
                                              |
                                              ├── /          static templates
                                              ├── /contact   forwards to inbox channel
                                              └── /health    liveness
```

## Deploy

`skills/deploy/SKILL.md` runs the canonical deploy. Summary:

1. `go test ./...`
2. `docker build` and push to the registry.
3. Roll the deployment via the orchestrator.

There is no database. The contact form posts to an inbox channel via HTTPS;
bodies are not stored on this server.

## Why html/template, not a JS framework?

We render 4 pages. The site changes <1x/month. Anything heavier than
`html/template` would be a tax on every future maintainer.
