#!/bin/bash
#
# check-test-build.sh absent|present BINARY
#
# Says whether a relay binary carries the test-build seams.
#   absent   release builds: no seam symbol, no relaytest build tag
#   present  --test-build builds: every seam and the tag are there
# Exit 0 on a match, 1 with the reason on stderr, 2 on bad usage.
#
# The byte check needs no Go toolchain: the build info Go embeds in the
# binary names the tag as plain text.

set -u

if [ $# -ne 2 ] || { [ "$1" != absent ] && [ "$1" != present ]; }; then
    echo "usage: $0 absent|present BINARY" >&2
    exit 2
fi
mode="$1"
bin="$2"
if [ ! -f "$bin" ]; then
    echo "check-test-build: no such file: $bin" >&2
    exit 2
fi

fail() { echo "check-test-build ($mode): $*" >&2; exit 1; }

symbols="$(go tool nm "$bin" 2>/dev/null)" || fail "go tool nm could not read $bin"
# Positive control: an empty or unreadable symbol table would make "absent" pass for the wrong reason.
echo "$symbols" | /usr/bin/grep -q ' main\.main$' || fail "symbol table has no main.main"

# One symbol per seam, each reachable only through the seam, so the linker keeps it in a test build.
seams=(
    '/internal/presence/testapprover\.\(\*Approver\)\.EvaluateOp$'
    '/internal/sealed\.\(\*fileKeyring\)\.Load$'
    ' main\.\(\*testClock\)\.Now$'
    ' main\.adminDebugClock$'
)

found=()
missing=()
for s in "${seams[@]}"; do
    if echo "$symbols" | /usr/bin/grep -Eq "$s"; then
        found+=("$s")
    else
        missing+=("$s")
    fi
done
other_testapprover=false
echo "$symbols" | /usr/bin/grep -q '/internal/presence/testapprover\.' && other_testapprover=true

has_tag=false
go version -m "$bin" 2>/dev/null | /usr/bin/grep -Eq 'build[[:space:]]+-tags=.*relaytest' && has_tag=true

bytes_found="$(LC_ALL=C /usr/bin/grep -c -a -E -e '-tags=[^ ]*relaytest' "$bin" || true)"
bytes_found="${bytes_found:-0}"

case "$mode" in
absent)
    [ ${#found[@]} -eq 0 ] || fail "binary contains test seams: ${found[*]}"
    $other_testapprover && fail "binary contains internal/presence/testapprover symbols"
    $has_tag && fail "build info names the relaytest tag"
    [ "$bytes_found" -eq 0 ] || fail "binary bytes contain the relaytest tag ($bytes_found matches)"
    ;;
present)
    [ ${#missing[@]} -eq 0 ] || fail "binary lacks test seams: ${missing[*]}"
    $has_tag || fail "build info does not name the relaytest tag"
    [ "$bytes_found" -ge 1 ] || fail "binary bytes do not contain the relaytest tag"
    ;;
esac
exit 0
