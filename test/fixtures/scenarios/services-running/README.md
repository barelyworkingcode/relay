# Scenario: services-running

Overlay that simulates a populated, healthy install with both services
live and the inspector showing recent activity. Used by the "tour the
tray" screencast.

This directory is overlaid on top of `relay-home/` by
`scripts/demo.sh --scenario services-running`. Anything here that shares
a path with `relay-home/` wins.
