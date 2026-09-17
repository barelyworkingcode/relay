#!/bin/bash
#
# build.sh — compile, sign, and install relay.
#
# Usage:
#   ./build.sh                  # build + install + launch
#   ./build.sh --test           # run hermetic test suite first; abort install on failure
#   ./build.sh --release        # sign, notarize, emit /tmp/Relay.dmg (implies --test)
#
# Tests run BEFORE install so a broken binary never lands in /Applications.
# Use --test on every developer-machine build; the pre-commit hook already
# gates commits, but install-from-local-changes deserves the same safety net.

set -euo pipefail

APP="Relay.app"
DEST="/Applications/$APP"
RELEASE=false
RUN_TESTS=false

for arg in "$@"; do
    case "$arg" in
        --release) RELEASE=true; RUN_TESTS=true ;;
        --test)    RUN_TESTS=true ;;
        --help|-h)
            # Print the header comment block (lines 3..first non-comment after).
            awk 'NR>=3 && /^[^#]/ {exit} NR>=3 {sub(/^# ?/,""); print}' "$0"
            exit 0 ;;
        *)
            echo "unknown flag: $arg" >&2
            echo "  see ./build.sh --help" >&2
            exit 1 ;;
    esac
done

# Regenerate the settings UI bundle (web/src/* -> internal/webassets/settings.html) FIRST,
# so BOTH the test suite and the build below embed the current source rather than
# a stale committed artifact. esbuild runs in-process via web/gen; no Node.
echo "Bundling settings UI..."
go run ./web/gen

# Run the hermetic test suite up front. Mirrors what .githooks/pre-commit
# runs — keeps the install path consistent with the commit gate.
if $RUN_TESTS; then
    echo "=== Pre-install: hermetic test suite ==="
    if ! go vet ./...; then
        echo "✗ go vet failed; install aborted" >&2
        exit 1
    fi
    if ! go test ./...; then
        echo "✗ tests failed; install aborted" >&2
        echo "  rerun with: go test -v ./..." >&2
        exit 1
    fi
    echo "✓ tests passed"
fi

# Kill running Relay
pkill -x relay 2>/dev/null && echo "Killed running relay" && sleep 1 || true
STAGE="/tmp/relay-build-$$"

