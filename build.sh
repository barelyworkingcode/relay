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

# Build Go binary with CGO enabled
echo "Building relay..."
CGO_ENABLED=1 go build -o relay .

# Build bundle in /tmp
# Use cat to copy binary -- breaks provenance chain that cp preserves
rm -rf "$STAGE"
mkdir -p "$STAGE/$APP/Contents/MacOS"
cat relay > "$STAGE/$APP/Contents/MacOS/relay"
chmod +x "$STAGE/$APP/Contents/MacOS/relay"
cat Info.plist > "$STAGE/$APP/Contents/Info.plist"
mkdir -p "$STAGE/$APP/Contents/Resources"
cp AppIcon.icns "$STAGE/$APP/Contents/Resources/AppIcon.icns"

# Code signing -- per-binary, innermost first
IDENTITY=$(security find-identity -v -p codesigning | grep "Developer ID Application" | grep -o '"[^"]*"' | head -1 | tr -d '"' || true)
if [ -n "$IDENTITY" ]; then
    echo "Signing with: $IDENTITY"
    codesign --force --sign "$IDENTITY" --entitlements Relay.entitlements --options runtime "$STAGE/$APP/Contents/MacOS/relay"
    codesign --force --sign "$IDENTITY" --entitlements Relay.entitlements --options runtime "$STAGE/$APP"
else
    echo "No Developer ID found, ad-hoc signing"
    codesign --force --sign - --entitlements Relay.entitlements "$STAGE/$APP/Contents/MacOS/relay"
    codesign --force --sign - --entitlements Relay.entitlements "$STAGE/$APP"
fi

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
