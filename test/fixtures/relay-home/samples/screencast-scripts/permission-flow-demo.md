# Screencast: Permission Flow

Goal: show how a project-scoped token gates MCP access. ~45 seconds.

## Setup

```bash
./scripts/demo.sh --reset --scenario permission-prompt-active
```

## Beat sheet

1. (0:00) Open the settings window, switch to the Projects tab. Show the
   field-notes project — `allowed_mcp_ids: ["fs"]`. Read it aloud.
2. (0:10) Switch to the Service Inspector. The scenario has dropped in a
   pending permission prompt — point at it.
3. (0:20) Click "Approve". Watch the permission badge transition.
4. (0:30) Switch back to Projects. Note that the permission was
   project-scoped — it does not apply to other projects.
5. (0:40) End on the menu bar icon.
