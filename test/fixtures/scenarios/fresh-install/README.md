# Scenario: fresh-install

Empty settings.json with no projects, MCPs, or services. Used to film
the "first launch" onboarding flow without manually wiping a real
install.

Overlay strategy: replaces `relay-home/settings.json` entirely. Project
directories under `relay-home/projects/` are still copied but never
referenced.
