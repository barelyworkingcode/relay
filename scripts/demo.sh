#!/usr/bin/env bash
# demo.sh — launch relay against an isolated copy of the test fixture tree.
#
# Why this exists:
#   - Reproducible screenshots / screencasts of a populated install.
#   - Never touches ~/Library/Application Support/relay/ (the real config).
#   - Same fixture set is what the test suite uses; demos can't drift from tests.
#
# Usage:
#   scripts/demo.sh                          # launch with the current /tmp/relay-demo-home/
#   scripts/demo.sh --reset                  # wipe and recopy fixtures first
#   scripts/demo.sh --scenario <name>        # overlay scenarios/<name>/ on top of relay-home/
#   scripts/demo.sh --build                  # rebuild relay before launching
#   scripts/demo.sh --help
#
# Recording recipes:
#   - Window: 1440x900 for screencasts; menu bar visible.
#   - QuickTime: File → New Screen Recording, click the dropdown, choose
#     "Built-in Microphone" off, capture region matching the window.
#   - CleanShot: Cmd+Shift+5, record selected area, FPS=30, MP4 output.
#   - Always run --reset between takes for deterministic state.

set -e

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FIXTURE_SRC="$REPO_ROOT/test/fixtures/relay-home"
SCENARIO_DIR="$REPO_ROOT/test/fixtures/scenarios"
DEMO_HOME="/tmp/relay-demo-home"

RESET=false
SCENARIO=""
BUILD=false

usage() {
    sed -n '1,/^set -e/p' "$0" | sed 's/^# \{0,1\}//' | head -n -1
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --reset)    RESET=true; shift ;;
        --scenario) SCENARIO="$2"; shift 2 ;;
        --build)    BUILD=true; shift ;;
        --help|-h)  usage ;;
        *) echo "unknown flag: $1"; exit 1 ;;
    esac
done

if [[ ! -d "$FIXTURE_SRC" ]]; then
    echo "✗ fixture tree missing: $FIXTURE_SRC" >&2
    exit 1
fi

if [[ -n "$SCENARIO" ]] && [[ ! -d "$SCENARIO_DIR/$SCENARIO" ]]; then
    echo "✗ unknown scenario: $SCENARIO" >&2
    echo "  available:"
    ls "$SCENARIO_DIR" | sed 's/^/    /'
    exit 1
fi

# Build first if asked. Reuses the canonical build path so the demo
# always runs the same binary the user would install.
if $BUILD; then
    echo "→ building relay"
    "$REPO_ROOT/build.sh"
fi

# Copy fixture set to a writable, isolated location.
if $RESET; then
    echo "→ resetting $DEMO_HOME"
    rm -rf "$DEMO_HOME"
fi

if [[ ! -d "$DEMO_HOME" ]]; then
    echo "→ copying fixtures → $DEMO_HOME"
    mkdir -p "$DEMO_HOME"
    cp -R "$FIXTURE_SRC"/. "$DEMO_HOME"/
fi

# Apply scenario overlay (overwrites any files that conflict with the base).
if [[ -n "$SCENARIO" ]]; then
    echo "→ applying scenario: $SCENARIO"
    # Skip README.md from the scenario dir — it's documentation, not state.
    find "$SCENARIO_DIR/$SCENARIO" -mindepth 1 -name README.md -prune -o -print | while read -r src; do
        rel="${src#$SCENARIO_DIR/$SCENARIO/}"
        [[ -z "$rel" ]] && continue
        dest="$DEMO_HOME/$rel"
        if [[ -d "$src" ]]; then
            mkdir -p "$dest"
        else
            cp "$src" "$dest"
        fi
    done
fi

# Substitute ${RELAY_HOME} placeholders in settings.json so project paths
# resolve to the demo tree, not the fixture source.
SETTINGS="$DEMO_HOME/settings.json"
if [[ -f "$SETTINGS" ]] && grep -q '${RELAY_HOME}' "$SETTINGS"; then
    echo "→ substituting \${RELAY_HOME} → $DEMO_HOME"
    # Use a tempfile to make the substitution atomic.
    tmp="$(mktemp)"
    sed "s|\${RELAY_HOME}|$DEMO_HOME|g" "$SETTINGS" > "$tmp"
    mv "$tmp" "$SETTINGS"
fi

RELAY_BIN="/Applications/Relay.app/Contents/MacOS/relay"
if [[ ! -x "$RELAY_BIN" ]]; then
    echo "✗ relay binary not installed at $RELAY_BIN" >&2
    echo "  run: ./build.sh"
    exit 1
fi

# Stop any running relay so the demo instance is the only one.
pkill -x relay 2>/dev/null && sleep 1 || true

echo "→ launching relay --config-dir $DEMO_HOME"
echo "  (Ctrl+C in this terminal stops the demo)"
exec "$RELAY_BIN" --config-dir "$DEMO_HOME"
