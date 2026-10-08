#!/bin/bash
#
# check-test-approver.sh absent|present BINARY
#
# Says whether a relay binary carries the test approver.
#   absent   release builds: no testapprover symbol, no testapprover build tag
#   present  --test-approver builds: the approver and the tag are both there
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
    echo "check-test-approver: no such file: $bin" >&2
    exit 2
fi

fail() { echo "check-test-approver ($mode): $*" >&2; exit 1; }

symbols="$(go tool nm "$bin" 2>/dev/null)" || fail "go tool nm could not read $bin"
# Positive control: an empty or unreadable symbol table would make "absent" pass for the wrong reason.
echo "$symbols" | /usr/bin/grep -q ' main\.main$' || fail "symbol table has no main.main"

has_symbol=false
echo "$symbols" | /usr/bin/grep -q '/internal/presence/testapprover\.' && has_symbol=true
has_evaluate_op=false
echo "$symbols" | /usr/bin/grep -q '/internal/presence/testapprover\.(\*Approver)\.EvaluateOp$' && has_evaluate_op=true

has_tag=false
go version -m "$bin" 2>/dev/null | /usr/bin/grep -Eq 'build[[:space:]]+-tags=.*testapprover' && has_tag=true

bytes_found="$(LC_ALL=C /usr/bin/grep -c -a -e '-tags=testapprover' "$bin" || true)"
bytes_found="${bytes_found:-0}"

case "$mode" in
absent)
    $has_symbol && fail "binary contains testapprover symbols"
    $has_tag && fail "build info names the testapprover tag"
    [ "$bytes_found" -eq 0 ] || fail "binary bytes contain the testapprover tag ($bytes_found matches)"
    ;;
present)
    $has_evaluate_op || fail "binary has no testapprover EvaluateOp symbol"
    $has_tag || fail "build info does not name the testapprover tag"
    [ "$bytes_found" -ge 1 ] || fail "binary bytes do not contain the testapprover tag"
    ;;
esac
exit 0
