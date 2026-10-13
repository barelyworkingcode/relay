#!/bin/sh
# Optional Claude hook. Stdin is display data; it never reaches an action API.
umask 077
dir="$HOME/.claude/relay-outside-reports"
[ ! -L "$HOME/.claude" ] && [ ! -L "$dir" ] || exit 1
mkdir -p "$dir" || exit 1
chmod 700 "$dir" || exit 1
file=$(mktemp "$dir/.report.XXXXXXXX") || exit 1
trap 'rm -f "$file"' EXIT HUP INT TERM
# A bad or oversize event is discarded. Relay validates the native hook shape.
head -c 65537 > "$file"
[ "$(wc -c < "$file")" -le 65536 ] || exit 1
mv "$file" "$dir/report.${file##*.}.json"
