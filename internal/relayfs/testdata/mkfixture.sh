#!/bin/bash
# Builds a fixture root plus out-of-root canaries. Layer-1 verification uses
# this directly; layers 2 and 3 reuse it through relay.
set -euo pipefail
BASE="${1:?usage: mkfixture.sh <base-dir>}"
# The fixture plants a "deny delete" ACL; clear ACLs before teardown or the
# fixture cannot remove its own previous run.
[ -e "$BASE" ] && chmod -RN "$BASE" 2>/dev/null
rm -rf "$BASE"; mkdir -p "$BASE/root" "$BASE/outside"
R="$BASE/root"

echo "TOP SECRET - must never be reachable" > "$BASE/outside/secret.txt"
mkdir -p "$BASE/outside/secretdir"; echo canary > "$BASE/outside/secretdir/c.txt"

mkdir -p "$R/notes" "$R/src" "$R/empty"
printf 'a=1\nb=2\nc=3\n'                        > "$R/notes/config.txt"
printf 'meeting notes\n'                        > "$R/notes/meeting.md"
printf 'x\nx\nx\n'                              > "$R/notes/repeat.txt"
printf '\xef\xbb\xbfBOM\r\nCRLF line\r\nno final newline' > "$R/notes/crlf-bom.txt"
printf 'ok=1\nbad=\xff\xfe\nmore=2\n'           > "$R/notes/latin1.txt"     # NOT valid UTF-8
printf 'plain'                                  > "$R/notes/nofinalnl.txt"
printf 'héllo wörld ünicode\n'                  > "$R/notes/utf8.txt"
head -c 300000 /dev/urandom                     > "$R/src/big.bin"
printf '%.0sX' {1..5000} > "$R/src/longline.txt"; echo >> "$R/src/longline.txt"

# 1x1 PNG, byte-exact
printf '\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\nIDATx\x9cc\x00\x01\x00\x00\x05\x00\x01\r\n-\xb4\x00\x00\x00\x00IEND\xaeB`\x82' > "$R/src/pixel.png"

# escape vectors
ln -s "$BASE/outside/secret.txt"    "$R/link-out-file"
ln -s "$BASE/outside/nonexistent"   "$R/link-out-dangling"
ln -s "$BASE/outside/secretdir"     "$R/link-out-dir"
ln -s /etc                          "$R/link-etc"
ln -s "$R/notes"                    "$R/link-in-dir"          # ABSOLUTE, inside root: refused by os.Root (A8b)
ln -s notes                         "$R/rel-link-in"          # relative, inside root: must work (A8a)
ln -s ../outside                    "$R/rel-link-out"         # relative, escaping: refused
ln -s cycle-b "$R/cycle-a"; ln -s cycle-a "$R/cycle-b"

# hostile filenames
printf 'newline named\n' > "$R/notes/$(printf 'we\nird').txt"
printf 'tab named\n'     > "$R/notes/$(printf 'ta\tb').txt"
printf 'quote named\n'   > "$R/notes/qu\"ote.txt"

# permission/metadata fixtures
printf 'perms\n' > "$R/src/mode644.txt"; chmod 644 "$R/src/mode644.txt"
printf 'perms\n' > "$R/src/mode600.txt"; chmod 600 "$R/src/mode600.txt"
printf 'perms\n' > "$R/src/mode755.txt"; chmod 755 "$R/src/mode755.txt"
# attrs.txt tests that metadata SURVIVES a replace, so its ACE must not block
# the rename a replace commits through.
printf 'attrs\n' > "$R/src/attrs.txt"
xattr -w com.example.marker "fsmcp-xattr-canary" "$R/src/attrs.txt"
chmod +a "everyone allow read" "$R/src/attrs.txt" 2>/dev/null || true

# guarded.txt tests the opposite: a deny-delete ACE blocks rename(2) on both
# sides, so the file cannot be replaced and the write must be refused.
printf 'protected\n' > "$R/src/guarded.txt"
chmod +a "everyone deny delete" "$R/src/guarded.txt" 2>/dev/null || true

echo "$R"