# Code signing -- per-binary, innermost first.
# Both branches enable hardened runtime so dev builds catch the same entitlement
# / JIT / dlopen issues that would otherwise only surface at notarization time.
# RELAY_SIGN_IDENTITY lets you pin a specific cert when multiple are present.
# Resolved before either binary is built: relay-sessions' own signing (below)
# needs it first, since its signed CDHash is what gets embedded into relay.
IDENTITY="${RELAY_SIGN_IDENTITY:-$(security find-identity -v -p codesigning | grep "Developer ID Application" | grep -o '"[^"]*"' | head -1 | tr -d '"' || true)}"
if [ -n "$IDENTITY" ]; then
    echo "Signing with: $IDENTITY"
    SIGN_BASE=(--force --sign "$IDENTITY" --options runtime --timestamp)
else
    echo "No Developer ID found, ad-hoc signing"
    # Ad-hoc can't --timestamp (no cert authority), but runtime stays on for parity.
    SIGN_BASE=(--force --sign - --options runtime)
fi

# Build relay-sessions first: it is signed, and its CDHash read back, before
# relay itself is built (see the helper block below) -- R-S9, spikes/SP3.md.
echo "Building relay-sessions..."
CGO_ENABLED=1 go build -o relay-sessions ./cmd/relaysessions

# Build bundle in /tmp
# Use cat to copy binary -- breaks provenance chain that cp preserves
rm -rf "$STAGE"
mkdir -p "$STAGE/$APP/Contents/MacOS" "$STAGE/$APP/Contents/Helpers"
cat relay-sessions > "$STAGE/$APP/Contents/Helpers/relay-sessions"
chmod +x "$STAGE/$APP/Contents/Helpers/relay-sessions"
rm -f relay-sessions

# relay-sessions is signed before relay is even built, with its own minimal
# entitlements (no get-task-allow, no JIT, no apple-events, no TCC --
# RelaySessions.entitlements). This order is load-bearing (spikes/SP3.md):
# relay's own static requirement pins the exact bytes of the binary it will
# later spawn (-ldflags -X below), not merely "signed by our team" -- a
# team-only pin was verified to accept a same-team WRONG binary.
codesign "${SIGN_BASE[@]}" --entitlements RelaySessions.entitlements "$STAGE/$APP/Contents/Helpers/relay-sessions"
HELPER_INFO="$(codesign -dvvvv "$STAGE/$APP/Contents/Helpers/relay-sessions" 2>&1)"
HELPER_CDHASH="$(echo "$HELPER_INFO" | sed -n 's/^CDHash=//p')"
HELPER_TEAM="$(echo "$HELPER_INFO" | sed -n 's/^TeamIdentifier=//p')"
if [ "$HELPER_TEAM" = "not set" ]; then
    # Ad-hoc build: no cert chain exists to pin a team to (spikes/SP3.md's
    # ad-hoc note), so relay gates on CDHash alone -- an empty HelperTeam is
    # how internal/service/codesign_darwin.go knows to skip that check.
    HELPER_TEAM=""
fi
if [ -z "$HELPER_CDHASH" ]; then
    echo "✗ could not read relay-sessions' own CDHash after signing it" >&2
    exit 1
fi
echo "relay-sessions signed: cdhash=$HELPER_CDHASH team=${HELPER_TEAM:-<ad-hoc>}"

# Build relay itself, with the helper's CDHash (and team, if any) embedded --
# cmd/relay/main.go's HelperCDHash/HelperTeam vars, read by codesign_darwin.go
# before relay ever starts or trusts the relay-sessions service.
echo "Building relay..."
RELAY_VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
CGO_ENABLED=1 go build -ldflags "-X main.buildVersion=$RELAY_VERSION -X main.HelperCDHash=$HELPER_CDHASH -X main.HelperTeam=$HELPER_TEAM" -o relay ./cmd/relay

cat relay > "$STAGE/$APP/Contents/MacOS/relay"
chmod +x "$STAGE/$APP/Contents/MacOS/relay"
cat Info.plist > "$STAGE/$APP/Contents/Info.plist"
mkdir -p "$STAGE/$APP/Contents/Resources"
cp AppIcon.icns "$STAGE/$APP/Contents/Resources/AppIcon.icns"

SIGN_ARGS=("${SIGN_BASE[@]}" --entitlements Relay.entitlements)
codesign "${SIGN_ARGS[@]}" "$STAGE/$APP/Contents/MacOS/relay"
codesign "${SIGN_ARGS[@]}" "$STAGE/$APP"

# Fail fast on malformed signatures rather than at launch / notarization.
# --deep also verifies Contents/Helpers/relay-sessions as nested code
# (confirmed in spikes/SP3.md: Helpers is a recognized nested-code location).
codesign --verify --deep --strict --verbose=2 "$STAGE/$APP"

# Move to destination
rm -rf "$DEST"
mv "$STAGE/$APP" "$DEST"
rm -rf "$STAGE"
rm -f relay

echo "Installed to $DEST"

if $RELEASE; then
    if [ -z "$IDENTITY" ]; then
        echo "ERROR: --release requires a Developer ID Application certificate"
        exit 1
    fi

    echo "=== Release: notarizing app ==="
    NOTARIZE_ZIP="/tmp/Relay-notarize-$$.zip"
    ditto -c -k --keepParent "$DEST" "$NOTARIZE_ZIP"
    xcrun notarytool submit "$NOTARIZE_ZIP" --keychain-profile "relay-notarize" --wait
    rm -f "$NOTARIZE_ZIP"
    xcrun stapler staple "$DEST"

    echo "=== Release: creating DMG ==="
    DMG_STAGE="/tmp/relay-dmg-$$"
    DMG_OUT="/tmp/Relay.dmg"
    rm -rf "$DMG_STAGE" "$DMG_OUT"
    mkdir -p "$DMG_STAGE"
    cp -R "$DEST" "$DMG_STAGE/"
    ln -s /Applications "$DMG_STAGE/Applications"
    hdiutil create -volname "Relay" -srcfolder "$DMG_STAGE" -ov -format UDZO "$DMG_OUT"
    rm -rf "$DMG_STAGE"

    echo "=== Release: notarizing DMG ==="
    xcrun notarytool submit "$DMG_OUT" --keychain-profile "relay-notarize" --wait
    xcrun stapler staple "$DMG_OUT"

    echo "DMG ready: $DMG_OUT"
else
    open "$DEST"
fi
